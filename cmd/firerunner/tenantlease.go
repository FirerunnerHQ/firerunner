package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

const (
	// leaseInterval is how often a shard renews its leases.
	leaseInterval = 10 * time.Second
	// leaseTTL is how long a lease lasts past a renewal, on the database's
	// clock: another shard takes a tenant once it has passed.
	leaseTTL = 30 * time.Second
	// leaseServe is how long a shard serves a tenant past the moment it
	// sent the renewal that last succeeded, on its own monotonic clock.
	// Shorter than leaseTTL, so it stops before another may start, whatever
	// the clocks say.
	leaseServe = 20 * time.Second
	// leaseRenewTimeout bounds one renewal: one that ends after its
	// deadline would serve nothing.
	leaseRenewTimeout = 5 * time.Second
	// leaseRetry is how soon a renewal that failed is tried again: a
	// deadline is 20s after the last success was sent, so one blip must
	// not wait out the next interval and hand every tenant off.
	leaseRetry = 2 * time.Second
	// leaseReleaseTimeout bounds releasing them all on the way out.
	leaseReleaseTimeout = 10 * time.Second
)

// leaseStore is where the shards and their leases are kept; a
// *jobstore.TenantStore.
type leaseStore interface {
	Join(ctx context.Context, sh jobstore.Shard) error
	Renew(ctx context.Context, sh jobstore.Shard, want jobstore.Want, ttl time.Duration) (jobstore.Renewal, error)
	Release(ctx context.Context, sh jobstore.Shard) error
	Placement(ctx context.Context) ([]jobstore.ShardState, map[string]jobstore.Assignment, map[string]string, error)
	Assign(ctx context.Context, sh jobstore.Shard, next map[string]jobstore.Assignment) error
	Drain(ctx context.Context, sh jobstore.Shard) error
	RemovedTenants(ctx context.Context) ([]jobstore.Removed, error)
	ForgetRemoved(ctx context.Context, id string, removedAt time.Time) error
}

// newShard is this process among the shards. Its id is its host name and a
// random part, so that no two processes share one, even on one host. A
// process started again is a new shard, which replaces the one before it
// on its host: its leases from before are taken again only once they
// expire, which GitHub's hold on the old sessions outlasts after a crash
// anyway.
func newShard(owner, tenantOwner string) jobstore.Shard {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "firerunner"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return jobstore.Shard{ID: host + "-" + hex.EncodeToString(b), Host: host, Owner: owner, TenantOwner: tenantOwner}
}

// tenantLeaser serves the tenants list read by a tenantCatalog under
// leases: each tenant only while this shard holds the lease on its GitHub
// account. It renews them every leaseInterval, and applies the list with a
// deadline per tenant leased: leaseServe after the renewal was sent. A
// renewal that fails changes nothing: the tenantSet hands each tenant off
// at its deadline, by itself.
//
// A lease is on an account, not a tenant id: two runs on one account would
// share its scale sets. It asks for the accounts assigned to this shard
// that its list names; those it serves, until they are assigned to another
// shard; the account of each tenant removed whose run still drains, until
// that run has deleted its scale sets; and the assigner's lease. The shard
// that holds that one assigns every listed tenant's account to a shard
// (place) after each renewal.
type tenantLeaser struct {
	store leaseStore
	shard jobstore.Shard
	set   *tenantSet
	log   *slog.Logger
	// now and tick replace the clock and the interval's ticker in tests,
	// and after the wait before a retry.
	now   func() time.Time
	tick  <-chan time.Time
	after func(time.Duration) <-chan time.Time

	// sweep, when set, deletes what a removed tenant left: its scale sets
	// by name, and its machines by owner tag. The assigner runs it for a
	// tenant removed while no shard served it (planSweeps).
	sweep func(ctx context.Context, c config.Tenant)

	// joined is run's: whether the shard is registered.
	joined bool
	// sweepWG counts the sweeps running.
	sweepWG sync.WaitGroup

	mu     sync.Mutex
	list   []config.Tenant
	listed bool
	// held is the deadline of each account leased at the last renewal that
	// succeeded.
	held        map[string]time.Time
	lastRenewed time.Time
	lastErr     string
	lastErrAt   time.Time
	// listOK is whether the catalog's last read of the list applied.
	listOK bool
	// assignerID is the shard that held the assigner's lease at the last
	// renewal, and unlisted the accounts assigned here that this shard's
	// list does not name.
	assignerID string
	unlisted   []string
	// unplaced is why each tenant this shard, assigning, could not place
	// was not; assignErr is its last failure to assign.
	unplaced    map[string]string
	assignErr   string
	assignErrAt time.Time
	// sweeps are the removed tenants' leftovers this shard deletes, by
	// account.
	sweeps map[string]*tenantSweep
}

