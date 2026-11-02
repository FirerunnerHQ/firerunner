package cacheserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu      sync.Mutex
	blobs   map[uint64][]byte
	index   []byte
	failPut bool
	failGet bool
	getGate chan struct{} // when set, Get waits for it to close
	// When set, Put signals putEntered, then waits for putGate to close.
	putEntered, putGate chan struct{}
}

func newMemStore() *memStore { return &memStore{blobs: map[uint64][]byte{}} }

func (m *memStore) Put(_ context.Context, id uint64, path string) error {
	if m.failPut {
		return errors.New("store down")
	}
	// Read before the gate, as s3store opens the file before it uploads: an
	// unlink meanwhile does not stop the upload.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if m.putGate != nil {
		m.putEntered <- struct{}{}
		<-m.putGate
	}
	m.mu.Lock()
	m.blobs[id] = data
	m.mu.Unlock()
	return nil
}

func (m *memStore) Get(_ context.Context, id uint64, offset, length int64) (io.ReadCloser, error) {
	if m.getGate != nil {
		<-m.getGate
	}
	if m.failGet {
		return nil, errors.New("store down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[id]
	if !ok {
		return nil, fmt.Errorf("blob %d: %w", id, fs.ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
}

func (m *memStore) Delete(_ context.Context, id uint64) error {
	m.mu.Lock()
	delete(m.blobs, id)
	m.mu.Unlock()
	return nil
}

func (m *memStore) LoadIndex(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.index, nil
}

func (m *memStore) SaveIndex(_ context.Context, data []byte) error {
	m.mu.Lock()
	m.index = append([]byte(nil), data...)
	m.mu.Unlock()
	return nil
}

func newStoreServer(t *testing.T, store BlobStore) (*Server, *httptest.Server) {
	t.Helper()
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetBlobStore(store); err != nil {
		t.Fatalf("SetBlobStore: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func downloadURL(t *testing.T, base, key string) string {
	t.Helper()
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("owner/pinned"), Key: key, Version: "v1"})
	if get["ok"] != true {
		t.Fatalf("lookup %q missed: %v", key, get)
	}
	return get["signed_download_url"].(string)
}

func TestStoreRoundTripWithRanges(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	payload := []byte("0123456789abcdefghij")
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
	putBlob(t, create["signed_upload_url"].(string), payload)
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1", SizeBytes: fmt.Sprint(len(payload))}); fin["ok"] != true {
		t.Fatalf("finalize: %v", fin)
	}
	if entries, _ := os.ReadDir(s.dir + "/blobs"); len(entries) != 0 {
		t.Fatalf("local blob kept after upload to the store: %v", entries)
	}
	url := downloadURL(t, ts.URL, "k")

	head, err := http.Head(url)
	if err != nil || head.StatusCode != http.StatusOK || head.ContentLength != int64(len(payload)) {
		t.Fatalf("HEAD = %v %v len %d", err, head.StatusCode, head.ContentLength)
	}
	for _, tc := range []struct {
		rng, want string
		code      int
	}{
		{"", string(payload), http.StatusOK},
		{"bytes=0-3", "0123", http.StatusPartialContent},
		{"bytes=10-", "abcdefghij", http.StatusPartialContent},
		{"bytes=-4", "ghij", http.StatusPartialContent},
		{"bytes=18-99", "ij", http.StatusPartialContent},
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if tc.rng != "" {
			req.Header.Set("Range", tc.rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %q: %v", tc.rng, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.code || string(body) != tc.want {
			t.Fatalf("GET %q = %d %q, want %d %q", tc.rng, resp.StatusCode, body, tc.code, tc.want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=50-")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-range GET = %d, want 416", resp.StatusCode)
	}
}

func TestStoreStagedBlocksAreNotFinalizableBeforeCommit(t *testing.T) {
	_, ts := newStoreServer(t, newMemStore())
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
	block := create["signed_upload_url"].(string) + "&comp=block&blockid=" + base64.StdEncoding.EncodeToString([]byte("b0"))
	req, _ := http.NewRequest(http.MethodPut, block, strings.NewReader("half"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("stage block: %v %v", err, resp.StatusCode)
	}
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1", SizeBytes: "4"}); fin["ok"] == true {
		t.Fatalf("finalize succeeded with blocks staged but never committed: %v", fin)
	}
}

func TestStoreDownFailsFastWithoutRetryableStatus(t *testing.T) {
	store := newMemStore()
	_, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 8)
	url := downloadURL(t, ts.URL, "k")

	store.failGet = true
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("GET with store down = %v %v, want 507", err, resp.StatusCode)
	}
	store.failPut = true
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k2", Version: "v1"})
	req, _ := http.NewRequest(http.MethodPut, create["signed_upload_url"].(string), strings.NewReader("data"))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("PUT with store down = %v %v, want 507", err, resp.StatusCode)
	}
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "k2", Version: "v1", SizeBytes: "4"}); fin["ok"] == true {
		t.Fatalf("finalize succeeded after the store refused the blob: %v", fin)
	}
}

