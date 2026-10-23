package cacheserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/jobidentity"
)

const mainRef = "refs/heads/main"
const baseRef = "refs/heads/release"
const prRef = "refs/pull/1/merge"

// scopeLab uses real HTTP/blob transfers and the production handler. Only the
// transport address is supplied by the fixture, as a tap would supply it.
type scopeLab struct {
	s          *Server
	mu         sync.Mutex
	identities map[string]jobidentity.Identity
}

func newScopeLab(t *testing.T) *scopeLab {
	t.Helper()
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &scopeLab{s: s, identities: map[string]jobidentity.Identity{}}
	if err := s.SetScope(l.lookup); err != nil {
		t.Fatal(err)
	}
	return l
}
func (l *scopeLab) lookup(ip string) (jobidentity.Identity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.identities[ip]
	if !ok {
		return i, errors.New("no job")
	}
	return i, nil
}
func (l *scopeLab) set(ip string, i jobidentity.Identity) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.identities[ip] = i
}
func (l *scopeLab) guest(t *testing.T, ip, ref, job string, readRefs ...string) string {
	t.Helper()
	if ref != "" {
		i := testIdentity("owner/repo", ref, job)
		if len(readRefs) > 0 {
			i.ReadRefs = readRefs
		}
		l.set(ip, i)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.RemoteAddr = ip + ":12345"; l.s.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	return ts.URL
}
func (l *scopeLab) save(t *testing.T, base, key, payload string) string {
	t.Helper()
	c := twirp(t, base, "CreateCacheEntry", createReq{Key: key, Version: "v"})
	if c["ok"] != true {
		t.Fatalf("create: %v", c)
	}
	u := c["signed_upload_url"].(string)
	putBlob(t, u, []byte(payload))
	f := twirp(t, base, "FinalizeCacheEntryUpload", finalizeReq{Key: key, Version: "v", SizeBytes: fmt.Sprint(len(payload))})
	if f["ok"] != true {
		t.Fatalf("finalize: %v", f)
	}
	return u
}
func restore(t *testing.T, base, key string, rks ...string) (string, string) {
	t.Helper()
	g := twirp(t, base, "GetCacheEntryDownloadURL", getReq{Key: key, Version: "v", RestoreKeys: rks})
	if g["ok"] != true {
		return "", ""
	}
	u := g["signed_download_url"].(string)
	r, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		t.Fatalf("download %d: %s", r.StatusCode, b)
	}
	return string(b), u
}
func atGuest(t *testing.T, raw, guest string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return guest + u.RequestURI()
}
func wantStatus(t *testing.T, method, raw string, body []byte, want int) {
	t.Helper()
	req, _ := http.NewRequest(method, raw, bytes.NewReader(body))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != want {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("%s %s: %d want %d: %s", method, raw, r.StatusCode, want, b)
	}
}

func TestRefIsolationAndPrecedenceHTTP(t *testing.T) {
	l := newScopeLab(t)
	main := l.guest(t, "172.16.0.2", mainRef, "main")
	base := l.guest(t, "172.16.1.2", baseRef, "base", baseRef, mainRef)
	pr := l.guest(t, "172.16.2.2", prRef, "pr", prRef, baseRef, mainRef)
	other := l.guest(t, "172.16.3.2", "refs/pull/2/merge", "other", "refs/pull/2/merge", baseRef, mainRef)
	// Identical keys can exist independently in all three scopes.
	l.save(t, pr, "shared", "PR")
	if b, _ := restore(t, main, "shared"); b != "" {
		t.Fatal("PR poisoned main")
	}
	if b, _ := restore(t, other, "shared", ""); b != "" {
		t.Fatal("PR read sibling via empty prefix")
	}
	l.save(t, base, "shared", "BASE")
	l.save(t, main, "shared", "MAIN") // newest globally must not win in the PR
	for _, tt := range []struct{ url, want string }{{pr, "PR"}, {base, "BASE"}, {main, "MAIN"}, {other, "BASE"}} {
		if got, _ := restore(t, tt.url, "shared"); got != tt.want {
			t.Fatalf("restore %s want %s", got, tt.want)
		}
	}
	l.save(t, main, "fallback", "DEFAULT")
	if b, _ := restore(t, pr, "fallback"); b != "DEFAULT" {
		t.Fatalf("default fallback: %q", b)
	}
	// Complete the whole lookup within each ref before trying another ref.
	l.save(t, pr, "dependencies-old", "OWN PREFIX")
	l.save(t, main, "dependencies-new", "DEFAULT EXACT")
	if b, _ := restore(t, pr, "dependencies-new", "dependencies-"); b != "OWN PREFIX" {
		t.Fatalf("scope precedence: %q", b)
	}
	l.save(t, pr, "primary-extra", "PRIMARY PREFIX")
	if b, _ := restore(t, pr, "primary"); b != "PRIMARY PREFIX" {
		t.Fatalf("primary prefix: %q", b)
	}
}

