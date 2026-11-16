package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// fakeLeases is a lease store that grants what the test says, and records
// what it was asked.
type fakeLeases struct {
	mu sync.Mutex
	// grant is the accounts the next renewals hold, of those asked; err,
	// when set, fails them.
	grant []string
	err   error
	// during runs inside each renewal, as a slow database would.
	during   func()
	asked    [][]string
	joinErr  error
	joined   []jobstore.Shard
	released []jobstore.Shard
}

func (f *fakeLeases) Join(_ context.Context, sh jobstore.Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = append(f.joined, sh)
	return f.joinErr
}

func (f *fakeLeases) Renew(_ context.Context, _ jobstore.Shard, accounts []string, ttl time.Duration) ([]string, error) {
	f.mu.Lock()
	during, err := f.during, f.err
	f.asked = append(f.asked, slices.Clone(accounts))
	var held []string
	for _, a := range accounts {
		if slices.Contains(f.grant, a) {
			held = append(held, a)
		}
	}
	f.mu.Unlock()
	if ttl != leaseTTL {
		return nil, errors.New("asked for another ttl")
	}
	if during != nil {
		during()
	}
	if err != nil {
		return nil, err
	}
	return held, nil
}

func (f *fakeLeases) Release(_ context.Context, sh jobstore.Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, sh)
	return nil
}

func (f *fakeLeases) set(grant ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grant, f.err = grant, nil
}

func (f *fakeLeases) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// lastAsked is the accounts the last renewal asked for.
func (f *fakeLeases) lastAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.asked) == 0 {
		return nil
	}
	return f.asked[len(f.asked)-1]
}

func acct(id string) string { return config.AccountOf(listed(id, 1, "x").URL) }

func leaserLab(t *testing.T) (*setLab, *leaseClock, *fakeLeases, *tenantLeaser) {
	l, c := leaseLab(t)
	store := &fakeLeases{}
	lr := &tenantLeaser{store: store, shard: jobstore.Shard{ID: "s1", Owner: "vm-1", TenantOwner: "fr"}, set: l.set,
		log: slog.New(slog.DiscardHandler), now: c.now}
	return l, c, store, lr
}

func mustRenew(t *testing.T, lr *tenantLeaser) {
	t.Helper()
	if err := lr.renew(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A shard serves the tenants whose accounts it holds the lease of, asks for
// the account of each tenant listed, and hands a tenant off once another
// shard holds its lease.
func TestLeaserServesWhatItLeases(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	// Before a list, a renewal only beats the heartbeat.
	mustRenew(t, lr)
	if got := store.lastAsked(); len(got) != 0 {
		t.Fatalf("asked for %v before a list", got)
	}

	list := []config.Tenant{listed("a", 2, "x"), listed("b", 2, "x")}
	lr.apply(list)
	if len(l.runs()) != 0 {
		t.Fatal("served before any lease")
	}
	store.set(acct("a"))
	mustRenew(t, lr)
	if got, want := store.lastAsked(), []string{acct("a"), acct("b")}; !slices.Equal(got, want) {
		t.Fatalf("asked for %v, want %v", got, want)
	}
	l.started("a")
	if _, ok := l.runs()["b"]; ok {
		t.Fatal("b served without its lease")
	}
	if st := lr.status(); st.Tenants != 1 || st.Listed != 2 || st.Shard != "s1" || st.Failing() || st.LastRenewed.IsZero() {
		t.Fatalf("status %+v", st)
	}

	store.set(acct("b"))
	mustRenew(t, lr)
	l.ended("a", errTenantHandedOff)
	l.started("b")

	// A list read between renewals applies under the leases held.
	lr.apply(list[:1])
	l.ended("b", errTenantRemoved)
}

// A renewal that fails changes nothing; the tenant is handed off by its
// deadline, counted from the moment the last renewal that succeeded was
// sent, however long that one took.
func TestLeaserHandsOffWhenRenewalsFail(t *testing.T) {
	l, c, store, lr := leaserLab(t)
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	store.set(acct("a"))
	store.during = func() { c.advance(3 * time.Second) } // a slow database
	mustRenew(t, lr)
	l.started("a")
	store.during = nil

	store.fail(errors.New("connection refused"))
	if err := lr.renew(context.Background()); err == nil {
		t.Fatal("a failed renewal returned no error")
	}
	if st := lr.status(); !st.Failing() || !strings.Contains(st.LastError, "refused") {
		t.Fatalf("status %+v, want failing", st)
	}
	before := l.runs()
	c.advance(16 * time.Second) // 19s after the renewal was sent
	l.unchanged(before)
	c.advance(time.Second) // 20s
	l.ended("a", errTenantHandedOff)
	if st := lr.status(); st.Tenants != 0 {
		t.Fatalf("status %+v, want no tenant held past its deadline", st)
	}

	// A renewal that succeeds again serves it anew, once the old run ends.
	store.set(acct("a"))
	mustRenew(t, lr)
	l.finish("a")
	l.started("a")
	if lr.status().Failing() {
		t.Fatal("still failing after a renewal")
	}
}

// A tenant removed whose run still drains keeps its account's lease: it
// deletes its scale sets, by name in the account, once its runners finish,
// and no other shard may register there meanwhile. Once it has, the lease
// is no longer asked for.
func TestLeaserKeepsTheLeaseOfARemovedTenantDraining(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	store.set(acct("a"))
	mustRenew(t, lr)
	l.started("a")

	lr.apply(nil)
	l.ended("a", errTenantRemoved)
	mustRenew(t, lr)
	if got := store.lastAsked(); !slices.Equal(got, []string{acct("a")}) {
		t.Fatalf("asked for %v while a drains, want its account", got)
	}
	l.finish("a")
	l.set.wg.Wait() // its run has returned, and deleted what it registered
	mustRenew(t, lr)
	if got := store.lastAsked(); len(got) != 0 {
		t.Fatalf("asked for %v once a had ended, want nothing", got)
	}
}

// The catalog applies each list it reads through the leaser.
func TestTenantCatalogAppliesUnderLeases(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	src := &fakeSource{b: []byte(`[{"id":"a","url":"https://github.com/a","installation_id":7,"tiers":["x"]}]`)}
	c := &tenantCatalog{src: src, set: l.set, leaser: lr, log: slog.New(slog.DiscardHandler),
		check: func([]config.Tenant) error { return nil }}
	if err := c.read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.runs()) != 0 {
		t.Fatal("served before its lease")
	}
	store.set(acct("a"))
	mustRenew(t, lr)
	l.started("a")
}

// join registers the shard, and says which when it may not.
func TestLeaserJoin(t *testing.T) {
	_, _, store, lr := leaserLab(t)
	if err := lr.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.joinErr = jobstore.ErrOwnerDiffers
	err := lr.join(context.Background())
	if !errors.Is(err, jobstore.ErrOwnerDiffers) || !strings.Contains(err.Error(), "s1") {
		t.Fatalf("join = %v", err)
	}
	lr.release(context.Background())
	if len(store.released) != 1 || store.released[0].ID != "s1" {
		t.Fatalf("released %v", store.released)
	}
}

// Two processes never share a shard id, even on one host.
func TestShardIDIsUnique(t *testing.T) {
	host, _ := os.Hostname()
	a, b := shardID(), shardID()
	if a == b || !strings.HasPrefix(a, host+"-") || !strings.HasPrefix(b, host+"-") {
		t.Fatalf("shard ids %q and %q, want two of host %q", a, b, host)
	}
}
