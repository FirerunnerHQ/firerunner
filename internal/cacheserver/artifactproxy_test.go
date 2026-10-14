package cacheserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamRecorder is a fake GitHub Results service. It captures every request
// it receives and replies with whatever the test configured.
type upstreamRecorder struct {
	mu   sync.Mutex
	reqs []recordedReq

	status  int
	body    string
	headers map[string]string
}

type recordedReq struct {
	method string
	path   string
	host   string
	header http.Header
	body   []byte
}

func newUpstream(t *testing.T, status int, body string) (*upstreamRecorder, *httptest.Server) {
	t.Helper()
	u := &upstreamRecorder{status: status, body: body, headers: map[string]string{"Content-Type": "application/json"}}
	ts := httptest.NewServer(u)
	t.Cleanup(ts.Close)
	return u, ts
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, recordedReq{method: r.Method, path: r.URL.Path, host: r.Host, header: r.Header.Clone(), body: body})
	status, resp, hdrs := u.status, u.body, u.headers
	u.mu.Unlock()
	for k, v := range hdrs {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp)
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstreamRecorder) last(t *testing.T) recordedReq {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatalf("upstream received no requests")
	}
	return u.reqs[len(u.reqs)-1]
}

// newArtifactServer returns a cache-server forwarding artifacts to upstream
// (empty = forwarding disabled) plus an httptest front for it.
func newArtifactServer(t *testing.T, upstream string) (*Server, *httptest.Server) {
	t.Helper()
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.SetArtifactUpstream(upstream); err != nil {
		t.Fatalf("SetArtifactUpstream(%q): %v", upstream, err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

// artifactRPC POSTs one ArtifactService RPC the way the @actions/artifact Twirp
// client does (JSON body, bearer ACTIONS_RUNTIME_TOKEN) and returns the raw reply.
func artifactRPC(t *testing.T, base, method string, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+artifactTwirpBase+method, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer runtime-token-123")
	req.Header.Set("User-Agent", "@actions/artifact-2.3.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func decodeTwirpErr(t *testing.T, raw []byte) (code, msg string) {
	t.Helper()
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("not a twirp error envelope: %q (%v)", raw, err)
	}
	return env["code"], env["msg"]
}

// TestArtifactProxyForwardsRPCVerbatim checks the forwarded request keeps the
// Twirp path, the bearer token, content type and body, targets the upstream's
// Host, and that the upstream's status, headers and body come back unchanged —
// for a bare-host upstream and for one mounted under a base path.
func TestArtifactProxyForwardsRPCVerbatim(t *testing.T) {
	const reply = `{"ok":true,"signed_upload_url":"https://productionresultssa1.blob.core.windows.net/x?sig=abc"}`
	for _, prefix := range []string{"", "/ghes/_services/pipelines"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			up, upTS := newUpstream(t, http.StatusOK, reply)
			up.headers["X-Upstream-Marker"] = "results-receiver"
			_, front := newArtifactServer(t, upTS.URL+prefix+"/")

			body := `{"workflow_run_backend_id":"run-1","workflow_job_run_backend_id":"job-1","name":"rspec-results-shard-0","version":4}`
			resp, raw := artifactRPC(t, front.URL, "CreateArtifact", body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, raw)
			}
			if string(raw) != reply {
				t.Fatalf("body not relayed verbatim:\n got %s\nwant %s", raw, reply)
			}
			if got := resp.Header.Get("X-Upstream-Marker"); got != "results-receiver" {
				t.Fatalf("upstream response header dropped: %q", got)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}

			r := up.last(t)
			if r.method != http.MethodPost {
				t.Fatalf("upstream method = %s", r.method)
			}
			if want := prefix + artifactTwirpBase + "CreateArtifact"; r.path != want {
				t.Fatalf("upstream path = %q, want %q", r.path, want)
			}
			if r.host != strings.TrimPrefix(upTS.URL, "http://") {
				t.Fatalf("upstream Host = %q, want %q", r.host, strings.TrimPrefix(upTS.URL, "http://"))
			}
			if got := r.header.Get("Authorization"); got != "Bearer runtime-token-123" {
				t.Fatalf("Authorization not forwarded: %q", got)
			}
			if got := r.header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type not forwarded: %q", got)
			}
			if got := r.header.Get("User-Agent"); got != "@actions/artifact-2.3.0" {
				t.Fatalf("User-Agent not forwarded: %q", got)
			}
			if string(r.body) != body {
				t.Fatalf("body not forwarded verbatim:\n got %s\nwant %s", r.body, body)
			}
		})
	}
}

