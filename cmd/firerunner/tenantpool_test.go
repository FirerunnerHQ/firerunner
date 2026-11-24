package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

// A tenant's stopped pool follows, at every pass: its entry as listed now,
// or its entitlement's pool in its place when the record names one; never
// more than the runners it may have at once, nor the tier's max; none on a
// tier its plan does not launch, and none while it is held.
func TestTenantPoolTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "entitlements.json")
	store := &entitlement.FileStore{Path: path}
	s := &shard{log: slog.New(slog.DiscardHandler), cfg: &config.Config{}}
	live := &liveTenants{}
	acme := &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}
	live.add(acme)
	if err := s.watchEntitlements(ctx, &config.Config{Entitlements: path}, live, s.log); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Add(24 * time.Hour)
	put := func(account entitlement.Account, l *entitlement.Limits) {
		t.Helper()
		if err := store.Put(ctx, "acme", entitlement.Record{Account: account, Budget: entitlement.OK, ValidUntil: day, Limits: l}); err != nil {
			t.Fatal(err)
		}
		if err := s.entitlements.Read(ctx); err != nil { // the next tick's read
			t.Fatal(err)
		}
	}
	tier := func(name string, max int) config.Tier {
		return config.Tier{Name: name, Max: max, Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}
	}
	small, big := tier("small", 6), tier("big", 6)
	// The entry as listed now: the test edits it between passes.
	entry := map[string]int{"small": 2, "big": 9}
	ten := tenant{ID: "acme", Tiers: []config.Tier{small, big}, PoolOf: func(n string) int { return entry[n] }}
	sm := s.tenantPoolTarget(ten, small, acme.bound)
	bg := s.tenantPoolTarget(ten, big, acme.bound)

	put(entitlement.Active, nil)
	if sm() != 2 || bg() != 6 {
		t.Fatalf("from the entry: %d %d, want 2 and the tier's max 6", sm(), bg())
	}
	entry["small"] = 0 // an edit of the pool alone restarts nothing
	if sm() != 0 {
		t.Fatalf("after the entry dropped its pool: %d, want 0", sm())
	}
	entry["small"] = 3

	// The entitlement names a pool: it takes the entry's place, and a tier
	// it leaves out keeps none.
	put(entitlement.Active, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if sm() != 5 || bg() != 0 {
		t.Fatalf("from the entitlement: %d %d, want 5 0", sm(), bg())
	}

	// Never more than the runners it may have at once.
	put(entitlement.Active, &entitlement.Limits{Pool: map[string]int{"small": 5}, MaxRunners: 2})
	if sm() != 2 {
		t.Fatalf("with 2 runners at most: %d, want 2", sm())
	}

	// None on a tier the plan does not launch.
	put(entitlement.Active, &entitlement.Limits{Tiers: []string{"big"}})
	if sm() != 0 || bg() != 6 {
		t.Fatalf("plan launching big alone: %d %d, want 0 6", sm(), bg())
	}

	// Held: none, whatever is granted; then back once it may run again.
	put(entitlement.Pending, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if sm() != 0 {
		t.Fatalf("a held tenant's target: %d, want 0", sm())
	}
	put(entitlement.Active, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if sm() != 5 {
		t.Fatalf("resumed: %d, want 5", sm())
	}

	// No entitlements at all: the entry's.
	s2 := &shard{log: s.log, cfg: s.cfg}
	if got := s2.tenantPoolTarget(ten, small, scheduler.NewTenant("acme", 0))(); got != 3 {
		t.Fatalf("without entitlements: %d, want the entry's 3", got)
	}
}

// Every tier of a tenant keeps a pool that follows its target, even one its
// entry and entitlement size at none when it starts: either may grant one
// while it runs. Its reaper keeps the proven pool instances of every tier,
// so a grant read late, or a read that failed at start, never has the
// adopted pool reaped. The install's tiers keep a pool only by their own
// config, and follow their schedules.
func TestTenantTiersAllKeepAPool(t *testing.T) {
	s := &shard{cfg: &config.Config{}, log: slog.New(slog.DiscardHandler)}
	tier := config.Tier{Name: "t", Max: 4, Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{InstanceTypes: []string{"c8a.large"}}}
	bound := scheduler.NewTenant("acme", 0)
	ten := tenant{ID: "acme", Owner: "fr/acme", Tiers: []config.Tier{tier}}
	o := s.ec2Options(ten, tier, tenantHooks{}, health.NewTracker("t"), assigned.New(nil))
	if !o.Pooled || o.EC2.Pool != 0 {
		t.Fatalf("a tenant's tier with no pool yet: pooled %v, size %d; want a pool, empty", o.Pooled, o.EC2.Pool)
	}
	if s.poolTargetFor(ten, tier, bound) == nil {
		t.Fatal("a tenant's tier follows no target")
	}
	if o := s.ec2Options(tenant{Owner: "fr"}, tier, tenantHooks{}, health.NewTracker("t"), assigned.New(nil)); o.Pooled {
		t.Fatal("an install tier with no pool keeps one")
	}
	if s.poolTargetFor(tenant{}, tier, bound) != nil {
		t.Fatal("an install tier follows a tenant's target")
	}
	micro := config.Tier{Name: "micro"}
	if pt := tenantPooledTiers([]config.Tier{tier, micro}); !pt["t"] || pt["micro"] {
		t.Fatalf("the tenant's reaper keeps pool instances of %v; want its EC2 tier alone", pt)
	}
}
