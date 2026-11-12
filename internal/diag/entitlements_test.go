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

// status shows each tenant's entitlement and what it allows; doctor passes
// while billing's records hold, naming who it holds, and warns while reads
// fail, while a tenant launches on an expired record, or has none.
func TestStatusAndDoctorShowEntitlements(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	until := now.Add(time.Hour).UTC().Truncate(time.Second)
	f := health.File{PID: 1, WrittenAt: now,
		Tiers: []health.Tier{{Name: "acme/small", State: health.Connected, Since: now}},
		Tenants: []health.Tenant{
			{ID: "", Limits: health.Limits{JobDuration: "0s"}},
			{ID: "acme", Limits: health.Limits{JobDuration: "0s"}, State: "held", Reason: `tenant "acme" is held: its spend cap is reached`,
				Entitlement: &health.Entitlement{Plan: "team", Account: "active", Budget: "capped", ValidUntil: until, Held: true, Why: "its spend cap is reached"}},
			{ID: "beta", Limits: health.Limits{JobDuration: "0s"},
				Entitlement: &health.Entitlement{Account: "trial", Budget: "warning", ValidUntil: until, Warning: true}},
		},
		Entitlements: &health.TenantStates{Store: "ssm:/fr/control/entitlements", LastRead: now}}
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
	for _, want := range []string{
		"acme entitlement: plan team, account active, budget capped, valid until " + until.Format(time.RFC3339) + "; HELD: its spend cap is reached",
		"beta entitlement: account trial, budget warning, valid until " + until.Format(time.RFC3339) + "; near its spend cap",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("status missing %q\n%s", want, text.String())
		}
	}
	check := func() Check {
		for _, c := range listenerChecks(cfg.Firecracker, now) {
			if c.Name == "entitlements" {
				return c
			}
		}
		t.Fatal("no entitlements check")
		return Check{}
	}
	if c := check(); c.Level != levelPass || !strings.Contains(c.Detail, "held by billing: acme (its spend cap is reached)") || !strings.Contains(c.Detail, "near their spend cap: beta") {
		t.Fatalf("doctor = %+v", c)
	}
	f.Tenants[2].Entitlement = &health.Entitlement{Account: "active", Budget: "ok", ValidUntil: now.Add(-time.Hour), FailOpen: true}
	write()
	if c := check(); c.Level != levelWarn || !strings.Contains(c.Detail, "launching on expired records") || !strings.Contains(c.Detail, "beta") {
		t.Fatalf("doctor with a fail-open tenant = %+v", c)
	}
	f.Tenants[2].Entitlement = &health.Entitlement{None: true, Held: true}
	write()
	if c := check(); c.Level != levelWarn || !strings.Contains(c.Detail, "no record") || !strings.Contains(c.Detail, "beta") {
		t.Fatalf("doctor with no record = %+v", c)
	}
	f.Entitlements.LastError, f.Entitlements.LastErrorAt = "AccessDeniedException", now.Add(time.Second)
	write()
	if c := check(); c.Level != levelWarn || !strings.Contains(c.Detail, "AccessDeniedException") {
		t.Fatalf("doctor while reads fail = %+v", c)
	}
}
