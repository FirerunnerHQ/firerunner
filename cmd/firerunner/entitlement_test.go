package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

// A served tenant's launches follow its entitlement at each read: held with
// no record, released by an active one, held again at its cap, and released
// when billing lifts it. The install is never held.
func TestEntitlementsHoldAndReleaseAServedTenant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "entitlements.json")
	store := &entitlement.FileStore{Path: path}
	s := &shard{log: slog.New(slog.DiscardHandler)}
	live := &liveTenants{}
	acme := &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}
	install := &servedTenant{t: tenant{}, bound: scheduler.NewTenant("", 0)}
	live.add(acme)
	live.add(install)
	if err := s.watchEntitlements(ctx, &config.Config{Entitlements: path}, live, s.log); err != nil {
		t.Fatal(err)
	}
	held := func(st *servedTenant) string { _, why := st.bound.State(); return why }
	write := func(r entitlement.Record) {
		t.Helper()
		if err := store.Put(ctx, "acme", r); err != nil {
			t.Fatal(err)
		}
		if err := s.entitlements.Read(ctx); err != nil { // the next tick's read
			t.Fatal(err)
		}
	}
	if why := held(acme); !strings.Contains(why, "no entitlement record") {
		t.Fatalf("acme with no record: %q", why)
	}
	if why := held(install); why != "" {
		t.Fatalf("the install was held: %q", why)
	}
	day := time.Now().Add(24 * time.Hour)
	write(entitlement.Record{Account: entitlement.Active, Budget: entitlement.OK, ValidUntil: day})
	if why := held(acme); why != "" {
		t.Fatalf("acme active: held %q", why)
	}
	write(entitlement.Record{Account: entitlement.Active, Budget: entitlement.Capped, ValidUntil: day, Reason: "November's cap"})
	if susp, why := acme.bound.State(); susp || !strings.Contains(why, "spend cap is reached: November's cap") {
		t.Fatalf("acme capped: suspended %v, %q; want held, not suspended", susp, why)
	}
	write(entitlement.Record{Account: entitlement.Trial, Budget: entitlement.OK, ValidUntil: time.Now().Add(-time.Minute)})
	if why := held(acme); !strings.Contains(why, "expired") {
		t.Fatalf("acme's trial expired: %q", why)
	}
	write(entitlement.Record{Account: entitlement.Active, Budget: entitlement.Warning, ValidUntil: time.Now().Add(-time.Minute)})
	if why := held(acme); why != "" {
		t.Fatalf("acme active, expired: held %q; want it launching (fail open)", why)
	}

	// status and doctor see it, through what the health publisher writes.
	f := health.File{Tenants: live.tenants(nil, nil)}
	s.entitlementHealth(&f)
	open := false
	for _, ht := range f.Tenants {
		open = open || (ht.ID == "acme" && ht.Entitlement != nil && ht.Entitlement.FailOpen)
		if ht.ID == "" && ht.Entitlement != nil {
			t.Fatalf("the install has an entitlement: %+v", ht.Entitlement)
		}
	}
	if !open || f.Entitlements == nil || f.Entitlements.Store != path {
		t.Fatalf("health = %+v, %+v; want acme launching on an expired record", f.Tenants, f.Entitlements)
	}
}

// With both the states and the entitlements read, a tenant served after the
// first reads is judged by both as it is added: held by its entitlement,
// though no state holds it.
func TestTenantAddedLaterIsJudgedByBothWatchers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	s := &shard{log: slog.New(slog.DiscardHandler)}
	live := &liveTenants{}
	if err := s.watchStates(ctx, &config.Config{TenantState: filepath.Join(dir, "tenant-state.json")}, live, s.log); err != nil {
		t.Fatal(err)
	}
	if err := s.watchEntitlements(ctx, &config.Config{Entitlements: filepath.Join(dir, "entitlements.json")}, live, s.log); err != nil {
		t.Fatal(err)
	}
	acme := &servedTenant{t: tenant{ID: "acme"}, bound: scheduler.NewTenant("acme", 0)}
	live.add(acme)
	if _, why := acme.bound.State(); !strings.Contains(why, "no entitlement record") {
		t.Fatalf("acme added after the reads: %q", why)
	}
}

// The watchers and the tenant loops judge tenants at once. Under -race, this
// finds no data race, and a suspension read meanwhile wins. That each
// decision is taken and applied as one step (applyMu), so a decision on
// older inputs never lands after one on newer, it does not prove: an
// interleaving that breaks it needs a pause inside applyState.
func TestApplyStateConcurrently(t *testing.T) {
	s, store := statesFor(t, tenantstate.States{})
	ctx := context.Background()
	estore := &entitlement.FileStore{Path: filepath.Join(t.TempDir(), "entitlements.json")}
	day := time.Now().Add(24 * time.Hour)
	if err := estore.Put(ctx, "acme", entitlement.Record{Account: entitlement.Active, Budget: entitlement.Capped, ValidUntil: day}); err != nil {
		t.Fatal(err)
	}
	s.entitlements = &entitlement.Watcher{Store: estore}
	if err := s.entitlements.Read(ctx); err != nil {
		t.Fatal(err)
	}
	acme := scheduler.NewTenant("acme", 0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.applyState("acme", acme, s.log)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = store.Put(ctx, "acme", tenantstate.Record{Reason: "abuse"})
		_ = s.states.Read(ctx)
		s.applyState("acme", acme, s.log)
	}()
	wg.Wait()
	s.applyState("acme", acme, s.log)
	if susp, why := acme.State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("acme: suspended %v, %q; want the suspension, as read last", susp, why)
	}
}

// A suspension outranks an entitlement that would let the tenant launch, and
// an entitlement's hold applies while the tenant is not suspended.
func TestApplyStateRanksSuspensionOverEntitlement(t *testing.T) {
	s, _ := statesFor(t, tenantstate.States{"acme": {Reason: "abuse"}})
	path := filepath.Join(t.TempDir(), "entitlements.json")
	store := &entitlement.FileStore{Path: path}
	day := time.Now().Add(24 * time.Hour)
	ctx := context.Background()
	if err := store.Put(ctx, "acme", entitlement.Record{Account: entitlement.Active, Budget: entitlement.OK, ValidUntil: day}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "beta", entitlement.Record{Account: entitlement.Suspended, Budget: entitlement.OK, ValidUntil: day}); err != nil {
		t.Fatal(err)
	}
	s.entitlements = &entitlement.Watcher{Store: store}
	if err := s.entitlements.Read(ctx); err != nil {
		t.Fatal(err)
	}
	acme, beta := scheduler.NewTenant("acme", 0), scheduler.NewTenant("beta", 0)
	s.applyState("acme", acme, s.log)
	s.applyState("beta", beta, s.log)
	if susp, why := acme.State(); !susp || !strings.Contains(why, "abuse") {
		t.Fatalf("acme: suspended %v, %q; want the operator's suspension", susp, why)
	}
	if susp, why := beta.State(); susp || !strings.Contains(why, "account is suspended") {
		t.Fatalf("beta: suspended %v, %q; want held by billing, its runners not stopped", susp, why)
	}
}