func (l *tenantLeaser) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// join registers the shard. It fails while a live shard's owners do not
// fit this one's.
func (l *tenantLeaser) join(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, tenantsReadTimeout)
	defer cancel()
	if err := l.store.Join(ctx, l.shard); err != nil {
		err = fmt.Errorf("join the shards as %s: %w", l.shard.ID, err)
		l.mu.Lock()
		l.lastErr, l.lastErrAt = err.Error(), time.Now()
		l.mu.Unlock()
		l.log.Error("not serving any tenant until this shard may join", "err", err)
		return err
	}
	l.log.Info("joined the shards: serving tenants under leases", "shard", l.shard.ID)
	return nil
}

// step joins, if the shard has not, and renews. Holding the assigner's
// lease, it then assigns, and renews again if that moved anything: a shard
// takes its new accounts at once, and lets go of those moved away.
func (l *tenantLeaser) step(ctx context.Context) error {
	if !l.joined {
		if err := l.join(ctx); err != nil {
			return err
		}
		l.joined = true
	}
	err := l.renew(ctx)
	if errors.Is(err, jobstore.ErrNotJoined) {
		l.joined = false
	}
	if err != nil {
		return err
	}
	moved, err := l.assign(ctx)
	if err != nil || !moved {
		return err
	}
	return l.renew(ctx)
}

// assign places the listed tenants on the shards, if this shard holds the
// assigner's lease and a list has been read, and reports whether any
// assignment changed.
func (l *tenantLeaser) assign(ctx context.Context) (bool, error) {
	l.mu.Lock()
	leader := l.assigningLocked()
	if !leader {
		l.unplaced, l.assignErr = nil, ""
	}
	places, listed := placesOf(l.list), l.listed
	l.mu.Unlock()
	if !leader || !listed {
		// Before a list, every tenant would read as removed.
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, leaseRenewTimeout)
	defer cancel()
	shards, current, holders, err := l.store.Placement(ctx)
	if err == nil {
		next, unplaced := place(places, shards, current, holders)
		l.mu.Lock()
		l.unplaced, l.assignErr = unplaced, ""
		l.mu.Unlock()
		l.planSweeps(ctx, holders)
		if maps.Equal(next, current) {
			return false, nil
		}
		if err = l.store.Assign(ctx, l.shard, next); err == nil {
			for _, account := range slices.Sorted(maps.Keys(next)) {
				if was, ok := current[account]; !ok || was.Shard != next[account].Shard {
					l.log.Info("assigned an account to a shard", "account", account, "shard", next[account].Shard, "from", was.Shard)
				}
			}
			return true, nil
		}
	}
	err = fmt.Errorf("assign the tenants: %w", err)
	l.mu.Lock()
	l.assignErr, l.assignErrAt = err.Error(), time.Now()
	l.mu.Unlock()
	l.log.Warn("tenants not assigned; trying again", "shard", l.shard.ID, "err", err)
	return false, err
}

// apply is the catalog's: it applies a list read and checked, under the
// leases held.
func (l *tenantLeaser) apply(list []config.Tenant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list, l.listed, l.listOK = list, true, true
	l.applyLocked()
}

// listFailed is the catalog's: its last read of the list did not apply.
// Until one does, the assigner puts no tenant on this shard, whose list may
// be older than the one it places.
func (l *tenantLeaser) listFailed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listOK = false
}

// assigningLocked reports whether this shard holds the assigner's lease,
// renewed and not past its deadline. The caller holds mu.
func (l *tenantLeaser) assigningLocked() bool {
	d, ok := l.held[jobstore.AssignerLease]
	return ok && l.clock().Before(d)
}

// applyLocked applies the list under the leases held. The caller holds mu.
func (l *tenantLeaser) applyLocked() {
	leases := map[string]time.Time{}
	for _, c := range l.list {
		if d, ok := l.held[config.AccountOf(c.URL)]; ok {
			leases[c.ID] = d
		}
	}
	l.set.applyLeases(l.list, leases)
}

