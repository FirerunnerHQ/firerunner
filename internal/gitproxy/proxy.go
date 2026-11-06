// Package gitproxy is the instance's side of the git checkout cache: a loopback
// HTTP proxy that git alone is pointed at, through
// http.https://github.com/.proxy. It terminates TLS for github.com and answers
// a fetch that package gitcache accepts from cache-server, storing what it had
// to fetch. Everything else, pushes and ref listings included, passes through.
//
// # Trust
//
// A job can read this process's leaf key and, with sudo, replace the process, so
// nothing here is a security boundary. cache-server scopes every entry by the
// job's host-authenticated identity, and git verifies every object against the
// commit it asked for. What this package does is keep an honest job correct:
// a hit needs GitHub to have accepted the same credentials for that repository
// moments earlier, because a cached response skips GitHub's own check.
//
// # Failure
//
// A cache fault must not fail a checkout that GitHub would have served, and not
// every client retries. So an entry is downloaded and checked before its first
// byte goes out, and any failure until then goes to GitHub. An entry can pass
// that and still hold the wrong objects, if a job wrote it so: git refuses it,
// and as a key is served from the cache once per proxy, a retry reaches GitHub.
//
// The one thing this cannot cover is this process dying: a fetch in flight
// fails. The unit then points git back at GitHub for the ones that follow.
package gitproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/firerunnerhq/firerunner/internal/gitcache"
)

const (
	// Host is the only host the proxy will carry.
	Host = "github.com"

	resultType = "application/x-git-upload-pack-result"

	// maxRequest bounds the request kept in memory to read it. A fresh fetch
	// is far smaller; one that is not goes to GitHub unread.
	maxRequest = 1 << 20

	// authorizedFor is how long a successful ref listing authorises hits for
	// the same credentials. git lists refs just before it fetches.
	authorizedFor = 10 * time.Minute

	// DefaultLookupTimeout bounds a cache lookup. It is long because
	// cache-server waits for the job's identity, and a checkout is usually a
	// job's first step: a shorter wait would turn most of them into misses.
	DefaultLookupTimeout = 15 * time.Second

	// DefaultHitTimeout bounds downloading an entry, lookup included. Past it
	// GitHub will likely do better.
	DefaultHitTimeout = 2 * time.Minute

	// DefaultSaveTimeout bounds storing one response.
	DefaultSaveTimeout = 5 * time.Minute

	// DefaultMaxSave is the largest response stored.
	DefaultMaxSave = 2 << 30

	// DefaultMinFree is the free space below which the proxy keeps nothing on
	// disk. A pack is on disk twice while it is being stored, and the disk is
	// the job's.
	DefaultMinFree = 5 << 30

	// recheckEvery is how much a response may grow between looks at the free
	// space, which the job and other fetches are using too.
	recheckEvery = 64 << 20
)

// ErrMiss is what Cache.Open returns when it holds no entry for a key.
var ErrMiss = errors.New("no cache entry")

// Cache stores responses by key.
type Cache interface {
	// Open returns the entry for key, or ErrMiss.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Save stores the file at path under key.
	Save(ctx context.Context, key, path string, size int64) error
}

// Config configures a Proxy.
type Config struct {
	Certificate tls.Certificate // for github.com, trusted through git's sslCAInfo
	Cache       Cache
	Transport   http.RoundTripper // to GitHub; nil means http.DefaultTransport
	TempDir     string            // for responses on their way; empty means the system's

	LookupTimeout time.Duration
	HitTimeout    time.Duration
	SaveTimeout   time.Duration
	MaxSave       int64
	MinFree       int64
	Log           *slog.Logger
}

// Proxy is an http.Handler for the CONNECT requests git sends its proxy.
type Proxy struct {
	cfg   Config
	saves sync.WaitGroup
	free  func(dir string) (int64, error)

	mu         sync.Mutex
	authorized map[string]time.Time // repository and credentials, to when it was seen
	served     map[string]bool      // keys answered from the cache already
	now        func() time.Time
}

