package diag

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/cacheserver"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// statsConfig is a config whose cache URL serves st at /stats.
func statsConfig(t *testing.T, st cacheserver.Stats) *config.Config {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	}))
	t.Cleanup(ts.Close)
	return &config.Config{ScaleSetName: "firerunner", Firecracker: provisioner.FirecrackerConfig{
		WorkDir: t.TempDir(), TapPrefix: "fr", NetBase: 16, CacheURL: ts.URL,
	}}
}

func statusText(t *testing.T, cfg *config.Config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Status(cfg, "test", &buf, false); err != nil {
		t.Fatalf("Status: %v", err)
	}
	return buf.String()
}

// Status shows the cache-server's refused connections and cut transfers
// when there are any, and says nothing of them otherwise.
func TestStatusShowsCacheRefusalsAndCuts(t *testing.T) {
	quiet := statusText(t, statsConfig(t, cacheserver.Stats{Entries: 1}))
	if strings.Contains(quiet, "refused") || strings.Contains(quiet, "cut by") {
		t.Errorf("refusal or cut lines with zero counters\n%s", quiet)
	}
	now := time.Now()
	busy := statusText(t, statsConfig(t, cacheserver.Stats{
		ConnRefused: 5, ConnRefusedByCap: map[string]uint64{"per_source": 2, "total": 3}, ConnRefusedLast: now.Add(-2 * time.Minute),
		IdleCuts: 4, IdleCutsByDirection: map[string]uint64{"request": 1, "response": 3}, IdleCutLast: now.Add(-time.Hour),
	}))
	for _, want := range []string{
		"refused 5 connection(s) at its caps (2 per-source, 3 total), the latest 2m",
		"4 transfer(s) cut by --body-idle-timeout (1 request bodies stalled, 3 responses unread), the latest 1h",
	} {
		if !strings.Contains(busy, want) {
			t.Errorf("status missing %q\n%s", want, busy)
		}
	}
}

// Doctor warns while the latest refusal is recent, and passes once it is
// old, or there was none.
func TestRefusalCheck(t *testing.T) {
	now := time.Now()
	byCap := map[string]uint64{"per_source": 1, "total": 2}
	for _, tc := range []struct {
		name  string
		total uint64
		last  time.Time
		want  string
	}{
		{"none", 0, time.Time{}, levelPass},
		{"recent", 3, now.Add(-time.Minute), levelWarn},
		{"just inside", 3, now.Add(-recentRefusal + time.Second), levelWarn},
		{"old", 3, now.Add(-recentRefusal), levelPass},
		{"no time", 3, time.Time{}, levelPass},
	} {
		c := refusalCheck("x", "the server", tc.total, byCap, tc.last, now)
		if c.Level != tc.want {
			t.Errorf("%s: %s %q, want %s", tc.name, c.Level, c.Detail, tc.want)
		}
	}
	if c := refusalCheck("x", "the server", 3, byCap, now.Add(-time.Minute), now); !strings.Contains(c.Detail, "(1 per-source, 2 total)") {
		t.Errorf("warning does not say which caps: %q", c.Detail)
	}
}

// Doctor reads the cache-server's refusals from /stats.
func TestDoctorWarnsOfRecentCacheRefusals(t *testing.T) {
	cfg := statsConfig(t, cacheserver.Stats{ConnRefused: 1, ConnRefusedByCap: map[string]uint64{"total": 1}, ConnRefusedLast: time.Now()})
	for _, c := range runDoctor(cfg, "test").Checks {
		if c.Name == "dep-cache-conns" {
			if c.Level != levelWarn {
				t.Fatalf("dep-cache-conns %s %q, want a warning", c.Level, c.Detail)
			}
			return
		}
	}
	t.Fatal("doctor has no dep-cache-conns check")
}

// The JIT endpoint's refusals come from the daemon's state file: status
// shows them, and doctor judges them while the file is fresh.
func TestJITRefusalsInStatusAndDoctor(t *testing.T) {
	now := time.Now()
	write := func(dir string, at time.Time, jr *health.ConnRefusals) {
		t.Helper()
		if err := health.Write(filepath.Join(dir, health.FileName), health.File{PID: 1, WrittenAt: at, JITRefused: jr}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	write(dir, now, &health.ConnRefusals{ByCap: map[string]uint64{"per_source": 3, "total": 0}, Last: now.Add(-time.Minute)})
	if text := statusText(t, cfg); !strings.Contains(text, "JIT endpoint: refused 3 connection(s) at its caps (3 per-source, 0 total), the latest 1m") {
		t.Errorf("status lacks the JIT refusals\n%s", text)
	}
	if c := jitRefusalCheck(cfg.Firecracker, now); c == nil || c.Level != levelWarn || c.Name != "jit-conns" {
		t.Fatalf("jit-conns = %+v, want a warning", c)
	}

	write(dir, now, &health.ConnRefusals{ByCap: map[string]uint64{"per_source": 0, "total": 0}})
	if text := statusText(t, cfg); strings.Contains(text, "JIT endpoint") {
		t.Errorf("status shows a JIT line with no refusals\n%s", text)
	}
	if c := jitRefusalCheck(cfg.Firecracker, now); c == nil || c.Level != levelPass {
		t.Fatalf("jit-conns = %+v, want a pass", c)
	}

	// Nothing current to judge: a stale file, or no JIT endpoint.
	write(dir, now.Add(-time.Hour), &health.ConnRefusals{ByCap: map[string]uint64{"total": 1}, Last: now})
	if c := jitRefusalCheck(cfg.Firecracker, now); c != nil {
		t.Fatalf("jit-conns from a stale file = %+v, want none", c)
	}
	write(dir, now, nil)
	if c := jitRefusalCheck(cfg.Firecracker, now); c != nil {
		t.Fatalf("jit-conns without a JIT endpoint = %+v, want none", c)
	}
}
