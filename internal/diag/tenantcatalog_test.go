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

// status says where the tenants list comes from and when it was read;
// doctor passes while it reads and applies, and warns while it does not:
// a tenant added or removed meanwhile is not.
func TestStatusAndDoctorShowTheTenantsList(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	f := health.File{PID: 1, WrittenAt: now,
		Tiers:         []health.Tier{{Name: "small", State: health.Connected, Since: now}},
		TenantCatalog: &health.TenantStates{Store: "ssm:/fr/control/tenants", LastRead: now.Add(-10 * time.Second)}}
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
			if c.Name == "tenant-catalog" {
				return &c
			}
		}
		return nil
	}
	if s := status(); !strings.Contains(s, "tenants list from ssm:/fr/control/tenants, read 10s ago") {
		t.Fatalf("status:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelPass || !strings.Contains(c.Detail, "ssm:/fr/control/tenants") {
		t.Fatalf("doctor = %+v", c)
	}

	f.TenantCatalog.LastError, f.TenantCatalog.LastErrorAt = `tenant "acme": tier "huge" is not in the catalog`, now
	write()
	if s := status(); !strings.Contains(s, "serving the tenants as they were: tenant \"acme\"") {
		t.Fatalf("status while rejected:\n%s", s)
	}
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, `tier "huge"`) {
		t.Fatalf("doctor while rejected = %+v", c)
	}

	f.TenantCatalog = &health.TenantStates{Store: "ssm:/fr/control/tenants"}
	write()
	if c := check(); c == nil || c.Level != levelWarn || !strings.Contains(c.Detail, "not read yet") {
		t.Fatalf("doctor before a read = %+v", c)
	}

	f.TenantCatalog = nil
	write()
	if c := check(); c != nil {
		t.Fatalf("doctor with no tenants list = %+v", c)
	}
}
