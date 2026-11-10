package cacheserver

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// putStatus PUTs data to url and returns the response status.
func putStatus(t *testing.T, url string, data []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// reserve opens a reservation for key and returns its upload URL.
func reserve(t *testing.T, base, key string) string {
	t.Helper()
	create := twirp(t, base, "CreateCacheEntry", createReq{Key: key, Version: "v"})
	if create["ok"] != true {
		t.Fatalf("create %q not ok: %v", key, create)
	}
	return create["signed_upload_url"].(string)
}

// diskBytes sums the sizes of every file under dir.
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() != "index.json" {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

func budgetServer(t *testing.T, maxSize int64) (*Server, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxSize(maxSize)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts, dir
}

// One job reserving many keys and never finalizing must not fill the disk
// past --max-size: eviction cannot touch in-flight uploads.
func TestUnfinalizedUploadsStayWithinMaxSize(t *testing.T) {
	_, ts, dir := budgetServer(t, 1000)
	var refused int
	for i := range 5 {
		url := reserve(t, ts.URL, fmt.Sprintf("never-finalized-%d", i))
		switch code := putStatus(t, url, bytes.Repeat([]byte("x"), 400)); code {
		case http.StatusCreated:
		case http.StatusInsufficientStorage:
			refused++
		default:
			t.Fatalf("upload %d -> %d", i, code)
		}
	}
	if refused != 3 {
		t.Fatalf("refused %d uploads, want 3 (two 400-byte uploads fit in 1000)", refused)
	}
	if got := diskBytes(t, dir); got > 1000 {
		t.Fatalf("%d bytes on disk, over max size 1000", got)
	}
}

// Staged blocks count as they arrive, before any block list is committed.
func TestUnfinalizedBlocksStayWithinMaxSize(t *testing.T) {
	_, ts, dir := budgetServer(t, 1000)
	a, b := reserve(t, ts.URL, "a"), reserve(t, ts.URL, "b")
	putBlockPart(t, a, "blk-0", bytes.Repeat([]byte("x"), 600))
	if code := putStatus(t, b+"&comp=block&blockid=blk-0", bytes.Repeat([]byte("y"), 600)); code != http.StatusInsufficientStorage {
		t.Fatalf("second block -> %d, want 507", code)
	}
	if got := diskBytes(t, dir); got > 1000 {
		t.Fatalf("%d bytes on disk, over max size 1000", got)
	}
}

// An in-flight upload makes room by evicting completed entries, as finalize
// does, rather than being refused.
func TestUploadEvictsCompletedEntriesForRoom(t *testing.T) {
	s, ts, _ := budgetServer(t, 1000)
	saveEntry(t, ts.URL, "1", "old", "v1", 700) // id 1
	url := reserve(t, ts.URL, "new")
	if code := putStatus(t, url, bytes.Repeat([]byte("x"), 700)); code != http.StatusCreated {
		t.Fatalf("upload -> %d, want 201 after evicting the old entry", code)
	}
	s.mu.Lock()
	_, kept := s.entries[1]
	s.mu.Unlock()
	if kept {
		t.Fatal("completed entry kept although the upload needed its room")
	}
}

// A block that fails partway, here at the per-entry cap, hands back what it
// claimed: its bytes were removed, so they must not count against the cache.
func TestFailedBlockReleasesItsClaim(t *testing.T) {
	s, ts, _ := budgetServer(t, 6400) // claims ahead in 100-byte steps
	s.SetMaxEntrySize(1000)
	url := reserve(t, ts.URL, "a")
	if code := putStatus(t, url+"&comp=block&blockid=blk-0", bytes.Repeat([]byte("x"), 1500)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap block -> %d, want 413", code)
	}
	s.mu.Lock()
	used := s.bytesUsed()
	s.mu.Unlock()
	if used != 0 {
		t.Fatalf("%d bytes still counted for a block that was removed", used)
	}
}

// A single PUT after staged blocks must not leave the blocks on disk
// uncounted, whether the PUT succeeds or fails; repeating the pair would
// otherwise grow the disk by an entry's cap each time.
func TestSinglePutDropsStagedBlocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  int
		want int
	}{
		{"succeeds", 1, http.StatusCreated},
		{"fails at entry cap", 800, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ts, dir := budgetServer(t, 1000)
			s.SetMaxEntrySize(700)
			url := reserve(t, ts.URL, "a")
			for i := range 5 {
				if code := putStatus(t, url+"&comp=block&blockid=blk-0", bytes.Repeat([]byte("x"), 600)); code != http.StatusCreated {
					t.Fatalf("round %d: block -> %d", i, code)
				}
				if code := putStatus(t, url, bytes.Repeat([]byte("y"), tc.put)); code != tc.want {
					t.Fatalf("round %d: single PUT -> %d, want %d", i, code, tc.want)
				}
			}
			if got := diskBytes(t, dir); got > 1000 {
				t.Fatalf("%d bytes on disk, over max size 1000", got)
			}
			s.mu.Lock()
			used := s.bytesUsed()
			s.mu.Unlock()
			if got := diskBytes(t, dir); used != got {
				t.Fatalf("%d bytes counted, %d on disk", used, got)
			}
		})
	}
}