// New returns a Proxy, filling in the defaults.
func New(cfg Config) *Proxy {
	if cfg.Transport == nil {
		cfg.Transport = http.DefaultTransport
	}
	if cfg.LookupTimeout <= 0 {
		cfg.LookupTimeout = DefaultLookupTimeout
	}
	if cfg.HitTimeout <= 0 {
		cfg.HitTimeout = DefaultHitTimeout
	}
	if cfg.SaveTimeout <= 0 {
		cfg.SaveTimeout = DefaultSaveTimeout
	}
	if cfg.MaxSave <= 0 {
		cfg.MaxSave = DefaultMaxSave
	}
	if cfg.MinFree <= 0 {
		cfg.MinFree = DefaultMinFree
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Proxy{
		cfg:        cfg,
		authorized: make(map[string]time.Time),
		served:     make(map[string]bool),
		now:        time.Now,
		free:       freeSpace,
	}
}

// tempPattern names this package's temporary files.
const tempPattern = "gitproxy-*"

// ClearTemp removes files a killed proxy left in dir, or in the system's
// temporary directory when dir is empty.
func ClearTemp(dir string) error {
	if dir == "" {
		dir = os.TempDir()
	}
	old, err := filepath.Glob(filepath.Join(dir, tempPattern))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range old {
		errs = append(errs, os.Remove(path))
	}
	return errors.Join(errs...)
}

func (p *Proxy) tempDir() string {
	if p.cfg.TempDir != "" {
		return p.cfg.TempDir
	}
	return os.TempDir()
}

// roomOnDisk reports whether there is room to keep a response: the job's
// writes matter more than this proxy's.
func (p *Proxy) roomOnDisk() bool {
	free, err := p.free(p.tempDir())
	if err != nil || free < p.cfg.MinFree {
		p.cfg.Log.Warn("git cache: disk short of room; keeping nothing", "err", err, "free", free)
		return false
	}
	return true
}

// tempFile returns a file for a response, or nil when it should not keep one.
func (p *Proxy) tempFile() *os.File {
	if !p.roomOnDisk() {
		return nil
	}
	f, err := os.CreateTemp(p.tempDir(), tempPattern)
	if err != nil {
		p.cfg.Log.Warn("git cache: no temporary file", "err", err)
		return nil
	}
	return f
}

func freeSpace(dir string) (int64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * int64(s.Bsize), nil
}

// Wait blocks until responses being stored have finished, or ctx ends.
func (p *Proxy) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { p.saves.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ServeHTTP answers CONNECT github.com:443 and refuses everything else, so the
// proxy cannot be used to reach any other host.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// git matches a URL's host without regard to case, so GitHub.com arrives.
	if r.Method != http.MethodConnect || !strings.EqualFold(r.Host, Host+":443") {
		http.Error(w, "only CONNECT "+Host+":443", http.StatusForbidden)
		return
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := h.Hijack()
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{p.cfg.Certificate},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	inner := &http.Server{
		Handler:           http.HandlerFunc(p.serveGit),
		ReadHeaderTimeout: time.Minute,
		ErrorLog:          slog.NewLogLogger(p.cfg.Log.Handler(), slog.LevelDebug),
	}
	listener := newOneConn(tlsConn)
	inner.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed || s == http.StateHijacked {
			listener.Close()
		}
	}
	go func() { _ = inner.Serve(listener) }()
}

func (p *Proxy) serveGit(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") &&
		r.URL.Query().Get("service") == "git-upload-pack":
		p.serveRefs(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		p.serveFetch(w, r)
	default:
		p.forward(w, r, r.Body, nil)
	}
}

// serveRefs forwards a ref listing. GitHub answering it is what lets these
// credentials read this repository from the cache.
func (p *Proxy) serveRefs(w http.ResponseWriter, r *http.Request) {
	status := p.forward(w, r, r.Body, nil)
	repo, ok := gitcache.Repository(strings.TrimSuffix(r.URL.Path, "/info/refs") + "/git-upload-pack")
	if status != http.StatusOK || !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorized[credentials(r, repo)] = p.now()
}

func (p *Proxy) serveFetch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequest+1))
	if err != nil {
		http.Error(w, "reading request", http.StatusBadGateway)
		return
	}
	if len(body) > maxRequest {
		p.forward(w, r, io.MultiReader(bytes.NewReader(body), r.Body), nil)
		return
	}
	entry, ok := p.cacheEntry(r, body)
	if !ok {
		p.forward(w, r, bytes.NewReader(body), nil)
		return
	}
	if p.serveHit(w, r, entry) {
		return
	}
	p.forward(w, r, bytes.NewReader(body), &entry)
}

