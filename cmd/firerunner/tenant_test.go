package main

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
)

// A tenant's EC2 tier carries its own owner tag and hears of its own runners'
// jobs: without the hook, a job's cache identity would wait on a message GitHub
// may send only after the job ends.
func TestEC2OptionsAreTheTenants(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	var heard string
	hooks := tenantHooks{onRunnerJob: func(runner string) { heard = runner }}
	tier := config.Tier{Name: "t", EC2: &config.EC2Tier{InstanceTypes: []string{"c8a.large"}, Pool: 2}}
	o := s.ec2Options(tenant{Owner: "tenant-1"}, tier, hooks, health.NewTracker("t"), assigned.New(nil))
	if o.Owner != "tenant-1" || o.Tier != "t" {
		t.Fatalf("owner %q, tier %q", o.Owner, o.Tier)
	}
	if o.EC2.Pool != 2 || !o.Pooled || len(o.EC2.InstanceTypes) != 1 {
		t.Fatalf("EC2 = %+v, pooled %v", o.EC2, o.Pooled)
	}
	if o.OnRunnerJob == nil {
		t.Fatal("the tier does not hear of its runners' jobs")
	}
	o.OnRunnerJob("runner-1")
	if heard != "runner-1" {
		t.Fatalf("heard %q", heard)
	}
}

func TestExtraTenant(t *testing.T) {
	cfg := &config.Config{AppClientID: "Iv1", RunnerGroup: "home", Owner: "base"}
	got := extraTenant(cfg, config.Tenant{ID: "t1", URL: "https://github.com/acme", InstallationID: 7}, "base", "pem")
	if got.Owner != "base/t1" {
		t.Fatalf("owner = %q: it scopes the reaper, and must differ from the install's", got.Owner)
	}
	if got.App.InstallationID != 7 || got.App.ClientID != "Iv1" || got.App.PrivateKey != "pem" || got.Token != "" {
		t.Fatalf("app = %+v", got.App)
	}
	if got.URL != "https://github.com/acme" || got.RunnerGroup != "home" || !got.KeepOnClose {
		t.Fatalf("tenant = %+v", got)
	}
	if got := extraTenant(cfg, config.Tenant{ID: "t1", RunnerGroup: "own"}, "base", ""); got.RunnerGroup != "own" {
		t.Fatalf("group = %q", got.RunnerGroup)
	}
}

// Tenants share tier names, so the shard's names must tell them apart, in the
// order of the runs they label.
func TestServedTenantNames(t *testing.T) {
	tiers := []config.Tier{{Name: "a"}, {Name: "b"}}
	ss := servedTenants{
		{tiers: tiers},
		{t: tenant{ID: "t1"}, tiers: tiers},
	}
	if got, want := fmt.Sprint(ss.names()), "[a b t1/a t1/b]"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
}

func TestServedTenantsReaper(t *testing.T) {
	early, late := time.Unix(100, 0), time.Unix(200, 0)
	first := &health.Reaper{At: early, Instances: []string{"i-1"}}
	ss := servedTenants{
		{},
		{reaperState: func() *health.Reaper { return nil }},
		{reaperState: func() *health.Reaper { return first }},
		{reaperState: func() *health.Reaper { return &health.Reaper{At: late, Instances: []string{"i-2"}, Error: "denied"} }},
	}
	got := ss.reaper()
	if !got.At.Equal(late) || fmt.Sprint(got.Instances) != "[i-1 i-2]" || got.Error != "denied" {
		t.Fatalf("sum = %+v", got)
	}
	if len(first.Instances) != 1 {
		t.Fatalf("the sum changed a reaper's own report: %+v", first)
	}
	if (servedTenants{{}}).reaper() != nil {
		t.Fatal("no reaper reports something")
	}
}