func TestBlobAuthorizationAndReservationOwnership(t *testing.T) {
	l := newScopeLab(t)
	pr := l.guest(t, "172.16.0.2", prRef, "pr", prRef, mainRef)
	peer := l.guest(t, "172.16.1.2", prRef, "peer", prRef, mainRef)
	main := l.guest(t, "172.16.2.2", mainRef, "main")
	c := twirp(t, pr, "CreateCacheEntry", createReq{Key: "pending", Version: "v"})
	u := c["signed_upload_url"].(string)
	putBlob(t, u, []byte("good"))
	for _, guest := range []string{peer, main} {
		wantStatus(t, "PUT", atGuest(t, u, guest), []byte("evil"), 403)
		f := twirp(t, guest, "FinalizeCacheEntryUpload", finalizeReq{Key: "pending", Version: "v", SizeBytes: "4"})
		if f["ok"] != false {
			t.Fatal("foreign job finalized reservation")
		}
	}
	// Even the owner cannot download an unfinished entry with its upload token.
	wantStatus(t, "GET", strings.Replace(u, "/upload/", "/download/", 1), nil, 403)
	twirp(t, pr, "FinalizeCacheEntryUpload", finalizeReq{Key: "pending", Version: "v", SizeBytes: "4"})
	b, download := restore(t, pr, "pending")
	if b != "good" {
		t.Fatal(b)
	}
	wantStatus(t, "GET", atGuest(t, download, main), nil, 403)
	wantStatus(t, "HEAD", atGuest(t, download, main), nil, 403)
	// Same ref can read across jobs, but finalized archives remain immutable.
	wantStatus(t, "GET", atGuest(t, download, peer), nil, 200)
	wantStatus(t, "PUT", u, []byte("evil"), 409)
	wantStatus(t, "PUT", u+"&comp=block&blockid=x", []byte("evil"), 409)
	wantStatus(t, "PUT", u+"&comp=blocklist", []byte("<BlockList/>"), 409)
	req, _ := http.NewRequest("GET", download, nil)
	req.Header.Set("Range", "bytes=1-2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(data) != "oo" {
		t.Fatalf("range %d %q", resp.StatusCode, data)
	}
}

