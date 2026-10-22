package cacheserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BlobStore keeps completed blobs and the entry index off the server's disk, so
// a replacement server picks up where the last one stopped. With a store set,
// the local directory only stages uploads and holds a working copy of the index.
type BlobStore interface {
	// Put copies the fully assembled blob at path to the store under id.
	Put(ctx context.Context, id uint64, path string) error
	// Get streams length bytes of blob id starting at offset.
	Get(ctx context.Context, id uint64, offset, length int64) (io.ReadCloser, error)
	Delete(ctx context.Context, id uint64) error
	// LoadIndex returns the stored index, or nil when there is none yet.
	LoadIndex(ctx context.Context) ([]byte, error)
	SaveIndex(ctx context.Context, data []byte) error
}

// errStoreUnavailable maps to 507, which the toolkit's Azure SDK does not
// retry: a job whose cache backend is down should drop the cache at once, not
// spend its retry budget waiting for it.
var errStoreUnavailable = errors.New("cache store unavailable")

const (
	storeCallTimeout  = 10 * time.Second
	indexSyncInterval = 5 * time.Second
	localFillTimeout  = 30 * time.Minute
)

type indexSync struct {
	mu    sync.Mutex
	data  []byte
	dirty bool
}

// SetLocalCacheSize keeps up to n bytes of completed blobs on local disk in
// front of the store, least recently used dropped first; 0 keeps none.
func (s *Server) SetLocalCacheSize(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localCap = n
	s.trimLocal(0)
}

// SetBlobStore moves blob storage to store. An empty local index is seeded from
// the store's, so a fresh server serves what its predecessor finalized; blobs
// already on local disk become local copies.
func (s *Server) SetBlobStore(store BlobStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
	defer s.adoptLocal()
	if len(s.entries) > 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
	defer cancel()
	data, err := store.LoadIndex(ctx)
	if err != nil {
		return fmt.Errorf("load cache index from store: %w", err)
	}
	if data == nil {
		return nil
	}
	if err := s.loadIndex(data); err != nil {
		return err
	}
	s.log.Info("cache index loaded from store", "entries", len(s.entries))
	return s.save()
}

// StartIndexSync uploads the index to the store whenever it changed, at most
// every indexSyncInterval, and once more when stopped.
func (s *Server) StartIndexSync() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(indexSyncInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				s.flushIndex()
				return
			case <-t.C:
				s.flushIndex()
			}
		}
	}()
	return func() { close(done); <-finished }
}

func (s *Server) flushIndex() {
	s.index.mu.Lock()
	data, dirty := s.index.data, s.index.dirty
	s.index.dirty = false
	s.index.mu.Unlock()
	if !dirty || s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
	defer cancel()
	if err := s.store.SaveIndex(ctx, data); err != nil {
		s.log.Warn("cache index upload failed; will retry", "err", err)
		s.index.mu.Lock()
		if !s.index.dirty {
			s.index.data, s.index.dirty = data, true
		}
		s.index.mu.Unlock()
	}
}