// A block list naming a missing block hands back what its assembly claimed;
// retried, the leak would evict every completed entry and refuse every
// other upload while costing no disk.
func TestBrokenBlockListReleasesItsClaim(t *testing.T) {
	s, ts, _ := budgetServer(t, 100000)
	saveEntry(t, ts.URL, "1", "kept", "v1", 50000)
	url := reserve(t, ts.URL, "a")
	putBlockPart(t, url, "b0", bytes.Repeat([]byte("x"), 10000))
	for i := range 10 {
		req, _ := http.NewRequest(http.MethodPut, url+"&comp=blocklist",
			bytes.NewReader([]byte("<BlockList><Latest>b0</Latest><Latest>nope</Latest></BlockList>")))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("commit %d -> %d, want 400", i, resp.StatusCode)
		}
	}
	s.mu.Lock()
	used, kept := s.bytesUsed(), s.entries[1] != nil
	s.mu.Unlock()
	if used != 60000 || !kept {
		t.Fatalf("after failed commits: %d bytes counted (want 60000), completed entry kept %v", used, kept)
	}
}

// A refused claim evicts nothing: with in-flight bytes alone over the cap,
// eviction cannot make room.
func TestRefusedClaimEvictsNothing(t *testing.T) {
	// Uncapped while setting up, since the API cannot otherwise reach a
	// completed entry beside in-flight bytes that fill the cap on their own.
	s, ts, _ := budgetServer(t, 0)
	saveEntry(t, ts.URL, "1", "kept", "v1", 300) // id 1
	a := reserve(t, ts.URL, "a")
	if code := putStatus(t, a, bytes.Repeat([]byte("x"), 1000)); code != http.StatusCreated {
		t.Fatalf("first upload -> %d", code)
	}
	s.SetMaxSize(1000)
	b := reserve(t, ts.URL, "b")
	if code := putStatus(t, b, bytes.Repeat([]byte("y"), 400)); code != http.StatusInsufficientStorage {
		t.Fatalf("second upload -> %d, want 507", code)
	}
	s.mu.Lock()
	_, kept := s.entries[1]
	s.mu.Unlock()
	if !kept {
		t.Fatal("completed entry evicted by a claim that was refused")
	}
}

// A refused upload hands its claim back, so later uploads still fit.
func TestRefusedUploadReleasesItsClaim(t *testing.T) {
	s, ts, _ := budgetServer(t, 1000)
	first := reserve(t, ts.URL, "first")
	if code := putStatus(t, first, bytes.Repeat([]byte("x"), 600)); code != http.StatusCreated {
		t.Fatalf("first upload -> %d", code)
	}
	second := reserve(t, ts.URL, "second")
	if code := putStatus(t, second, bytes.Repeat([]byte("y"), 600)); code != http.StatusInsufficientStorage {
		t.Fatalf("second upload -> %d, want 507", code)
	}
	s.mu.Lock()
	used := s.bytesUsed()
	s.mu.Unlock()
	if used != 600 {
		t.Fatalf("%d bytes counted after the refusal, want the first upload's 600", used)
	}
	if code := putStatus(t, second, bytes.Repeat([]byte("y"), 400)); code != http.StatusCreated {
		t.Fatalf("retry within the budget -> %d, want 201", code)
	}
}

// A block list that cannot commit fails before it claims a byte: claims evict
// completed entries for good, so a failing list must not cost anyone theirs.
// One staged block named over and over would otherwise claim, and evict for,
// bytes nobody uploaded.
func TestFailingBlockListEvictsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"a block named twice", []string{"b0", "b0"}},
		{"named again, then missing", []string{"b0", "b0", "b0", "nope"}},
		{"a missing block after a real one", []string{"b0", "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ts, _ := budgetServer(t, 1000)
			saveEntry(t, ts.URL, "1", "old", "v1", 400) // id 1, completed
			url := reserve(t, ts.URL, "new")
			putBlockPart(t, url, "b0", bytes.Repeat([]byte("x"), 300))
			putBlockPart(t, url, "b1", bytes.Repeat([]byte("y"), 300))
			list := "<BlockList>"
			for _, id := range tc.ids {
				list += "<Latest>" + id + "</Latest>"
			}
			list += "</BlockList>"
			if code := putStatus(t, url+"&comp=blocklist", []byte(list)); code != http.StatusBadRequest {
				t.Fatalf("block list %v -> %d, want 400", tc.ids, code)
			}
			s.mu.Lock()
			_, kept := s.entries[1]
			s.mu.Unlock()
			if !kept {
				t.Fatal("a block list that failed evicted a completed entry")
			}
		})
	}
}
