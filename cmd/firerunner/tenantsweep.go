package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// tenantSweep deletes what one tenant removed left, under the lease on
// its account: the tenant no shard was serving when it was removed (its
// shard crashed or was handed off, it was placed nowhere, or no daemon
// ran), whose run so never deleted it.
type tenantSweep struct {
	removed jobstore.Removed
	tenant  config.Tenant
	// cancel stops it once started; nil until then.
	cancel context.CancelFunc
}

// planSweeps is the assigner's: it reads the tenants removed whose
// leftovers are not deleted yet, and sweeps each that no shard can be
// deleting: one whose account no shard holds the lease of. One listed
// again, by its id or its account, is forgotten instead: the tenant there
// now serves that account's scale sets. holders are the accounts' lease
// holders as just read.
func (l *tenantLeaser) planSweeps(ctx context.Context, holders map[string]string) {
	if l.sweep == nil {
		return
	}
	removed, err := l.store.RemovedTenants(ctx)
	if err != nil {
		l.log.Warn("removed tenants not read; their leftovers are swept later", "err", err)
		return
	}
	l.mu.Lock()
	ids, accounts := map[string]bool{}, map[string]bool{}
	for _, c := range l.list {
		ids[c.ID], accounts[config.AccountOf(c.URL)] = true, true
	}
	var forget []jobstore.Removed
	for _, r := range removed {
		var c config.Tenant
		if err := json.Unmarshal(r.Entry, &c); err != nil || config.AccountOf(c.URL) == "" {
			l.log.Warn("removed tenant's entry does not read: delete its scale sets and machines by hand", "tenant", r.ID, "err", err)
			forget = append(forget, r)
			continue
		}
		account := config.AccountOf(c.URL)
		switch {
		case ids[r.ID] || accounts[account]:
			forget = append(forget, r)
		case l.sweeps[account] != nil || holders[account] != "":
			// Being swept here, or held: its shard is removing it, or a
			// sweep elsewhere is.
		default:
			if l.sweeps == nil {
				l.sweeps = map[string]*tenantSweep{}
			}
			l.sweeps[account] = &tenantSweep{removed: r, tenant: c}
			l.log.Info("tenant removed while no shard served it: deleting what it left once this shard holds its account's lease", "tenant", r.ID, "account", account)
		}
	}
	l.mu.Unlock()
	for _, r := range forget {
		if err := l.store.ForgetRemoved(ctx, r.ID, r.RemovedAt); err != nil {
			l.log.Warn("removed tenant not forgotten", "tenant", r.ID, "err", err)
		}
	}
}

// sweepAccounts are the accounts this shard sweeps, whose leases it asks
// for. The caller holds mu.
func (l *tenantLeaser) sweepAccountsLocked() []string {
	var accounts []string
	for a := range l.sweeps {
		accounts = append(accounts, a)
	}
	return accounts
}

// runSweepsLocked starts each sweep whose account's lease this shard now
// holds, and stops each started whose lease it no longer holds: another
// shard may serve that account. A sweep stopped is planned again later.
// The caller holds mu.
func (l *tenantLeaser) runSweepsLocked(ctx context.Context) {
	for account, s := range l.sweeps {
		_, held := l.held[account]
		switch {
		case s.cancel == nil && held:
			sctx, cancel := context.WithCancel(ctx)
			s.cancel = cancel
			l.sweepWG.Go(func() {
				defer cancel()
				l.sweep(sctx, s.tenant)
				done := sctx.Err() == nil
				if done {
					fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), leaseRenewTimeout)
					if err := l.store.ForgetRemoved(fctx, s.removed.ID, s.removed.RemovedAt); err != nil {
						l.log.Warn("removed tenant swept but not forgotten: swept again later", "tenant", s.removed.ID, "err", err)
					}
					fcancel()
					l.log.Info("deleted what a tenant removed while no shard served it left", "tenant", s.removed.ID, "account", account)
				}
				l.mu.Lock()
				if l.sweeps[account] == s {
					delete(l.sweeps, account)
				}
				l.mu.Unlock()
			})
		case s.cancel != nil && !held:
			l.log.Warn("lost the lease of a removed tenant's account while deleting what it left: stopping; swept again later", "tenant", s.removed.ID, "account", account)
			s.cancel()
			delete(l.sweeps, account)
		}
	}
}

// sweepingLocked returns the ids of the tenants this shard sweeps. The
// caller holds mu.
func (l *tenantLeaser) sweepingLocked() []string {
	var ids []string
	for _, s := range l.sweeps {
		ids = append(ids, s.removed.ID)
	}
	return ids
}

// forgetRemovedTimeout bounds forgetting a tenant's removal once its run
// has deleted what it left.
const forgetRemovedTimeout = 5 * time.Second
