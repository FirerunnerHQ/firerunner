package main

import (
	"log/slog"
	"testing"

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
	o := s.ec2Options(tenant{Owner: "tenant-1"}, config.Tier{Name: "t"}, hooks, health.NewTracker("t"), assigned.New(nil))
	if o.Owner != "tenant-1" || o.Tier != "t" {
		t.Fatalf("owner %q, tier %q", o.Owner, o.Tier)
	}
	if o.OnRunnerJob == nil {
		t.Fatal("the tier does not hear of its runners' jobs")
	}
	o.OnRunnerJob("runner-1")
	if heard != "runner-1" {
		t.Fatalf("heard %q", heard)
	}
}