// TestArtifactProxyForwardsEveryMethod checks the route is method-agnostic
// within the service: each RPC the toolkit uses reaches the upstream under its
// own name.
func TestArtifactProxyForwardsEveryMethod(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{"ok":true}`)
	_, front := newArtifactServer(t, upTS.URL)
	methods := []string{"CreateArtifact", "FinalizeArtifact", "ListArtifacts", "GetSignedArtifactURL", "DeleteArtifact"}
	for _, m := range methods {
		resp, raw := artifactRPC(t, front.URL, m, `{}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s -> %d: %s", m, resp.StatusCode, raw)
		}
		if r := up.last(t); !strings.HasSuffix(r.path, "/"+m) {
			t.Fatalf("%s forwarded to %q", m, r.path)
		}
	}
	if up.count() != len(methods) {
		t.Fatalf("upstream saw %d requests, want %d", up.count(), len(methods))
	}
}

// TestArtifactProxyPassesThroughTwirpError checks a Twirp error from GitHub
// (here the 409 upload-artifact hits when a name already exists on the run) is
// relayed with its status and envelope intact so the toolkit's own error
// handling sees exactly what GitHub said.
func TestArtifactProxyPassesThroughTwirpError(t *testing.T) {
	const conflict = `{"code":"already_exists","msg":"an artifact with this name already exists on the workflow run"}`
	_, upTS := newUpstream(t, http.StatusConflict, conflict)
	s, front := newArtifactServer(t, upTS.URL)

	resp, raw := artifactRPC(t, front.URL, "CreateArtifact", `{"name":"dup"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if string(raw) != conflict {
		t.Fatalf("envelope altered: %s", raw)
	}
	// A 4xx is GitHub's answer, not a proxy failure.
	if got := s.artifactErrors.Load(); got != 0 {
		t.Fatalf("artifact_errors = %d, want 0 for a relayed 4xx", got)
	}
	if got := s.artifactRPCs.Load(); got != 1 {
		t.Fatalf("artifact_rpcs = %d, want 1", got)
	}
}

// TestArtifactProxyUpstream5xxCountsError checks an upstream 5xx is relayed
// as-is (the toolkit retries those itself) and counted as an error.
func TestArtifactProxyUpstream5xxCountsError(t *testing.T) {
	_, upTS := newUpstream(t, http.StatusServiceUnavailable, `{"code":"unavailable","msg":"try again"}`)
	s, front := newArtifactServer(t, upTS.URL)

	resp, _ := artifactRPC(t, front.URL, "FinalizeArtifact", `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := s.artifactErrors.Load(); got != 1 {
		t.Fatalf("artifact_errors = %d, want 1", got)
	}
}

// TestArtifactProxyUpstreamUnreachable checks a dead upstream yields a 502 with
// a Twirp "unavailable" envelope (a readable reason in the job log rather than
// a bare gateway error) and is counted.
func TestArtifactProxyUpstreamUnreachable(t *testing.T) {
	// Grab a port that nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String() + "/"
	l.Close()

	s, front := newArtifactServer(t, dead)
	resp, raw := artifactRPC(t, front.URL, "CreateArtifact", `{}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, raw)
	}
	code, msg := decodeTwirpErr(t, raw)
	if code != "unavailable" || msg != "artifact upstream unavailable" {
		t.Fatalf("envelope = %s", raw)
	}
	// The reply must not tell an unauthenticated guest where the hop goes or
	// why it failed; that detail belongs in the server log only.
	if strings.Contains(string(raw), l.Addr().String()) || strings.Contains(string(raw), "connection refused") {
		t.Fatalf("envelope leaks upstream/transport detail: %s", raw)
	}
	if got := s.artifactErrors.Load(); got != 1 {
		t.Fatalf("artifact_errors = %d, want 1", got)
	}
}

// TestArtifactProxyDisabled checks that with no upstream configured artifact
// RPCs fail fast with a Twirp "unimplemented" error naming the setting, and
// are counted as errors but not as forwarded RPCs.
func TestArtifactProxyDisabled(t *testing.T) {
	s, front := newArtifactServer(t, "")
	resp, raw := artifactRPC(t, front.URL, "CreateArtifact", `{}`)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", resp.StatusCode, raw)
	}
	code, msg := decodeTwirpErr(t, raw)
	if code != "unimplemented" || !strings.Contains(msg, "--artifact-upstream") {
		t.Fatalf("envelope = %s", raw)
	}
	if s.artifactRPCs.Load() != 0 || s.artifactErrors.Load() != 1 {
		t.Fatalf("counters rpcs=%d errors=%d, want 0/1", s.artifactRPCs.Load(), s.artifactErrors.Load())
	}
}

// TestArtifactProxyIsNotAnOpenProxy checks only the ArtifactService route is
// forwarded: other Twirp services 404, non-POST verbs are refused, and the
// CacheService keeps being served locally without touching the upstream.
func TestArtifactProxyIsNotAnOpenProxy(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{"ok":true}`)
	_, front := newArtifactServer(t, upTS.URL)

	// A different service under the same Twirp prefix is not ours to forward.
	resp, err := http.Post(front.URL+"/twirp/github.actions.results.api.v1.SomeOtherService/Do", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other service -> %d, want 404", resp.StatusCode)
	}

	// Twirp is POST-only; a GET must not be forwarded either.
	resp, err = http.Get(front.URL + artifactTwirpBase + "ListArtifacts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", resp.StatusCode)
	}

	// The cache service is still answered from local disk.
	get := twirp(t, front.URL, "GetCacheEntryDownloadURL", getReq{Key: "nothing", Version: "v"})
	if get["ok"] != false {
		t.Fatalf("cache RPC not served locally: %v", get)
	}

	if up.count() != 0 {
		t.Fatalf("upstream received %d requests; none should have been forwarded", up.count())
	}
}

