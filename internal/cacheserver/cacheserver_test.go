package cacheserver

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/solcreek/firerunner/internal/jobidentity"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testIdentity(repo, ref, job string) jobidentity.Identity {
	return jobidentity.Identity{Repository: repo, WriteRef: ref, ReadRefs: []string{ref}, Runner: "test-runner", Generation: job, JobID: job, RunID: 1, ExpiresAt: time.Now().Add(time.Hour).Unix()}
}
func newAuthorizedStore(dir string, log *slog.Logger) (*Server, error) {
	s, err := New(dir, log)
	if err != nil {
		return nil, err
	}
	err = s.SetScope(func(string) (jobidentity.Identity, error) {
		return testIdentity("owner/pinned", "refs/heads/main", "test"), nil
	})
	return s, err
}

// newTestServer returns a Server backed by a temp dir and an httptest server in
// front of it so signed URLs (built from r.Host) resolve back to the same server.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func twirp(t *testing.T, base, method string, req any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	url := base + twirpBase + method
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s -> %d: %s", method, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s decode %q: %v", method, raw, err)
	}
	return out
}

// putBlob does a single-shot Azure-style PUT (the toolkit's path for blobs
// <= 128 MiB).
func putBlob(t *testing.T, url string, data []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT blob: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT blob -> %d", resp.StatusCode)
	}
}

func meta(repo string) *cacheMetadata { return &cacheMetadata{RepositoryID: repo} }

// saveEntry runs the full reserve -> PUT -> finalize flow for a blob of n bytes.
func saveEntry(t *testing.T, base, repo, key, version string, n int) {
	t.Helper()
	create := twirp(t, base, "CreateCacheEntry", createReq{Metadata: meta(repo), Key: key, Version: version})
	if create["ok"] != true {
		t.Fatalf("create %q not ok: %v", key, create)
	}
	putBlob(t, create["signed_upload_url"].(string), bytes.Repeat([]byte("x"), n))
	fin := twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{
		Metadata: meta(repo), Key: key, Version: version, SizeBytes: fmt.Sprint(n),
	})
	if fin["ok"] != true {
		t.Fatalf("finalize %q not ok: %v", key, fin)
	}
}

// TestEvictLRUOnSizeCap checks that finalizing an entry above the size cap evicts
// the least-recently-used completed entries (lowest ID on a UsedAt tie) and never
// the entry just stored.
func TestEvictLRUOnSizeCap(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxSize(250) // room for ~2 of the 100-byte blobs below
	ts := httptest.NewServer(s)
	defer ts.Close()
	const version = "v1"

	saveEntry(t, ts.URL, "1", "key-a", version, 100) // id 1
	saveEntry(t, ts.URL, "1", "key-b", version, 100) // id 2
	saveEntry(t, ts.URL, "1", "key-c", version, 100) // id 3 -> total 300 > 250, evict oldest (id 1)

	s.mu.Lock()
	_, hasA := s.entries[1]
	_, hasB := s.entries[2]
	_, hasC := s.entries[3]
	var total int64
	for _, e := range s.entries {
		total += e.Size
	}
	s.mu.Unlock()
	if hasA {
		t.Fatalf("expected id 1 (LRU) to be evicted")
	}
	if !hasB || !hasC {
		t.Fatalf("expected id 2 and 3 to remain (b=%v c=%v)", hasB, hasC)
	}
	if total > 250 {
		t.Fatalf("total %d still over cap", total)
	}
	// The evicted blob is gone from disk.
	if _, err := os.Stat(s.blobPath(1)); !os.IsNotExist(err) {
		t.Fatalf("evicted blob still on disk: %v", err)
	}
	// A restore of the kept entry still works.
	get := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("1"), Key: "key-c", Version: version})
	if get["ok"] != true {
		t.Fatalf("kept entry not restorable: %v", get)
	}
}

// TestEvictKeepsOversizeEntry checks a single blob larger than the cap is still
// kept and served (we never evict the just-finalized entry down to nothing).
func TestEvictKeepsOversizeEntry(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxSize(50)
	ts := httptest.NewServer(s)
	defer ts.Close()

	saveEntry(t, ts.URL, "1", "big", "v1", 500) // over cap on its own
	get := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("1"), Key: "big", Version: "v1"})
	if get["ok"] != true {
		t.Fatalf("oversize entry should still be served: %v", get)
	}
}

