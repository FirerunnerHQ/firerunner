package gitproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // a pack's own checksum
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

const oid = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"

func pkt(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

var fetchBody = "" + pkt("command=fetch\n") + pkt("agent=git/2.56.0\n") + pkt("object-format=sha1\n") +
	"0001" + pkt("thin-pack\n") + pkt("ofs-delta\n") + pkt("deepen 1\n") + pkt("want "+oid+"\n") + pkt("done\n") + "0000"

// fakePack has a pack's magic and a correct trailer.
func fakePack(body string) string {
	sum := sha1.Sum([]byte("PACK" + body)) //nolint:gosec // as git does
	return "PACK" + body + string(sum[:])
}

// goodPack is a whole response: a shallow section, then a pack on sideband 1,
// in two packets, with progress between them.
var goodPack = func() string {
	p := fakePack("-pack-data-")
	return pkt("shallow-info\n") + pkt("shallow "+oid+"\n") + "0001" + pkt("packfile\n") +
		pkt("\x02Counting objects\n") + pkt("\x01"+p[:6]) + pkt("\x02more\n") + pkt("\x01"+p[6:]) + "0000"
}()

type fakeCache struct {
	mu      sync.Mutex
	entries map[string]string
	openErr error
	saveErr error
	opens   int
	saved   chan string
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: map[string]string{}, saved: make(chan string, 8)}
}

func (c *fakeCache) Open(_ context.Context, key string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens++
	if c.openErr != nil {
		return nil, c.openErr
	}
	e, ok := c.entries[key]
	if !ok {
		return nil, ErrMiss
	}
	return io.NopCloser(strings.NewReader(e)), nil
}

func (c *fakeCache) Save(_ context.Context, key, path string, _ int64) error {
	defer func() { c.saved <- key }()
	if c.saveErr != nil {
		return c.saveErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = string(b)
	return nil
}

func (c *fakeCache) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeGitHub answers ref listings and fetches, and counts the fetches.
type fakeGitHub struct {
	fetches     atomic.Int32
	refCode     int
	status      int
	body        io.Reader // the next fetch's body; goodPack when nil
	contentType string
	lastReq     atomic.Pointer[[]byte]
	header      atomic.Pointer[http.Header]
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	g.lastReq.Store(&b)
	h := r.Header.Clone()
	g.header.Store(&h)
	res := func(code int, body io.Reader) *http.Response {
		contentType := g.contentType
		if contentType == "" {
			contentType = resultType
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(body), Header: http.Header{"Content-Type": {contentType}}, Request: r}
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		code := g.refCode
		if code == 0 {
			code = 200
		}
		return res(code, strings.NewReader("refs")), nil
	}
	g.fetches.Add(1)
	code := g.status
	if code == 0 {
		code = 200
	}
	body := g.body
	if body == nil {
		body = strings.NewReader(goodPack)
	}
	return res(code, body), nil
}

type harness struct {
	t     *testing.T
	gh    *fakeGitHub
	cache *fakeCache
	p     *Proxy
	srv   *httptest.Server
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, gh: &fakeGitHub{}, cache: newFakeCache()}
	cfg := Config{Cache: h.cache, Transport: h.gh, TempDir: t.TempDir(), LookupTimeout: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	if c, ok := cfg.Cache.(*fakeCache); ok {
		h.cache = c
	}
	h.p = New(cfg)
	h.srv = httptest.NewServer(http.HandlerFunc(h.p.serveGit))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(method, path, token, body string, extra ...string) (int, string, error) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, string(b), err
}

func (h *harness) refs(token string) {
	h.t.Helper()
	if code, _, err := h.do("GET", "/O/R.git/info/refs?service=git-upload-pack", token, ""); err != nil || code >= 500 {
		h.t.Fatalf("refs: %d %v", code, err)
	}
}

func (h *harness) fetch(token string) (string, error) {
	h.t.Helper()
	_, body, err := h.do("POST", "/O/R.git/git-upload-pack", token, fetchBody)
	return body, err
}

func (h *harness) waitSaved() string {
	h.t.Helper()
	select {
	case k := <-h.cache.saved:
		return k
	case <-time.After(5 * time.Second):
		h.t.Fatal("nothing was stored")
		return ""
	}
}

// requireSettled waits for background stores, cleanup included.
func (h *harness) requireSettled() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.p.Wait(ctx)
	if ctx.Err() != nil {
		h.t.Fatal("stores did not finish")
	}
}

