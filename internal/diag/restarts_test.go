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

// status shows how often a tenant was stopped and started again, and why
// last; doctor warns while one has panicked since the daemon started, and
// says nothing while none has.
func TestStatusAndDoctorShowTenantRestarts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	last := now.Add(-time.Minute).UTC().Truncate(time.Second)
	f := health.File{PID: 1, WrittenAt: now,
		Tiers: []health.Tier{{Name: "acme/small", State: health.Connected, Since: now}},
		Tenants: []health.Tenant{
			{ID: "", Limits: health.Limits{JobDuration: "0s"}},
			{ID: "acme", Limits: health.Limits{JobDuration: "0s"},
				Restarts: &health.Restarts{Count: 3, LastError: "start: 502", LastAt: last}},
		}}
	write := func() {
		if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
			t.Fatal(err)
		}
	}
	write()
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	var text bytes.Buffer
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	if want := "acme stopped and started again 3 time(s) since the daemon started; last at " + last.Format(time.RFC3339) + ": start: 502"; !strings.Contains(text.String(), want) {
		t.Fatalf("status missing %q\n%s", want, text.String())
	}
	check := func() *Check {
		for _, c := range listenerChecks(cfg.Firecracker, now) {
			if c.Name == "tenant-panics" {
				return &c
			}
		}
		return nil
	}
	if c := check(); c != nil {
		t.Fatalf("doctor with no panic = %+v", c)
	}
	f.Tenants[1].Restarts = &health.Restarts{Count: 4, Panics: 1, LastError: `tenant "acme" panicked: nil map`, LastAt: last}
	write()
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "acme (1, last: tenant \"acme\" panicked: nil map)") {
		t.Fatalf("doctor with a panic = %+v", c)
	}
	text.Reset()
	if err := Status(cfg, "test", &text, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "4 time(s) since the daemon started, 1 for a PANIC") {
		t.Fatalf("status of a panic:\n%s", text.String())
	}
}
