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

	f.TenantLeases.Assigner = true
	f.TenantLeases.Unplaced = []string{"big: it lists 41 tiers, over the 40 a tenant may have"}
	write()
	if s := status(); !strings.Contains(s, "this shard assigns the tenants") || !strings.Contains(s, "not served: big: it lists 41 tiers") {
		t.Fatalf("status of the assigner:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "big: it lists 41 tiers") {
		t.Fatalf("doctor with a tenant unplaced = %+v", c)
	}
	f.TenantLeases.Unplaced = nil

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
