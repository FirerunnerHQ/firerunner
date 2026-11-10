package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
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

// A tier's listener reports the IDs of the runners it registers to the
// tenant's hook, which the job identity matches runners by.
func TestListenerReportsRunnerIDs(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	var gotName string
	var gotID int64
	hooks := tenantHooks{onRunnerID: func(name string, id int64) { gotName, gotID = name, id }}
	c := s.listenerConfig(tenant{Owner: "tenant-1"}, config.Tier{Name: "t"}, hooks, health.NewTracker("t"), assigned.New(nil), slog.New(slog.DiscardHandler))
	if c.OnRunnerID == nil {
		t.Fatal("the listener does not report runner IDs")
	}
	c.OnRunnerID("runner-1", 900)
	if gotName != "runner-1" || gotID != 900 {
		t.Fatalf("reported %q %d", gotName, gotID)
	}
}

// With an EC2 cache, the identity hooks note each runner's ID in the job
// registry, which Discover then matches the runner's job by.
func TestIdentityHooksNoteRunnerIDs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobs, err := jobidentity.NewRegistry(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jobs.Close)
	cfg := &config.Config{Tiers: []config.Tier{{Name: "t", Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{Cache: true}}}}
	if !cfg.UsesEC2Cache() {
		t.Fatal("the test config uses no EC2 cache")
	}
	s := &shard{cfg: cfg, log: slog.New(slog.DiscardHandler), cacheJobs: jobs}
	hooks, err := s.identityHooks(context.Background(), tenant{URL: "https://github.com/acme", Token: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	if hooks.onRunnerID == nil {
		t.Fatal("the hooks do not note runner IDs")
	}
}

func TestExtraTenant(t *testing.T) {
	cfg := &config.Config{AppClientID: "Iv1", RunnerGroup: "home", Owner: "base"}
	jwt := ghauth.NewJWT("Iv1", nil)
	got := extraTenant(cfg, config.Tenant{ID: "t1", URL: "https://github.com/acme", InstallationID: 7}, "base", jwt)
	if got.Owner != "base/t1" {
		t.Fatalf("owner = %q: it scopes the reaper, and must differ from the install's", got.Owner)
	}
	if got.Auth.InstallationID != 7 || got.Auth.JWT != jwt || got.Token != "" {
		t.Fatalf("auth = %+v: the tenant would act as another installation", got.Auth)
	}
	if got.URL != "https://github.com/acme" || got.RunnerGroup != "home" || !got.KeepOnClose {
		t.Fatalf("tenant = %+v", got)
	}
	if got := extraTenant(cfg, config.Tenant{ID: "t1", RunnerGroup: "own"}, "base", nil); got.RunnerGroup != "own" {
		t.Fatalf("group = %q", got.RunnerGroup)
	}
}

// A tenant's scale sets authenticate as its own installation, with the shard's
// one JWT source: the library is given no key.
func TestListenerConfigIsTheTenantsApp(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	jwt := ghauth.NewJWT("Iv1", nil)
	ten := tenant{URL: "https://github.com/acme", Auth: ghauth.App{InstallationID: 7, JWT: jwt}}
	lc := s.listenerConfig(ten, config.Tier{Name: "t", Max: 3}, tenantHooks{}, health.NewTracker("t"), assigned.New(nil), s.log)
	if lc.App.InstallationID != 7 || lc.App.JWT != jwt || lc.URL != ten.URL || lc.MaxRunners != 3 {
		t.Fatalf("listener config = %+v", lc)
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

// A key in another region is signed with there.
func TestKMSClientUsesTheKeysRegion(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cfg := &config.Config{AWSRegion: "us-east-1", AppKMSKey: "arn:aws:kms:eu-west-1:111122223333:key/0aaa"}
	c, err := kmsClient(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Options().Region; got != "eu-west-1" {
		t.Fatalf("region = %s, want the key's", got)
	}
	cfg.AppKMSKey = "alias/github-app"
	if c, _ := kmsClient(context.Background(), cfg); c.Options().Region != "us-east-1" {
		t.Fatalf("an alias left the VM's region: %s", c.Options().Region)
	}
}