func TestStoreSeedsAFreshServer(t *testing.T) {
	store := newMemStore()
	first, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 16)
	stop := first.StartIndexSync()
	stop()
	if store.index == nil {
		t.Fatal("index never reached the store")
	}

	_, ts2 := newStoreServer(t, store)
	resp, err := http.Get(downloadURL(t, ts2.URL, "k"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) != 16 {
		t.Fatalf("fresh server GET = %d len %d, want 200 len 16", resp.StatusCode, len(body))
	}
}

func TestIndexSyncRetriesAfterFailure(t *testing.T) {
	store := &flakyIndexStore{memStore: newMemStore(), failures: 1}
	s, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 4)
	s.flushIndex()
	if store.index != nil {
		t.Fatal("index saved despite the injected failure")
	}
	deadline := time.Now().Add(time.Second)
	for store.index == nil && time.Now().Before(deadline) {
		s.flushIndex()
	}
	if store.index == nil {
		t.Fatal("index not retried after a failed upload")
	}
}

type flakyIndexStore struct {
	*memStore
	failures int
}

func (f *flakyIndexStore) SaveIndex(ctx context.Context, data []byte) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("transient")
	}
	return f.memStore.SaveIndex(ctx, data)
}

func getBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func TestLocalCopyServesWhileStoreDown(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	s.SetLocalCacheSize(1 << 20)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 64)

	store.failGet = true
	if code, body := getBody(t, downloadURL(t, ts.URL, "k")); code != http.StatusOK || len(body) != 64 {
		t.Fatalf("GET with store down = %d len %d, want the local copy", code, len(body))
	}
}

func TestFreshServerFillsLocalCopyFromStore(t *testing.T) {
	store := newMemStore()
	first, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 64)
	first.StartIndexSync()()

	fresh, ts2 := newStoreServer(t, store)
	fresh.SetLocalCacheSize(1 << 20)
	url := downloadURL(t, ts2.URL, "k")
	if code, body := getBody(t, url); code != http.StatusOK || len(body) != 64 {
		t.Fatalf("first GET = %d len %d", code, len(body))
	}
	waitUntil(t, "a local copy filled after a store read", func() bool {
		fresh.mu.Lock()
		defer fresh.mu.Unlock()
		_, ok := fresh.local[1]
		return ok
	})
	store.failGet = true
	if code, body := getBody(t, url); code != http.StatusOK || len(body) != 64 {
		t.Fatalf("GET after fill with store down = %d len %d", code, len(body))
	}
}

func TestUnfinalizedUploadDisplacesNoLocalCopy(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	s.SetLocalCacheSize(40)
	for _, k := range []string{"a", "b"} {
		saveEntry(t, ts.URL, "owner/pinned", k, "v1", 16)
	}
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "c", Version: "v1"})
	putBlob(t, create["signed_upload_url"].(string), bytes.Repeat([]byte("c"), 16))

	s.mu.Lock()
	_, a := s.local[1]
	_, b := s.local[2]
	_, c := s.local[3]
	s.mu.Unlock()
	if !a || !b || c {
		t.Fatalf("local copies a %v b %v c %v, want a and b kept, none for the unfinalized c", a, b, c)
	}
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "c", Version: "v1", SizeBytes: "16"}); fin["ok"] != true {
		t.Fatalf("finalize: %v", fin)
	}
	s.mu.Lock()
	_, c = s.local[3]
	s.mu.Unlock()
	if !c {
		t.Fatal("no local copy of c once finalized")
	}
}

func TestLocalCopiesTrimmedButStillServedFromStore(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	s.SetLocalCacheSize(40)
	for _, k := range []string{"a", "b", "c"} {
		saveEntry(t, ts.URL, "owner/pinned", k, "v1", 16)
	}
	s.mu.Lock()
	kept := len(s.local)
	_, newest := s.local[3]
	s.mu.Unlock()
	if kept != 2 || !newest {
		t.Fatalf("local copies = %d (newest kept %v), want 2 including the newest", kept, newest)
	}
	if code, body := getBody(t, downloadURL(t, ts.URL, "a")); code != http.StatusOK || len(body) != 16 {
		t.Fatalf("trimmed entry GET = %d len %d, want it from the store", code, len(body))
	}
}

func TestBlobMissingFromStoreDropsTheEntry(t *testing.T) {
	store := newMemStore()
	_, ts := newStoreServer(t, store)
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 8)
	url := downloadURL(t, ts.URL, "k")

	// A lifecycle rule, or a delete whose index update a crash lost.
	if err := store.Delete(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if code, _ := getBody(t, url); code != http.StatusNotFound {
		t.Fatalf("GET of a blob the store lost = %d, want 404", code)
	}
	if get := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"}); get["ok"] == true {
		t.Fatalf("lost entry still found: %v", get)
	}
	saveEntry(t, ts.URL, "owner/pinned", "k", "v1", 8) // the key is free again
}