// TestArtifactProxyDoesNotLeakGuestIdentity checks the hop adds no
// X-Forwarded-* headers (the guest's per-slot address is not GitHub's
// business) and strips hop-by-hop headers such as Proxy-Authorization.
func TestArtifactProxyDoesNotLeakGuestIdentity(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{}`)
	_, front := newArtifactServer(t, upTS.URL)

	req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+"ListArtifacts", strings.NewReader(`{}`))
	req.Header.Set("Proxy-Authorization", "Basic leak")
	req.Header.Set("Connection", "keep-alive")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	r := up.last(t)
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Proxy-Authorization"} {
		if v := r.header.Get(h); v != "" {
			t.Fatalf("upstream received %s=%q; must not be forwarded", h, v)
		}
	}
}

// TestArtifactProxyBodyLimit checks an oversized RPC body is refused before it
// reaches the upstream: artifact RPCs are small metadata, so anything large is
// a misuse and must not be relayed.
func TestArtifactProxyBodyLimit(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{}`)
	s, front := newArtifactServer(t, upTS.URL)

	big := bytes.Repeat([]byte("a"), maxArtifactRPCBody+1)
	resp, err := http.Post(front.URL+artifactTwirpBase+"CreateArtifact", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, raw)
	}
	if code, _ := decodeTwirpErr(t, raw); code != "invalid_argument" {
		t.Fatalf("envelope = %s", raw)
	}
	if up.count() != 0 {
		t.Fatalf("oversized body reached the upstream")
	}
	if s.artifactRPCs.Load() != 0 || s.artifactErrors.Load() != 1 {
		t.Fatalf("counters rpcs=%d errors=%d, want 0/1", s.artifactRPCs.Load(), s.artifactErrors.Load())
	}

	// Exactly at the limit is still fine.
	ok := bytes.Repeat([]byte("b"), maxArtifactRPCBody)
	resp, err = http.Post(front.URL+artifactTwirpBase+"CreateArtifact", "application/json", bytes.NewReader(ok))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("at-limit body -> %d, want 200", resp.StatusCode)
	}
	if len(up.last(t).body) != maxArtifactRPCBody {
		t.Fatalf("at-limit body truncated to %d bytes", len(up.last(t).body))
	}
}

