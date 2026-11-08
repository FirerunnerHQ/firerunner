package cacheserver

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/gitcache"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

// A git entry is saved only under the repository its key names, which must
// be the job's own; actions/cache keys that merely start with "git/", under
// any other version, are untouched.
func TestGitEntriesOnlyForTheJobsOwnRepository(t *testing.T) {
	_, ts, _ := budgetServer(t, 0) // the job is owner/pinned
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		key, version string
		ok           bool
	}{
		{"git/owner/pinned/" + hash, gitcache.EntryVersion, true},
		{"git/Owner/Pinned/" + strings.Repeat("b", 64), gitcache.EntryVersion, true}, // keys match without case
		{"git/owner/private/" + hash, gitcache.EntryVersion, false},
		{"git/" + hash, gitcache.EntryVersion, false},
		{"git/owner/private/" + hash, "v", true},
	} {
		create := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: tc.key, Version: tc.version})
		if got := create["ok"] == true; got != tc.ok {
			t.Errorf("create %q version %q: ok = %v, want %v (%v)", tc.key, tc.version, got, tc.ok, create)
		}
	}
}

// An index saved before git keys named their repository may hold a git
// entry with another repository's objects under this one's scope. A prefix
// lookup, as a fork PR can send directly, must not restore it.
func TestOldGitEntriesAreNotRestored(t *testing.T) {
	dir := t.TempDir()
	id := testIdentity("owner/repo", mainRef, "job")
	old := fmt.Sprintf(`[{"id":5,"sig":"tok","ref":%q,"repo_id":%q,"key":"git/%s","version":%q,"size":4,"complete":true}]`,
		mainRef, id.ScopeKey(), strings.Repeat("c", 64), gitcache.EntryVersion)
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.blobPath(5), []byte("pack"), 0600); err != nil {
		t.Fatal(err)
	}
	pr := testIdentity("owner/repo", "refs/pull/7/merge", "fork")
	pr.ReadRefs = []string{"refs/pull/7/merge", mainRef}
	if err := s.SetScope(func(string) (jobidentity.Identity, error) { return pr, nil }); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	g := twirp(t, ts.URL, "GetCacheEntryDownloadURL", getReq{Key: "git/", Version: gitcache.EntryVersion, RestoreKeys: []string{"git/"}})
	if g["ok"] == true {
		t.Fatalf("an old-format git entry was restored: %v", g)
	}
}
