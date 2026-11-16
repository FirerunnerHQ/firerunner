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

// status says which shard this is and how many tenants it holds the leases
// of; doctor passes while renewals succeed, and warns while they fail (its
// tenants are handed off) or before the first.
func TestStatusAndDoctorShowTheTenantLeases(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	f := health.File{PID: 1, WrittenAt: now,
		Tiers:        []health.Tier{{Name: "small", State: health.Connected, Since: now}},
		TenantLeases: &health.TenantLeases{Shard: "vm-1a2b3c4d", Tenants: 2, Listed: 5, LastRenewed: now.Add(-4 * time.Second)}}
	write := func() {
		if err := health.Write(filepath.Join(dir, health.FileName), f); err != nil {
			t.Fatal(err)
		}
	}
	write()
	cfg := &config.Config{Firecracker: provisioner.FirecrackerConfig{WorkDir: dir, TapPrefix: "fr", NetBase: 16}}
	status := func() string {
		var text bytes.Buffer
		if err := Status(cfg, "test", &text, false); err != nil {
			t.Fatal(err)
		}
		return text.String()
	}
	check := func() *Check {
		for _, c := range listenerChecks(cfg.Firecracker, now) {
			if c.Name == "tenant-leases" {
				return &c
			}
		}
		return nil
	}
	if s := status(); !strings.Contains(s, "shard vm-1a2b3c4d holds the leases of 2 of 5 tenant(s) listed, renewed 4s ago") {
		t.Fatalf("status:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelPass || !strings.Contains(c.Detail, "2 of 5") {
		t.Fatalf("doctor = %+v", c)
	}

	if s := status(); !strings.Contains(s, "no shard assigns the tenants yet") {
		t.Fatalf("status before an assigner:\n%s", s)
	}
	f.TenantLeases.AssignerShard = "vm-9"
	write()
	if s := status(); !strings.Contains(s, "shard vm-9 assigns the tenants") {
		t.Fatalf("status of another shard:\n%s", s)
	}

	f.TenantLeases.Assigner, f.TenantLeases.AssignerShard = true, "vm-1a2b3c4d"
	f.TenantLeases.Unplaced = []string{"big: it lists 41 tiers, over the 40 a tenant may have"}
	write()
	if s := status(); !strings.Contains(s, "this shard assigns the tenants") || !strings.Contains(s, "placed on no shard: big: it lists 41 tiers") {
		t.Fatalf("status of the assigner:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "big: it lists 41 tiers") {
		t.Fatalf("doctor with a tenant unplaced = %+v", c)
	}
	f.TenantLeases.Unplaced = nil

	// A failed assignment is not a failed renewal.
	f.TenantLeases.AssignError, f.TenantLeases.AssignErrorAt = "assign the tenants: this shard does not hold the assigner's lease", now
	write()
	if s := status(); !strings.Contains(s, "last assignment failed") || strings.Contains(s, "last renewal failed") {
		t.Fatalf("status with an assignment failed:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "failed to assign") || strings.Contains(c.Detail, "cannot renew") {
		t.Fatalf("doctor with an assignment failed = %+v", c)
	}
	f.TenantLeases.AssignError = ""

	f.TenantLeases.Unlisted = []string{"github.com/acme"}
	write()
	if s := status(); !strings.Contains(s, "not in this shard's list, so not served: github.com/acme") {
		t.Fatalf("status with an account unlisted:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "github.com/acme") {
		t.Fatalf("doctor with an account unlisted = %+v", c)
	}
	f.TenantLeases.Unlisted = nil

	f.TenantLeases.LastError, f.TenantLeases.LastErrorAt = "renew the leases: connection refused", now
	write()
	if s := status(); !strings.Contains(s, "last renewal failed") || !strings.Contains(s, "connection refused") {
		t.Fatalf("status while failing:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "connection refused") {
		t.Fatalf("doctor while failing = %+v", c)
	}

	f.TenantLeases = &health.TenantLeases{Shard: "vm-1a2b3c4d"}
	write()
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "not renewed") {
		t.Fatalf("doctor before a renewal = %+v", c)
	}

	f.TenantLeases = nil
	write()
	if c := check(); c != nil {
		t.Fatalf("doctor with no leases = %+v", c)
	}
}