// requireNothingStored waits for background stores, then checks there were none.
func (h *harness) requireNothingStored() {
	h.t.Helper()
	h.requireSettled()
	if n := h.cache.count(); n != 0 {
		h.t.Fatalf("%d entries stored", n)
	}
}

func (h *harness) tempFiles() int {
	h.t.Helper()
	m, err := filepath.Glob(filepath.Join(h.p.cfg.TempDir, "gitproxy-*"))
	if err != nil {
		h.t.Fatal(err)
	}
	return len(m)
}

func TestMiss_StoresThenAnotherInstanceHits(t *testing.T) {
	h := newHarness(t, nil)
	h.refs("token")
	if got, err := h.fetch("token"); err != nil || got != goodPack {
		t.Fatalf("miss = %q, %v", got, err)
	}
	h.waitSaved()
	h.requireSettled()
	if h.tempFiles() != 0 {
		t.Fatal("the temporary file stayed")
	}

	other := newHarness(t, func(c *Config) { c.Cache = h.cache })
	other.refs("token")
	if got, err := other.fetch("token"); err != nil || got != goodPack {
		t.Fatalf("hit = %q, %v", got, err)
	}
	if n := other.gh.fetches.Load(); n != 0 {
		t.Fatalf("a hit reached GitHub %d times", n)
	}
}

// A cached response skips GitHub's own check, so GitHub must have accepted
// these credentials for this repository first.
func TestHit_NeedsGitHubToHaveAcceptedTheCredentials(t *testing.T) {
	seed := func(h *harness) {
		h.t.Helper()
		other := newHarness(h.t, func(c *Config) { c.Cache = h.cache })
		other.refs("token")
		if _, err := other.fetch("token"); err != nil {
			h.t.Fatal(err)
		}
		other.waitSaved()
	}
	t.Run("no ref listing", func(t *testing.T) {
		h := newHarness(t, nil)
		seed(h)
		if _, err := h.fetch("token"); err != nil {
			t.Fatal(err)
		}
		if h.gh.fetches.Load() != 1 {
			t.Fatal("served from the cache with no ref listing")
		}
	})
	t.Run("other credentials", func(t *testing.T) {
		h := newHarness(t, nil)
		seed(h)
		h.refs("token")
		if _, err := h.fetch("someone-else"); err != nil {
			t.Fatal(err)
		}
		if h.gh.fetches.Load() != 1 {
			t.Fatal("one token's listing let another read the cache")
		}
	})
	t.Run("another repository", func(t *testing.T) {
		h := newHarness(t, nil)
		seed(h)
		if code, _, err := h.do("GET", "/O/Other.git/info/refs?service=git-upload-pack", "token", ""); err != nil || code != 200 {
			t.Fatal(code, err)
		}
		if _, err := h.fetch("token"); err != nil {
			t.Fatal(err)
		}
		if h.gh.fetches.Load() != 1 {
			t.Fatal("one repository's listing authorised another")
		}
	})
	t.Run("refused listing", func(t *testing.T) {
		h := newHarness(t, nil)
		seed(h)
		h.gh.refCode = http.StatusUnauthorized
		h.refs("token")
		if _, err := h.fetch("token"); err != nil {
			t.Fatal(err)
		}
		if h.gh.fetches.Load() != 1 {
			t.Fatal("a refused listing authorised a read")
		}
	})
	t.Run("stale listing", func(t *testing.T) {
		h := newHarness(t, nil)
		seed(h)
		h.refs("token")
		h.p.now = func() time.Time { return time.Now().Add(authorizedFor + time.Minute) }
		if _, err := h.fetch("token"); err != nil {
			t.Fatal(err)
		}
		if h.gh.fetches.Load() != 1 {
			t.Fatal("a stale listing authorised a read")
		}
	})
}

func TestLogs_LeaveOutSignedURLs(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(c *Config) { c.Log = slog.New(slog.NewTextHandler(&logged, nil)) })
	h.cache.openErr = &url.Error{Op: "Get", URL: "http://127.0.0.1/download/1?sig=secret", Err: errors.New("refused")}
	h.refs("t")
	if _, err := h.fetch("t"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "refused") || strings.Contains(logged.String(), "secret") {
		t.Fatalf("logged %q", logged.String())
	}
}