func TestLocalFillsInFlightStayWithinTheCap(t *testing.T) {
	store := newMemStore()
	first, ts := newStoreServer(t, store)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		saveEntry(t, ts.URL, "owner/pinned", k, "v1", 80)
	}
	first.StartIndexSync()()

	store.getGate = make(chan struct{})
	fresh, _ := newStoreServer(t, store)
	fresh.SetLocalCacheSize(100)
	fresh.mu.Lock()
	entries := make([]*Entry, 0, len(fresh.entries))
	for _, e := range fresh.entries {
		entries = append(entries, e)
	}
	fresh.mu.Unlock()
	for _, e := range entries {
		fresh.fillLocal(e) // as five restores would, each of a different entry
	}
	fresh.mu.Lock()
	inFlight, bytes := len(fresh.filling), fresh.fillBytes
	fresh.mu.Unlock()
	close(store.getGate)
	if inFlight != 1 || bytes != 80 {
		t.Fatalf("fills in flight = %d (%d bytes), want 1 (80 bytes) under a 100-byte cap", inFlight, bytes)
	}
	waitUntil(t, "the fill to finish", func() bool {
		fresh.mu.Lock()
		defer fresh.mu.Unlock()
		return len(fresh.filling) == 0 && fresh.fillBytes == 0
	})
}

// waitUntil waits for cond, which background work makes true.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUploadOutlivingItsReservationLeavesNothing(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	s.SetLocalCacheSize(1 << 20)
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
	store.putEntered, store.putGate = make(chan struct{}), make(chan struct{})
	code := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPut, create["signed_upload_url"].(string), strings.NewReader("data"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("PUT: %v", err)
			code <- 0
			return
		}
		resp.Body.Close()
		code <- resp.StatusCode
	}()
	<-store.putEntered
	// gc reclaims the reservation while its blob is on the way to the store.
	s.mu.Lock()
	for _, e := range s.entries {
		e.UsedAt = 0
	}
	s.mu.Unlock()
	s.gc()
	close(store.putGate)

	if c := <-code; c != http.StatusNotFound {
		t.Fatalf("PUT outliving its reservation = %d, want 404", c)
	}
	s.mu.Lock()
	_, staged := s.staged[1]
	_, stored := s.stored[1]
	_, local := s.local[1]
	s.mu.Unlock()
	if staged || stored || local {
		t.Fatalf("reclaimed entry still tracked: staged %v stored %v local %v", staged, stored, local)
	}
	if _, err := os.Stat(s.blobPath(1)); !os.IsNotExist(err) {
		t.Fatalf("local blob of a reclaimed entry: %v", err)
	}
	waitUntil(t, "the store's copy deleted", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		_, ok := store.blobs[1]
		return !ok
	})
}

func TestBlockOutlivingItsReservationLeavesNothing(t *testing.T) {
	for _, mode := range []string{"disk", "store"} {
		t.Run(mode, func(t *testing.T) {
			s, err := newAuthorizedStore(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "store" {
				if err := s.SetBlobStore(newMemStore()); err != nil {
					t.Fatal(err)
				}
			}
			ts := httptest.NewServer(s)
			t.Cleanup(ts.Close)
			create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
			url := create["signed_upload_url"].(string) + "&comp=block&blockid=" + base64.StdEncoding.EncodeToString([]byte("b0"))

			body, feed := io.Pipe()
			code := make(chan int, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPut, url, body)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("PUT block: %v", err)
					code <- 0
					return
				}
				resp.Body.Close()
				code <- resp.StatusCode
			}()
			if _, err := feed.Write([]byte("half")); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the block streaming in", func() bool {
				blocks, _ := os.ReadDir(s.tmpDir(1))
				return len(blocks) == 1
			})
			// gc reclaims the reservation while the block streams in.
			s.mu.Lock()
			for _, e := range s.entries {
				e.UsedAt = 0
			}
			s.mu.Unlock()
			s.gc()
			_, _ = feed.Write([]byte(" more"))
			feed.Close()

			if c := <-code; c != http.StatusNotFound {
				t.Fatalf("block outliving its reservation = %d, want 404", c)
			}
			s.mu.Lock()
			n, staged := s.staged[1]
			s.mu.Unlock()
			if staged {
				t.Fatalf("reclaimed entry still has %d staged bytes", n)
			}
			if _, err := os.Stat(s.tmpDir(1)); !os.IsNotExist(err) {
				t.Fatalf("block directory of a reclaimed entry: %v", err)
			}
		})
	}
}

func TestFailedRetryLeavesNothingToFinalize(t *testing.T) {
	store := newMemStore()
	s, ts := newStoreServer(t, store)
	s.SetLocalCacheSize(1 << 20)
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"})
	upload := create["signed_upload_url"].(string)
	putBlob(t, upload, []byte("first"))

	store.failPut = true // the retry's blob never reaches the store
	req, _ := http.NewRequest(http.MethodPut, upload, strings.NewReader("second"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("retried PUT with store down = %v %v, want 507", err, resp.StatusCode)
	}
	resp.Body.Close()
	if fin := twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/pinned"), Key: "k", Version: "v1"}); fin["ok"] == true {
		t.Fatalf("finalize succeeded after the last upload failed: %v", fin)
	}
	s.mu.Lock()
	_, local := s.local[1]
	s.mu.Unlock()
	if local {
		t.Fatal("local copy recorded for a blob whose upload failed")
	}
}
