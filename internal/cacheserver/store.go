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
)

type indexSync struct {
	mu    sync.Mutex
	data  []byte
	dirty bool
}

// SetBlobStore moves blob storage to store. An empty local index is seeded from
// the store's, so a fresh server serves what its predecessor finalized.
func (s *Server) SetBlobStore(store BlobStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
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

// putBlob hands an assembled local blob to the store and drops the local copy.
func (s *Server) putBlob(r *http.Request, id uint64) error {
	if s.store == nil {
		return nil
	}
	path := s.blobPath(id)
	defer os.Remove(path)
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := s.store.Put(r.Context(), id, path); err != nil {
		s.log.Warn("cache blob upload to store failed", "id", id, "err", err)
		return errStoreUnavailable
	}
	s.mu.Lock()
	if s.stored == nil {
		s.stored = make(map[uint64]int64)
	}
	s.stored[id] = fi.Size()
	s.mu.Unlock()
	return nil
}

// removeBlob deletes a blob wherever it lives; the caller holds s.mu. Store deletes run in the
// background: an orphaned object costs storage, never correctness.
func (s *Server) removeBlob(id uint64) {
	_ = os.Remove(s.blobPath(id))
	if s.store == nil {
		return
	}
	delete(s.stored, id)
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