func TestLookupFailure_FallsThroughToGitHub(t *testing.T) {
	h := newHarness(t, nil)
	h.cache.openErr = errors.New("cache-server is down")
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if h.gh.fetches.Load() != 1 {
		t.Fatal("GitHub was not asked")
	}
	if h.cache.opens != 1 {
		t.Fatal("the cache was never asked, so this proved nothing")
	}
}

// A lookup stuck past its limit must not hold the checkout.
func TestLookupStuck_FallsThroughToGitHub(t *testing.T) {
	stuck := &stuckCache{}
	h := newHarness(t, func(c *Config) {
		c.Cache = stuck
		c.LookupTimeout = 50 * time.Millisecond
	})
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if !stuck.opened.Load() {
		t.Fatal("the cache was never asked, so this proved nothing")
	}
}

type stuckCache struct {
	Cache
	opened atomic.Bool
}

func (s *stuckCache) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	s.opened.Store(true)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*stuckCache) Save(context.Context, string, string, int64) error { return nil }

// The lookup's limit must not reach the stream that follows it.
func TestHit_OutlivesTheLookupLimit(t *testing.T) {
	entry := slowEntry{step: 30 * time.Millisecond}
	h := newHarness(t, func(c *Config) {
		c.Cache = entry
		c.LookupTimeout = 20 * time.Millisecond
	})
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
}

type slowEntry struct {
	Cache
	step time.Duration
}

func (s slowEntry) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		for _, part := range []string{goodPack[:10], goodPack[10:]} {
			select {
			case <-ctx.Done():
				pw.CloseWithError(ctx.Err())
				return
			case <-time.After(s.step):
			}
			_, _ = pw.Write([]byte(part))
		}
		pw.Close()
	}()
	return pr, nil
}
func (slowEntry) Save(context.Context, string, string, int64) error { return nil }

// A client may not retry, so nothing broken reaches it.
func TestUnusableHit_FallsThroughToGitHub(t *testing.T) {
	for name, entry := range map[string]func() io.Reader{
		"empty":               func() io.Reader { return strings.NewReader("") },
		"read failure":        func() io.Reader { return &brokenBody{data: strings.NewReader("")} },
		"breaks midway":       func() io.Reader { return &brokenBody{data: strings.NewReader(goodPack[:20])} },
		"error with its data": func() io.Reader { return &failsWithData{data: goodPack} },
		"truncated":           func() io.Reader { return strings.NewReader(goodPack[:len(goodPack)-8]) },
		"not a pack":          func() io.Reader { return strings.NewReader("not a git response at all") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) { c.Cache = readerCache{entry()} })
			h.refs("t")
			if got, err := h.fetch("t"); err != nil || got != goodPack {
				t.Fatalf("fetch = %q, %v", got, err)
			}
			if h.gh.fetches.Load() != 1 {
				t.Fatal("GitHub was not asked")
			}
			h.requireSettled()
			if h.tempFiles() != 0 {
				t.Fatal("the temporary file stayed")
			}
		})
	}
}

// A hit stuck mid-stream must not hold the checkout either.
func TestHitStuck_FallsThroughToGitHub(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Cache = slowEntry{step: time.Hour}
		c.HitTimeout = 50 * time.Millisecond
	})
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
}

func TestOversizedEntry_FallsThroughToGitHub(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Cache = readerCache{strings.NewReader(goodPack)}
		c.MaxSave = 8
	})
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if h.gh.fetches.Load() != 1 {
		t.Fatal("GitHub was not asked")
	}
}

type readerCache struct{ r io.Reader }

func (c readerCache) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(c.r), nil
}
func (readerCache) Save(context.Context, string, string, int64) error { return nil }

// failsWithData returns its data and an error from the same Read.
type failsWithData struct {
	data string
	done bool
}

func (f *failsWithData) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	f.done = true
	return copy(p, f.data), errors.New("connection reset")
}

// A read that returns data and an error together must not lose the data.
func TestHit_ServesDataThatArrivesWithEOF(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Cache = readerCache{iotest.DataErrReader(strings.NewReader(goodPack))} })
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	if h.gh.fetches.Load() != 0 {
		t.Fatal("GitHub was asked")
	}
}