// TestArtifactProxyBodyLimitChunked covers the other half of the size guard: a
// request with no Content-Length (chunked) cannot be refused up front, so the
// limit trips mid-stream inside the proxy hop and must still surface as a 413
// Twirp error rather than a generic upstream failure.
func TestArtifactProxyBodyLimitChunked(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{}`)
	s, front := newArtifactServer(t, upTS.URL)

	// An io.Reader that is not a *bytes.Reader/strings.Reader leaves
	// ContentLength unknown, so the client sends chunked.
	big := io.LimitReader(neverEnding('c'), int64(maxArtifactRPCBody)+1)
	req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+"CreateArtifact", big)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", resp.StatusCode, raw)
	}
	if code, _ := decodeTwirpErr(t, raw); code != "invalid_argument" {
		t.Fatalf("envelope = %s", raw)
	}
	if got := s.artifactErrors.Load(); got != 1 {
		t.Fatalf("artifact_errors = %d, want 1", got)
	}
	// The upstream may have seen the truncated stream open, but never a whole
	// oversized body.
	if up.count() > 0 && len(up.last(t).body) > maxArtifactRPCBody {
		t.Fatalf("upstream received %d bytes, over the limit", len(up.last(t).body))
	}
}

// neverEnding is an io.Reader that yields the same byte forever.
type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// TestArtifactProxyRelaysChunkedResponse checks a streamed (no Content-Length)
// upstream reply is flushed through to the client intact; the reverse proxy
// reaches the underlying writer's Flush via statusRecorder.Unwrap.
func TestArtifactProxyRelaysChunkedResponse(t *testing.T) {
	const part1, part2 = `{"artifacts":[{"name":"a"},`, `{"name":"b"}]}`
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush() // commit headers with no Content-Length -> chunked
		_, _ = io.WriteString(w, part1)
		fl.Flush()
		_, _ = io.WriteString(w, part2)
	}))
	t.Cleanup(upTS.Close)
	s, front := newArtifactServer(t, upTS.URL)

	resp, raw := artifactRPC(t, front.URL, "ListArtifacts", `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(raw) != part1+part2 {
		t.Fatalf("streamed body mangled: %q", raw)
	}
	if s.artifactRPCs.Load() != 1 || s.artifactErrors.Load() != 0 {
		t.Fatalf("counters rpcs=%d errors=%d", s.artifactRPCs.Load(), s.artifactErrors.Load())
	}
}

