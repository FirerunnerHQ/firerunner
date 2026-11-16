package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

const (
	// forgetRemovedTimeout bounds forgetting a tenant's removal once what
	// it left is deleted.
	forgetRemovedTimeout = 5 * time.Second
	// sweepRetryMin and sweepRetryMax bound the wait before a sweep that
	// failed is tried again, doubling between.
	sweepRetryMin = time.Minute
	sweepRetryMax = time.Hour
	// sweepStuck is how many failures in a row doctor warns at.
	sweepStuck = 3
)

// sweepKey is one removal: a tenant id, and when its removal was recorded.
type sweepKey struct {
	id string
	at time.Time
}

// sweepFailure is how sweeping one removal has gone wrong so far.
type sweepFailure struct {
	attempts int
	next     time.Time
	err      string
}

// plannedSweep is a tenant removed while no shard served it, whose
// account's lease the assigner asks for, so that it may sweep it.
type plannedSweep struct {
	removed jobstore.Removed
	tenant  config.Tenant
}

// removedTenants reads the removals not yet done, with each one's entry.
// One whose entry does not read is forgotten, with an error to delete what
// it left by hand: nothing can sweep it.
func (l *tenantLeaser) removedTenants(ctx context.Context) ([]plannedSweep, error) {
	ctx, cancel := context.WithTimeout(ctx, leaseRenewTimeout)
	defer cancel()
	removed, err := l.store.RemovedTenants(ctx)
	if err != nil {
		return nil, err
	}
	var out []plannedSweep
	for _, r := range removed {
		var c config.Tenant
		if err := json.Unmarshal(r.Entry, &c); err != nil || config.AccountOf(c.URL) == "" || c.ID != r.ID {
			l.log.Error("removed tenant's entry does not read: delete what it left by hand", "tenant", r.ID, "removed_at", r.RemovedAt)
			if err := l.store.ForgetRemoved(ctx, r.ID, r.RemovedAt, c.URL); err != nil {
				l.log.Warn("removed tenant not forgotten", "tenant", r.ID, "err", err)
			}
			continue
		}
		out = append(out, plannedSweep{removed: r, tenant: c})
	}
	return out, nil
}

// due reports whether a removal may be swept now: one that failed waits.
// The caller holds mu.
func (l *tenantLeaser) dueLocked(k sweepKey) bool {
	f, ok := l.sweepFailures[k]
	return !ok || !l.clock().Before(f.next)
}

// sweepHeld is every shard's, after each renewal: it sweeps each removal
// due on an account it holds the lease of, beside whatever it runs there
// (tenantSet.sweep), so that one whose removal failed, or lost its lease,
// here or elsewhere, is swept again even while its account is served.
func (l *tenantLeaser) sweepHeld(ctx context.Context) {
	removed, err := l.removedTenants(ctx)
	if err != nil {
		l.log.Warn("removed tenants not read; their leftovers are swept later", "err", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range removed {
		account := config.AccountOf(p.tenant.URL)
		d, held := l.held[account]
		k := sweepKey{p.removed.ID, p.removed.RemovedAt}
		if !held || !l.dueLocked(k) {
			continue
		}
		if l.sweeps[account] != nil && l.sweeps[account].removed.ID == p.removed.ID {
			delete(l.sweeps, account) // planned for this: done here now
		}
		l.set.sweep(p.tenant, p.removed.RemovedAt, d, func(err error) { l.swept(k, err) })
	}
}

// swept records how a sweep ended: one that failed waits before the next,
// longer each time.
func (l *tenantLeaser) swept(k sweepKey, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.sweepFailures, k)
		return
	}
	if l.sweepFailures == nil {
		l.sweepFailures = map[sweepKey]*sweepFailure{}
	}
	f := l.sweepFailures[k]
	if f == nil {
		f = &sweepFailure{}
		l.sweepFailures[k] = f
	}
	f.attempts++
	wait := sweepRetryMin << min(f.attempts-1, 6)
	f.next, f.err = l.clock().Add(min(wait, sweepRetryMax)), err.Error()
}

// planSweeps is the assigner's. It plans to sweep each removal due whose
// account no shard holds the lease of, nor is being assigned (next): it
// asks for that account's lease, and once it holds it sweeps it
// (sweepHeld). An account held, or assigned, is swept by its holder.
// Plans it no longer holds true of are dropped: the removal done or the
// id listed again, or the account held elsewhere.
//
// It decides from the database alone, never from this shard's list, which
// may be older.
func (l *tenantLeaser) planSweeps(ctx context.Context, holders map[string]string, next map[string]jobstore.Assignment) {
	removed, err := l.removedTenants(ctx)
	if err != nil {
		l.log.Warn("removed tenants not read; their leftovers are swept later", "err", err)
		return
	}
	current := map[sweepKey]bool{}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range removed {
		k := sweepKey{p.removed.ID, p.removed.RemovedAt}
		current[k] = true
		account := config.AccountOf(p.tenant.URL)
		if _, assigned := next[account]; l.sweeps[account] != nil || holders[account] != "" || assigned || !l.dueLocked(k) {
			continue
		}
		if l.sweeps == nil {
			l.sweeps = map[string]*plannedSweep{}
		}
		l.sweeps[account] = &p
		l.log.Info("tenant removed while no shard served it: deleting what it left once this shard holds its account's lease", "tenant", p.removed.ID, "account", account)
	}
	for account, p := range l.sweeps {
		_, assigned := next[account]
		if !current[sweepKey{p.removed.ID, p.removed.RemovedAt}] || assigned || (holders[account] != "" && holders[account] != l.shard.ID) {
			delete(l.sweeps, account)
		}
	}
	for k := range l.sweepFailures {
		if !current[k] {
			delete(l.sweepFailures, k)
		}
	}
}

// sweepAccountsLocked are the accounts of the sweeps planned, whose leases
// this shard asks for. The caller holds mu.
func (l *tenantLeaser) sweepAccountsLocked() []string {
	var accounts []string
	for a := range l.sweeps {
		accounts = append(accounts, a)
	}
	return accounts
}

// plannedLocked returns the ids of the sweeps planned. The caller holds mu.
func (l *tenantLeaser) plannedLocked() []string {
	var ids []string
	for _, p := range l.sweeps {
		ids = append(ids, p.removed.ID)
	}
	return ids
}

// failingLocked describes each removal whose sweeps keep failing, and
// reports whether any has failed sweepStuck times. The caller holds mu.
func (l *tenantLeaser) failingLocked() (failing []string, stuck bool) {
	for k, f := range l.sweepFailures {
		failing = append(failing, fmt.Sprintf("%s: %d attempt(s), next in %s: %s", k.id, f.attempts,
			max(0, f.next.Sub(l.clock())).Truncate(time.Second), f.err))
		stuck = stuck || f.attempts >= sweepStuck
	}
	return failing, stuck
}
