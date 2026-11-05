package gitproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// certFor returns a leaf for github.com and the PEM of the CA that signed it.
func certFor(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: Host}, DNSNames: []string{Host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// realGit is a real git client, through this proxy over TLS, to a real
// git http-backend standing in for github.com.
type realGit struct {
	t       *testing.T
	proxy   string
	caFile  string
	head    string
	fetches atomic.Int32
	cert    tls.Certificate
	dial    string
}

func newRealGit(t *testing.T) *realGit {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	exe := gitCmd(t, t.TempDir(), "--exec-path")
	backend := filepath.Join(exe, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("needs git http-backend")
	}
	root := t.TempDir()
	work := t.TempDir()
	gitCmd(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "file")
	gitCmd(t, work, "commit", "-q", "-m", "first")
	bare := filepath.Join(root, "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "clone", "-q", "--bare", work, bare)
	gitCmd(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	g := &realGit{t: t, head: gitCmd(t, work, "rev-parse", "HEAD")}
	cgiHandler := &cgi.Handler{
		Path: backend,
		Dir:  root,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			g.fetches.Add(1)
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	g.dial = upstream.Listener.Addr().String()

	leaf, caPEM := certFor(t)
	g.cert = leaf
	g.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(g.caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return g
}

// startProxy runs a proxy, as one instance would, and returns it.
func (g *realGit) startProxy(c Cache) *Proxy {
	g.t.Helper()
	p := New(Config{
		Certificate: g.cert,
		Cache:       c,
		TempDir:     g.t.TempDir(),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test's own upstream
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, g.dial)
			},
		},
	})
	srv := httptest.NewServer(p)
	g.t.Cleanup(srv.Close)
	g.proxy = srv.URL
	return p
}

// fetch runs what a shallow checkout runs, in a fresh repository.
func (g *realGit) fetch() (string, error) {
	dir := g.t.TempDir()
	gitCmd(g.t, dir, "init", "-q")
	cmd := exec.Command("git",
		"-c", "protocol.version=2",
		"-c", "http.https://github.com/.proxy="+g.proxy,
		"-c", "http.https://github.com/.sslCAInfo="+g.caFile,
		"fetch", "-q", "--depth=1", "--no-tags", "https://github.com/o/r.git", g.head)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &fetchError{err: err, out: string(out)}
	}
	return gitCmd(g.t, dir, "rev-parse", "FETCH_HEAD"), nil
}

type fetchError struct {
	err error
	out string
}

func (e *fetchError) Error() string { return e.err.Error() + ": " + e.out }

func waitFor(t *testing.T, p *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.Wait(ctx)
}

func TestRealGit_MissThenHit(t *testing.T) {
	g := newRealGit(t)
	c := newFakeCache()

	p := g.startProxy(c)
	got, err := g.fetch()
	if err != nil || got != g.head {
		t.Fatalf("first fetch = %q, %v", got, err)
	}
	waitFor(t, p)
	if c.count() != 1 {
		t.Fatalf("%d entries stored", c.count())
	}
	before := g.fetches.Load()

	g.startProxy(c) // a new instance
	got, err = g.fetch()
	if err != nil || got != g.head {
		t.Fatalf("second fetch = %q, %v", got, err)
	}
	if g.fetches.Load() != before {
		t.Fatal("the hit reached the upstream")
	}
}

func TestRealGit_RefusesATamperedEntryAndRecoversOnRetry(t *testing.T) {
	g := newRealGit(t)
	c := newFakeCache()
	p := g.startProxy(c)
	if _, err := g.fetch(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p)
	for k, v := range c.entries {
		i := strings.Index(v, "PACK")
		if i < 0 {
			t.Fatal("no pack in the entry")
		}
		b := []byte(v)
		b[i+40] ^= 0xff // inside the pack, so the framing still reads as whole
		c.entries[k] = string(b)
	}
	before := g.fetches.Load()

	g.startProxy(c)
	if got, err := g.fetch(); err == nil {
		t.Fatalf("git accepted a tampered pack: %s", got)
	}
	// actions/checkout retries a failed fetch.
	got, err := g.fetch()
	if err != nil || got != g.head {
		t.Fatalf("retry = %q, %v", got, err)
	}
	if g.fetches.Load() == before {
		t.Fatal("the retry did not reach the upstream")
	}
}

func TestRealGit_CacheDownIsInvisible(t *testing.T) {
	g := newRealGit(t)
	c := newFakeCache()
	c.openErr = io.ErrUnexpectedEOF
	c.saveErr = io.ErrUnexpectedEOF
	g.startProxy(c)
	if got, err := g.fetch(); err != nil || got != g.head {
		t.Fatalf("fetch = %q, %v", got, err)
	}
}