// TestArtifactStatsAndMetrics checks the artifact counters surface in both the
// JSON /stats snapshot `firerunner status` reads and the Prometheus /metrics
// text, alongside the existing cache counters.
func TestArtifactStatsAndMetrics(t *testing.T) {
	_, upTS := newUpstream(t, http.StatusOK, `{"ok":true}`)
	s, front := newArtifactServer(t, upTS.URL)

	artifactRPC(t, front.URL, "CreateArtifact", `{}`)
	artifactRPC(t, front.URL, "FinalizeArtifact", `{}`)
	// One refused (disabled) call to exercise the error counter.
	if err := s.SetArtifactUpstream(""); err != nil {
		t.Fatal(err)
	}
	artifactRPC(t, front.URL, "ListArtifacts", `{}`)

	resp, err := http.Get(front.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st.ArtifactRPCs != 2 || st.ArtifactErrors != 1 {
		t.Fatalf("stats artifact_rpcs=%d artifact_errors=%d, want 2/1", st.ArtifactRPCs, st.ArtifactErrors)
	}

	resp, err = http.Get(front.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		"# TYPE firerunner_artifact_rpcs_total counter\nfirerunner_artifact_rpcs_total 2\n",
		"# TYPE firerunner_artifact_errors_total counter\nfirerunner_artifact_errors_total 1\n",
		"firerunner_cache_entries 0\n", // the cache metrics are still there
	} {
		if !strings.Contains(string(metrics), want) {
			t.Fatalf("metrics missing %q in:\n%s", want, metrics)
		}
	}
}

// TestSetArtifactUpstreamValidation checks the upstream must be an absolute
// http(s) URL without query/fragment, and that empty cleanly disables.
func TestSetArtifactUpstreamValidation(t *testing.T) {
	s, err := newAuthorizedStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every rejected form smuggles a "secret" so the test can prove no
	// validation error ever echoes the input: cacheServe logs these errors.
	for _, bad := range []string{
		"results-receiver.actions.githubusercontent.com/hunter2", // no scheme
		"ftp://hunter2@example.com/",                             // wrong scheme
		"https://?x=hunter2",                                     // no host
		"http://:8080/hunter2",                                   // port-only authority, no host to dial
		"https://example.com/?token=hunter2",                     // query
		"https://example.com/#hunter2",                           // fragment
		"https://user:hunter2@example.com/",                      // userinfo
		"https://hunter2@example.com/",
		"://hunter2", // unparseable
	} {
		err := s.SetArtifactUpstream(bad)
		if err == nil {
			t.Errorf("SetArtifactUpstream(%q) accepted, want error", bad)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("rejection error for %q echoes the input: %v", bad, err)
		}
	}
	for _, good := range []string{DefaultArtifactUpstream, "http://10.0.0.1:8080", "https://ghes.example.com/_services/results/"} {
		if err := s.SetArtifactUpstream(good); err != nil {
			t.Errorf("SetArtifactUpstream(%q): %v", good, err)
		}
	}
	if err := s.SetArtifactUpstream(""); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if s.artifactProxy.Load() != nil {
		t.Fatalf("empty upstream did not disable forwarding")
	}
}

// TestArtifactProxyConcurrent hammers the route from many goroutines (run with
// -race) and checks every call is relayed and counted exactly once.
func TestArtifactProxyConcurrent(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{"ok":true}`)
	s, front := newArtifactServer(t, upTS.URL)

	const n = 64
	var wg sync.WaitGroup
	var okCount atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"artifact-%d"}`, i)
			req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+"CreateArtifact", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				okCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if okCount.Load() != n {
		t.Fatalf("%d/%d calls succeeded", okCount.Load(), n)
	}
	if up.count() != n {
		t.Fatalf("upstream saw %d requests, want %d", up.count(), n)
	}
	if s.artifactRPCs.Load() != n || s.artifactErrors.Load() != 0 {
		t.Fatalf("counters rpcs=%d errors=%d, want %d/0", s.artifactRPCs.Load(), s.artifactErrors.Load(), n)
	}
}

