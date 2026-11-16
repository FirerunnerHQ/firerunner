package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	// leaseReleaseTimeout bounds releasing them all on the way out.
	leaseReleaseTimeout = 10 * time.Second
)

// leaseStore is where the shards and their leases are kept; a
// *jobstore.TenantStore.
type leaseStore interface {
	Join(ctx context.Context, sh jobstore.Shard) error
	Renew(ctx context.Context, sh jobstore.Shard, accounts []string, ttl time.Duration) ([]string, error)
	Release(ctx context.Context, sh jobstore.Shard) error
}

// shardID is an id for this process among the shards: its host name and a
// random part, so that no two processes share one, even on one host. A
// process started again is a new shard: its leases from before are taken
// again only once they expire, which GitHub's hold on the old sessions
// outlasts after a crash anyway.
func shardID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "firerunner"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return host + "-" + hex.EncodeToString(b)
}

// tenantLeaser serves the tenants list read by a tenantCatalog under
// leases: each tenant only while this shard holds the lease on its GitHub
// account. It renews them every leaseInterval, and applies the list with a
// deadline per tenant leased: leaseServe after the renewal was sent. A
// renewal that fails changes nothing: the tenantSet hands each tenant off
// at its deadline, by itself.
//
// A lease is on an account, not a tenant id: two runs on one account would
// share its scale sets. It asks for the account of each tenant listed, and
// of each tenant removed whose run still drains, until that run has deleted
// its scale sets.
type tenantLeaser struct {
	store leaseStore
	shard jobstore.Shard
	set   *tenantSet
	log   *slog.Logger
	// now and tick replace the clock and the interval's ticker in tests.
	now  func() time.Time
	tick <-chan time.Time

	mu     sync.Mutex
	list   []config.Tenant
	listed bool
	// held is the deadline of each account leased at the last renewal that
	// succeeded.
	held        map[string]time.Time
	lastRenewed time.Time
	lastErr     string
	lastErrAt   time.Time
}

func (l *tenantLeaser) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// join registers the shard. It fails while a live shard has another owner.
func (l *tenantLeaser) join(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, tenantsReadTimeout)
	defer cancel()
	if err := l.store.Join(ctx, l.shard); err != nil {
		return fmt.Errorf("join the shards as %s: %w", l.shard.ID, err)
	}
	l.log.Info("joined the shards: serving tenants under leases", "shard", l.shard.ID)
	return nil
}

// apply is the catalog's: it applies a list read and checked, under the
// leases held.
func (l *tenantLeaser) apply(list []config.Tenant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list, l.listed = list, true
	l.applyLocked()
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

// renew renews the leases once and applies the list under them. Before a
// list has been read it only beats the heartbeat.
func (l *tenantLeaser) renew(ctx context.Context) error {
	l.mu.Lock()
	var accounts []string
	for _, c := range l.list {
		accounts = append(accounts, config.AccountOf(c.URL))
	}
	l.mu.Unlock()
	accounts = append(accounts, l.set.removing()...)
	slices.Sort(accounts)
	accounts = slices.Compact(accounts)

	sent := l.clock()
	rctx, cancel := context.WithTimeout(ctx, leaseRenewTimeout)
	got, err := l.store.Renew(rctx, l.shard, accounts, leaseTTL)
	cancel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.lastErr, l.lastErrAt = err.Error(), time.Now()
		l.log.Warn("tenant leases not renewed: each tenant is handed off at its deadline unless a renewal succeeds first", "shard", l.shard.ID, "err", err)
		return err
	}
	l.lastRenewed = time.Now()
	deadline := sent.Add(leaseServe)
	l.held = make(map[string]time.Time, len(got))
	for _, a := range got {
		l.held[a] = deadline
	}
	if l.listed {
		l.applyLocked()
	}
	return nil
}

// run renews the leases every leaseInterval until ctx ends. It does not
// renew at once: call renew first.
func (l *tenantLeaser) run(ctx context.Context) {
	tick := l.tick
	if tick == nil {
		t := time.NewTicker(leaseInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			_ = l.renew(ctx)
		}
	}
}

// release lets every lease this shard holds expire now, for another shard
// to take at once. Call it once the tenants have stopped: their sessions
// are closed.
func (l *tenantLeaser) release(ctx context.Context) {
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
	return &health.TenantLeases{Shard: l.shard.ID, Tenants: n, Listed: len(l.list),
		LastRenewed: l.lastRenewed, LastError: l.lastErr, LastErrorAt: l.lastErrAt}
}
