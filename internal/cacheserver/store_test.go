package cacheserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
}

func newMemStore() *memStore { return &memStore{blobs: map[uint64][]byte{}} }

func (m *memStore) Put(_ context.Context, id uint64, path string) error {
	if m.failPut {
		return errors.New("store down")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.blobs[id] = data
	m.mu.Unlock()
	return nil
}

func (m *memStore) Get(_ context.Context, id uint64, offset, length int64) (io.ReadCloser, error) {
	if m.failGet {
		return nil, errors.New("store down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.blobs[id]
	if !ok {
		return nil, errors.New("no such blob")
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
