package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// fakeLeases is a lease store for shard s1 that decides what a renewal
// asks for as the real one does, grants what the test says of that, and
// records what it was asked.
type fakeLeases struct {
	mu sync.Mutex
	// grant is the accounts whose lease is free for s1: of those a renewal
	// asks for, it holds these. err, when set, fails renewals.
	grant []string
	err   error
	// holders is what Placement reads as the leases' holders.
	holders map[string]string
	// removed is what RemovedTenants reads; ForgetRemoved takes from it
	// and records in forgotten.
	removed   []jobstore.Removed
	forgotten []string
	// forgotByTime are the removals forgotten whatever their entry.
	forgotByTime []string
	// mine are the leases s1 holds as of its last renewal: Placement reads
	// them as held, as the store does.
	mine []string
	// wants are the renewals' asks.
	// shards and assignments are what Placement reads; Assign writes
	// assignments, and counts in assigns.
	shards      []jobstore.ShardState
	assignments map[string]jobstore.Assignment
	assigns     int
	assignErr   error
	drained     []string
	// errs fail the next renewals, one each, before err.
	errs []error
	// during runs inside each renewal, as a slow database would.
	during   func()
	wants    []jobstore.Want
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

// Renew asks, as the real store does, for want.Accounts, the accounts
// assigned to s1 that its list names, and those it runs that are assigned
// to no other shard.
func (f *fakeLeases) Renew(_ context.Context, sh jobstore.Shard, want jobstore.Want, ttl time.Duration) (jobstore.Renewal, error) {
	f.mu.Lock()
	during, err := f.during, f.err
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	f.wants = append(f.wants, want)
	f.asked = append(f.asked, slices.Clone(want.Accounts))
	var r jobstore.Renewal
	ask := slices.Clone(want.Accounts)
	for a, to := range f.assignments {
		switch {
		case to.Shard != sh.ID:
		case slices.Contains(want.Listed, a):
			ask = append(ask, a)
		default:
			r.Unlisted = append(r.Unlisted, a)
		}
	}
	for _, a := range want.Running {
		if to, ok := f.assignments[a]; !ok || to.Shard == sh.ID {
			ask = append(ask, a)
		}
	}
	for _, a := range ask {
		if slices.Contains(f.grant, a) && !slices.Contains(r.Held, a) {
			r.Held = append(r.Held, a)
		}
	}
	if slices.Contains(r.Held, jobstore.AssignerLease) {
		r.Assigner = sh.ID
	}
	if err == nil {
		f.mine = slices.Clone(r.Held)
	}
	f.mu.Unlock()
	if ttl != leaseTTL {
		return jobstore.Renewal{}, errors.New("asked for another ttl")
	}
	if during != nil {
		during()
	}
	if err != nil {
		return jobstore.Renewal{}, err
	}
	return r, nil
}

func (f *fakeLeases) Release(_ context.Context, sh jobstore.Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, sh)
	return nil
}

func (f *fakeLeases) Placement(context.Context) ([]jobstore.ShardState, map[string]jobstore.Assignment, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	holders := maps.Clone(f.holders)
	for _, a := range f.mine {
		if holders == nil {
			holders = map[string]string{}
		}
		if _, ok := holders[a]; !ok && a != jobstore.AssignerLease {
			holders[a] = "s1"
		}
	}
	return slices.Clone(f.shards), maps.Clone(f.assignments), holders, nil
}

func (f *fakeLeases) Assign(_ context.Context, _ jobstore.Shard, next map[string]jobstore.Assignment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assignErr != nil {
		return f.assignErr
	}
	f.assignments = maps.Clone(next)
	f.assigns++
	return nil
}

func (f *fakeLeases) RemovedTenants(context.Context) ([]jobstore.Removed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed), nil
}

func (f *fakeLeases) ForgetRemoved(_ context.Context, id string, upTo time.Time, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = slices.DeleteFunc(f.removed, func(r jobstore.Removed) bool {
		return r.ID == id && !r.RemovedAt.After(upTo)
	})
	if url == "" {
		f.forgotByTime = append(f.forgotByTime, id)
	}
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeLeases) Drain(_ context.Context, sh jobstore.Shard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drained = append(f.drained, sh.ID)
	return nil
}

// set makes grant the leases free for s1, and assigns each account of them
// to s1, and every other account away from it.
func (f *fakeLeases) set(grant ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grant, f.err = grant, nil
	if f.assignments == nil {
		f.assignments = map[string]jobstore.Assignment{}
	}
	for a, to := range f.assignments {
		if to.Shard == "s1" && !slices.Contains(grant, a) {
			f.assignments[a] = jobstore.Assignment{Shard: "s2", Tiers: to.Tiers}
		}
	}
	for _, a := range grant {
		if a != jobstore.AssignerLease {
			f.assignments[a] = jobstore.Assignment{Shard: "s1", Tiers: 1}
		}
	}
}

func (f *fakeLeases) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// counts is how many joins and renewals there have been.
func (f *fakeLeases) counts() (joins, renewals int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.joined), len(f.asked)
}

// lastAsked is the accounts the last renewal asked for beyond those
// assigned to it, but for the assigner's lease, which every one asks for.
func (f *fakeLeases) lastAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.asked) == 0 {
		return nil
	}
	last := f.asked[len(f.asked)-1]
	if !slices.Contains(last, jobstore.AssignerLease) {
		return []string{"(not the assigner's lease)"}
	}
	return slices.DeleteFunc(slices.Clone(last), func(a string) bool { return a == jobstore.AssignerLease })
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

