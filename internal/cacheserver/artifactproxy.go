package cacheserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

const (
	// artifactTwirpBase is the Twirp route prefix of the GitHub Actions
	// artifact service (actions/upload-artifact and download-artifact v4+).
	artifactTwirpBase = "/twirp/github.actions.results.api.v1.ArtifactService/"

	// DefaultArtifactUpstream is the github.com Results service that owns
	// artifacts. The runner receives it per job as ResultsServiceUrl and would
	// have exported it as ACTIONS_RESULTS_URL; a cache-redirect golden diverts
	// that variable here, so the server has to know where to send artifact
	// traffic back to. SetArtifactUpstream substitutes any protocol-compatible
	// endpoint (upload-artifact v4+ has no GHES service to point at today).
	DefaultArtifactUpstream = "https://results-receiver.actions.githubusercontent.com/"

	// maxArtifactRPCBody bounds a forwarded Twirp request body. Artifact RPCs
	// carry only small JSON metadata (names, ids, sizes); the archive itself is
	// streamed by the client straight to the signed blob URL the upstream
	// returns and never passes through this server.
	maxArtifactRPCBody = 1 << 20

	// defaultArtifactRPCTimeout bounds one forwarded RPC end to end — inbound
	// body, upstream round trip and response relay — so a trickling client or
	// a stalled upstream cannot pin a goroutine and two sockets indefinitely.
	// The per-phase transport timeouts stop once headers arrive; this is the
	// deadline that covers everything after that as well.
	defaultArtifactRPCTimeout = 2 * time.Minute

	// artifactWriteGrace is how much longer than the RPC deadline the client
	// socket stays writable, so the deadline's own Twirp error envelope can
	// still reach a client whose read side has just been cut off.
	artifactWriteGrace = 5 * time.Second
)

// artifactMethods is the complete ArtifactService surface the @actions/artifact
// toolkit calls. Whitelisting the decoded method name is what makes the
// single-service boundary hold: the route wildcard alone would also accept an
// encoded dot segment such as %2e%2e%2fCacheService%2fCreateCacheEntry, which
// an upstream that normalizes paths could route outside ArtifactService.
var artifactMethods = map[string]bool{
	"CreateArtifact":       true,
	"FinalizeArtifact":     true,
	"ListArtifacts":        true,
	"GetSignedArtifactURL": true,
	"DeleteArtifact":       true,
}

// SetArtifactUpstream configures where ArtifactService RPCs are forwarded.
// An empty value disables forwarding: artifact RPCs then fail fast with a
// Twirp "unimplemented" error that names this setting, instead of the opaque
// 404 the toolkit would otherwise surface. Call before serving.
func (s *Server) SetArtifactUpstream(raw string) error {
	if raw == "" {
		s.artifactProxy.Store(nil)
		return nil
	}
	// None of these errors echo the value: cacheServe logs the returned error,
	// and a rejected URL may be the one carrying a credential.
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("artifact upstream: not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		// Hostname(), not Host: a port-only authority like http://:8080 has a
		// non-empty Host but no host to dial, and should fail here rather than
		// on the first artifact RPC.
		return errors.New("artifact upstream: must be an absolute http(s) URL with a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("artifact upstream: must not carry a query or fragment")
	}
	if u.User != nil {
		// The upstream authenticates the guest's bearer token, not the operator,
		// so userinfo has no legitimate use and would only end up in a log.
		return errors.New("artifact upstream: must not carry userinfo (user:password@)")
	}
	s.artifactProxy.Store(&httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL joins the upstream base path with the inbound Twirp path and
			// rewrites Host to the upstream. It deliberately does not add
			// X-Forwarded-* headers: the upstream is GitHub, and the guest's
			// per-slot address is meaningless (and none of its business) there.
			pr.SetURL(u)
		},
		Transport:    artifactTransport(),
		ErrorHandler: s.artifactProxyError,
	})
	return nil
}

// artifactTransport is a bounded HTTP transport for the upstream hop. The
// server sits between a guest and the internet, so the connection phases get
// deadlines rather than inheriting the client's patience; the request-level
// deadline in handleArtifactProxy covers the body phases these cannot.
func artifactTransport() http.RoundTripper {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		ForceAttemptHTTP2:     true,
	}
}