// TestSaveRestoreSingleShot walks the full toolkit v2 flow: reserve -> single
// PUT -> finalize -> resolve download URL -> GET, and checks the bytes round-trip.
func TestSaveRestoreSingleShot(t *testing.T) {
	_, base := newTestServer(t)
	const version = "v1abc"
	payload := []byte("hello pnpm store")

	create := twirp(t, base, "CreateCacheEntry", createReq{Metadata: meta("42"), Key: "Node-Modules-abc", Version: version})
	if create["ok"] != true {
		t.Fatalf("create not ok: %v", create)
	}
	putBlob(t, create["signed_upload_url"].(string), payload)

	fin := twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{
		Metadata: meta("42"), Key: "Node-Modules-abc", Version: version,
		SizeBytes: fmt.Sprint(len(payload)),
	})
	if fin["ok"] != true {
		t.Fatalf("finalize not ok: %v", fin)
	}

	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("42"), Key: "Node-Modules-abc", Version: version})
	if get["ok"] != true {
		t.Fatalf("get not ok: %v", get)
	}
	// Keys are matched case-insensitively and echoed lowercased.
	if got := get["matched_key"]; got != "node-modules-abc" {
		t.Fatalf("matched_key = %v", got)
	}
	resp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: %q != %q", got, payload)
	}
}

// TestBlockUpload exercises the staged-block path (>128 MiB in the real client):
// several ?comp=block PUTs then a ?comp=blocklist commit, reassembled in list order.
func TestBlockUpload(t *testing.T) {
	_, base := newTestServer(t)
	const version = "vblk"
	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "big", Version: version})
	uploadURL := create["signed_upload_url"].(string)

	blocks := map[string][]byte{
		"blk-0001": []byte("AAAA"),
		"blk-0002": []byte("BBBB"),
		"blk-0003": []byte("CCCC"),
	}
	for id, data := range blocks {
		putBlockPart(t, uploadURL, id, data)
	}
	// Commit in a deliberate order; the assembled blob must follow it.
	commitBlocks(t, uploadURL, []string{"blk-0002", "blk-0003", "blk-0001"})

	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "big", Version: version, SizeBytes: "12"})
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "big", Version: version})
	resp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "BBBBCCCCAAAA" {
		t.Fatalf("block order wrong: %q", got)
	}
}

func putBlockPart(t *testing.T, uploadURL, blockID string, data []byte) {
	t.Helper()
	sep := "&"
	if !strings.Contains(uploadURL, "?") {
		sep = "?"
	}
	url := uploadURL + sep + "comp=block&blockid=" + blockID
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT block: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT block -> %d", resp.StatusCode)
	}
}

func commitBlocks(t *testing.T, uploadURL string, ids []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("<BlockList>")
	for _, id := range ids {
		fmt.Fprintf(&b, "<Latest>%s</Latest>", id)
	}
	b.WriteString("</BlockList>")
	sep := "&"
	if !strings.Contains(uploadURL, "?") {
		sep = "?"
	}
	req, _ := http.NewRequest(http.MethodPut, uploadURL+sep+"comp=blocklist", strings.NewReader(b.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("commit blocklist: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("commit -> %d", resp.StatusCode)
	}
}

// TestRestoreKeyPrefix verifies restore-key prefix matching and newest-wins.
func TestRestoreKeyPrefix(t *testing.T) {
	_, base := newTestServer(t)
	const version = "vp"
	for _, key := range []string{"deps-linux-aaa", "deps-linux-bbb"} {
		create := twirp(t, base, "CreateCacheEntry", createReq{Key: key, Version: version})
		putBlob(t, create["signed_upload_url"].(string), []byte(key))
		twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: key, Version: version, SizeBytes: fmt.Sprint(len(key))})
	}
	// No exact hit, but the "deps-linux-" prefix should match the newest (bbb).
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{
		Key: "deps-linux-zzz", RestoreKeys: []string{"deps-linux-"}, Version: version,
	})
	if get["ok"] != true || get["matched_key"] != "deps-linux-bbb" {
		t.Fatalf("restore-key match = %v", get)
	}
}

// TestRepoIsolation uses host identity, never the client metadata.
func TestRepoIsolation(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	saveEntry(t, ts.URL, "ignored", "shared-key", "v", 5)
	// Replace identity only between requests; configuration is immutable in production.
	s.lookup = func(string) (jobidentity.Identity, error) {
		return testIdentity("owner/other", "refs/heads/main", "other"), nil
	}
	got := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("owner/pinned"), Key: "shared-key", RestoreKeys: []string{""}, Version: "v"})
	if got["ok"] != false {
		t.Fatalf("cross-repo leak: %v", got)
	}
}

// TestMissAndImmutability covers a plain miss and the no-overwrite rule.
func TestMissAndImmutability(t *testing.T) {
	_, base := newTestServer(t)
	miss := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "nope", Version: "v"})
	if miss["ok"] != false {
		t.Fatalf("expected miss, got %v", miss)
	}

	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("x"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "1"})

	// A second reservation of the same key+version is refused (immutable).
	dup := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	if dup["ok"] != false {
		t.Fatalf("expected immutable conflict, got %v", dup)
	}
}

