package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
)

// leaseClock is a clock the test moves, with timers that fire as it does.
type leaseClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*leaseTimer
}

type leaseTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newLeaseClock() *leaseClock { return &leaseClock{t: time.Unix(1_800_000_000, 0)} }

func (c *leaseClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *leaseClock) after(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &leaseTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, tm)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// advance moves the clock by d, and fires each timer due, outside the lock.
func (c *leaseClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []func()
	for _, tm := range c.timers {
		if !tm.stopped && !tm.at.After(c.t) {
			tm.stopped = true
			due = append(due, tm.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// armed is how many timers are set and not stopped.
func (c *leaseClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tm := range c.timers {
		if !tm.stopped {
			n++
		}
	}
	return n
}

func leaseLab(t *testing.T) (*setLab, *leaseClock) {
	l := newSetLab(t)
	c := newLeaseClock()
	l.set.now, l.set.afterFunc = c.now, c.after
	return l, c
}

// A shard serves the tenants it holds the lease of, and no other. One whose
// lease another shard takes is handed off: its scale sets stay, and nothing
// it registered is deleted. Leased again, it starts anew, only once the old
// run has ended.
func TestTenantSetServesOnlyWhatItLeases(t *testing.T) {
	l, c := leaseLab(t)
	list := []config.Tenant{listed("a", 2, "x"), listed("b", 2, "x")}
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.started("a")
	if _, ok := l.runs()["b"]; ok {
		t.Fatal("b is served without its lease")
	}

	l.set.applyLeases(list, map[string]time.Time{"b": c.now().Add(20 * time.Second)})
	l.ended("a", errTenantHandedOff)
	l.started("b")
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second), "b": c.now().Add(20 * time.Second)})
	select {
	case s := <-l.starts:
		t.Fatalf("%s started before a's last run ended", s.id)
	default:
	}
	l.finish("a")
	l.started("a")
	l.noOverlap()
	l.mu.Lock()
	removed := slices.Clone(l.removed)
	l.mu.Unlock()
	if len(removed) != 0 {
		t.Fatalf("a hand-off deleted what %v registered", removed)
	}

	// Removed from the list: removed, whether leased or not.
	l.set.applyLeases(list[:1], map[string]time.Time{"a": c.now().Add(20 * time.Second), "b": c.now().Add(20 * time.Second)})
	l.ended("b", errTenantRemoved)
}

// A lease not renewed by its deadline hands the tenant off when the deadline
// passes, with nothing else running: as when the database stops answering.
// A renewal moves the deadline, and a run past its deadline is never
// revived by a renewal that comes late.
func TestTenantSetHandsOffAtTheDeadline(t *testing.T) {
	l, c := leaseLab(t)
	list := []config.Tenant{listed("a", 2, "x")}
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.started("a")
	before := l.runs()

	c.advance(10 * time.Second)
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.unchanged(before)
	if n := c.armed(); n != 1 {
		t.Fatalf("%d timers armed, want the renewal's alone", n)
	}
	c.advance(15 * time.Second) // past the first deadline, before the second
	l.unchanged(before)

	c.advance(5 * time.Second) // the renewed deadline
	l.ended("a", errTenantHandedOff)
	if _, ok := l.runs()["a"]; ok {
		t.Fatal("a still served past its deadline")
	}

	// A renewal after the deadline starts a new run, after the old one.
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.finish("a")
	l.started("a")
	l.noOverlap()

	// A deadline already past serves nothing.
	l.set.applyLeases(nil, nil)
	l.ended("a", errTenantRemoved)
	l.finish("a")
	l.set.applyLeases(list, map[string]time.Time{"a": c.now()})
	if _, ok := l.runs()["a"]; ok {
		t.Fatal("a served under a lease already over")
	}
}

// A run past its deadline whose timer has not fired yet is handed off by
// the next apply, renewed or not.
func TestTenantSetApplyHandsOffARunPastItsDeadline(t *testing.T) {
	l, c := leaseLab(t)
	list := []config.Tenant{listed("a", 2, "x")}
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.started("a")
	c.mu.Lock()
	c.t = c.t.Add(21 * time.Second) // the timer has not fired
	c.mu.Unlock()
	l.set.applyLeases(list, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.ended("a", errTenantHandedOff)
	l.finish("a")
	l.started("a")
}

// The tenant's launches are fenced by its deadline from the moment it is
// served: a tier asks, and is held once the deadline has passed.
func TestTenantSetFencesLaunchesByTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newLeaseClock()
	bound := make(chan *scheduler.Tenant, 1)
	set := &tenantSet{ctx: ctx, log: slog.New(slog.DiscardHandler), setMax: func(string, int) {},
		now: c.now, afterFunc: c.after,
		run: func(ctx context.Context, cur func() config.Tenant, served func(*servedTenant)) {
			st := &servedTenant{bound: scheduler.NewTenant("a", 0)}
			served(st)
			bound <- st.bound
			<-ctx.Done()
		}}
	defer set.wait()
	defer cancel()
	set.applyLeases([]config.Tenant{listed("a", 2, "x")}, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	b := <-bound

	launched := make(chan string, 4)
	sched := scheduler.New(scheduler.Options{Max: 2, Provisioner: causeProv{launched, make(chan struct{}), make(chan error, 4)},
		JIT: oneJIT{}, Logger: slog.New(slog.DiscardHandler), Tenant: b})
	sched.Reconcile(ctx, 1)
	<-launched

	c.mu.Lock()
	c.t = c.t.Add(20 * time.Second) // past the deadline; the timer not fired yet
	c.mu.Unlock()
	sched.Reconcile(ctx, 2)
	n, h := sched.Holding()
	if n != 1 || h.Kind != scheduler.HoldFenced || !strings.Contains(h.Why, `lease on tenant "a" was not renewed`) {
		t.Fatalf("held %d %+v", n, h)
	}
}