// handleArtifactProxy forwards one ArtifactService RPC to the configured
// upstream and relays the response verbatim.
//
// Only the five known ArtifactService methods are forwarded, and only to the
// operator-configured upstream, so the server never acts as a general proxy
// for guests. The client's Authorization header (its ACTIONS_RUNTIME_TOKEN) is
// passed through untouched: it is GitHub's credential for GitHub's service,
// and this server neither needs nor is able to validate it. The Twirp reply —
// including any Twirp error envelope such as 409 already_exists — is returned
// as-is so the toolkit sees exactly what GitHub said.
func (s *Server) handleArtifactProxy(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	if !artifactMethods[method] {
		s.artifactErrors.Add(1)
		s.log.Warn("artifact rpc refused: unknown method", "method", method)
		twirpError(w, "bad_route", "unknown ArtifactService method")
		return
	}
	p := s.artifactProxy.Load()
	if p == nil {
		s.artifactErrors.Add(1)
		s.log.Warn("artifact rpc refused: no upstream configured", "method", method)
		twirpError(w, "unimplemented",
			"artifact service forwarding is disabled on this cache-server; configure --artifact-upstream")
		return
	}
	if r.ContentLength > maxArtifactRPCBody {
		s.artifactErrors.Add(1)
		twirpError(w, "invalid_argument", "artifact rpc body too large")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.artifactTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	// The context bounds the upstream hop, but not the accepted client socket:
	// the enclosing http.Server sets no ReadTimeout, so a client that declares
	// a Content-Length, sends one byte and stalls would keep the body read (and
	// this handler) open past the deadline. Put the same bound on the socket
	// itself, route-locally, so the unlimited cache upload paths are unaffected.
	// The write deadline gets a little slack so the timeout's own error
	// envelope can still be delivered after the read side has expired.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.artifactTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(s.artifactTimeout + artifactWriteGrace))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	// Watch the client body read ourselves: when the socket deadline trips the
	// HTTP/1 transport reports it as a flattened "connection broken" string
	// that carries neither the net.Error nor the context error, so the error
	// handler could not otherwise tell a stalled client from a dead upstream.
	r.Body = http.MaxBytesReader(w, &timeoutObservingBody{ReadCloser: r.Body, timedOut: &rec.bodyTimedOut}, maxArtifactRPCBody)
	s.artifactRPCs.Add(1)
	// ReverseProxy aborts the handler with http.ErrAbortHandler when relaying
	// the upstream body fails after headers went out (truncated or reset
	// reply). That is a failed RPC too, so count it on the way past and let
	// net/http finish handling the abort as usual.
	defer func() {
		if v := recover(); v != nil {
			s.artifactErrors.Add(1)
			s.log.Warn("artifact rpc aborted mid-response", "method", method, "status", rec.status)
			panic(v)
		}
	}()
	p.ServeHTTP(rec, r)
	if rec.proxyErr || rec.status >= 500 {
		s.artifactErrors.Add(1)
	}
	s.log.Info("artifact rpc forwarded", "method", method, "status", rec.status)
}

// artifactProxyError turns a failure on the upstream hop into a Twirp error
// envelope so the toolkit reports a readable reason rather than a bare 502.
// The message is deliberately generic: the underlying error names the
// upstream host and transport details, which belong in the server log, not
// in a reply to an unauthenticated guest.
func (s *Server) artifactProxyError(w http.ResponseWriter, r *http.Request, err error) {
	rec, _ := w.(*statusRecorder)
	if rec != nil {
		rec.proxyErr = true
	}
	code, twirpCode, msg := http.StatusBadGateway, "unavailable", "artifact upstream unavailable"
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		code, twirpCode, msg = http.StatusBadRequest, "invalid_argument", "artifact rpc body too large"
	case errors.Is(r.Context().Err(), context.DeadlineExceeded), rec != nil && rec.bodyTimedOut.Load():
		// Only the RPC's own deadlines count as a timeout: the request context
		// expiring, or the client-socket read deadline tripping on a stalled
		// request body (observed via the body wrapper, since the transport
		// flattens that error). A transport dial/TLS/response-header timeout
		// while the context is still live is an upstream fault and stays 502.
		code, twirpCode, msg = http.StatusGatewayTimeout, "deadline_exceeded", "artifact rpc timed out"
	}
	s.log.Warn("artifact rpc failed", "method", r.PathValue("method"), "err", err)
	twirpErrorAt(w, code, twirpCode, msg)
}

// statusRecorder captures the status the proxy wrote, and whether the reply
// came from the proxy's own error path rather than the upstream, so the handler
// can count failures without buffering the body.
type statusRecorder struct {
	http.ResponseWriter
	status       int
	proxyErr     bool
	bodyTimedOut atomic.Bool // set from the transport's body-write goroutine
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (the reverse
// proxy uses it to flush streamed responses).
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// timeoutObservingBody records whether reading the client body failed on the
// socket deadline, which is the only reliable way to attribute that failure
// once the transport has flattened the error.
type timeoutObservingBody struct {
	io.ReadCloser
	timedOut *atomic.Bool
}

func (b *timeoutObservingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var ne net.Error
	if err != nil && (errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())) {
		b.timedOut.Store(true)
	}
	return n, err
}