// TestArtifactProxyRejectsUnknownMethod checks the method whitelist: names the
// toolkit never calls — including an encoded dot-segment that the route
// wildcard alone would accept and an upstream might normalize into a different
// service — are refused before anything is forwarded.
func TestArtifactProxyRejectsUnknownMethod(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{}`)
	s, front := newArtifactServer(t, upTS.URL)

	for _, m := range []string{
		"MigrateArtifact",
		"createartifact", // case matters in Twirp routes
		"%2e%2e%2fCacheService%2fCreateCacheEntry",
		"CreateArtifact%2f..%2fFinalizeArtifact",
		"..%2FCacheService%2FCreateCacheEntry",
	} {
		req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+m, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q -> %d, want 404: %s", m, resp.StatusCode, raw)
		}
		if code, _ := decodeTwirpErr(t, raw); code != "bad_route" {
			t.Fatalf("%q envelope = %s", m, raw)
		}
	}
	if up.count() != 0 {
		t.Fatalf("upstream received %d requests; unknown methods must never be forwarded", up.count())
	}
	if s.artifactRPCs.Load() != 0 || s.artifactErrors.Load() != 5 {
		t.Fatalf("counters rpcs=%d errors=%d, want 0/5", s.artifactRPCs.Load(), s.artifactErrors.Load())
	}
}

// TestArtifactProxyTimeout checks the end-to-end RPC deadline: an upstream that
// accepts the request but never answers is cut off with a 504 Twirp
// deadline_exceeded, counted as an error, and does not hold the handler open.
func TestArtifactProxyTimeout(t *testing.T) {
	release := make(chan struct{})
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); upTS.Close() })
	s, front := newArtifactServer(t, upTS.URL)
	s.artifactTimeout = 150 * time.Millisecond

	start := time.Now()
	resp, raw := artifactRPC(t, front.URL, "ListArtifacts", `{}`)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handler held open %v despite deadline", elapsed)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", resp.StatusCode, raw)
	}
	if code, msg := decodeTwirpErr(t, raw); code != "deadline_exceeded" || msg != "artifact rpc timed out" {
		t.Fatalf("envelope = %s", raw)
	}
	if s.artifactErrors.Load() != 1 {
		t.Fatalf("artifact_errors = %d, want 1", s.artifactErrors.Load())
	}
}

// TestArtifactProxyTimeoutCoversStalledBody checks the deadline also applies
// after headers arrive, where the transport's ResponseHeaderTimeout no longer
// helps: an upstream that sends headers and then stalls is still cut off, and
// the truncated relay is counted as a failed RPC.
func TestArtifactProxyTimeoutCoversStalledBody(t *testing.T) {
	release := make(chan struct{})
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `{"artifacts":[`)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); upTS.Close() })
	s, front := newArtifactServer(t, upTS.URL)
	s.artifactTimeout = 150 * time.Millisecond

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+"ListArtifacts", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled body held the handler open %v", elapsed)
	}
	// Headers were already relayed, so the client sees a 200 whose body is cut
	// short; what matters is that it ends, and that the server counted it.
	if readErr == nil {
		t.Fatalf("expected the truncated body to surface a read error")
	}
	if s.artifactErrors.Load() != 1 {
		t.Fatalf("artifact_errors = %d, want 1 for a truncated relay", s.artifactErrors.Load())
	}
}

// TestArtifactProxyCountsTruncatedUpstreamReply checks a reply whose body is
// cut off by the upstream (connection reset after headers, no deadline
// involved) is recorded in artifact_errors even though ReverseProxy aborts the
// handler before the normal post-call accounting runs.
func TestArtifactProxyCountsTruncatedUpstreamReply(t *testing.T) {
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"partial":`)
		// Returning with fewer bytes than Content-Length makes net/http close
		// the connection mid-body: the proxy's copy fails after headers went out.
	}))
	t.Cleanup(upTS.Close)
	s, front := newArtifactServer(t, upTS.URL)

	req, _ := http.NewRequest(http.MethodPost, front.URL+artifactTwirpBase+"ListArtifacts", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("expected the client to observe a truncated reply")
	}
	if s.artifactRPCs.Load() != 1 || s.artifactErrors.Load() != 1 {
		t.Fatalf("counters rpcs=%d errors=%d, want 1/1", s.artifactRPCs.Load(), s.artifactErrors.Load())
	}
}