func TestIdentityRevocationAndUnknownJob(t *testing.T) {
	l := newScopeLab(t)
	main := l.guest(t, "172.16.0.2", mainRef, "main")
	pr := l.guest(t, "172.16.1.2", prRef, "old", prRef, mainRef)
	unknown := l.guest(t, "172.16.9.2", "", "missing")
	l.save(t, main, "default", "TRUSTED")
	l.save(t, pr, "private", "PR")
	c := twirp(t, pr, "CreateCacheEntry", createReq{Key: "pending", Version: "v"})
	u := c["signed_upload_url"].(string)
	// Slot reuses the same ref but gets a new job and generation.
	l.set("172.16.1.2", testIdentity("owner/repo", prRef, "new"))
	wantStatus(t, "PUT", u, []byte("stale"), 403)
	f := twirp(t, pr, "FinalizeCacheEntryUpload", finalizeReq{Key: "pending", Version: "v"})
	if f["ok"] != false {
		t.Fatal("new occupant finalized old job")
	}
	for _, i := range []jobidentity.Identity{{}, func() jobidentity.Identity { i := testIdentity("owner/repo", prRef, "old"); i.ExpiresAt = 1; return i }()} {
		l.set("172.16.1.2", i)
		if b, _ := restore(t, pr, "private"); b != "" {
			t.Fatal("revoked job reads PR")
		}
		wantStatus(t, "PUT", u, nil, 403)
	}
	c = twirp(t, unknown, "CreateCacheEntry", createReq{Key: "poison", Version: "v"})
	if c["ok"] != false {
		t.Fatal("unknown job may create")
	}
	if b, _ := restore(t, unknown, "default"); b != "" {
		t.Fatal("unknown job read default cache")
	}
	if b, _ := restore(t, unknown, "private"); b != "" {
		t.Fatal("unknown job reads PR")
	}
	// A known different repository gets no default fallback even with forged metadata.
	l.set("172.16.9.2", testIdentity("owner/foreign", mainRef, "foreign"))
	if b, _ := restore(t, unknown, "default", ""); b != "" {
		t.Fatal("foreign repo read default")
	}
}

func TestConcurrentReservationsPerRef(t *testing.T) {
	l := newScopeLab(t)
	a := l.guest(t, "172.16.0.2", mainRef, "a")
	b := l.guest(t, "172.16.1.2", mainRef, "b")
	p := l.guest(t, "172.16.2.2", prRef, "p")
	start := make(chan struct{})
	results := make(chan bool, 2)
	for _, base := range []string{a, b} {
		go func() {
			<-start
			payload := strings.NewReader(`{"key":"race","version":"v"}`)
			r, err := http.Post(base+twirpBase+"CreateCacheEntry", "application/json", payload)
			if err != nil {
				results <- false
				return
			}
			defer r.Body.Close()
			var result createResp
			_ = json.NewDecoder(r.Body).Decode(&result)
			results <- result.OK
		}()
	}
	close(start)
	wins := 0
	for range 2 {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners", wins)
	}
	c := twirp(t, p, "CreateCacheEntry", createReq{Key: "race", Version: "v"})
	if c["ok"] != true {
		t.Fatal("other ref reservation blocked")
	}
}

func TestScopeMatchingDecisionTable(t *testing.T) {
	s := &Server{entries: map[uint64]*Entry{}}
	add := func(id uint64, ref, key, version string, complete bool) {
		s.entries[id] = &Entry{ID: id, RepoID: "owner/repo", Ref: ref, Key: key, Version: version, Complete: complete, CreatedAt: int64(id)}
	}
	add(1, prRef, "exact", "v", true)
	add(2, prRef, "exact-suffix", "v", true)
	add(3, prRef, "restore", "v", true)
	add(4, prRef, "restore-new", "v", true)
	add(5, baseRef, "base", "v", true)
	add(6, mainRef, "default", "v", true)
	add(7, prRef, "unfinished", "v", false)
	add(8, prRef, "other-version", "v2", true)
	add(9, "refs/pull/2/merge", "sibling", "v", true)
	add(10, "", "legacy", "v", true)
	cases := []struct {
		name, key string
		rks       []string
		want      uint64
	}{
		{"exact before primary prefix", "exact", nil, 1}, {"primary prefix", "ex", nil, 2},
		{"restore exact before newer prefix", "missing", []string{"restore"}, 3},
		{"newest prefix", "missing", []string{"res"}, 4}, {"ordered restore keys", "missing", []string{"exact", "restore"}, 1},
		{"base", "base", nil, 5}, {"default", "default", nil, 6}, {"incomplete", "unfinished", nil, 0},
		{"version", "other-version", nil, 0}, {"sibling", "sibling", nil, 0}, {"legacy", "legacy", nil, 0},
		{"empty prefix stays in own ref", "missing", []string{""}, 4}, {"case insensitive restore", "missing", []string{"EXACT"}, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e := s.match("owner/repo", []string{prRef, baseRef, mainRef}, tt.key, tt.rks, "v")
			var got uint64
			if e != nil {
				got = e.ID
			}
			if got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
	s.entries[4].CreatedAt = s.entries[3].CreatedAt
	if e := s.match("owner/repo", []string{prRef}, "res", nil, "v"); e.ID != 4 {
		t.Fatal("tie break unstable")
	}
	if e := s.match("owner/foreign", []string{prRef}, "exact", []string{""}, "v"); e != nil {
		t.Fatal("repo mismatch")
	}
	if e := s.match("owner/repo", []string{""}, "legacy", nil, "v"); e != nil {
		t.Fatal("empty ref authorized")
	}
}

func TestLegacyEntriesNeverAuthorized(t *testing.T) {
	dir := t.TempDir()
	legacy := `[{"id":90,"sig":"old-token","repo_id":"owner/repo","key":"legacy","version":"v","size":3,"complete":true}]`
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.blobPath(90), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(func(string) (jobidentity.Identity, error) { return testIdentity("owner/repo", mainRef, "job"), nil }); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	if b, _ := restore(t, ts.URL, "legacy", ""); b != "" {
		t.Fatal("legacy restored")
	}
	wantStatus(t, "GET", ts.URL+"/download/90?sig=old-token", nil, 403)
	wantStatus(t, "PUT", ts.URL+"/upload/90?sig=old-token", nil, 403)
	c := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "legacy", Version: "v"})
	if c["ok"] != true || !strings.Contains(c["signed_upload_url"].(string), "/91?") {
		t.Fatalf("legacy conflicts/reuses ID: %v", c)
	}
}

