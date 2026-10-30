package jitserver

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	guest   = netip.MustParseAddr("10.0.1.17")
	foreign = netip.MustParseAddr("10.0.1.99")
)

// fetch sends method path to s as if from the remote address.
func fetch(t *testing.T, s *Server, method, path, remote string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func from(ip netip.Addr) string { return netip.AddrPortFrom(ip, 40000).String() }

func TestServesOnceThen404(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "eyJqaXQiOiJ4In0="); err != nil {
		t.Fatal(err)
	}
	if !s.Pending(guest) {
		t.Fatal("Pending = false before the fetch")
	}
	code, body := fetch(t, s, http.MethodGet, Path, from(guest))
	if code != http.StatusOK || body != "eyJqaXQiOiJ4In0=" {
		t.Fatalf("first fetch = %d %q, want 200 with the config", code, body)
	}
	if s.Pending(guest) || !s.Served(guest) {
		t.Fatalf("after the fetch Pending=%v Served=%v, want false/true", s.Pending(guest), s.Served(guest))
	}
	// The config is revoked once served: a job on the instance cannot read it
	// back, even from the same address.
	code, body = fetch(t, s, http.MethodGet, Path, from(guest))
	if code != http.StatusNotFound || strings.Contains(body, "eyJ") {
		t.Fatalf("second fetch = %d %q, want 404 without the config", code, body)
	}
}

func TestForeignAddressForbidden(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	code, body := fetch(t, s, http.MethodGet, Path, from(foreign))
	if code != http.StatusForbidden || strings.Contains(body, "secret") {
		t.Fatalf("foreign fetch = %d %q, want 403", code, body)
	}
	// The foreign attempt must not consume the real instance's config.
	if code, body := fetch(t, s, http.MethodGet, Path, from(guest)); code != http.StatusOK || body != "secret" {
		t.Fatalf("owner fetch after a foreign attempt = %d %q, want 200", code, body)
	}
}

func TestExpiresWhenNeverFetched(t *testing.T) {
	s := New(time.Minute, nil)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute) // exactly at expiry counts as expired
	if s.Pending(guest) || s.Served(guest) {
		t.Fatalf("after the TTL Pending=%v Served=%v, want both false", s.Pending(guest), s.Served(guest))
	}
	code, body := fetch(t, s, http.MethodGet, Path, from(guest))
	if code != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("fetch after expiry = %d %q, want 404", code, body)
	}
	if s.Served(guest) {
		t.Fatal("a fetch after expiry counted as served")
	}
}

func TestRevokeAndReuseAddress(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "one"); err != nil {
		t.Fatal(err)
	}
	// While the address holds a registration, served or not, a second launch
	// on it is refused: the old instance may still be the one asking.
	if err := s.Register(guest, "runner-2", "two"); !errors.Is(err, ErrRegistered) {
		t.Fatalf("double register err = %v, want ErrRegistered", err)
	}
	fetch(t, s, http.MethodGet, Path, from(guest))
	if err := s.Register(guest, "runner-2", "two"); !errors.Is(err, ErrRegistered) {
		t.Fatalf("register over a served tombstone err = %v, want ErrRegistered", err)
	}
	s.Revoke(guest)
	if code, _ := fetch(t, s, http.MethodGet, Path, from(guest)); code != http.StatusForbidden {
		t.Fatalf("fetch after revoke = %d, want 403", code)
	}
	// A recycled IP gets only its new instance's config.
	if err := s.Register(guest, "runner-2", "two"); err != nil {
		t.Fatalf("register after revoke: %v", err)
	}
	if code, body := fetch(t, s, http.MethodGet, Path, from(guest)); code != http.StatusOK || body != "two" {
		t.Fatalf("fetch for recycled address = %d %q, want the new config", code, body)
	}
}

func TestRevokeBeforeFetch(t *testing.T) {
	// A launch abandoned before the instance asked leaves nothing to fetch.
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	s.Revoke(guest)
	if code, _ := fetch(t, s, http.MethodGet, Path, from(guest)); code != http.StatusForbidden {
		t.Fatalf("fetch after revoke = %d, want 403", code)
	}
}

func TestIPv4MappedAddresses(t *testing.T) {
	// A dual-stack listener reports IPv4 peers as ::ffff:a.b.c.d; they must
	// match a registration made with the plain IPv4 address, and vice versa.
	s := New(time.Minute, nil)
	if err := s.Register(netip.MustParseAddr("::ffff:10.0.1.17"), "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	if !s.Pending(guest) {
		t.Fatal("mapped registration not found by its IPv4 form")
	}
	if code, body := fetch(t, s, http.MethodGet, Path, "[::ffff:10.0.1.17]:40000"); code != http.StatusOK || body != "secret" {
		t.Fatalf("mapped fetch = %d %q, want 200", code, body)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path, remote string
		want                       int
	}{
		{"post", http.MethodPost, Path, from(guest), http.StatusMethodNotAllowed},
		{"other path", http.MethodGet, "/jit/x", from(guest), http.StatusNotFound},
		{"root", http.MethodGet, "/", from(guest), http.StatusNotFound},
		{"unparseable peer", http.MethodGet, Path, "not-an-addr", http.StatusForbidden},
	}
	for _, c := range cases {
		if code, body := fetch(t, s, c.method, c.path, c.remote); code != c.want || strings.Contains(body, "secret") {
			t.Errorf("%s: %d %q, want %d", c.name, code, body, c.want)
		}
	}
	// None of those consumed the config.
	if code, _ := fetch(t, s, http.MethodGet, Path, from(guest)); code != http.StatusOK {
		t.Fatalf("real fetch after bad requests = %d, want 200", code)
	}
}

