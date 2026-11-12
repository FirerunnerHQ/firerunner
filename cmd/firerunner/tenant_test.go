package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
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

// A tenant's job limit also bounds its machines' lives, from launch, so a
// machine whose job's start is never reported still ends; the install's own
// backstop applies when it is shorter.
func TestMaxLifetimeFollowsTheJobLimit(t *testing.T) {
	for _, c := range []struct{ install, job, want time.Duration }{
		{6 * time.Hour, 0, 6 * time.Hour},
		{6 * time.Hour, 30 * time.Minute, 45 * time.Minute},
		{0, 30 * time.Minute, 45 * time.Minute},
		{20 * time.Minute, 30 * time.Minute, 20 * time.Minute},
		{0, 0, 0},
	} {
		if got := maxLifetime(c.install, c.job); got != c.want {
			t.Errorf("maxLifetime(%s, %s) = %s, want %s", c.install, c.job, got, c.want)
		}
	}
	s := &shard{cfg: &config.Config{Firecracker: provisioner.FirecrackerConfig{MaxVMLifetime: 6 * time.Hour}}, log: slog.New(slog.DiscardHandler)}
	tier := config.Tier{Name: "t", EC2: &config.EC2Tier{InstanceTypes: []string{"c8a.large"}}}
	o := s.ec2Options(tenant{Owner: "o/t1", Limits: config.Limits{JobDuration: 30 * time.Minute}}, tier, tenantHooks{}, health.NewTracker("t"), assigned.New(nil))
	if o.MaxLifetime != 45*time.Minute {
		t.Fatalf("a limited tenant's tier lives %s", o.MaxLifetime)
	}
	// A tier that keeps warm runners keeps the install's backstop: a warm
	// runner may wait longer than the slack for its job.
	tier.Min = 1
	o = s.ec2Options(tenant{Owner: "o", Limits: config.Limits{JobDuration: 30 * time.Minute}}, tier, tenantHooks{}, health.NewTracker("t"), assigned.New(nil))
	if o.MaxLifetime != 6*time.Hour {
		t.Fatalf("a tier with warm runners lives %s", o.MaxLifetime)
	}
}

// An EC2 instance's job report marks its runner busy, which starts the job
// limit's clock, once the tier's scheduler exists; the cache hook still hears.
func TestInstanceJobReportMarksBusy(t *testing.T) {
	var heard []string
	var sched atomic.Pointer[scheduler.Scheduler]
	f := alsoMarkBusy(func(r string) { heard = append(heard, r) }, sched.Load)
	f("runner-1") // before the scheduler: no panic, the hook hears
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launched := make(chan struct{})
	s := scheduler.New(scheduler.Options{Max: 1, Logger: slog.New(slog.DiscardHandler), JIT: oneJIT{},
		Provisioner: blockingProv{launched}})
	sched.Store(s)
	s.Reconcile(ctx, 1)
	<-launched
	if idle := s.IdleRunners(-time.Hour); len(idle) != 1 {
		t.Fatalf("idle %v before the report", idle)
	}
	f("runner-2")
	if len(heard) != 2 {
		t.Fatalf("heard %v", heard)
	}
	if idle := s.IdleRunners(-time.Hour); len(idle) != 0 {
		t.Fatalf("idle %v: the report did not mark the runner busy", idle)
	}
	alsoMarkBusy(nil, sched.Load)("runner-3") // no cache hook
}

type oneJIT struct{}

func (oneJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner-2", "jit", nil
}

// blockingProv's runner runs until its context ends.
type blockingProv struct{ launched chan<- struct{} }

func (p blockingProv) Launch(ctx context.Context, _, _ string, _ core.RunnerSpec, _ func()) error {
	close(p.launched)
	<-ctx.Done()
	return nil
}

func (blockingProv) Name() string { return "fake" }

// The health file lists the tenants with their limits only when there is
// something to say: an install alone and unlimited lists none.
func TestLiveTenantsReportLimits(t *testing.T) {
	install := &servedTenant{t: tenant{}, bound: scheduler.NewTenant("", 0)}
	l := &liveTenants{}
	l.add(install)
	if got := l.tenants(nil, nil); got != nil {
		t.Fatalf("an unlimited install alone: %+v", got)
	}
	flagged := tenantstate.Flags{tenantstate.Install: {Type: "CryptoCurrency:EC2/BitcoinTool.B!DNS", Severity: 8, Count: 1}}
	if got := l.tenants(nil, flagged); len(got) != 1 || got[0].Flag == nil || got[0].Flag.Severity != 8 {
		t.Fatalf("an unlimited install alone, flagged: %+v", got)
	}
	acme := &servedTenant{t: tenant{ID: "acme", Limits: config.Limits{Runners: 2, VCPU: 2, JobDuration: 30 * time.Minute}, Refused: []string{"big"}},
		bound: scheduler.NewTenant("acme", 2)}
	l.add(acme)
	l.skip(tenant{ID: "aaa", Limits: config.Limits{VCPU: 1}}, "none of its tiers is within its limits")
	got := l.tenants(nil, nil)
	if len(got) != 3 || got[0].ID != "" || got[1].ID != "aaa" || got[2].ID != "acme" {
		t.Fatalf("tenants = %+v", got)
	}
	if a := got[2]; a.Limits != (health.Limits{Runners: 2, VCPU: 2, JobDuration: "30m0s"}) || len(a.Refused) != 1 {
		t.Fatalf("acme = %+v", a)
	}
	if got[1].NotServed == "" {
		t.Fatalf("a tenant not served does not say why: %+v", got[1])
	}
	// A flag on a tenant this daemon does not serve still shows.
	got = l.tenants(nil, tenantstate.Flags{"gone": {Type: "x", Count: 1}, "acme": {Type: "y", Count: 1}})
	if len(got) != 4 || got[3].ID != "gone" || got[3].Flag == nil || got[3].NotServed == "" || got[2].Flag == nil || got[2].Flag.Type != "y" {
		t.Fatalf("tenants with flags = %+v", got)
	}
}

