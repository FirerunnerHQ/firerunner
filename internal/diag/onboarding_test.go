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

// status says what onboarding applied and dropped; doctor passes while
// deliveries apply, and warns while they fail or keeping the runner groups
// does. A shard that is not the assigner says another applies them.
func TestStatusAndDoctorShowOnboarding(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	f := health.File{PID: 1, WrittenAt: now,
		Tiers:      []health.Tier{{Name: "small", State: health.Connected, Since: now}},
		Onboarding: &health.Onboarding{Active: true, Applied: 3, Dropped: 1, LastDelivery: now.Add(-5 * time.Second)}}
	write := func() {
		if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	status := func() string {
		write()
		var text bytes.Buffer
		if err := Status(cfg, "test", &text, false); err != nil {
			t.Fatal(err)
		}
		return text.String()
	}
	check := func() *Check {
		write()
		for _, c := range listenerChecks(cfg.Firecracker, now) {
			if c.Name == "onboarding" {
				return &c
			}
		}
		return nil
	}
	if s := status(); !strings.Contains(s, "applied 3 webhook deliveries (last 5s ago)") || !strings.Contains(s, "dropped 1 malformed") {
		t.Fatalf("status:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelPass {
		t.Fatalf("doctor = %+v", c)
	}

	f.Onboarding.LastError, f.Onboarding.LastErrorAt = "delivery d-1: database down", now.Add(-time.Second)
	if s := status(); !strings.Contains(s, "last delivery failed") || !strings.Contains(s, "database down") {
		t.Fatalf("status failing:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "dead-letter") {
		t.Fatalf("doctor failing = %+v", c)
	}
	// Applied one since: no longer failing.
	f.Onboarding.LastDelivery = now
	if c := check(); c == nil || c.Level != levelPass {
		t.Fatalf("doctor after a later delivery = %+v", c)
	}

	f.Onboarding.Rejected, f.Onboarding.LastRejectAt = 2, now
	if s := status(); !strings.Contains(s, "is --onboard-webhook-secret-file the App's webhook secret") {
		t.Fatalf("status rejecting:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "dead-letter") {
		t.Fatalf("doctor rejecting = %+v", c)
	}
	f.Onboarding.LastDelivery = now.Add(time.Second)
	if c := check(); c == nil || c.Level != levelPass {
		t.Fatalf("doctor after a delivery applied since = %+v", c)
	}

	f.Onboarding.ReceiveError, f.Onboarding.ReceiveErrorAt = "throttled", now
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "queue") {
		t.Fatalf("doctor receive = %+v", c)
	}
	f.Onboarding.ReceiveError = ""

	f.Onboarding.KeepAllError = "installation 7 (acme): 502"
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "runner groups") {
		t.Fatalf("doctor keep-all = %+v", c)
	}

	f.Onboarding = &health.Onboarding{}
	if s := status(); !strings.Contains(s, "another shard (the assigner) applies") {
		t.Fatalf("status of another shard:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelPass {
		t.Fatalf("doctor of another shard = %+v", c)
	}

	f.Onboarding = nil
	if c := check(); c != nil {
		t.Fatalf("doctor without onboarding = %+v", c)
	}
}