// TestArtifactProxyTimeoutCoversStalledClientBody checks the bound on the
// accepted client socket: a request that declares a Content-Length, sends one
// byte and then stalls must not hold the handler open past the deadline. The
// RPC context alone cannot do this (it bounds the upstream hop, not the client
// read), which is why the handler also sets a route-local read deadline.
func TestArtifactProxyTimeoutCoversStalledClientBody(t *testing.T) {
	up, upTS := newUpstream(t, http.StatusOK, `{}`)
	s, front := newArtifactServer(t, upTS.URL)
	s.artifactTimeout = 200 * time.Millisecond

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	fmt.Fprintf(conn, "POST %sCreateArtifact HTTP/1.1\r\nHost: cache\r\nContent-Type: application/json\r\nContent-Length: 40\r\n\r\n{",
		artifactTwirpBase)
	// ...and never send the remaining 39 bytes.

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body within 5s (handler held open?): %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled client body held the handler open %v", elapsed)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", resp.StatusCode, raw)
	}
	if code, msg := decodeTwirpErr(t, raw); code != "deadline_exceeded" || msg != "artifact rpc timed out" {
		t.Fatalf("envelope = %s", raw)
	}
	if s.artifactErrors.Load() != 1 {
		t.Fatalf("artifact_errors = %d, want 1", s.artifactErrors.Load())
	}
	// Whatever reached the upstream, it was never a complete 40-byte body.
	if up.count() > 0 && len(up.last(t).body) >= 40 {
		t.Fatalf("upstream received a full body from a stalled client")
	}
}

// TestArtifactProxyDeadlineIsRouteLocal checks the socket deadlines set for an
// artifact RPC do not bleed into the cache paths, which legitimately stream
// large blobs with no overall deadline: a cache upload on a fresh connection
// after an artifact timeout is unaffected.
func TestArtifactProxyDeadlineIsRouteLocal(t *testing.T) {
	_, upTS := newUpstream(t, http.StatusOK, `{}`)
	s, front := newArtifactServer(t, upTS.URL)
	s.artifactTimeout = 100 * time.Millisecond

	// Trip an artifact deadline first.
	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST %sCreateArtifact HTTP/1.1\r\nHost: cache\r\nContent-Length: 10\r\n\r\n{", artifactTwirpBase)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatalf("artifact timeout reply: %v", err)
	}
	conn.Close()

	// A slow cache upload that takes longer than the artifact deadline still
	// succeeds end to end.
	create := twirp(t, front.URL, "CreateCacheEntry", createReq{Key: "slow", Version: "v1"})
	if create["ok"] != true {
		t.Fatalf("create: %v", create)
	}
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("half-"))
		time.Sleep(300 * time.Millisecond) // > artifactTimeout
		pw.Write([]byte("blob"))
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPut, create["signed_upload_url"].(string), pr)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow cache upload: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("slow cache upload -> %d, want 201 (artifact deadline leaked into cache path?)", resp.StatusCode)
	}
}

// TestArtifactProxyTransportTimeoutIs502 checks that a timeout inside the
// upstream hop while the RPC's own deadline is still live — here the
// transport's response-header timeout — is reported as an upstream fault
// (502 unavailable), not as the caller's deadline (504).
func TestArtifactProxyTransportTimeoutIs502(t *testing.T) {
	release := make(chan struct{})
	upTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); upTS.Close() })
	s, front := newArtifactServer(t, upTS.URL)
	// Make the hop's header timeout fire well before the 2-minute RPC deadline.
	s.artifactProxy.Load().Transport = &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}

	resp, raw := artifactRPC(t, front.URL, "ListArtifacts", `{}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an upstream-hop timeout: %s", resp.StatusCode, raw)
	}
	if code, msg := decodeTwirpErr(t, raw); code != "unavailable" || msg != "artifact upstream unavailable" {
		t.Fatalf("envelope = %s", raw)
	}
	if s.artifactErrors.Load() != 1 {
		t.Fatalf("artifact_errors = %d, want 1", s.artifactErrors.Load())
	}
}
