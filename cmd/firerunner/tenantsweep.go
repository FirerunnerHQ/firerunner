package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// forgetRemovedTimeout bounds forgetting a tenant's removal once what it
// left is deleted.
const forgetRemovedTimeout = 5 * time.Second

// plannedSweep is a tenant removed while no shard served it, whose
// account's lease this shard asks for: once it holds it, the tenant set
// deletes what the tenant left, as a removing run (tenantSet.sweep).
type plannedSweep struct {
	removed jobstore.Removed
	tenant  config.Tenant
}

// planSweeps is the assigner's. It reads the tenants removed whose
// leftovers are not deleted yet, and plans to sweep each whose account no
// shard holds the lease of: a shard that holds it is removing the tenant,
// sweeping it, or serving the account, and a later run there waits on the
// removal. holders are the leases' holders, as just read with the
// assignments. Plans it no longer holds true of are dropped: the removal
// forgotten (done elsewhere, or the id listed again), or the account held.
//
// It decides from the database alone, never from this shard's list,
// which may be older: a tenant listed again under its id is no longer
// removed there, and one listed again on its account waits, here or on
// the shard that holds the account, for a sweep to end.
func (l *tenantLeaser) planSweeps(ctx context.Context, holders map[string]string) {
	removed, err := l.store.RemovedTenants(ctx)
	if err != nil {
		l.log.Warn("removed tenants not read; their leftovers are swept later", "err", err)
		return
	}
	type key struct {
		id string
		at time.Time
	}
	current := map[key]bool{}
	var unreadable []jobstore.Removed
	l.mu.Lock()
	for _, r := range removed {
		current[key{r.ID, r.RemovedAt}] = true
		var c config.Tenant
		if err := json.Unmarshal(r.Entry, &c); err != nil || config.AccountOf(c.URL) == "" || c.ID != r.ID {
			unreadable = append(unreadable, r)
			continue
		}
		account := config.AccountOf(c.URL)
		if l.sweeps[account] != nil || holders[account] != "" {
			continue
		}
		if l.sweeps == nil {
			l.sweeps = map[string]*plannedSweep{}
		}
		l.sweeps[account] = &plannedSweep{removed: r, tenant: c}
		l.log.Info("tenant removed while no shard served it: deleting what it left once this shard holds its account's lease", "tenant", r.ID, "account", account)
	}
	for account, p := range l.sweeps {
		if !current[key{p.removed.ID, p.removed.RemovedAt}] || (holders[account] != "" && holders[account] != l.shard.ID) {
			delete(l.sweeps, account)
		}
	}
	l.mu.Unlock()
	for _, r := range unreadable {
		l.log.Error("removed tenant's entry does not read: delete what it left by hand, then forget it", "tenant", r.ID, "removed_at", r.RemovedAt)
		if err := l.store.ForgetRemoved(ctx, r.ID, r.RemovedAt); err != nil {
			l.log.Warn("removed tenant not forgotten", "tenant", r.ID, "err", err)
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

// startSweepsLocked hands each sweep planned whose account's lease this
// shard now holds to the tenant set, which deletes what the tenant left
// under that lease. One the set refuses, as when a run serves the account
// here now, is dropped and planned again if still due. The caller holds
// mu.
func (l *tenantLeaser) startSweepsLocked() {
	for account, p := range l.sweeps {
		d, held := l.held[account]
		if !held {
			continue
		}
		delete(l.sweeps, account)
		l.set.sweep(p.tenant, p.removed.RemovedAt, d)
	}
}

// plannedLocked returns the ids of the sweeps planned. The caller holds mu.
func (l *tenantLeaser) plannedLocked() []string {
	var ids []string
	for _, p := range l.sweeps {
		ids = append(ids, p.removed.ID)
	}
	return ids
}
