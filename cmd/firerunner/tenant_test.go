package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	home, other := servedTenant{tiers: tiers}, servedTenant{t: tenant{ID: "t1"}, tiers: tiers}
	if got := fmt.Sprint(home.names(), other.names()); got != "[a b] [t1/a t1/b]" {
		t.Fatalf("names = %s", got)
	}
}

func TestLiveTenantsReaper(t *testing.T) {
	early, late := time.Unix(100, 0), time.Unix(200, 0)
	first := &health.Reaper{At: early, Instances: []string{"i-1"}}
	l := &liveTenants{served: []*servedTenant{
		{},
		{reaperState: func() *health.Reaper { return nil }},
		{reaperState: func() *health.Reaper { return first }},
		{reaperState: func() *health.Reaper { return &health.Reaper{At: late, Instances: []string{"i-2"}, Error: "denied"} }},
	}}
	got := l.reaper()
	if !got.At.Equal(late) || fmt.Sprint(got.Instances) != "[i-1 i-2]" || got.Error != "denied" {
		t.Fatalf("sum = %+v", got)
	}
	if len(first.Instances) != 1 {
		t.Fatalf("the sum changed a reaper's own report: %+v", first)
	}
	if (&liveTenants{served: []*servedTenant{{}}}).reaper() != nil {
		t.Fatal("no reaper reports something")
	}
}

// A tenant that cannot start is reported, is tried again, and leaves health
// when it stops.
func TestTenantLoopRetriesAFailedStart(t *testing.T) {
	live := &liveTenants{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serving := make(chan *servedTenant, 1)
	stopped := make(chan *servedTenant, 1)
	sawDown := make(chan bool, 1)
	tries := 0
	l := &tenantLoop{
		id: "t1", live: live, log: slog.New(slog.DiscardHandler), min: time.Millisecond, max: time.Millisecond,
		start: func(context.Context) (*servedTenant, error) {
			tries++
			if tries == 1 {
				return nil, errors.New("no such App")
			}
			sawDown <- slices.ContainsFunc(live.trackers(), func(tr *health.Tracker) bool {
				return tr.Snapshot().Name == "t1" && tr.Snapshot().State == health.Reconnecting
			})
			return &servedTenant{t: tenant{ID: "t1"}, trackers: []*health.Tracker{health.NewTracker("t1/a")}}, nil
		},
		serve: func(ctx context.Context, _ context.CancelFunc, st *servedTenant) error {
			serving <- st
			<-ctx.Done()
			return nil
		},
		stop: func(st *servedTenant) { stopped <- st },
	}
	done := make(chan struct{})
	go func() { l.run(ctx); close(done) }()

	st := <-serving
	if !<-sawDown {
		t.Fatal("a tenant that failed to start was not reported")
	}
	if names := trackerNames(live); names != "[t1/a]" {
		t.Fatalf("while serving, health shows %s", names)
	}
	cancel()
	if got := <-stopped; got != st {
		t.Fatal("stopped another tenant")
	}
	<-done
	if names := trackerNames(live); names != "[]" {
		t.Fatalf("after stopping, health shows %s", names)
	}
}

// The context a try runs in ends before the tenant is drained, taking its
// pools and reaper, and the tenant is then tried again.
func TestTenantLoopEndsATriesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	tries := 0
	l := &tenantLoop{
		id: "t1", live: &liveTenants{}, log: slog.New(slog.DiscardHandler), min: time.Millisecond, max: time.Millisecond,
		start: func(tctx context.Context) (*servedTenant, error) {
			tries++
			if tries == 1 {
				go func() { <-tctx.Done(); close(ended) }()
				return &servedTenant{t: tenant{ID: "t1"}}, nil
			}
			cancel()
			return nil, errors.New("done")
		},
		serve: func(context.Context, context.CancelFunc, *servedTenant) error { return errors.New("restart") },
		stop:  func(*servedTenant) { <-ended },
	}
	l.run(ctx)
	if tries != 2 {
		t.Fatalf("tries = %d, want it tried again after stopping", tries)
	}
}

func trackerNames(l *liveTenants) string {
	names := []string{}
	for _, tr := range l.trackers() {
		names = append(names, tr.Snapshot().Name)
	}
	return fmt.Sprint(names)
}