// An entry that looks whole can still hold the wrong objects. git refuses it,
// and its retry must not meet the same entry.
func TestServedKey_GoesToGitHubTheSecondTime(t *testing.T) {
	c := newFakeCache()
	seed := newHarness(t, func(cfg *Config) { cfg.Cache = c })
	seed.refs("t")
	if _, err := seed.fetch("t"); err != nil {
		t.Fatal(err)
	}
	seed.waitSaved()
	h := newHarness(t, func(cfg *Config) { cfg.Cache = c })
	h.refs("t")
	for i := 0; i < 3; i++ {
		if _, err := h.fetch("t"); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.gh.fetches.Load(); n != 2 {
		t.Fatalf("GitHub saw %d fetches, want 2 after one hit", n)
	}
}

func TestNotStored(t *testing.T) {
	cases := map[string]string{
		"truncated":        goodPack[:len(goodPack)-4],
		"error line":       pkt("ERR upload-pack: not our ref\n"),
		"sideband failure": pkt("packfile\n") + pkt("\x03fatal: pack failed\n") + "0000",
		"bad checksum":     pkt("packfile\n") + pkt("\x01PACK-and-no-trailer-at-all") + "0000",
		"no packfile":      pkt("acknowledgments\n") + pkt("NAK\n") + "0000",
		"trailing bytes":   goodPack + "0000",
		"not pkt-lines":    "<html>rate limited</html>",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.gh.body = strings.NewReader(body)
			h.refs("t")
			got, err := h.fetch("t")
			if err != nil || got != body {
				t.Fatalf("the client must get what GitHub sent: %q, %v", got, err)
			}
			h.requireNothingStored()
			if h.tempFiles() != 0 {
				t.Fatal("the temporary file stayed")
			}
		})
	}
}

func TestNotStored_OtherContentType(t *testing.T) {
	h := newHarness(t, nil)
	h.gh.contentType = "text/html"
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	h.requireNothingStored()
}

func TestNotStored_ErrorStatus(t *testing.T) {
	h := newHarness(t, nil)
	h.gh.status = http.StatusServiceUnavailable
	h.refs("t")
	code, _, err := h.do("POST", "/O/R.git/git-upload-pack", "t", fetchBody)
	if err != nil || code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, %v", code, err)
	}
	h.requireNothingStored()
}

type brokenBody struct{ data io.Reader }

func (b *brokenBody) Read(p []byte) (int, error) {
	n, err := b.data.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestGitHubBreaksMidway_TheClientSeesItAndNothingIsStored(t *testing.T) {
	h := newHarness(t, nil)
	h.gh.body = &brokenBody{data: strings.NewReader(goodPack)}
	h.refs("t")
	if _, err := h.fetch("t"); err == nil {
		t.Fatal("a broken response ended cleanly")
	}
	h.requireNothingStored()
	if h.tempFiles() != 0 {
		t.Fatal("the temporary file stayed")
	}
}

func TestTooLarge_ReachesTheClientUnstored(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxSave = 16 })
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	h.requireNothingStored()
	if h.tempFiles() != 0 {
		t.Fatal("the temporary file stayed")
	}
}

func TestSaveFailure_CostsOnlyTheEntry(t *testing.T) {
	h := newHarness(t, nil)
	h.cache.saveErr = errors.New("disk full")
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	h.waitSaved()
	h.requireSettled()
	if h.tempFiles() != 0 {
		t.Fatal("the temporary file stayed")
	}
}

func TestNoTempDir_StillServes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.TempDir = filepath.Join(t.TempDir(), "absent") })
	h.refs("t")
	if got, err := h.fetch("t"); err != nil || got != goodPack {
		t.Fatalf("fetch = %q, %v", got, err)
	}
	h.requireNothingStored()
}

func TestUncacheable_PassesThroughUnchanged(t *testing.T) {
	h := newHarness(t, nil)
	h.refs("t")
	have := strings.Replace(fetchBody, pkt("done\n"), pkt("have "+oid+"\n")+pkt("done\n"), 1)
	if _, body, err := h.do("POST", "/O/R.git/git-upload-pack", "t", have); err != nil || body != goodPack {
		t.Fatalf("fetch = %q, %v", body, err)
	}
	if got := *h.gh.lastReq.Load(); string(got) != have {
		t.Fatalf("GitHub got %q", got)
	}
	if h.cache.opens != 0 {
		t.Fatal("the cache was consulted")
	}
	h.requireNothingStored()
}