// A shard serves the tenants whose accounts it holds the lease of, and
// hands a tenant off once another shard holds its lease. It asks for no
// listed account itself: those assigned to it are renewed as such.
func TestLeaserServesWhatItLeases(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	mustRenew(t, lr)
	if got := store.lastAsked(); len(got) != 0 {
		t.Fatalf("asked for %v", got)
	}

	list := []config.Tenant{listed("a", 2, "x"), listed("b", 2, "x")}
	lr.apply(list)
	if len(l.runs()) != 0 {
		t.Fatal("served before any lease")
	}
	store.set(acct("a"))
	mustRenew(t, lr)
	if got := store.lastAsked(); len(got) != 0 {
		t.Fatalf("asked for %v, want the assigned alone", got)
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

// A removed tenant deletes what it registered only while its lease holds:
// renewals that keep its account move its deadline on; once they fail past
// it, another shard may serve the account under the same scale set names,
// and it deletes nothing.
func TestLeaserRemovedTenantDeletesOnlyUnderItsLease(t *testing.T) {
	for _, renewed := range []bool{true, false} {
		l, c, store, lr := leaserLab(t)
		lr.apply([]config.Tenant{listed("a", 2, "x")})
		store.set(acct("a"))
		mustRenew(t, lr)
		l.started("a")

		lr.apply(nil)
		l.ended("a", errTenantRemoved)
		if !renewed {
			store.fail(errors.New("connection refused"))
		}
		for range 3 { // 30s of draining, renewed every 10s or not at all
			c.advance(10 * time.Second)
			_ = lr.renew(context.Background())
		}
		l.finish("a")
		l.set.wg.Wait()
		l.mu.Lock()
		removed := slices.Clone(l.removed)
		l.mu.Unlock()
		_, forgot := l.removals()
		if renewed && (len(removed) != 1 || !slices.Equal(forgot, []string{"a"})) {
			t.Fatalf("renewed throughout: deleted %v, forgot %v, want a's", removed, forgot)
		}
		if !renewed && (len(removed) != 0 || len(forgot) != 0) {
			t.Fatalf("lease lapsed: deleted %v, forgot %v, want nothing: it is swept again", removed, forgot)
		}
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

	// A list that does not apply tells the assigner, through the leaser,
	// to put nothing new here; the next that does lifts it.
	listOK := func() bool { lr.mu.Lock(); defer lr.mu.Unlock(); return lr.listOK }
	src.b = []byte(`[{"id":`)
	if err := c.read(context.Background()); err == nil || listOK() {
		t.Fatalf("read = %v, list ok %v after a bad list", err, listOK())
	}
	src.b = []byte(`[{"id":"a","url":"https://github.com/a","installation_id":7,"tiers":["x"]}]`)
	if err := c.read(context.Background()); err != nil || !listOK() {
		t.Fatalf("read = %v, list ok %v after a good list", err, listOK())
	}
}

// join registers the shard, and says which when it may not, in status too.
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
	if st := lr.status(); !st.Failing() || !strings.Contains(st.LastError, "owner") {
		t.Fatalf("status %+v, want the join's error", st)
	}
	lr.release(context.Background())
	if len(store.released) != 1 || store.released[0].ID != "s1" {
		t.Fatalf("released %v", store.released)
	}
}

// Two processes never share a shard id, even on one host.
func TestShardIDIsUnique(t *testing.T) {
	host, _ := os.Hostname()
	a, b := newShard("vm-1", "fr"), newShard("vm-1", "fr")
	if a.ID == b.ID || !strings.HasPrefix(a.ID, host+"-") || a.Host != host || a.Owner != "vm-1" || a.TenantOwner != "fr" {
		t.Fatalf("shards %+v and %+v, want two of host %q", a, b, host)
	}
}

// run joins and renews at once, and every tick after. A failure, of the
// join or a renewal, is tried again after leaseRetry rather than at the
// next tick: one blip must not let every tenant's deadline pass. A shard
// the database lost joins again.
func TestLeaserRunRetries(t *testing.T) {
	_, _, store, lr := leaserLab(t)
	tick, retry := make(chan time.Time), make(chan time.Time)
	lr.tick = tick
	waits := make(chan time.Duration)
	lr.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return retry
	}
	store.joinErr = errors.New("connection refused")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); lr.run(ctx) }()
	defer func() { cancel(); <-done }()

	retried := func(what string, wantJoins, wantRenewals int) {
		t.Helper()
		select {
		case d := <-waits:
			if d != leaseRetry {
				t.Fatalf("%s: waits %s, want %s", what, d, leaseRetry)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no retry", what)
		}
		if j, r := store.counts(); j != wantJoins || r != wantRenewals {
			t.Fatalf("%s: %d joins and %d renewals, want %d and %d", what, j, r, wantJoins, wantRenewals)
		}
	}
	retried("join refused", 1, 0)

	store.mu.Lock()
	store.joinErr = nil
	store.errs = []error{errors.New("timeout"), jobstore.ErrNotJoined}
	store.mu.Unlock()
	retry <- time.Time{}
	retried("renewal failed", 2, 1)
	retry <- time.Time{}
	retried("shard lost", 2, 2)
	retry <- time.Time{} // joins again, and renews
	waitFor(t, "a join and a renewal", func() bool { j, r := store.counts(); return j == 3 && r == 3 })
	tick <- time.Time{}
	waitFor(t, "the tick's renewal", func() bool { _, r := store.counts(); return r == 4 })
	if st := lr.status(); st.Failing() {
		t.Fatalf("status %+v after a renewal", st)
	}
}