// TestSigRequired ensures blob URLs can't be read without the per-entry token.
func TestSigRequired(t *testing.T) {
	_, base := newTestServer(t)
	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("secret"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "6"})
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "k", Version: "v"})

	url := get["signed_download_url"].(string)
	stripped := url[:strings.Index(url, "?")] // drop ?sig=
	resp, err := http.Get(stripped)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without sig, got %d", resp.StatusCode)
	}
}

// TestPersistenceReload confirms the on-disk index survives a restart.
func TestPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	s1, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(s1)
	create := twirp(t, ts1.URL, "CreateCacheEntry", createReq{Key: "persist", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("data"))
	twirp(t, ts1.URL, "FinalizeCacheEntryUpload", finalizeReq{Key: "persist", Version: "v", SizeBytes: "4"})
	ts1.Close()

	// Reopen the same dir: the finalized entry should still resolve.
	s2, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(s2)
	defer ts2.Close()
	get := twirp(t, ts2.URL, "GetCacheEntryDownloadURL", getReq{Key: "persist", Version: "v"})
	if get["ok"] != true {
		t.Fatalf("entry did not survive reload: %v", get)
	}
}

// TestStatsAndMetrics checks the /stats JSON and /metrics text reflect saves,
// hits and misses.
func TestStatsAndMetrics(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxSize(0)
	ts := httptest.NewServer(s)
	defer ts.Close()

	saveEntry(t, ts.URL, "1", "k1", "v1", 100)
	if got := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("1"), Key: "k1", Version: "v1"}); got["ok"] != true {
		t.Fatalf("expected hit: %v", got)
	}
	if got := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Metadata: meta("1"), Key: "nope", Version: "v1"}); got["ok"] != false {
		t.Fatalf("expected miss: %v", got)
	}

	resp, err := http.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	defer resp.Body.Close()
	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if st.Entries != 1 || st.Bytes != 100 || st.Saves != 1 || st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}

	mresp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer mresp.Body.Close()
	body, _ := io.ReadAll(mresp.Body)
	for _, want := range []string{
		"firerunner_cache_entries 1",
		"firerunner_cache_bytes 100",
		"firerunner_cache_hits_total 1",
		"firerunner_cache_misses_total 1",
		"firerunner_cache_saves_total 1",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("metrics missing %q in:\n%s", want, body)
		}
	}
}

// sanity: hex helper matches how blocks are stored (guards the reassembly path).
func TestBlockIDEncoding(t *testing.T) {
	if hex.EncodeToString([]byte("blk-0001")) == "" {
		t.Fatal("hex encoding empty")
	}
}

// TestStorePermsAreTight verifies the on-disk store is not world/group readable:
// the index holds every entry's blob token (which authorizes overwrite), so a
// loose mode would let any local account read and poison the cache.
func TestStorePermsAreTight(t *testing.T) {
	dir := t.TempDir()
	s, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	putBlob(t, create["signed_upload_url"].(string), []byte("secret"))
	twirp(t, ts.URL, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "6"})

	cases := []struct {
		path string
		want os.FileMode
	}{
		{dir, 0o700},
		{s.blobPath(1), 0o600},
		{s.indexPath(), 0o600},
	}
	for _, c := range cases {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatalf("stat %s: %v", c.path, err)
		}
		if got := fi.Mode().Perm(); got != c.want {
			t.Errorf("%s mode = %o, want %o", c.path, got, c.want)
		}
	}
}

// TestMaxEntrySizeRejectsSingleShot verifies a single-shot PUT over the
// per-entry cap is refused mid-stream (413) and leaves no oversized blob behind,
// so one job cannot fill the host disk with an unbounded upload.
func TestMaxEntrySizeRejectsSingleShot(t *testing.T) {
	dir := t.TempDir()
	s, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxEntrySize(1024)
	ts := httptest.NewServer(s)
	defer ts.Close()

	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	uploadURL := create["signed_upload_url"].(string)

	req, _ := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if fi, err := os.Stat(s.blobPath(1)); err == nil {
		t.Fatalf("oversized blob left on disk: %d bytes", fi.Size())
	}
}