func TestRegisterValidation(t *testing.T) {
	s := New(time.Minute, nil)
	for name, c := range map[string]struct {
		ip          netip.Addr
		runner, jit string
	}{
		"zero addr":   {netip.Addr{}, "r", "j"},
		"unspecified": {netip.IPv4Unspecified(), "r", "j"},
		"no runner":   {guest, "", "j"},
		"no config":   {guest, "r", ""},
	} {
		if err := s.Register(c.ip, c.runner, c.jit); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestConcurrentFetchesServeOnce(t *testing.T) {
	// A retrying guest may race itself; exactly one request gets the config.
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := fetch(t, s, http.MethodGet, Path, from(guest)); code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := ok.Load(); n != 1 {
		t.Fatalf("%d requests got the config, want exactly 1", n)
	}
}

func TestOverRealListener(t *testing.T) {
	// End to end through net/http: the server sees the loopback peer address.
	s := New(time.Minute, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()
	if err := s.Register(netip.MustParseAddr("127.0.0.1"), "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	get := func() (int, string) {
		resp, err := http.Get(ts.URL + Path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK && resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
		}
		return resp.StatusCode, string(b)
	}
	if code, body := get(); code != http.StatusOK || body != "secret" {
		t.Fatalf("first = %d %q", code, body)
	}
	if code, _ := get(); code != http.StatusNotFound {
		t.Fatalf("second = %d, want 404", code)
	}
}

func TestUserDataCarriesOnlyTheEndpoint(t *testing.T) {
	u := URL("10.0.0.5:8098")
	if u != "http://10.0.0.5:8098/jit" {
		t.Fatalf("URL = %q", u)
	}
	ud := UserData(u)
	if ud != "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit\n" {
		t.Fatalf("UserData = %q, want exactly one key=url line", ud)
	}
}

func report(t *testing.T, s *Server, remote, query string) int {
	t.Helper()
	code, _ := fetch(t, s, http.MethodPost, ReportPath+"?"+query, remote)
	return code
}

func TestReportFromTheServedAddress(t *testing.T) {
	s := New(time.Minute, nil)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	// Before its config is served, the address cannot report: the runner
	// cannot have started.
	if code := report(t, s, from(guest), "phase=listening"); code != http.StatusForbidden {
		t.Fatalf("report before the fetch = %d, want 403", code)
	}
	fetch(t, s, http.MethodGet, Path, from(guest))
	served := now
	steps := []struct {
		query string
		after time.Duration
	}{
		{"phase=listening", 2 * time.Second},
		{"phase=job", 3 * time.Second},
		{"phase=exited&rc=3", 4 * time.Second},
	}
	for _, st := range steps {
		now = served.Add(st.after)
		if code := report(t, s, from(guest), st.query); code != http.StatusNoContent {
			t.Fatalf("report %s = %d, want 204", st.query, code)
		}
	}
	want := Report{
		Served: served, Listening: served.Add(2 * time.Second), Job: served.Add(3 * time.Second),
		Exited: served.Add(4 * time.Second), ExitCode: 3,
	}
	if got := s.Report(guest); got != want {
		t.Fatalf("Report = %+v, want %+v", got, want)
	}
	// A repeated report keeps the first one.
	now = served.Add(time.Minute)
	if code := report(t, s, from(guest), "phase=exited&rc=0"); code != http.StatusNoContent {
		t.Fatalf("repeated exit report = %d, want 204", code)
	}
	if got := s.Report(guest); got != want {
		t.Fatalf("Report after a repeat = %+v, want the first %+v", got, want)
	}
}

func TestReportFromAForeignAddressForbidden(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	fetch(t, s, http.MethodGet, Path, from(guest))
	if code := report(t, s, from(foreign), "phase=exited&rc=1"); code != http.StatusForbidden {
		t.Fatalf("foreign report = %d, want 403", code)
	}
	if got := s.Report(guest); !got.Exited.IsZero() {
		t.Fatalf("a foreign report was recorded: %+v", got)
	}
}

func TestReportAfterRevokeIgnored(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	fetch(t, s, http.MethodGet, Path, from(guest))
	s.Revoke(guest)
	if code := report(t, s, from(guest), "phase=exited&rc=1"); code != http.StatusForbidden {
		t.Fatalf("report after revoke = %d, want 403", code)
	}
	// The address's next registration starts with no report.
	if err := s.Register(guest, "runner-2", "secret2"); err != nil {
		t.Fatal(err)
	}
	if got := s.Report(guest); got != (Report{}) {
		t.Fatalf("Report of a new registration = %+v, want zero", got)
	}
}

func TestReportRejectsBadRequests(t *testing.T) {
	s := New(time.Minute, nil)
	if err := s.Register(guest, "runner-1", "secret"); err != nil {
		t.Fatal(err)
	}
	fetch(t, s, http.MethodGet, Path, from(guest))
	for _, c := range []struct {
		method, query string
		want          int
	}{
		{http.MethodGet, "phase=listening", http.StatusMethodNotAllowed},
		{http.MethodPost, "phase=booted", http.StatusBadRequest},
		{http.MethodPost, "", http.StatusBadRequest},
		{http.MethodPost, "phase=exited", http.StatusBadRequest},
		{http.MethodPost, "phase=exited&rc=x", http.StatusBadRequest},
		{http.MethodPost, "phase=exited&rc=-1", http.StatusBadRequest},
		{http.MethodPost, "phase=exited&rc=256", http.StatusBadRequest},
	} {
		if code, _ := fetch(t, s, c.method, ReportPath+"?"+c.query, from(guest)); code != c.want {
			t.Errorf("%s ?%s = %d, want %d", c.method, c.query, code, c.want)
		}
	}
	if got := s.Report(guest); !got.Listening.IsZero() || !got.Exited.IsZero() {
		t.Fatalf("a bad request was recorded: %+v", got)
	}
}