func TestGzipRequest_IsKeyedAndForwardedAsItCame(t *testing.T) {
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write([]byte(fetchBody))
	w.Close()
	h := newHarness(t, nil)
	h.refs("t")
	if _, body, err := h.do("POST", "/O/R.git/git-upload-pack", "t", z.String(), "Content-Encoding", "gzip"); err != nil || body != goodPack {
		t.Fatalf("fetch = %q, %v", body, err)
	}
	h.waitSaved()
	if got := *h.gh.lastReq.Load(); !bytes.Equal(got, z.Bytes()) {
		t.Fatal("GitHub did not get the compressed body")
	}
	if enc := (*h.gh.header.Load()).Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q", enc)
	}
}

func TestBadGzip_GoesToGitHub(t *testing.T) {
	h := newHarness(t, nil)
	h.refs("t")
	if _, _, err := h.do("POST", "/O/R.git/git-upload-pack", "t", "not gzip", "Content-Encoding", "gzip"); err != nil {
		t.Fatal(err)
	}
	if h.cache.opens != 0 || h.gh.fetches.Load() != 1 {
		t.Fatal("a body that cannot be read must go to GitHub")
	}
}

func TestLargeRequest_GoesToGitHubWhole(t *testing.T) {
	h := newHarness(t, nil)
	h.refs("t")
	big := strings.Repeat("x", maxRequest+10)
	if _, _, err := h.do("POST", "/O/R.git/git-upload-pack", "t", big); err != nil {
		t.Fatal(err)
	}
	if got := *h.gh.lastReq.Load(); string(got) != big {
		t.Fatalf("GitHub got %d of %d bytes", len(got), len(big))
	}
}

func TestPush_PassesThrough(t *testing.T) {
	h := newHarness(t, nil)
	if _, _, err := h.do("POST", "/O/R.git/git-receive-pack", "t", "PACK-to-push"); err != nil {
		t.Fatal(err)
	}
	if got := *h.gh.lastReq.Load(); string(got) != "PACK-to-push" {
		t.Fatalf("GitHub got %q", got)
	}
	if auth := (*h.gh.header.Load()).Get("Authorization"); auth != "t" {
		t.Fatalf("Authorization = %q", auth)
	}
	if h.cache.opens != 0 {
		t.Fatal("a push consulted the cache")
	}
}

func TestProxyCredentialsStopHere(t *testing.T) {
	h := newHarness(t, nil)
	if _, _, err := h.do("GET", "/O/R.git/info/refs?service=git-upload-pack", "t", "", "Proxy-Authorization", "secret"); err != nil {
		t.Fatal(err)
	}
	if got := (*h.gh.header.Load()).Get("Proxy-Authorization"); got != "" {
		t.Fatalf("forwarded Proxy-Authorization %q", got)
	}
}

// A request target is the client's to choose. None may leave github.com, where
// the job's token would follow it.
func TestForward_StaysOnGitHub(t *testing.T) {
	h := newHarness(t, nil)
	var hosts []string
	h.p.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		hosts = append(hosts, r.URL.Host)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})
	send := func(target string) {
		t.Helper()
		conn, err := net.Dial("tcp", h.srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: github.com\r\nAuthorization: secret\r\nConnection: close\r\n\r\n", target)
		_, _ = io.ReadAll(conn)
	}
	for _, target := range []string{"https:@example.com/path", "http://example.com/path", "*"} {
		send(target)
		if len(hosts) != 0 {
			t.Fatalf("%q was forwarded, to %v", target, hosts)
		}
	}
	// An odd path is only a path, and stays on github.com.
	send("//example.com/path")
	send("/@example.com/path")
	if len(hosts) != 2 {
		t.Fatalf("forwarded to %v, want the two plain paths", hosts)
	}
	for _, host := range hosts {
		if host != Host {
			t.Errorf("forwarded to %q", host)
		}
	}
}

func TestForward_KeepsThePathAndQuery(t *testing.T) {
	h := newHarness(t, nil)
	var got string
	h.p.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
	})
	h.refs("t")
	if want := "https://github.com/O/R.git/info/refs?service=git-upload-pack"; got != want {
		t.Fatalf("forwarded %q, want %q", got, want)
	}
}

func TestGitHubUnreachable_IsABadGateway(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Transport = roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })
	})
	if code, _, err := h.do("GET", "/O/R.git/info/refs?service=git-upload-pack", "t", ""); err != nil || code != http.StatusBadGateway {
		t.Fatalf("status = %d, %v", code, err)
	}
}

