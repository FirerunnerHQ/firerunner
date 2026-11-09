package cacheserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cache RPCs refuse a body over maxTwirpBody instead of buffering it: the
// decoder would otherwise hold a JSON value of any size in memory.
func TestTwirpBodiesAreBounded(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	huge := strings.Repeat("k", maxTwirpBody+1)
	for _, tc := range []struct {
		method string
		req    any
	}{
		{"CreateCacheEntry", createReq{Key: huge, Version: "v"}},
		{"FinalizeCacheEntryUpload", finalizeReq{Key: huge, Version: "v"}},
		{"GetCacheEntryDownloadURL", getReq{Key: huge, Version: "v"}},
	} {
		body, _ := json.Marshal(tc.req)
		resp, err := http.Post(ts.URL+twirpBase+tc.method, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", tc.method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest { // Twirp: invalid_argument is 400
			t.Errorf("%s with a %d-byte body -> %d, want 400", tc.method, len(body), resp.StatusCode)
		}
	}
}

// A block list naming more than Azure's maxBlocks ids is refused, even when
// each one is a real block: every id costs an open and a copy.
func TestBlockListIDCountIsBounded(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	url := reserve(t, ts.URL, "a")
	putBlockPart(t, url, "b0", []byte("x"))
	list := "<BlockList>" + strings.Repeat("<Latest>b0</Latest>", maxBlocks+1) + "</BlockList>"
	req, _ := http.NewRequest(http.MethodPut, url+"&comp=blocklist", strings.NewReader(list))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("block list of %d ids -> %d, want 400", maxBlocks+1, resp.StatusCode)
	}
}

// A block id longer than Azure accepts is a 400, not a file name too long.
func TestBlockIDLengthIsBounded(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	url := reserve(t, ts.URL, "a")
	if code := putStatus(t, url+"&comp=block&blockid="+strings.Repeat("A", maxBlockIDLen), []byte("x")); code != http.StatusCreated {
		t.Fatalf("%d-character block id -> %d, want 201", maxBlockIDLen, code)
	}
	if code := putStatus(t, url+"&comp=block&blockid="+strings.Repeat("A", maxBlockIDLen+1), []byte("x")); code != http.StatusBadRequest {
		t.Fatalf("%d-character block id -> %d, want 400", maxBlockIDLen+1, code)
	}
}

// A block list over maxBlockListBody is refused before it is parsed.
func TestBlockListBodyIsBounded(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	url := reserve(t, ts.URL, "a")
	putBlockPart(t, url, "b0", []byte("x"))
	var list strings.Builder
	list.WriteString("<BlockList>")
	for list.Len() <= maxBlockListBody {
		list.WriteString("<Latest>b0</Latest>")
	}
	list.WriteString("</BlockList>")
	req, _ := http.NewRequest(http.MethodPut, url+"&comp=blocklist", strings.NewReader(list.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d-byte block list -> %d, want 413", list.Len(), resp.StatusCode)
	}
}