// A tier's listener reports the IDs of the runners it registers to the
// tenant's hook, which the job identity matches runners by.
func TestListenerReportsRunnerIDs(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	var gotName string
	var gotID int64
	hooks := tenantHooks{onRunnerID: func(name string, id int64) { gotName, gotID = name, id }}
	c := s.listenerConfig(tenant{Owner: "tenant-1"}, config.Tier{Name: "t"}, hooks, health.NewTracker("t"), assigned.New(nil), slog.New(slog.DiscardHandler), nil)
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
	// The noted ID reaches the runner's lookup once it registers.
	hooks.onRunnerID("runner-1", 900)
	if err := jobs.Register("runner-1", "172.16.0.2"); err != nil {
		t.Fatal(err)
	}
	cands := &jobidentity.Candidates{}
	cands.Offer(jobidentity.Job{Repository: "acme/app", RunID: 1})
	f := &idFinder{}
	jobs.Discover(context.Background(), "runner-1", cands, f, noResolve{})
	if f.got != 900 {
		t.Fatalf("the runner's lookup got ID %d, want 900", f.got)
	}
}

// idFinder records the runner ID it is asked about and reports a job, so
// Discover stops at once.
type idFinder struct{ got int64 }

func (f *idFinder) RunnerJob(_ context.Context, _ string, _ int64, _ string, runnerID int64) (int64, bool, error) {
	f.got = runnerID
	return 1, true, nil
}

// noResolve leaves the identity unresolved; the test looks only at the lookup.
type noResolve struct{}

func (noResolve) Resolve(context.Context, jobidentity.Job) (jobidentity.Identity, error) {
	return jobidentity.Identity{}, errors.New("not resolved in this test")
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
	if got.Limits != (config.Limits{Runners: config.DefaultTenantRunners, VCPU: config.DefaultTenantVCPU, JobDuration: config.DefaultTenantJobDuration}) {
		t.Fatalf("limits = %+v: a tenant with none set runs under the defaults", got.Limits)
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
	lc := s.listenerConfig(ten, config.Tier{Name: "t", Max: 3}, tenantHooks{}, health.NewTracker("t"), assigned.New(nil), s.log, nil)
	if lc.App.InstallationID != 7 || lc.App.JWT != jwt || lc.URL != ten.URL || lc.MaxRunners != 3 {
		t.Fatalf("listener config = %+v", lc)
	}
}

// A tier's scheduler and listener share its tenant's bound and limits: the
// scheduler holds launches past the bound and ends jobs past the limit,
// reporting both in the tier's health, and the listener records a job
// assigned meanwhile as held.
func TestTierGetsItsTenantsLimits(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	ten := tenant{ID: "acme", Limits: config.Limits{Runners: 1, JobDuration: 30 * time.Minute}}
	bound := scheduler.NewTenant("acme", 1)
	tracker := health.NewTracker("acme/t")
	tier := config.Tier{Name: "t", Max: 3}
	o := s.schedulerOptions(ten, tier, bound, tracker, nil, nil, s.log)
	if o.Tenant != bound || o.Tier != "t" || o.MaxJob != 30*time.Minute || o.Max != 3 {
		t.Fatalf("scheduler options = %+v", o)
	}
	o.OnHold(2, "at its limit")
	if h := tracker.Snapshot().Held; h == nil || h.Jobs != 2 {
		t.Fatalf("a hold is not in the tier's health: %+v", h)
	}
	o.OnStop(scheduler.Stop{Runner: "r", Kind: scheduler.StopJobDuration, Reason: "past"})
	if n := tracker.Snapshot().Stopped[scheduler.StopJobDuration]; n != 1 {
		t.Fatalf("a stop is not in the tier's health: %d", n)
	}

	held := heldBy(func() *scheduler.Scheduler { return nil })
	if n, why := held(); n != 0 || why != "" {
		t.Fatalf("before the scheduler: %d %q", n, why)
	}
	sched := scheduler.New(o)
	lc := s.listenerConfig(ten, tier, tenantHooks{}, tracker, assigned.New(nil), s.log, heldBy(func() *scheduler.Scheduler { return sched }))
	if lc.Held == nil {
		t.Fatal("the listener does not ask the tier's scheduler what it holds")
	}
	if n, _ := lc.Held(); n != 0 {
		t.Fatalf("held %d", n)
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
		stop: func(_ context.Context, st *servedTenant) { stopped <- st },
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
		stop:  func(context.Context, *servedTenant) { <-ended },
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
