// Package jitserver hands an EC2 runner instance its JIT runner config over the
// VPC, so the config never travels in user-data.
//
// User-data is readable by anything on the instance that reaches IMDS, and it
// outlives the boot. Instead the launcher registers the config under the
// instance's private IP, and the instance fetches it once from GET /jit. The
// VPC delivers packets with their real source address, so a source IP inside
// the runner subnet cannot be spoofed by another tenant; the control VM must
// see guests' addresses unchanged (no proxy or SNAT in between).
//
// The same address reports its runner's progress back on POST /jit/report:
// when the runner is listening, when its job starts, and its exit status. The
// launcher reads them as a Report, so a runner that fails is an error, not
// healthy churn, and boot time can be split into its phases.
package jitserver

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Path is the URL path instances fetch their config from.
const Path = "/jit"

// ReportPath is the URL path instances report their runner's phases to, with
// POST ?phase=<phase>, plus &rc=<status> for PhaseExited.
const ReportPath = Path + "/report"

// Phases an instance reports, in the order they happen.
const (
	// PhaseListening: the runner has connected and waits for its job.
	PhaseListening = "listening"
	// PhaseJob: the runner has started its job.
	PhaseJob = "job"
	// PhaseExited: the runner has exited, or could not be started, with
	// the status in rc (0-255).
	PhaseExited = "exited"
)

// UserDataKey names the only line firerunner writes into an instance's
// user-data: the URL of this server's JIT endpoint.
const UserDataKey = "FIRERUNNER_JIT_URL"

// DefaultTTL bounds how long a registered config waits to be fetched. A cold
// instance fetches within about a minute of launch; a config nobody claimed
// in this long belongs to an instance that is not coming.
const DefaultTTL = 5 * time.Minute

// Errors returned by Register.
var (
	ErrInvalid    = errors.New("jitserver: invalid registration")
	ErrRegistered = errors.New("jitserver: address already has a registration")
)

type entry struct {
	runner  string
	jit     string
	expires time.Time
	// claimed is set once the config has been served or has expired. The
	// entry stays as a tombstone, answering 404 instead of 403, until the
	// launcher revokes it when the instance is gone.
	claimed bool
	// served is set when the instance actually received the config.
	served bool
	report Report
}

// Report is what an instance told the server about its runner. A zero time
// means the phase was not reported (yet). Times are the server's, taken when
// the report arrived, so they compare with the launcher's clock.
type Report struct {
	Served    time.Time // the config was handed out
	Listening time.Time
	Job       time.Time
	Exited    time.Time
	// ExitCode is the runner's exit status; meaningful once Exited is set.
	ExitCode int
}

// Server serves each registered config exactly once, to the address it was
// registered for.
type Server struct {
	mu      sync.Mutex
	entries map[netip.Addr]*entry
	ttl     time.Duration
	now     func() time.Time
	log     *slog.Logger
}

// New returns a Server whose unclaimed configs expire after ttl (DefaultTTL
// when ttl <= 0).
func New(ttl time.Duration, log *slog.Logger) *Server {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{entries: map[netip.Addr]*entry{}, ttl: ttl, now: time.Now, log: log}
}

// Register makes jit available, once, to requests from ip. Call it as soon as
// the instance's private IP is known and before it can boot far enough to ask;
// an early request gets 403 and the guest retries. An address keeps its
// registration, served or not, until Revoke, so a recycled IP never inherits a
// stale config.
func (s *Server) Register(ip netip.Addr, runner, jit string) error {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || runner == "" || jit == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[ip]; ok {
		return errors.Join(ErrRegistered, errors.New("held by runner "+e.runner))
	}
	s.entries[ip] = &entry{runner: runner, jit: jit, expires: s.now().Add(s.ttl)}
	return nil
}

// Revoke drops ip's registration, served or not. The launcher calls it when
// the instance is terminated or its launch is abandoned.
func (s *Server) Revoke(ip netip.Addr) {
	s.mu.Lock()
	delete(s.entries, ip.Unmap())
	s.mu.Unlock()
}

// Pending reports whether ip holds a config that has not been fetched yet and
// has not expired.
func (s *Server) Pending(ip netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ip.Unmap()]
	return ok && !e.claimed && s.now().Before(e.expires)
}

