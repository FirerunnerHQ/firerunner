package diag

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// status shows the jobs stranded through an outage, in text and JSON.
func TestStatusShowsStrandedJobs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	f := health.File{PID: 1, WrittenAt: now,
		Tiers: []health.Tier{{Name: "small", State: health.Connected, Since: now, Stranded: 2, Rescuing: 1}}}
	if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	want := "small STRANDED since start: 2 job(s) queued through an outage of more than about 4 minutes; a runner each was launched for them (1 still on)"
	if text := statusText(t, cfg); !strings.Contains(text, want) {
		t.Fatalf("status lacks %q\n%s", want, text)
	}
	var js bytes.Buffer
	if err := Status(cfg, "test", &js, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stranded": 2`, `"rescuing": 1`} {
		if !strings.Contains(js.String(), want) {
			t.Errorf("status --json lacks %q", want)
		}
	}
}