// putBlob hands an assembled local blob to the store, keeping it as a local copy
// when there is room for one.
func (s *Server) putBlob(r *http.Request, id uint64) error {
	if s.store == nil {
		return nil
	}
	path := s.blobPath(id)
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := s.store.Put(r.Context(), id, path); err != nil {
		_ = os.Remove(path)
		s.log.Warn("cache blob upload to store failed", "id", id, "err", err)
		return errStoreUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stored == nil {
		s.stored = make(map[uint64]int64)
	}
	s.stored[id] = fi.Size()
	s.keepLocal(id, fi.Size())
	return nil
}

// keepLocal records the blob at blobPath(id) as a local copy, or removes it when
// copies are off or it cannot fit. The caller holds s.mu.
func (s *Server) keepLocal(id uint64, size int64) {
	if s.localCap <= 0 || size > s.localCap {
		_ = os.Remove(s.blobPath(id))
		return
	}
	if s.local == nil {
		s.local = make(map[uint64]int64)
	}
	s.local[id] = size
	s.trimLocal(id)
}

// adoptLocal turns blobs already on disk for completed entries into local
// copies, e.g. after a restart. The caller holds s.mu.
func (s *Server) adoptLocal() {
	for id, e := range s.entries {
		if !e.Complete {
			continue
		}
		if fi, err := os.Stat(s.blobPath(id)); err == nil && fi.Size() == e.Size {
			s.keepLocal(id, e.Size)
		}
	}
}

// trimLocal drops the least recently used local copies until they fit the cap,
// never keep. The store still holds every dropped blob. The caller holds s.mu.
func (s *Server) trimLocal(keep uint64) {
	var total int64
	for _, n := range s.local {
		total += n
	}
	for total > s.localCap && len(s.local) > 0 {
		var victim *Entry
		for id := range s.local {
			e := s.entries[id]
			if id == keep || e == nil {
				continue
			}
			if victim == nil || lessRecentlyUsed(e, victim) {
				victim = e
			}
		}
		if victim == nil {
			return
		}
		total -= s.local[victim.ID]
		delete(s.local, victim.ID)
		_ = os.Remove(s.blobPath(victim.ID))
	}
}

// openLocal opens blob id from local disk: always without a store, and only a
// recorded local copy with one.
func (s *Server) openLocal(id uint64) (*os.File, error) {
	if s.store != nil {
		s.mu.Lock()
		_, ok := s.local[id]
		s.mu.Unlock()
		if !ok {
			return nil, os.ErrNotExist
		}
	}
	return os.Open(s.blobPath(id))
}

// fillLocal fetches a completed blob from the store into a local copy in the
// background, once at a time per blob, so the next restore reads from disk.
func (s *Server) fillLocal(e *Entry) {
	s.mu.Lock()
	if s.localCap <= 0 || e.Size <= 0 || e.Size > s.localCap || s.filling[e.ID] {
		s.mu.Unlock()
		return
	}
	if _, ok := s.local[e.ID]; ok {
		s.mu.Unlock()
		return
	}
	if s.filling == nil {
		s.filling = make(map[uint64]bool)
	}
	s.filling[e.ID] = true
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.filling, e.ID)
			s.mu.Unlock()
		}()
		tmp := s.blobPath(e.ID) + ".fill"
		if err := s.fetchBlob(e.ID, e.Size, tmp); err != nil {
			_ = os.Remove(tmp)
			s.log.Warn("cache local copy fetch failed", "id", e.ID, "err", err)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur := s.entries[e.ID]; cur != e || !e.Complete {
			_ = os.Remove(tmp)
			return
		}
		if err := os.Rename(tmp, s.blobPath(e.ID)); err != nil {
			_ = os.Remove(tmp)
			return
		}
		s.keepLocal(e.ID, e.Size)
	}()
}

func (s *Server) fetchBlob(id uint64, size int64, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), localFillTimeout)
	defer cancel()
	body, err := s.store.Get(ctx, id, 0, size)
	if err != nil {
		return err
	}
	defer body.Close()
	n, err := writeFile(path, body)
	if err == nil && n != size {
		err = fmt.Errorf("fetched %d of %d bytes", n, size)
	}
	return err
}

// removeBlob deletes a blob wherever it lives; the caller holds s.mu. Store deletes run in the
// background: an orphaned object costs storage, never correctness.
func (s *Server) removeBlob(id uint64) {
	_ = os.Remove(s.blobPath(id))
	if s.store == nil {
		return
	}
	delete(s.stored, id)
	delete(s.local, id)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), storeCallTimeout)
		defer cancel()
		if err := s.store.Delete(ctx, id); err != nil {
			s.log.Warn("cache blob delete from store failed", "id", id, "err", err)
		}
	}()
}

// serveFromStore answers the toolkit's getProperties (HEAD) and segmented
// download (single-range GETs) by streaming the matching range from the store.
func (s *Server) serveFromStore(w http.ResponseWriter, r *http.Request, e *Entry) {
	size := e.Size
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Last-Modified", time.Unix(e.CreatedAt, 0).UTC().Format(http.TimeFormat))
	if r.Method == http.MethodHead || size == 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	start, end, partial, ok := parseRange(r.Header.Get("Range"), size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body, err := s.store.Get(r.Context(), e.ID, start, end-start+1)
	if err != nil {
		s.log.Warn("cache blob read from store failed", "id", e.ID, "err", err)
		http.Error(w, errStoreUnavailable.Error(), http.StatusInsufficientStorage)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, body)
}

// parseRange reads a single "bytes=" range; anything else serves the whole blob.
func parseRange(h string, size int64) (start, end int64, partial, ok bool) {
	if h == "" || !strings.HasPrefix(h, "bytes=") || strings.Contains(h, ",") {
		return 0, size - 1, false, true
	}
	a, b, found := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	if !found {
		return 0, 0, false, false
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		start, end = max(size-n, 0), size-1
	default:
		var err error
		if start, err = strconv.ParseInt(a, 10, 64); err != nil {
			return 0, 0, false, false
		}
		end = size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil {
				return 0, 0, false, false
			}
			end = min(end, size-1)
		}
	}
	if start < 0 || start >= size || end < start {
		return 0, 0, false, false
	}
	return start, end, true, true
}