// Served reports whether ip's registered config has been handed out. With
// Pending, it tells a launcher whose instance is still waiting (Pending), has
// its config (Served), or let it expire unclaimed (neither).
func (s *Server) Served(ip netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ip.Unmap()]
	return ok && e.served
}

// Report returns what ip's instance has reported. The zero Report means
// nothing, or no registration.
func (s *Server) Report(ip netip.Addr) Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[ip.Unmap()]; ok {
		return e.report
	}
	return Report{}
}

// ServeHTTP answers GET /jit:
//   - 200 with the config, the first time a registered address asks;
//   - 404 when that address's config was already served or expired;
//   - 403 for an address with no registration.
//
// and POST /jit/report (see reportHTTP).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == ReportPath {
		s.reportHTTP(w, r)
		return
	}
	if r.URL.Path != Path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip, err := remoteIP(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	jit, runner, status := s.claim(ip)
	switch status {
	case http.StatusOK:
		s.log.Info("served jit config", "runner", runner, "ip", ip)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, jit)
	case http.StatusNotFound:
		s.log.Warn("jit config requested again after it was served or expired", "runner", runner, "ip", ip)
		http.NotFound(w, r)
	default:
		s.log.Debug("jit config requested by unregistered address", "ip", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
	}
}

// claim hands out ip's config once and turns the entry into a tombstone.
func (s *Server) claim(ip netip.Addr) (jit, runner string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ip]
	if !ok {
		return "", "", http.StatusForbidden
	}
	if e.claimed || !s.now().Before(e.expires) {
		e.claimed, e.jit = true, ""
		return "", e.runner, http.StatusNotFound
	}
	jit = e.jit
	e.claimed, e.served, e.jit = true, true, ""
	e.report.Served = s.now()
	return jit, e.runner, http.StatusOK
}

// reportHTTP answers POST /jit/report?phase=<phase>[&rc=<status>]:
//   - 204 when recorded; a phase already reported keeps its first report;
//   - 400 for an unknown phase, or an exit without a status in 0-255;
//   - 403 unless the address holds a registration whose config it was
//     served: only the instance running that runner may speak for it.
func (s *Server) reportHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip, err := remoteIP(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	phase, rc := q.Get("phase"), 0
	switch phase {
	case PhaseListening, PhaseJob:
	case PhaseExited:
		if rc, err = strconv.Atoi(q.Get("rc")); err != nil || rc < 0 || rc > 255 {
			http.Error(w, "rc must be 0-255", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unknown phase", http.StatusBadRequest)
		return
	}
	runner, rep, first, ok := s.record(ip, phase, rc)
	if !ok {
		s.log.Debug("runner report from an address without a served config", "ip", ip, "phase", phase)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if first {
		attrs := []any{"runner", runner, "ip", ip, "phase", phase,
			"sinceServed", s.now().Sub(rep.Served).Round(time.Millisecond)}
		if phase == PhaseExited {
			attrs = append(attrs, "rc", rc)
		}
		s.log.Info("runner phase", attrs...)
	}
	w.WriteHeader(http.StatusNoContent)
}

// record notes phase for ip's runner, keeping a phase's first report. ok is
// false when ip has no registration or was never served its config.
func (s *Server) record(ip netip.Addr, phase string, rc int) (runner string, rep Report, first, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, found := s.entries[ip]
	if !found || !e.served {
		return "", Report{}, false, false
	}
	var at *time.Time
	switch phase {
	case PhaseListening:
		at = &e.report.Listening
	case PhaseJob:
		at = &e.report.Job
	case PhaseExited:
		at = &e.report.Exited
	}
	if at.IsZero() {
		*at, first = s.now(), true
		if phase == PhaseExited {
			e.report.ExitCode = rc
		}
	}
	return e.runner, e.report, first, true
}

func remoteIP(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return ip.Unmap(), nil
}

// URL returns the endpoint instances fetch from, given the host:port they
// reach the server at (its VPC IP, or a private DNS name for it).
func URL(addr string) string {
	return (&url.URL{Scheme: "http", Host: addr, Path: Path}).String()
}

// UserData returns the complete user-data for a runner instance: one line that
// names the JIT endpoint, and nothing secret.
func UserData(endpoint string) string {
	return UserDataKey + "=" + strings.TrimSpace(endpoint) + "\n"
}