// renew renews the leases once and applies the list under them.
func (l *tenantLeaser) renew(ctx context.Context) error {
	want := jobstore.Want{Accounts: append(l.set.removing(), jobstore.AssignerLease), Running: l.set.runningAccounts()}
	l.mu.Lock()
	for _, c := range l.list {
		want.Listed = append(want.Listed, config.AccountOf(c.URL))
	}
	want.ListOK = l.listOK
	want.Accounts = append(want.Accounts, l.sweepAccountsLocked()...)
	l.mu.Unlock()

	sent := l.clock()
	rctx, cancel := context.WithTimeout(ctx, leaseRenewTimeout)
	r, err := l.store.Renew(rctx, l.shard, want, leaseTTL)
	cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.lastErr, l.lastErrAt = err.Error(), time.Now()
		l.log.Warn("tenant leases not renewed: each tenant is handed off at its deadline unless a renewal succeeds first", "shard", l.shard.ID, "err", err)
		// A sweep may not outlive a lease it cannot confirm.
		for account, s := range l.sweeps {
			if s.cancel != nil {
				s.cancel()
				delete(l.sweeps, account)
			}
		}
		return err
	}
	l.lastRenewed = time.Now()
	deadline := sent.Add(leaseServe)
	l.held = make(map[string]time.Time, len(r.Held))
	for _, a := range r.Held {
		l.held[a] = deadline
	}
	l.assignerID, l.unlisted = r.Assigner, r.Unlisted
	l.runSweepsLocked(ctx)
	l.set.renewRemoving(l.held)
	if l.listed {
		l.applyLocked()
	}
	return nil
}

// run joins the shards and renews the leases at once, then every
// leaseInterval until ctx ends. A join or renewal that fails is tried again
// after leaseRetry, and a shard the database lost joins again. Until it has
// joined, it serves no tenant; the install's own tiers serve meanwhile.
func (l *tenantLeaser) run(ctx context.Context) {
	tick := l.tick
	if tick == nil {
		t := time.NewTicker(leaseInterval)
		defer t.Stop()
		tick = t.C
	}
	after := l.after
	if after == nil {
		after = time.After
	}
	for {
		for l.step(ctx) != nil {
			select {
			case <-ctx.Done():
				return
			case <-after(leaseRetry):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// drain marks the shard as stopping, so that the assigner moves its
// tenants to the other shards at once. They take each once this shard has
// released its lease, or it has expired.
func (l *tenantLeaser) drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	if err := l.store.Drain(ctx, l.shard); err != nil {
		l.log.Warn("shard not marked draining: its tenants move once its heartbeat is stale", "shard", l.shard.ID, "err", err)
	}
}

// release lets every lease this shard holds expire now, for another shard
// to take at once. Call it once the tenants have stopped: their sessions
// are closed.
func (l *tenantLeaser) release(ctx context.Context) {
	l.sweepWG.Wait() // stopped with the leases' renewals
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()
	if err := l.store.Release(ctx, l.shard); err != nil {
		l.log.Warn("tenant leases not released: other shards take them once they expire", "shard", l.shard.ID, "err", err)
		return
	}
	l.log.Info("released the tenant leases", "shard", l.shard.ID)
}

// status is how renewing the leases goes, for health.
func (l *tenantLeaser) status() *health.TenantLeases {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	n := 0
	for _, c := range l.list {
		if d, ok := l.held[config.AccountOf(c.URL)]; ok && now.Before(d) {
			n++
		}
	}
	st := &health.TenantLeases{Shard: l.shard.ID, Tenants: n, Listed: len(l.list),
		LastRenewed: l.lastRenewed, LastError: l.lastErr, LastErrorAt: l.lastErrAt,
		AssignerShard: l.assignerID, Unlisted: slices.Clone(l.unlisted)}
	st.Sweeping = l.sweepingLocked()
	slices.Sort(st.Sweeping)
	if l.assigningLocked() {
		st.Assigner = true
		for _, id := range slices.Sorted(maps.Keys(l.unplaced)) {
			st.Unplaced = append(st.Unplaced, id+": "+l.unplaced[id])
		}
		st.AssignError, st.AssignErrorAt = l.assignErr, l.assignErrAt
	}
	return st
}
