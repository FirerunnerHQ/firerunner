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

// A tenant's hot runners: its entitlement's in place of its entry's, as
// listed now; none while it is held, on a tier its plan does not launch, or
// on a Firecracker tier; at most the tier's max; and fewer in all than the
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
	entry := map[string]int{"small": 2, "big": 9, "vm": 2}
	vm := config.Tier{Name: "vm", Max: 6}
	tiers := []config.Tier{hotTier("small", 6), hotTier("big", 6), hotTier("none", 6), vm}
	ten := tenant{ID: "acme", Tiers: tiers, MinOf: func(n string) int { return entry[n] },
		Limits: config.Limits{Runners: 20, JobDuration: time.Hour}}

	put(entitlement.Active, nil)
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{2, 6, 0, 0}) {
		t.Fatalf("from the entry: %v, want [2 6 0 0]", got)
	}
	entry["small"] = 1 // the entry as listed now
	if got := s.hotTargets(ten, tiers, bound); got[0] != 1 {
		t.Fatalf("after an edit: %v", got)
	}
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 3, "none": 4, "vm": 1}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{3, 0, 4, 0}) {
		t.Fatalf("from the entitlement: %v, want [3 0 4 0]: big left out, a grant applies at once", got)
	}
	// Fewer in all than the runners it may have at once: 4 runners leave 3.
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 2, "big": 5}, MaxRunners: 4})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{2, 1, 0, 0}) {
		t.Fatalf("within 4 runners: %v, want [2 1 0 0]", got)
	}
	put(entitlement.Active, &entitlement.Limits{Min: map[string]int{"small": 2, "big": 2}, Tiers: []string{"big"}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{0, 2, 0, 0}) {
		t.Fatalf("plan launching big alone: %v, want [0 2 0 0]", got)
	}
	put(entitlement.Pending, &entitlement.Limits{Min: map[string]int{"small": 2}})
	if got := s.hotTargets(ten, tiers, bound); !slices.Equal(got, []int{0, 0, 0, 0}) {
		t.Fatalf("held: %v, want none", got)
	}
}

// Every machine of a tenant lives the job limit plus tenantHotIdle plus
// slack, within the install's, hot or not, and a hot runner is recycled past
// what that leaves; a lifetime that leaves no room for a wait keeps none.
func TestTenantHotLifetime(t *testing.T) {
	ten := tenant{ID: "acme", Limits: config.Limits{JobDuration: time.Hour}}
	small := hotTier("small", 4)
	for _, c := range []struct{ install, life, idle time.Duration }{
		{6 * time.Hour, time.Hour + 45*time.Minute, 30 * time.Minute},
		{90 * time.Minute, 90 * time.Minute, 15 * time.Minute},
		{time.Hour, time.Hour, 0},
	} {
		s := &shard{cfg: &config.Config{Firecracker: firecrackerLife(c.install)}}
		life, idle := s.tenantTierLifetime(ten, small), max(s.hotIdleMax(ten, small), 0)
		if life != c.life || idle != c.idle {
			t.Errorf("install %s: lifetime %s, idle %s; want %s, %s", c.install, life, idle, c.life, c.idle)
		}
		if r := tenantReaperLifetime(c.install, ten); r < life {
			t.Errorf("install %s: the reaper's %s is shorter than the launch's %s", c.install, r, life)
		}
	}
	// The install's own tiers keep their lifetime.
	s := &shard{cfg: &config.Config{Firecracker: firecrackerLife(6 * time.Hour)}}
	own := tenant{Limits: ten.Limits}
	if got, want := s.tenantTierLifetime(own, small), tierLifetime(6*time.Hour, time.Hour, small); got != want {
		t.Errorf("the install's tier: %s, want %s", got, want)
	}
	short := &shard{cfg: &config.Config{Firecracker: firecrackerLife(time.Hour)}}
	bound := scheduler.NewTenant("acme", 0)
	ten.MinOf = func(string) int { return 2 }
	if got := short.hotTargets(ten, []config.Tier{small}, bound); got[0] != 0 {
		t.Fatalf("hot runners on a lifetime that leaves no wait: %v", got)
	}
	if got := s.hotTargets(ten, []config.Tier{small}, bound); got[0] != 2 {
		t.Fatalf("hot runners on a 6h install: %v, want 2", got)
	}
}

// followHot leaves a tier alone until its listener has planned it, so the
// floor launches under the tier's own context; it then sets the floor to its
// target as it changes; and the hot runners end with the tier's context.
func TestFollowHot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tierCtx, stopTier := context.WithCancel(ctx)
	defer stopTier()
	old := hotEvery
	hotEvery = 5 * time.Millisecond
	defer func() { hotEvery = old }()
	s := &shard{log: slog.New(slog.DiscardHandler), cfg: &config.Config{Firecracker: firecrackerLife(6 * time.Hour)}}
	bound := scheduler.NewTenant("acme", 0)
	var want atomic.Int64
	want.Store(2)
	tier := hotTier("small", 4)
	ten := tenant{ID: "acme", Tiers: []config.Tier{tier},
		MinOf: func(string) int { return int(want.Load()) }, Limits: config.Limits{Runners: 10, JobDuration: time.Hour}}
	sched := scheduler.New(scheduler.Options{Max: 4, Provisioner: idleUntilDone{}, JIT: &seqJIT{}, Tenant: bound, Logger: s.log})
	scheds := []*scheduler.Scheduler{sched}
	s.hotPass(ctx, ten, ten.Tiers, scheds, bound, s.log)
	if sched.Min() != 0 || sched.Running() != 0 {
		t.Fatalf("before a plan: floor %d, %d running", sched.Min(), sched.Running())
	}
	sched.Reconcile(tierCtx, 0) // the listener's first plan: no jobs
	go s.followHot(ctx, ten, ten.Tiers, scheds, bound, s.log)
	waitFor(t, "two hot runners", func() bool { return sched.Running() == 2 && sched.Min() == 2 })
	want.Store(1)
	waitFor(t, "one", func() bool { return sched.Running() == 1 && sched.Min() == 1 })
	stopTier() // the tier stops: its hot runners end, and none relaunch
	waitFor(t, "none after the tier stopped", func() bool { return sched.Running() == 0 })
	want.Store(3)
	s.hotPass(ctx, ten, ten.Tiers, scheds, bound, s.log)
	if sched.Min() != 1 || sched.Running() != 0 {
		t.Fatalf("after the tier stopped: floor %d, %d running", sched.Min(), sched.Running())
	}
}

// An edit of a tenant's hot runners applies in place: followHot reads the
// entry as listed now.
func TestEditOfHotRunnersAppliesInPlace(t *testing.T) {
	two := 2
	a := config.Tenant{ID: "acme", Limits: &config.TenantLimits{MaxRunners: &two}}
	b := a
	b.Limits = &config.TenantLimits{MaxRunners: &two, Min: map[string]int{"small": 1}}
	if !onlyRunnersDiffer(a, b) {
		t.Fatal("an edit of limits.min restarts the tenant")
	}
}

func firecrackerLife(d time.Duration) provisioner.FirecrackerConfig {
	return provisioner.FirecrackerConfig{MaxVMLifetime: d}
}
