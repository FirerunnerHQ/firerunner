package gitproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

// cacheFixture is the real cache-server. who decides the identity behind each
// request, as the host's job records do by address.
type cacheFixture struct {
	url string
	who *jobidentity.Identity
}

func newCacheFixture(t *testing.T, who jobidentity.Identity) *cacheFixture {
	t.Helper()
	srv, err := cacheserver.New(t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &cacheFixture{who: &who}
	if err := srv.SetScope(func(string) (jobidentity.Identity, error) { return *f.who, nil }); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return ctx
	}
	ts.Start()
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

func job(repo, ref string, reads ...string) jobidentity.Identity {
	return jobidentity.Identity{
		Runner: "runner-1", Generation: "g1", Repository: repo, RepositoryID: int64(len(repo)) + 1000, WriteRef: ref,
		ReadRefs: append([]string{ref}, reads...), RunID: 1, JobID: "j1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

// gitKey is a well-formed git cache key for repo: cache-server saves git
// entries only under the repository their key names.
func gitKey(repo, tag string) string {
	return "git/" + repo + "/" + strings.Repeat(tag, 64/len(tag))
}

var (
	keyAbc = gitKey("o/r", "ab")
	keyK   = gitKey("o/r", "cd")
	keyPR  = gitKey("o/r", "ef")
)

// A job saves git entries only for its own repository: one that fetched
// another private repository must not store it where a fork PR reading the
// default branch's caches could restore it.
func TestCacheServer_SavesOnlyTheJobsOwnRepository(t *testing.T) {
	f := newCacheFixture(t, job("o/r", "refs/heads/main"))
	c := &CacheServer{URL: f.url}
	other := gitKey("o/private", "12")
	saveFile(t, c, other, goodPack)
	*f.who = job("o/r", "refs/pull/7/merge", "refs/heads/main")
	if _, err := read(t, c, other); !errors.Is(err, ErrMiss) {
		t.Fatalf("another repository's objects were saved under this one: %v", err)
	}
}

func saveFile(t *testing.T, c *CacheServer, key, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(context.Background(), key, path, int64(len(content))); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func read(t *testing.T, c *CacheServer, key string) (string, error) {
	t.Helper()
	rc, err := c.Open(context.Background(), key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestCacheServer_RoundTrip(t *testing.T) {
	f := newCacheFixture(t, job("o/r", "refs/heads/main"))
	c := &CacheServer{URL: f.url}
	if _, err := read(t, c, keyAbc); !errors.Is(err, ErrMiss) {
		t.Fatalf("empty cache: %v", err)
	}
	saveFile(t, c, keyAbc, goodPack)
	got, err := read(t, c, keyAbc)
	if err != nil || got != goodPack {
		t.Fatalf("read = %q, %v", got, err)
	}
	// A second job storing the same key is not an error.
	saveFile(t, c, keyAbc, goodPack)
}

// Isolation is cache-server's, by the job's identity. A client cannot ask for
// more, so these go through the real server.
func TestCacheServer_Isolation(t *testing.T) {
	f := newCacheFixture(t, job("o/r", "refs/heads/main"))
	c := &CacheServer{URL: f.url}
	saveFile(t, c, keyK, goodPack)

	*f.who = job("o/other", "refs/heads/main")
	if _, err := read(t, c, keyK); !errors.Is(err, ErrMiss) {
		t.Fatalf("another repository read the entry: %v", err)
	}

	// A pull request's writes stay on its ref, and the default branch never
	// reads them, so a pull request cannot poison main.
	*f.who = job("o/r", "refs/pull/7/merge", "refs/heads/main")
	saveFile(t, c, keyPR, "written by a pull request")
	*f.who = job("o/r", "refs/heads/main")
	if _, err := read(t, c, keyPR); !errors.Is(err, ErrMiss) {
		t.Fatalf("main read a pull request's entry: %v", err)
	}

	// The pull request can read main's.
	*f.who = job("o/r", "refs/pull/7/merge", "refs/heads/main")
	if got, err := read(t, c, keyK); err != nil || got != goodPack {
		t.Fatalf("a pull request could not read main: %q, %v", got, err)
	}
}

func TestCacheServer_Unreachable(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	c := &CacheServer{URL: url}
	if _, err := c.Open(context.Background(), keyK); err == nil || errors.Is(err, ErrMiss) {
		t.Fatalf("Open = %v, want a failure that is not a miss", err)
	}
	if err := c.Save(context.Background(), keyK, "/nonexistent", 1); err == nil {
		t.Fatal("Save reported success")
	}
}