func TestUnconfiguredScopeFailsClosed(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()
	if c := twirp(t, ts.URL, "CreateCacheEntry", createReq{Key: "k", Version: "v"}); c["ok"] != false {
		t.Fatal(c)
	}
	if err := s.SetScope(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
}

type gatedUpload struct {
	started, release chan struct{}
	sent             bool
}

func (g *gatedUpload) Read(p []byte) (int, error) {
	if !g.sent {
		g.sent = true
		close(g.started)
		<-g.release
		return copy(p, []byte("complete")), nil
	}
	return 0, io.EOF
}
func (*gatedUpload) Close() error { return nil }

func TestFinalizeWaitsForUploadAndThenIsImmutable(t *testing.T) {
	l := newScopeLab(t)
	guest := l.guest(t, "172.16.0.2", mainRef, "job")
	c := twirp(t, guest, "CreateCacheEntry", createReq{Key: "stream", Version: "v"})
	u := c["signed_upload_url"].(string)
	gate := &gatedUpload{started: make(chan struct{}), release: make(chan struct{})}
	upload := httptest.NewRequest("PUT", u, gate)
	upload.RemoteAddr = "172.16.0.2:1234"
	uploadResp := httptest.NewRecorder()
	uploaded := make(chan struct{})
	go func() { l.s.ServeHTTP(uploadResp, upload); close(uploaded) }()
	<-gate.started
	finalized := make(chan struct{})
	finResp := httptest.NewRecorder()
	fin := httptest.NewRequest("POST", guest+twirpBase+"FinalizeCacheEntryUpload", strings.NewReader(`{"key":"stream","version":"v","size_bytes":"8"}`))
	fin.RemoteAddr = upload.RemoteAddr
	go func() { l.s.ServeHTTP(finResp, fin); close(finalized) }()
	select {
	case <-finalized:
		close(gate.release)
		<-uploaded
		t.Fatal("finalize raced past an active upload")
	case <-time.After(30 * time.Millisecond):
	}
	close(gate.release)
	<-uploaded
	<-finalized
	if uploadResp.Code != 201 {
		t.Fatal(uploadResp.Body.String())
	}
	var f finalizeResp
	if err := json.Unmarshal(finResp.Body.Bytes(), &f); err != nil || !f.OK {
		t.Fatalf("finalize %s %v", finResp.Body, err)
	}
	if b, _ := restore(t, guest, "stream"); b != "complete" {
		t.Fatalf("partial archive published: %q", b)
	}
	wantStatus(t, "PUT", u, []byte("overwrite"), 409)
}

func TestGuestHeadersCannotSelectIdentity(t *testing.T) {
	l := newScopeLab(t)
	main := l.guest(t, "172.16.0.2", mainRef, "main")
	l.save(t, main, "secret", "MAIN")
	req := httptest.NewRequest("POST", main+twirpBase+"CreateCacheEntry", strings.NewReader(`{"key":"forged","version":"v","metadata":{"repository_id":"owner/repo","ref":"refs/heads/main"}}`))
	req.RemoteAddr = "172.16.99.2:1234"
	req.Header.Set("X-Forwarded-For", "172.16.0.2")
	req.Header.Set("Authorization", "Bearer forged-claims")
	w := httptest.NewRecorder()
	l.s.ServeHTTP(w, req)
	var c createResp
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || c.OK {
		t.Fatalf("forged identity accepted: %s", w.Body)
	}
	req.RemoteAddr = "not-an-address"
	if i := l.s.identity(req); len(i.ReadRefs) != 0 {
		t.Fatal("invalid address granted fallback")
	}
}

func TestKeepAliveCannotAcquireReusedSlotIdentity(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := testIdentity("owner/repo", prRef, "old")
	if err := s.SetScope(func(string) (jobidentity.Identity, error) { mu.Lock(); defer mu.Unlock(); return current, nil }); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s)
	ts.Config.ConnContext = s.ConnectionContext
	ts.Start()
	defer ts.Close()
	transport := &http.Transport{MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(key string) createResp {
		t.Helper()
		r, err := client.Post(ts.URL+twirpBase+"CreateCacheEntry", "application/json", strings.NewReader(fmt.Sprintf(`{"key":%q,"version":"v"}`, key)))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out createResp
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return out
	}
	if !request("old").OK {
		t.Fatal("initial generation denied")
	}
	mu.Lock()
	current = testIdentity("owner/repo", mainRef, "new")
	mu.Unlock()
	if request("poison").OK {
		t.Fatal("old TCP connection acquired new generation")
	}
	transport.CloseIdleConnections()
	if !request("fresh-connection").OK {
		t.Fatal("new occupant cannot use a new connection")
	}
}

func TestSlowBodyCannotAcquireReusedSlotIdentity(t *testing.T) {
	l := newScopeLab(t)
	guest := l.guest(t, "172.16.0.2", prRef, "old")
	reader, writer := io.Pipe()
	req := httptest.NewRequest("POST", guest+twirpBase+"CreateCacheEntry", reader)
	req.RemoteAddr = "172.16.0.2:1234"
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { l.s.ServeHTTP(w, req); close(done) }()
	// Write returns only once the handler is reading the old request body.
	if _, err := writer.Write([]byte(`{"key":`)); err != nil {
		t.Fatal(err)
	}
	l.set("172.16.0.2", testIdentity("owner/repo", mainRef, "new"))
	_, _ = writer.Write([]byte(`"poison","version":"v"}`))
	_ = writer.Close()
	<-done
	var c createResp
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil || c.OK {
		t.Fatalf("old body acquired new job rights: %s", w.Body)
	}
}

func TestPendingConnectionCanUseItsOwnJobAfterStart(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := jobidentity.Identity{Generation: "same-vm"}
	if err := s.SetScope(func(string) (jobidentity.Identity, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, current.Validate(time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s)
	ts.Config.ConnContext = s.ConnectionContext
	ts.Start()
	defer ts.Close()
	transport := &http.Transport{MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	resp, err := client.Get(ts.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	mu.Lock()
	current = testIdentity("owner/repo", mainRef, "job")
	current.Generation = "same-vm"
	mu.Unlock()
	resp, err = client.Post(ts.URL+twirpBase+"CreateCacheEntry", "application/json", strings.NewReader(`{"key":"started","version":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c createResp
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil || !c.OK {
		t.Fatalf("pending connection denied: %+v %v", c, err)
	}
}

func TestMultiRepositoryAndUnresolvedIsolation(t *testing.T) {
	l := newScopeLab(t)
	a := l.guest(t, "172.16.0.2", mainRef, "a")
	b := l.guest(t, "172.16.1.2", "", "b")
	aBlob := l.save(t, a, "same-key", "REPO A")
	for _, state := range []string{"missing", "pending", "expired", "unresolved"} {
		t.Run(state, func(t *testing.T) {
			i := testIdentity("owner/b", mainRef, "b")
			switch state {
			case "missing":
				i = jobidentity.Identity{}
			case "pending":
				i.ReadRefs = nil
				i.WriteRef = ""
				l.s.SetIdentityWait(50 * time.Millisecond)
				t.Cleanup(func() { l.s.SetIdentityWait(DefaultIdentityWait) })
			case "expired":
				i.ExpiresAt = 1
			case "unresolved":
				i.ReadRefs = nil
			}
			l.set("172.16.1.2", i)
			if got, _ := restore(t, b, "same-key", ""); got != "" {
				t.Fatal("unresolved repo read another repo")
			}
			if c := twirp(t, b, "CreateCacheEntry", createReq{Key: "same-key", Version: "v"}); c["ok"] != false {
				t.Fatal("unresolved identity wrote cache")
			}
		})
	}
	l.set("172.16.1.2", testIdentity("owner/b", mainRef, "b"))
	if got, _ := restore(t, b, "same-key", ""); got != "" {
		t.Fatal("resolved repo B read A")
	}
	l.save(t, b, "same-key", "REPO B")
	for _, tt := range []struct{ url, want string }{{a, "REPO A"}, {b, "REPO B"}} {
		if got, _ := restore(t, tt.url, "same-key"); got != tt.want {
			t.Fatalf("tenant cache: %q", got)
		}
	}
	// A valid blob URL is still scoped to its repository on GET and HEAD.
	u, _ := url.Parse(aBlob)
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, b+strings.Replace(u.RequestURI(), "/upload/", "/download/", 1), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign blob: %d", resp.StatusCode)
		}
	}
	if l.s.snapshot().IdentityDenied == 0 {
		t.Fatal("identity failures not observable")
	}
}

func pendingIdentity(repo, generation string) jobidentity.Identity {
	return jobidentity.Identity{Repository: repo, Runner: "test-runner", Generation: generation, ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestPendingIdentityWaitsForResolution(t *testing.T) {
	l := newScopeLab(t)
	l.save(t, l.guest(t, "172.16.0.2", mainRef, "seed"), "deps", "DEFAULT")
	b := l.guest(t, "172.16.1.2", "", "")
	l.set("172.16.1.2", pendingIdentity("owner/repo", "late"))
	go func() {
		time.Sleep(300 * time.Millisecond)
		i := testIdentity("owner/repo", prRef, "late")
		i.ReadRefs = []string{prRef, mainRef}
		l.set("172.16.1.2", i)
	}()
	start := time.Now()
	if got, _ := restore(t, b, "deps"); got != "DEFAULT" {
		t.Fatalf("restore issued before JobStarted missed: %q", got)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("restore did not wait for the pending identity")
	}
	st := l.s.snapshot()
	if st.IdentityWaits != 1 || st.IdentityDenied != 0 || st.IdentityWaitSeconds < 0.25 {
		t.Fatalf("wait not observable: %+v", st)
	}
}

func TestPendingIdentityWaitEnds(t *testing.T) {
	const ip = "172.16.1.2"
	for _, tt := range []struct {
		name   string
		wait   time.Duration
		bound  string
		after  func(*scopeLab, context.CancelFunc)
		reason string
		bounds [2]time.Duration
	}{
		{name: "deadline", wait: 150 * time.Millisecond, reason: denyPending, bounds: [2]time.Duration{150 * time.Millisecond, 5 * time.Second}},
		{name: "unresolved", wait: 10 * time.Second, reason: denyUnresolved, bounds: [2]time.Duration{100 * time.Millisecond, 5 * time.Second},
			after: func(l *scopeLab, _ context.CancelFunc) {
				i := pendingIdentity("owner/repo", "g")
				i.Unresolved = true
				l.set(ip, i)
			}},
		{name: "slot reused", wait: 10 * time.Second, bound: "g", reason: denyGeneration, bounds: [2]time.Duration{100 * time.Millisecond, 5 * time.Second},
			after: func(l *scopeLab, _ context.CancelFunc) {
				i := testIdentity("owner/repo", mainRef, "next")
				l.set(ip, i)
			}},
		{name: "client gone", wait: 10 * time.Second, reason: denyPending, bounds: [2]time.Duration{100 * time.Millisecond, 5 * time.Second},
			after: func(_ *scopeLab, cancel context.CancelFunc) { cancel() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := newScopeLab(t)
			l.s.SetIdentityWait(tt.wait)
			l.set(ip, pendingIdentity("owner/repo", "g"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.bound != "" {
				ctx = context.WithValue(ctx, connectionGenerationKey{}, tt.bound)
			}
			if tt.after != nil {
				go func() { time.Sleep(100 * time.Millisecond); tt.after(l, cancel) }()
			}
			start := time.Now()
			i, reason, waited := l.s.awaitIdentity(ctx, ip, start.Add(tt.wait))
			elapsed := time.Since(start)
			if reason != tt.reason || i.Generation != "" || !waited {
				t.Fatalf("got reason %q identity %+v waited %v, want %q", reason, i, waited, tt.reason)
			}
			if elapsed < tt.bounds[0] || elapsed > tt.bounds[1] {
				t.Fatalf("waited %v, want within %v", elapsed, tt.bounds)
			}
		})
	}
}

func TestIdentityDenialsDoNotWait(t *testing.T) {
	const ip = "172.16.1.2"
	for _, tt := range []struct {
		name     string
		identity *jobidentity.Identity
		reason   string
	}{
		{name: "no record", reason: denyMissing},
		{name: "unresolved", identity: func() *jobidentity.Identity { i := pendingIdentity("owner/repo", "g"); i.Unresolved = true; return &i }(), reason: denyUnresolved},
		{name: "expired", identity: func() *jobidentity.Identity { i := pendingIdentity("owner/repo", "g"); i.ExpiresAt = 1; return &i }(), reason: denyExpired},
		{name: "invalid", identity: func() *jobidentity.Identity {
			i := testIdentity("owner/repo", mainRef, "g")
			i.ReadRefs = nil
			return &i
		}(), reason: denyInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := newScopeLab(t)
			if tt.identity != nil {
				l.set(ip, *tt.identity)
			}
			start := time.Now()
			_, reason, waited := l.s.awaitIdentity(context.Background(), ip, start.Add(10*time.Second))
			if reason != tt.reason || waited || time.Since(start) > time.Second {
				t.Fatalf("got %q waited=%v after %v, want %q at once", reason, waited, time.Since(start), tt.reason)
			}
		})
	}
}

func TestIdentityDenialMetricsByReason(t *testing.T) {
	l := newScopeLab(t)
	l.s.SetIdentityWait(50 * time.Millisecond)
	b := l.guest(t, "172.16.1.2", "", "")
	l.set("172.16.1.2", pendingIdentity("owner/repo", "g"))
	restore(t, b, "deps")
	unresolved := pendingIdentity("owner/repo", "g")
	unresolved.Unresolved = true
	l.set("172.16.1.2", unresolved)
	restore(t, b, "deps")
	rec := httptest.NewRecorder()
	l.s.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{
		`firerunner_cache_identity_denied_total{reason="pending"} 1`,
		`firerunner_cache_identity_denied_total{reason="unresolved"} 1`,
		`firerunner_cache_identity_denied_total{reason="missing"} 0`,
		"firerunner_cache_identity_waits_total 1",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, rec.Body.String())
		}
	}
	if st := l.s.snapshot(); st.IdentityDenied != 2 || st.IdentityDeniedByReason[denyPending] != 1 {
		t.Fatalf("stats: %+v", st)
	}
}