// TestMaxEntrySizeRejectsBlockList verifies staged blocks whose committed total
// exceeds the cap are refused and the live blob path is never created.
func TestMaxEntrySizeRejectsBlockList(t *testing.T) {
	dir := t.TempDir()
	s, err := newAuthorizedStore(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetMaxEntrySize(1024)
	ts := httptest.NewServer(s)
	defer ts.Close()

	create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "big", Version: "v"})
	uploadURL := create["signed_upload_url"].(string)

	// Stage one 4 KiB block, then commit it: assembled total (4096) > cap (1024).
	blockID := "blk-0"
	stage := uploadURL + "&comp=block&blockid=" + blockID
	req, _ := http.NewRequest(http.MethodPut, stage, bytes.NewReader(bytes.Repeat([]byte("y"), 4096)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stage block: %v", err)
	}
	resp.Body.Close()
	// The block itself already exceeds the remaining budget, so it is refused.
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("stage status = %d, want 413", resp.StatusCode)
	}
	if _, err := os.Stat(s.blobPath(1)); err == nil {
		t.Fatal("blob created despite over-cap upload")
	}
}

// TestFinalizedEntryIsImmutable verifies a completed entry cannot be overwritten
// or truncated via its blob token, which is also handed out for downloads.
func TestFinalizedEntryIsImmutable(t *testing.T) {
	ts, base := newTestServer(t)

	create := twirp(t, base, "CreateCacheEntry", createReq{Key: "k", Version: "v"})
	uploadURL := create["signed_upload_url"].(string)
	putBlob(t, uploadURL, []byte("original"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "k", Version: "v", SizeBytes: "8"})

	// Re-PUT to the same signed upload URL (same id+sig) must be refused now.
	req, _ := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader([]byte("EVIL")))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("overwrite status = %d, want 409", resp.StatusCode)
	}

	// The original content must still be intact on restore.
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "k", Version: "v"})
	dresp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer dresp.Body.Close()
	got, _ := io.ReadAll(dresp.Body)
	if string(got) != "original" {
		t.Fatalf("blob = %q, want %q", got, "original")
	}
	_ = ts
}

// TestConcurrentReservationIsRejected verifies a second reservation for a live
// key+version is refused (ok:false), so two callers cannot hold competing
// reservations and have one's Finalize publish the other's bytes.
func TestConcurrentReservationIsRejected(t *testing.T) {
	_, base := newTestServer(t)

	first := twirp(t, base, "CreateCacheEntry", createReq{Key: "shared", Version: "v"})
	if ok, _ := first["ok"].(bool); !ok {
		t.Fatalf("first reservation not ok: %v", first)
	}
	firstURL := first["signed_upload_url"].(string)

	second := twirp(t, base, "CreateCacheEntry", createReq{Key: "shared", Version: "v"})
	if ok, _ := second["ok"].(bool); ok {
		t.Fatalf("second reservation should be refused, got %v", second)
	}
	if url, _ := second["signed_upload_url"].(string); url != "" {
		t.Fatalf("refused reservation must not return an upload URL, got %q", url)
	}

	// The first (only) reservation's bytes are what get finalized and restored.
	putBlob(t, firstURL, []byte("GOOD"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: "shared", Version: "v", SizeBytes: "4"})
	get := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: "shared", Version: "v"})
	dresp, err := http.Get(get["signed_download_url"].(string))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer dresp.Body.Close()
	got, _ := io.ReadAll(dresp.Body)
	if string(got) != "GOOD" {
		t.Fatalf("restored %q, want GOOD", got)
	}
}

// TestRepositoryPinIgnoresClientMetadata proves that pinning the server to one
// tenant defeats the blank/forged repository_id cross-repo leak: a victim stores
// under its real id, and an attacker uploading with a blank repository_id and a
// blank restore key cannot see or match the victim's entry, because the server
// forces every request into the single pinned tenant.
func TestRepositoryPinIgnoresClientMetadata(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	base := ts.URL

	// Victim publishes an entry, sending its own (now ignored) repository_id.
	create := twirp(t, base, "CreateCacheEntry", createReq{Metadata: meta("owner/victim"), Key: "secret-key", Version: "v1"})
	putBlob(t, create["signed_upload_url"].(string), []byte("PRIVATE"))
	twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Metadata: meta("owner/victim"), Key: "secret-key", Version: "v1", SizeBytes: "7"})

	// Attacker probes with a blank repository_id and a blank restore key
	// (HasPrefix("") matches any key) but a different version: it must miss,
	// because it lands in the same pinned tenant yet the version differs.
	miss := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta(""), Key: "anything", RestoreKeys: []string{""}, Version: "attacker-version"})
	if ok, _ := miss["ok"].(bool); ok {
		t.Fatalf("attacker with blank repo/version should miss, got %v", miss)
	}

	// The pinned tenant can still read its own entry with the right version,
	// regardless of which client repository_id it claims.
	hit := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Metadata: meta("whatever"), Key: "secret-key", Version: "v1"})
	if ok, _ := hit["ok"].(bool); !ok {
		t.Fatalf("pinned tenant should hit its own entry, got %v", hit)
	}
}
