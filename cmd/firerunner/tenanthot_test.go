package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

// seqJIT names each runner anew.
type seqJIT struct{ n atomic.Int64 }

func (j *seqJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return fmt.Sprintf("runner-%d", j.n.Add(1)), "jit", nil
}

// idleUntilDone is a provisioner whose runner waits idle until its context
// ends, as a hot runner does.
type idleUntilDone struct{}

func (idleUntilDone) Name() string { return "idle" }
func (idleUntilDone) Launch(ctx context.Context, _, _ string, _ core.RunnerSpec, _ func()) error {
	<-ctx.Done()
	return nil
}

func hotTier(name string, max int) config.Tier {
	return config.Tier{Name: name, Max: max, Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}
}

// A tenant's hot runners: on the tiers granted some as it started; its
// entitlement's in place of its entry's; none while it is held or on a tier
// its plan does not launch; at most the tier's max; and fewer in all than the
// runners it may have at once, the first tiers served first.
func TestTenantHotTargets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "entitlements.json")
	store := &entitlement.FileStore{Path: path}
	s := &shard{log: slog.New(slog.DiscardHandler), cfg: &config.Config{Firecracker: firecrackerLife(6 * time.Hour)}}
	live := &liveTenants{}
	bound := scheduler.NewTenant("acme", 0)
	live.add(&servedTenant{t: tenant{ID: "acme"}, bound: bound})
	if err := s.watchEntitlements(ctx, &config.Config{Entitlements: path}, live, s.log); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(24 * time.Hour)
	put := func(account entitlement.Account, l *entitlement.Limits) {
		t.Helper()
		if err := store.Put(ctx, "acme", entitlement.Record{Account: account, Budget: entitlement.OK, ValidUntil: day, Limits: l}); err != nil {
			t.Fatal(err)
		}
		if err := s.entitlements.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	entry := map[string]int{"small": 2, "big": 9}
	tiers := []config.Tier{hotTier("small", 6), hotTier("big", 6), hotTier("none", 6)}
	ten := s.withHot(tenant{ID: "acme", Tiers: tiers, MinOf: func(n string) int { return entry[n] },
		Limits: config.Limits{Runners: 20, JobDuration: time.Hour}})
	if !ten.Hot["small"] || !ten.Hot["big"] || ten.Hot["none"] {
		t.Fatalf("hot tiers at start %v, want small and big", ten.Hot)
	}

	put(entitlement.Active, nil)
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{2, 6, 0}) {
		t.Fatalf("from the entry: %v, want [2 6 0]", got)
	}
	entry["small"] = 1 // the entry as listed now
	if got := s.hotTargets(ten, tiers, bound); got[0] != 1 {
		t.Fatalf("after an edit: %v", got)
	}
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 3, "none": 4}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{3, 0, 0}) {
		t.Fatalf("from the entitlement: %v, want [3 0 0]: big left out, none not granted at start", got)
	}
	// Fewer in all than the runners it may have at once: 4 runners leave 3.
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 2, "big": 5}, MaxRunners: 4})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{2, 1, 0}) {
		t.Fatalf("within 4 runners: %v, want [2 1 0]", got)
	}
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 2, "big": 2}, Tiers: []string{"big"}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{0, 2, 0}) {
		t.Fatalf("plan launching big alone: %v, want [0 2 0]", got)
	}
	put(entitlement.Pending, &entitlement.Limits{Min: map[string]int{"small": 2}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{0, 0, 0}) {
		t.Fatalf("held: %v, want none", got)
	}
}

