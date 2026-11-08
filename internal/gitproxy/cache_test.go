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
	if _, err := read(t, c, "git/abc"); !errors.Is(err, ErrMiss) {
		t.Fatalf("empty cache: %v", err)
	}
	saveFile(t, c, "git/abc", goodPack)
	got, err := read(t, c, "git/abc")
	if err != nil || got != goodPack {
		t.Fatalf("read = %q, %v", got, err)
	}
	// A second job storing the same key is not an error.
	saveFile(t, c, "git/abc", goodPack)
}

// Isolation is cache-server's, by the job's identity. A client cannot ask for
// more, so these go through the real server.
func TestCacheServer_Isolation(t *testing.T) {
	f := newCacheFixture(t, job("o/r", "refs/heads/main"))
	c := &CacheServer{URL: f.url}
	saveFile(t, c, "git/k", goodPack)

	*f.who = job("o/other", "refs/heads/main")
	if _, err := read(t, c, "git/k"); !errors.Is(err, ErrMiss) {
		t.Fatalf("another repository read the entry: %v", err)
	}

	// A pull request's writes stay on its ref, and the default branch never
	// reads them, so a pull request cannot poison main.
	*f.who = job("o/r", "refs/pull/7/merge", "refs/heads/main")
	saveFile(t, c, "git/pr", "written by a pull request")
	*f.who = job("o/r", "refs/heads/main")
	if _, err := read(t, c, "git/pr"); !errors.Is(err, ErrMiss) {
		t.Fatalf("main read a pull request's entry: %v", err)
	}

	// The pull request can read main's.
	*f.who = job("o/r", "refs/pull/7/merge", "refs/heads/main")
	if got, err := read(t, c, "git/k"); err != nil || got != goodPack {
		t.Fatalf("a pull request could not read main: %q, %v", got, err)
	}
}

func TestCacheServer_Unreachable(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	c := &CacheServer{URL: url}
	if _, err := c.Open(context.Background(), "git/k"); err == nil || errors.Is(err, ErrMiss) {
		t.Fatalf("Open = %v, want a failure that is not a miss", err)
	}
	if err := c.Save(context.Background(), "git/k", "/nonexistent", 1); err == nil {
		t.Fatal("Save reported success")
	}
}
