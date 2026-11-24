package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

// A tenant's stopped pool: sized at start at the most its entry or its
// entitlement grants, so the pool and its reaper's pooled tiers exist; then
// kept at what applies now, the entitlement's when it names a pool, else
// the entry's, never above the tier's max, and none while the tenant is
// held. A tier with no pool from either keeps none, and is not pooled.
func TestTenantPools(t *testing.T) {
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
	ec2 := func(name string, max int) config.Tier {
		return config.Tier{Name: name, Max: max, Provisioner: config.ProvisionerEC2, EC2: &config.EC2Tier{}}
	}
	ten := tenant{ID: "acme", Tiers: []config.Tier{ec2("small", 6), ec2("big", 6), ec2("none", 6)},
		ListPool: map[string]int{"small": 2, "big": 9}}
	pools := func(ts []config.Tier) []int {
		var out []int
		for _, tr := range ts {
			out = append(out, tr.EC2.Pool)
		}
		return out
	}

	// No pool in the record: the entry's, within the tier's max.
	put(entitlement.Active, nil)
	got := s.withPools(ten)
	if p := pools(got.Tiers); !slices.Equal(p, []int{2, 6, 0}) {
		t.Fatalf("pools from the entry: %v, want [2 6 0]", p)
	}
	if ten.Tiers[0].EC2.Pool != 0 {
		t.Fatal("withPools changed the tiers of the tenant it was given")
	}
	if pt := pooledTiers(got.Tiers); !pt["small"] || !pt["big"] || pt["none"] {
		t.Fatalf("pooled tiers %v: the reaper would end the pool's instances, or keep a tier's that has none", pt)
	}
	small := s.tenantPoolTarget(got, got.Tiers[0], acme.bound)
	big := s.tenantPoolTarget(got, got.Tiers[1], acme.bound)
	if small() != 2 || big() != 6 {
		t.Fatalf("targets from the entry: %d %d, want 2 6", small(), big())
	}

	// The entitlement names a pool: it takes the entry's place, and a tier
	// it leaves out keeps none. At start, the larger of the two is made.
	put(entitlement.Active, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if small() != 5 || big() != 0 {
		t.Fatalf("targets from the entitlement: %d %d, want 5 0", small(), big())
	}
	if p := pools(s.withPools(ten).Tiers); !slices.Equal(p, []int{5, 6, 0}) {
		t.Fatalf("pools at start with an entitlement: %v, want [5 6 0]", p)
	}

	// Held: none, whatever is granted; then back once it may run again.
	put(entitlement.Pending, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if small() != 0 {
		t.Fatalf("a held tenant's target: %d, want 0", small())
	}
	put(entitlement.Active, &entitlement.Limits{Pool: map[string]int{"small": 5}})
	if small() != 5 {
		t.Fatalf("resumed: %d, want 5", small())
	}

	// Only a tenant's pooled tier follows a target: the install's pools
	// follow their schedules.
	bound := scheduler.NewTenant("acme", 0)
	if s.poolTargetFor(got, got.Tiers[0], bound) == nil {
		t.Fatal("a tenant's pooled tier follows no target")
	}
	if s.poolTargetFor(got, got.Tiers[2], bound) != nil {
		t.Fatal("a tenant's tier with no pool follows a target")
	}
	if s.poolTargetFor(tenant{}, got.Tiers[0], bound) != nil {
		t.Fatal("the install's tier follows a tenant's target")
	}

	// No entitlements at all: the entry's.
	s2 := &shard{log: s.log, cfg: s.cfg}
	if got := s2.tenantPoolTarget(ten, ten.Tiers[0], scheduler.NewTenant("acme", 0))(); got != 2 {
		t.Fatalf("without entitlements: %d, want the entry's 2", got)
	}
}