// cacheEntry names a fetch this proxy may answer from the cache.
type cacheEntry struct {
	key          string
	objectFormat string
}

func (p *Proxy) cacheEntry(r *http.Request, body []byte) (cacheEntry, bool) {
	none := cacheEntry{}
	repo, ok := gitcache.Repository(r.URL.Path)
	if !ok {
		return none, false
	}
	plain, err := decode(r, body)
	if err != nil {
		return none, false
	}
	f, err := gitcache.Parse(plain)
	if err != nil {
		return none, false
	}
	key := f.Key(repo)
	p.mu.Lock()
	defer p.mu.Unlock()
	seen, ok := p.authorized[credentials(r, repo)]
	if !ok || p.now().Sub(seen) > authorizedFor {
		return none, false
	}
	// A second request for a key is a retry, or a job fetching a commit twice.
	// GitHub is right for both, and it gets a retry past a bad entry.
	if p.served[key] {
		return none, false
	}
	return cacheEntry{key: key, objectFormat: f.ObjectFormat}, true
}

// serveHit reports whether the request was answered. The entry comes down and
// is checked before the first byte goes out: until then GitHub can still answer,
// which is the only recovery a client that does not retry has.
func (p *Proxy) serveHit(w http.ResponseWriter, r *http.Request, e cacheEntry) bool {
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.HitTimeout)
	defer cancel()
	lookup := time.AfterFunc(p.cfg.LookupTimeout, cancel)
	entry, err := p.cfg.Cache.Open(ctx, e.key)
	lookup.Stop()
	if err != nil {
		if !errors.Is(err, ErrMiss) {
			p.cfg.Log.Warn("git cache lookup failed", "err", plainError(err))
		}
		return false
	}
	defer entry.Close()
	f := p.tempFile()
	if f == nil {
		return false
	}
	defer os.Remove(f.Name())
	defer f.Close()
	check := newResponseCheck(e.objectFormat)
	size, err := io.Copy(f, io.TeeReader(io.LimitReader(entry, p.cfg.MaxSave+1), check))
	// An entry past the limit is cut off here, which fails the check.
	if err != nil || !check.complete() {
		p.cfg.Log.Warn("git cache entry unusable; asking GitHub", "err", plainError(err), "key", e.key, "bytes", size)
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	p.mu.Lock()
	p.served[e.key] = true
	p.mu.Unlock()
	w.Header().Set("Content-Type", resultType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	if _, err := io.Copy(w, f); err != nil {
		panic(http.ErrAbortHandler) // the client has gone
	}
	return true
}

// forward sends r to GitHub and streams the answer back. With saveAs set, it
// also stores a complete answer. It returns GitHub's status.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body io.Reader, saveAs *cacheEntry) int {
	// Built from parts: a request target can hold anything, and pasted after
	// the host, "@example.com/" would send the job's token to example.com.
	if r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, "/") {
		http.Error(w, "bad request target", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	target := url.URL{Scheme: "https", Host: Host, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	req.Header = r.Header.Clone()
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	req.ContentLength = -1
	if b, ok := body.(*bytes.Reader); ok {
		req.ContentLength = int64(b.Len())
	} else if r.ContentLength >= 0 {
		req.ContentLength = r.ContentLength
	}
	res, err := p.cfg.Transport.RoundTrip(req)
	if err != nil {
		p.cfg.Log.Warn("reaching GitHub failed", "err", plainError(err))
		http.Error(w, "reaching "+Host, http.StatusBadGateway)
		return http.StatusBadGateway
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)

	var save *pendingSave
	if saveAs != nil && res.StatusCode == http.StatusOK && res.Header.Get("Content-Type") == resultType {
		save = p.newSave(*saveAs)
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if save != nil {
				save.write(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				save.discard()
				return res.StatusCode
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			save.finish()
			return res.StatusCode
		}
		if rerr != nil {
			save.discard()
			panic(http.ErrAbortHandler) // git must see the break, not a clean end
		}
	}
}

// pendingSave collects one response on its way to the client. Trouble here
// costs an entry, never the client's response.
type pendingSave struct {
	p     *Proxy
	key   string
	file  *os.File
	check *responseCheck
	size  int64
}

func (p *Proxy) newSave(e cacheEntry) *pendingSave {
	f := p.tempFile()
	if f == nil {
		return nil
	}
	return &pendingSave{p: p, key: e.key, file: f, check: newResponseCheck(e.objectFormat)}
}

func (s *pendingSave) write(b []byte) {
	if s.file == nil {
		return
	}
	s.size += int64(len(b))
	_, _ = s.check.Write(b)
	if s.size > s.p.cfg.MaxSave {
		s.discard()
		return
	}
	if s.size/recheckEvery != (s.size-int64(len(b)))/recheckEvery && !s.p.roomOnDisk() {
		s.discard()
		return
	}
	if _, err := s.file.Write(b); err != nil {
		s.p.cfg.Log.Warn("git cache: temporary file", "err", err)
		s.discard()
	}
}

func (s *pendingSave) discard() {
	if s == nil || s.file == nil {
		return
	}
	s.file.Close()
	os.Remove(s.file.Name())
	s.file = nil
}

// finish stores the response in the background, as the client has all of it. A
// response that is not whole is never stored: every later job would download it
// only to refuse it.
func (s *pendingSave) finish() {
	if s == nil || s.file == nil {
		return
	}
	if !s.check.complete() {
		s.p.cfg.Log.Warn("git cache: incomplete response not stored", "key", s.key)
		s.discard()
		return
	}
	path := s.file.Name()
	if err := s.file.Close(); err != nil {
		os.Remove(path)
		return
	}
	s.p.saves.Add(1)
	go func() {
		defer s.p.saves.Done()
		defer os.Remove(path)
		ctx, cancel := context.WithTimeout(context.Background(), s.p.cfg.SaveTimeout)
		defer cancel()
		if err := s.p.cfg.Cache.Save(ctx, s.key, path, s.size); err != nil {
			s.p.cfg.Log.Warn("git cache: store failed", "err", plainError(err))
		}
	}()
}

// plainError leaves out a failed request's URL, which holds a blob's signature.
func plainError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// credentials names a repository and the credentials used on it, without
// keeping the credentials.
func credentials(r *http.Request, repo string) string {
	h := sha256.Sum256([]byte(repo + "\x00" + r.Header.Get("Authorization")))
	return string(h[:])
}

// decode returns the request's body, gunzipped as git sends a large one. The
// limit is the same as for a plain one: a body that inflates past it is no
// fresh fetch.
func decode(r *http.Request, body []byte) ([]byte, error) {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return body, nil
	}
	z, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	plain, err := io.ReadAll(io.LimitReader(z, maxRequest+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > maxRequest {
		return nil, fmt.Errorf("request inflates past %d bytes", maxRequest)
	}
	return plain, nil
}

// oneConn is a listener that hands out a single connection, then blocks until
// closed, as http.Server wants a listener.
type oneConn struct {
	conn net.Conn
	once sync.Once
	stop chan struct{}
	end  sync.Once
}

func newOneConn(c net.Conn) *oneConn { return &oneConn{conn: c, stop: make(chan struct{})} }

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.stop
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.end.Do(func() { close(l.stop) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }
