package cacheserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// twirpStatus POSTs a cache RPC and returns the response status.
func twirpStatus(t *testing.T, base, method string, req any) int {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(base+twirpBase+method, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Lookups and reservations keep to the toolkit's own limits: ten keys in all,
// each at most 512 characters. Every restore key costs two scans of every
// entry under the lock.
func TestKeysAreBounded(t *testing.T) {
	_, ts, _ := budgetServer(t, 0)
	nine := make([]string, maxKeys-1)
	for i := range nine {
		nine[i] = fmt.Sprintf("rk-%d", i)
	}
	long := strings.Repeat("k", maxKeyLen+1)
	for _, tc := range []struct {
		name   string
		method string
		req    any
		want   int
	}{
		{"nine restore keys", "GetCacheEntryDownloadURL", getReq{Key: "k", RestoreKeys: nine, Version: "v"}, http.StatusOK},
		{"ten restore keys", "GetCacheEntryDownloadURL", getReq{Key: "k", RestoreKeys: append(nine, "rk-9"), Version: "v"}, http.StatusBadRequest},
		{"long restore key", "GetCacheEntryDownloadURL", getReq{Key: "k", RestoreKeys: []string{long}, Version: "v"}, http.StatusBadRequest},
		{"long key", "CreateCacheEntry", createReq{Key: long, Version: "v"}, http.StatusBadRequest},
		{"long version", "CreateCacheEntry", createReq{Key: "k", Version: long}, http.StatusBadRequest},
		{"key at the limit", "CreateCacheEntry", createReq{Key: strings.Repeat("k", maxKeyLen), Version: "v"}, http.StatusOK},
		// Characters, not bytes: 512 of them are 1536 bytes here.
		{"multi-byte key at the limit", "CreateCacheEntry", createReq{Key: strings.Repeat("鍵", maxKeyLen), Version: "v"}, http.StatusOK},
	} {
		if got := twirpStatus(t, ts.URL, tc.method, tc.req); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Empty entries cost no bytes, so only a count bounds them: past it, a new
// reservation evicts the least-recently-used completed entry.
func TestEntryCountIsBounded(t *testing.T) {
	s, ts, _ := budgetServer(t, 0)
	s.SetMaxEntries(3, 0)
	for i := range 3 {
		saveEntry(t, ts.URL, "1", fmt.Sprintf("empty-%d", i), "v1", 0)
	}
	// Entry 1 is the oldest but the most recently used; entry 2 goes first.
	s.mu.Lock()
	s.entries[1].UsedAt += 10
	s.mu.Unlock()
	saveEntry(t, ts.URL, "1", "empty-3", "v1", 0)
	s.mu.Lock()
	n := len(s.entries)
	_, used := s.entries[1]
	_, lru := s.entries[2]
	s.mu.Unlock()
	if n != 3 || !used || lru {
		t.Fatalf("%d entries, recently used kept %v, least recently used kept %v; want 3, true, false", n, used, lru)
	}
}

// A repository past its own cap evicts its own entries, never another
// repository's: one job cannot flush the host's cache.
func TestRepoEntryCapEvictsOnlyThatRepo(t *testing.T) {
	l := newScopeLab(t)
	l.s.SetMaxEntries(0, 2)
	other := l.guest(t, "172.16.9.2", "", "")
	l.set("172.16.9.2", testIdentity("owner/other", "refs/heads/main", "other"))
	l.save(t, other, "theirs", "x")
	noisy := l.guest(t, "172.16.8.2", "refs/heads/main", "noisy")
	for i := range 5 {
		l.save(t, noisy, fmt.Sprintf("mine-%d", i), "")
	}
	if got, _ := restore(t, other, "theirs"); got != "x" {
		t.Fatal("another repository's entry was evicted")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	mine := 0
	for _, e := range l.s.entries {
		if e.RepoID == "owner/repo" {
			mine++
		}
	}
	if mine != 2 {
		t.Fatalf("the noisy repository holds %d entries, want its cap of 2", mine)
	}
}

// One job may hold only maxJobReservations unfinished uploads, so it cannot
// fill the cap with reservations, which are never evicted.
func TestJobReservationsAreBounded(t *testing.T) {
	_, ts, _ := budgetServer(t, 0)
	for i := range maxJobReservations {
		reserve(t, ts.URL, fmt.Sprintf("r-%d", i))
	}
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "one-more", Version: "v"})
	if create["ok"] != false {
		t.Fatalf("reservation %d accepted: %v", maxJobReservations+1, create)
	}
}

// Reservations are never evicted, so once they alone fill the count a new one
// is refused rather than let the index grow.
func TestEntryCountRefusesWhenOnlyReservationsRemain(t *testing.T) {
	s, ts, _ := budgetServer(t, 0)
	s.SetMaxEntries(2, 0)
	reserve(t, ts.URL, "a")
	reserve(t, ts.URL, "b")
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "c", Version: "v"})
	if create["ok"] != false {
		t.Fatalf("a third reservation past a count of 2 was accepted: %v", create)
	}
}

// At the cap, a create for a key that already exists evicts nothing.
func TestEntryCapDoesNotEvictForAnExistingKey(t *testing.T) {
	s, ts, _ := budgetServer(t, 0)
	s.SetMaxEntries(2, 0)
	saveEntry(t, ts.URL, "1", "a", "v", 0)
	saveEntry(t, ts.URL, "1", "b", "v", 0)
	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "b", Version: "v"})
	if create["ok"] != false {
		t.Fatalf("a second create of b was accepted: %v", create)
	}
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d entries left, want both", n)
	}
}