func TestServeHTTP_AcceptsGitHubInAnyCase(t *testing.T) {
	srv := httptest.NewServer(New(Config{Cache: newFakeCache()}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT GitHub.com:443 HTTP/1.1\r\nHost: GitHub.com:443\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d", res.StatusCode)
	}
}

func TestServeHTTP_RefusesAnythingButGitHub(t *testing.T) {
	srv := httptest.NewServer(New(Config{Cache: newFakeCache()}))
	defer srv.Close()
	for _, request := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"CONNECT github.com:22 HTTP/1.1\r\nHost: github.com:22\r\n\r\n",
		"CONNECT github.com.example.com:443 HTTP/1.1\r\nHost: github.com.example.com:443\r\n\r\n",
		"GET http://github.com/ HTTP/1.1\r\nHost: github.com\r\n\r\n",
	} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil {
			t.Fatalf("%q: %v", request, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%q = %d, want 403", strings.Fields(request)[1], res.StatusCode)
		}
	}
}

func TestResponseCheck(t *testing.T) {
	pack := fakePack("data")
	corrupt := []byte(pack)
	corrupt[6] ^= 0xff
	notPackSum := sha1.Sum([]byte("NOPEdata")) //nolint:gosec // as git does
	notPack := "NOPEdata" + string(notPackSum[:])
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"whole", goodPack, true},
		{"no shallow section", pkt("packfile\n") + pkt("\x01"+pack) + "0000", true},
		{"packfile without newline", pkt("packfile") + pkt("\x01"+pack) + "0000", true},
		{"a keepalive", pkt("packfile\n") + "0005\x01" + pkt("\x01"+pack) + "0000", true},
		{"no flush", goodPack[:len(goodPack)-4], false},
		{"cut inside a packet", goodPack[:len(goodPack)-7], false},
		{"cut inside a header", goodPack + "00"[:1], false},
		{"error before the pack", pkt("ERR no\n"), false},
		{"error in the pack section", pkt("packfile\n") + pkt("\x01"+pack) + pkt("ERR boom\n") + "0000", false},
		{"sideband error", pkt("packfile\n") + pkt("\x01"+pack) + pkt("\x03boom") + "0000", false},
		{"unknown sideband", pkt("packfile\n") + pkt("\x09"+pack) + "0000", false},
		{"no pack section", pkt("acknowledgments\n") + "0000", false},
		{"no pack data", pkt("packfile\n") + "0000", false},
		{"only progress", pkt("packfile\n") + pkt("\x02hello\n") + "0000", false},
		{"not a pack", pkt("packfile\n") + pkt("\x01"+notPack) + "0000", false},
		{"corrupt pack", pkt("packfile\n") + pkt("\x01"+string(corrupt)) + "0000", false},
		{"short of its trailer", pkt("packfile\n") + pkt("\x01"+pack[:len(pack)-1]) + "0000", false},
		{"bad length", "zzzz", false},
		{"short length", "0003", false},
		{"bytes after the flush", goodPack + "x", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			whole := newResponseCheck("sha1")
			_, _ = whole.Write([]byte(tc.body))
			if whole.complete() != tc.want {
				t.Fatalf("complete = %v, want %v", whole.complete(), tc.want)
			}
			// However the network splits it, the answer is the same.
			for size := 1; size < 8; size++ {
				c := newResponseCheck("sha1")
				for rest := []byte(tc.body); len(rest) > 0; {
					n := min(size, len(rest))
					_, _ = c.Write(rest[:n])
					rest = rest[n:]
				}
				if c.complete() != tc.want {
					t.Fatalf("in pieces of %d: complete = %v, want %v", size, c.complete(), tc.want)
				}
			}
		})
	}
}

func TestResponseCheck_SHA256(t *testing.T) {
	body := "PACKdata"
	sum := sha256.Sum256([]byte(body))
	response := pkt("packfile\n") + pkt("\x01"+body+string(sum[:])) + "0000"
	c := newResponseCheck("sha256")
	_, _ = c.Write([]byte(response))
	if !c.complete() {
		t.Fatal("a sha256 pack was refused")
	}
	c = newResponseCheck("sha1")
	_, _ = c.Write([]byte(response))
	if c.complete() {
		t.Fatal("a sha256 pack passed as sha1")
	}
}