// A tier that may keep hot runners lives the job limit plus tenantHotIdle
// plus slack, within the install's, and recycles a runner past what that
// leaves; one granted none keeps the job limit plus slack. A lifetime that
// leaves no room for a wait keeps none.
func TestTenantHotLifetime(t *testing.T) {
	ten := tenant{ID: "acme", Hot: map[string]bool{"small": true}, Limits: config.Limits{JobDuration: time.Hour}}
	small, other := hotTier("small", 4), hotTier("other", 4)
	for _, c := range []struct {
		install              time.Duration
		life, idle, coldLife time.Duration
	}{
		{6 * time.Hour, 3*time.Hour + 15*time.Minute, 2 * time.Hour, 75 * time.Minute},
		{2 * time.Hour, 2 * time.Hour, 45 * time.Minute, 75 * time.Minute},
		{time.Hour, time.Hour, 0, time.Hour},
	} {
		s := &shard{cfg: &config.Config{Firecracker: firecrackerLife(c.install)}}
		life, cold := s.tenantTierLifetime(ten, small), s.tenantTierLifetime(ten, other)
		idle := max(s.hotIdleMax(ten, small), 0)
		if life != c.life || idle != c.idle || cold != c.coldLife {
			t.Errorf("install %s: hot lifetime %s, idle %s, cold lifetime %s; want %s, %s, %s", c.install, life, idle, cold, c.life, c.idle, c.coldLife)
		}
	}
	s := &shard{cfg: &config.Config{Firecracker: firecrackerLife(time.Hour)}}
	bound := scheduler.NewTenant("acme", 0)
	ten.MinOf = func(string) int { return 2 }
	if got := s.hotTargets(ten, []config.Tier{small}, bound); got[0] != 0 {
		t.Fatalf("hot runners on a lifetime that leaves no wait: %v", got)
	}
	// With no lifetime backstop and no job limit, nothing bounds a wait: only
	// the tiers granted at start keep hot runners all the same.
	none := &shard{cfg: &config.Config{Firecracker: firecrackerLife(0)}}
	free := ten
	free.Limits = config.Limits{}
	if got := none.hotTargets(free, []config.Tier{small, other}, bound); !slices.Equal(got, []int{2, 0}) {
		t.Fatalf("no backstop and no job limit: %v, want [2 0]", got)
	}
}

// The reaper ends no instance of a tenant with hot runners sooner than its
// launch's own lifetime does.
func TestTenantReaperLifetime(t *testing.T) {
	cold := tenant{ID: "acme", Limits: config.Limits{JobDuration: time.Hour}}
	hot := cold
	hot.Hot = map[string]bool{"small": true}
	if got := tenantReaperLifetime(6*time.Hour, cold); got != 75*time.Minute {
		t.Fatalf("cold: %s, want 1h15m", got)
	}
	if got := tenantReaperLifetime(6*time.Hour, hot); got != 3*time.Hour+15*time.Minute {
		t.Fatalf("hot: %s, want 3h15m", got)
	}
	s := &shard{cfg: &config.Config{Firecracker: firecrackerLife(6 * time.Hour)}}
	if launch := s.tenantTierLifetime(hot, hotTier("small", 4)); tenantReaperLifetime(6*time.Hour, hot) < launch {
		t.Fatalf("the reaper's %s is shorter than the launch's %s", tenantReaperLifetime(6*time.Hour, hot), launch)
	}
}

// followHot sets each tier's floor to its target as it changes, and the
// floor launches and ends the hot runners.
func TestFollowHot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := hotEvery
	hotEvery = 5 * time.Millisecond
	defer func() { hotEvery = old }()
	s := &shard{log: slog.New(slog.DiscardHandler), cfg: &config.Config{Firecracker: firecrackerLife(6 * time.Hour)}}
	bound := scheduler.NewTenant("acme", 0)
	var want atomic.Int64
	want.Store(2)
	tier := hotTier("small", 4)
	ten := tenant{ID: "acme", Tiers: []config.Tier{tier}, Hot: map[string]bool{"small": true},
		MinOf: func(string) int { return int(want.Load()) }, Limits: config.Limits{Runners: 10, JobDuration: time.Hour}}
	sched := scheduler.New(scheduler.Options{Max: 4, Provisioner: idleUntilDone{}, JIT: &seqJIT{}, Tenant: bound, Logger: s.log})
	sched.Reconcile(ctx, 0) // the listener's first plan: no jobs
	go s.followHot(ctx, ten, ten.Tiers, []*scheduler.Scheduler{sched}, bound, s.log)
	waitFor(t, "two hot runners", func() bool { return sched.Running() == 2 && sched.Min() == 2 })
	want.Store(0)
	waitFor(t, "none", func() bool { return sched.Running() == 0 && sched.Min() == 0 })
}

// An edit of a tenant's hot runners restarts it: which tiers may keep them,
// and their machines' lifetime, are set as it starts.
func TestEditOfHotRunnersRestarts(t *testing.T) {
	two := 2
	a := config.Tenant{ID: "acme", Limits: &config.TenantLimits{MaxRunners: &two}}
	b := a
	b.Limits = &config.TenantLimits{MaxRunners: &two, Min: map[string]int{"small": 1}}
	if onlyRunnersDiffer(a, b) {
		t.Fatal("an edit of limits.min applies in place")
	}
	c := a
	three := 3
	c.Limits = &config.TenantLimits{MaxRunners: &three}
	if !onlyRunnersDiffer(a, c) {
		t.Fatal("an edit of max_runners alone restarts")
	}
}

func firecrackerLife(d time.Duration) provisioner.FirecrackerConfig {
	return provisioner.FirecrackerConfig{MaxVMLifetime: d}
}
