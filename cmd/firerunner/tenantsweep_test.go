package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// removedRecord is tenant id as the job store keeps it once removed.
func removedRecord(id string, tiers ...string) jobstore.Removed {
	if len(tiers) == 0 {
		tiers = []string{"x", "y"}
	}
	entry, _ := json.Marshal(listed(id, 2, tiers...))
	return jobstore.Removed{ID: id, Entry: entry, RemovedAt: time.Unix(1_800_000_000, 0)}
}

// gated makes the lab's removals wait until the returned func is called,
// or their lease lapses, and report which.
func gated(l *setLab) (release func(), ended chan error) {
	gate, ended := make(chan struct{}), make(chan error, 4)
	l.mu.Lock()
	l.removing = func(ctx context.Context) error {
		l.mu.Lock()
		l.removeCtx = ctx
		l.mu.Unlock()
		lapse := leaseLapse(ctx)
		if lapse == nil {
			lapse = context.Background()
		}
		select {
		case <-gate:
			ended <- nil
			return nil
		case <-lapse.Done():
			err := context.Cause(lapse)
			ended <- err
			return err
		}
	}
	l.mu.Unlock()
	return sync1(func() { close(gate) }), ended
}

// sync1 runs f once, however often it is called.
func sync1(f func()) func() {
	done := false
	return func() {
		if !done {
			done = true
			f()
		}
	}
}

func (l *setLab) removals() (removed, forgot []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.removed), slices.Clone(l.forgot)
}

func waitErr(t *testing.T, ch chan error, want error) {
	t.Helper()
	select {
	case err := <-ch:
		if !errors.Is(err, want) {
			t.Fatalf("removal ended with %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("removal never ended")
	}
}

// mustSweep starts a sweep, and returns where its outcome lands.
func mustSweep(t *testing.T, l *setLab, c config.Tenant, deadline time.Time) chan error {
	t.Helper()
	out := make(chan error, 1)
	if !l.set.sweep(c, time.Unix(1_800_000_000, 0), deadline, func(err error) { out <- err }) {
		t.Fatalf("sweep of %s refused", c.ID)
	}
	return out
}

// A sweep deletes what the tenant left under the lease's deadline, and
// forgets its removal once all of it is gone; a run on its account listed
// again waits for it to end.
func TestTenantSetSweep(t *testing.T) {
	l, c := leaseLab(t)
	release, ended := gated(l)
	gone := listed("gone", 2, "x")
	out := mustSweep(t, l, gone, c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })
	if got := l.set.sweeping(); !slices.Equal(got, []string{"gone"}) {
		t.Fatalf("sweeping %v", got)
	}
	if got := l.set.removing(); !slices.Equal(got, []string{acct("gone")}) {
		t.Fatalf("removing %v, want gone's account: its lease is kept", got)
	}
	if l.set.sweep(gone, time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second), nil) {
		t.Fatal("swept the same removal twice at once")
	}

	// Listed again on its account: waits for the sweep.
	l.set.applyLeases([]config.Tenant{gone}, map[string]time.Time{"gone": c.now().Add(20 * time.Second)})
	select {
	case s := <-l.starts:
		t.Fatalf("%s started beside the sweep of its account", s.id)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	waitErr(t, ended, nil)
	waitErr(t, out, nil)
	l.started("gone")
	if _, forgot := l.removals(); !slices.Equal(forgot, []string{"gone"}) {
		t.Fatalf("forgot %v, want gone", forgot)
	}
	if got := l.set.sweeping(); len(got) != 0 {
		t.Fatalf("sweeping %v once done", got)
	}
	// Refused while its id is served here again.
	if l.set.sweep(gone, time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second), nil) {
		t.Fatal("swept a tenant served here")
	}
}

// Beside a run that serves its account here, a sweep goes on, and deletes
// none of the scale sets that run took over by its tiers' names.
func TestTenantSetSweepBesideARunOnItsAccount(t *testing.T) {
	l, c := leaseLab(t)
	l.set.applyLeases([]config.Tenant{listed("a", 2, "x")}, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.started("a")
	old := listed("a", 2, "x", "y")
	old.ID = "old"
	out := mustSweep(t, l, old, c.now().Add(20*time.Second))
	waitErr(t, out, nil)
	if r, _ := l.removals(); !slices.Equal(r, []string{"old y"}) {
		t.Fatalf("removed %v, want old's y alone: x is a's", r)
	}
	if _, ok := l.runs()["a"]; !ok {
		t.Fatal("a stopped for the sweep")
	}
}

// A sweep waits for a removal on its account still under way here: the
// two would delete by the same names.
func TestTenantSetSweepWaitsForTheAccountsRemoval(t *testing.T) {
	l, c := leaseLab(t)
	l.set.applyLeases([]config.Tenant{listed("a", 2, "x")}, map[string]time.Time{"a": c.now().Add(20 * time.Second)})
	l.started("a")
	l.set.applyLeases(nil, map[string]time.Time{})
	l.ended("a", errTenantRemoved) // draining: its runners not finished
	old := listed("a", 2, "y")
	old.ID = "old"
	l.set.renewRemoving(map[string]time.Time{acct("a"): c.now().Add(20 * time.Second)})
	mustSweep(t, l, old, c.now().Add(20*time.Second))
	for range 20 {
		if r, _ := l.removals(); len(r) != 0 {
			t.Fatalf("removed %v while a's run drains", r)
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.finish("a")
	l.set.wg.Wait()
	if r, _ := l.removals(); !slices.Equal(r, []string{"a x", "old y"}) {
		t.Fatalf("removed %v, want a's, then old's", r)
	}
}

// A removal whose lease lapses stops at once, with errLeaseLapsed, and is
// not forgotten; a renewal before the deadline lets it go on. One that
// fails is not forgotten either, and says why.
func TestTenantSetSweepUnderItsLease(t *testing.T) {
	l, c := leaseLab(t)
	release, ended := gated(l)
	defer release()
	out := mustSweep(t, l, listed("gone", 2, "x"), c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })

	c.advance(15 * time.Second)
	l.set.renewRemoving(map[string]time.Time{acct("gone"): c.now().Add(20 * time.Second)})
	c.advance(10 * time.Second) // past the first deadline, not the renewed one
	l.mu.Lock()
	rctx := l.removeCtx
	l.mu.Unlock()
	if err := leaseLapse(rctx).Err(); err != nil {
		t.Fatalf("removal's lease lapsed at a deadline renewed: %v", err)
	}
	c.advance(10 * time.Second)
	waitErr(t, ended, errLeaseLapsed)
	waitErr(t, out, errLeaseLapsed)
	if _, forgot := l.removals(); len(forgot) != 0 {
		t.Fatalf("forgot %v after the lease lapsed", forgot)
	}

	l.mu.Lock()
	l.removing, l.removeErr = nil, errors.New("502")
	l.mu.Unlock()
	out = mustSweep(t, l, listed("other", 2, "x"), c.now().Add(20*time.Second))
	if err := <-out; err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("sweep ended with %v, want the 502", err)
	}
	if r, forgot := l.removals(); !slices.Contains(r, "other x") || len(forgot) != 0 {
		t.Fatalf("removed %v, forgot %v: want other tried and not forgotten", r, forgot)
	}
}

// As the daemon stops, nothing renews leases any more: a removal under way
// still stops once its lease lapses.
func TestTenantSetRemovalStopsAtTheLapseWhileStopping(t *testing.T) {
	l, c := leaseLab(t)
	release, ended := gated(l)
	defer release()
	out := mustSweep(t, l, listed("gone", 2, "x"), c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })
	l.stop() // the daemon stopping
	c.advance(20 * time.Second)
	waitErr(t, ended, errLeaseLapsed)
	waitErr(t, out, errLeaseLapsed)
}

// A removing run whose deadline has passed is not revived by a renewal.
func TestRenewRemovingRevivesNothingLapsed(t *testing.T) {
	l, c := leaseLab(t)
	release, _ := gated(l)
	defer release()
	mustSweep(t, l, listed("gone", 2, "x"), c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })
	c.mu.Lock()
	c.t = c.t.Add(21 * time.Second) // past it, its timer not fired yet
	c.mu.Unlock()
	l.set.renewRemoving(map[string]time.Time{acct("gone"): c.now().Add(20 * time.Second)})
	l.set.mu.Lock()
	defer l.set.mu.Unlock()
	if len(l.set.ending) != 1 {
		t.Fatalf("%d runs ending, want the sweep", len(l.set.ending))
	}
	if !l.set.ending[0].expired(c.now()) {
		t.Fatal("a lapsed removal was revived")
	}
}

// Before each EC2 sweep by a removed tenant's owner tag: not once its lease
// has lapsed, nor once the id is listed again anywhere, whose run's reaper
// then owns what is left; and not when that cannot be read.
func TestMayEndRemoved(t *testing.T) {
	listedIDs := map[string]bool{"back": true}
	var readErr error
	s := &shard{tenantListed: func(_ context.Context, id string) (bool, error) { return listedIDs[id], readErr }}
	lapse, lapsed := context.WithCancel(context.Background())
	ctx := context.Background()
	if err := s.mayEndRemoved(lapse, "gone")(ctx); err != nil {
		t.Fatalf("gone: %v", err)
	}
	if err := s.mayEndRemoved(lapse, "back")(ctx); !errors.Is(err, errListedAgain) {
		t.Fatalf("back: %v, want errListedAgain", err)
	}
	readErr = errors.New("connection refused")
	if err := s.mayEndRemoved(lapse, "gone")(ctx); !errors.Is(err, readErr) {
		t.Fatalf("unread: %v, want the read's error", err)
	}
	lapsed()
	if err := s.mayEndRemoved(lapse, "gone")(ctx); !errors.Is(err, errLeaseLapsed) {
		t.Fatalf("lapsed: %v, want errLeaseLapsed", err)
	}
	if err := (&shard{}).mayEndRemoved(nil, "gone")(ctx); err != nil {
		t.Fatalf("no lease, no store: %v", err)
	}
}

// sweepLab is a leaser that assigns, over a set whose removals the test
// sees.
func sweepLab(t *testing.T) (*setLab, *fakeLeases, *tenantLeaser, func()) {
	l, _, store, lr := leaserLab(t)
	lr.joined = true
	store.shards = live("s1")
	store.set(jobstore.AssignerLease)
	lr.apply(nil)
	return l, store, lr, func() {
		t.Helper()
		if err := lr.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// The assigner plans to sweep a removal whose account no shard holds nor
// is being assigned, asks for its lease, and sweeps it once held. It plans
// none whose account is held or assigned, drops a plan once the removal is
// done or the account taken, and plans nothing once it no longer assigns.
func TestLeaserPlansSweeps(t *testing.T) {
	l, store, lr, step := sweepLab(t)
	store.removed = []jobstore.Removed{removedRecord("gone"), removedRecord("held"), removedRecord("busy"), removedRecord("next")}
	store.holders = map[string]string{acct("held"): "s2"}
	store.assignments = placed{} // next's account is assigned by this step
	lr.apply([]config.Tenant{listed("next", 2, "z")})
	step()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"busy", "gone"}) {
		t.Fatalf("planned %v, want busy and gone", st.SweepsPlanned)
	}
	store.grant = append(store.grant, acct("gone")) // busy's lease is not free
	step()
	if got := store.lastAsked(); !slices.Contains(got, acct("gone")) || !slices.Contains(got, acct("busy")) {
		t.Fatalf("asked for %v, want both planned accounts", got)
	}
	// Granted at once, in that renewal: swept in the same step.
	waitFor(t, "gone's sweep", func() bool { r, _ := l.removals(); return slices.Contains(r, "gone x,y") })
	l.set.wg.Wait()
	if _, forgot := l.removals(); !slices.Equal(forgot, []string{"gone"}) {
		t.Fatalf("forgot %v", forgot)
	}
	store.mu.Lock() // as forgetting it in the job store does
	store.removed = slices.DeleteFunc(store.removed, func(r jobstore.Removed) bool { return r.ID == "gone" })
	store.mu.Unlock()
	step()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"busy"}) {
		t.Fatalf("planned %v, want busy alone", st.SweepsPlanned)
	}

	// busy's account listed again, and so assigned: its holder sweeps.
	lr.apply([]config.Tenant{listed("next", 2, "z"), listed("busy", 2, "z")})
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 {
		t.Fatalf("planned %v once busy's account was assigned", st.SweepsPlanned)
	}
	lr.apply([]config.Tenant{listed("next", 2, "z")})
	step()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"busy"}) {
		t.Fatalf("planned %v, want busy again once unassigned", st.SweepsPlanned)
	}

	store.holders[acct("busy")] = "s2"
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 {
		t.Fatalf("planned %v once busy's account was held elsewhere", st.SweepsPlanned)
	}
	delete(store.holders, acct("busy"))
	step()
	store.mu.Lock()
	store.removed = slices.DeleteFunc(store.removed, func(r jobstore.Removed) bool { return r.ID == "busy" })
	store.mu.Unlock()
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 {
		t.Fatalf("planned %v once the removal was done", st.SweepsPlanned)
	}

	store.mu.Lock()
	store.removed = append(store.removed, removedRecord("late"))
	store.mu.Unlock()
	step()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"late"}) {
		t.Fatalf("planned %v, want late", st.SweepsPlanned)
	}
	store.set()
	step()
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 || slices.Contains(store.lastAsked(), acct("late")) {
		t.Fatalf("planned %v, asked %v, once not assigning", st.SweepsPlanned, store.lastAsked())
	}
}

// Any shard sweeps a removal on an account it holds, assigning or not; one
// that fails is tried again only after a wait that grows, and status and
// doctor say so once it keeps failing.
func TestLeaserSweepsWhatItHoldsAndBacksOff(t *testing.T) {
	l, c, store, lr := leaserLab(t)
	lr.joined = true
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	store.set(acct("a")) // not assigning
	store.removed = []jobstore.Removed{removedRecord("old")}
	entry, _ := json.Marshal(config.Tenant{ID: "old", URL: listed("a", 1).URL, InstallationID: 7, Tiers: []string{"x", "y"}})
	store.removed[0].Entry = entry
	l.mu.Lock()
	l.removeErr = errors.New("502")
	l.mu.Unlock()
	tries := func() int {
		r, _ := l.removals()
		n := 0
		for _, x := range r {
			if strings.HasPrefix(x, "old ") {
				n++
			}
		}
		return n
	}
	once := func() {
		t.Helper()
		if err := lr.step(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "no sweep under way", func() bool { return len(l.set.sweeping()) == 0 })
	}
	// pass lets d go by, renewing every 10s as the leaser does.
	pass := func(d time.Duration) {
		t.Helper()
		for ; d > 0; d -= leaseInterval {
			c.advance(leaseInterval)
			once()
		}
	}
	once()
	l.started("a")
	waitFor(t, "the first sweep", func() bool { return tries() == 1 })
	if r, _ := l.removals(); !slices.Contains(r, "old y") {
		t.Fatalf("removed %v, want old's y alone, beside a", r)
	}
	waitFor(t, "the failure recorded", func() bool { return len(lr.status().SweepsFailing) == 1 })
	pass(sweepRetryMin - leaseInterval)
	if n := tries(); n != 1 {
		t.Fatalf("%d tries before the wait was up", n)
	}
	for want := 2; want <= sweepStuck; want++ {
		pass(leaseInterval) // the wait is up
		waitFor(t, "a retry", func() bool { return tries() == want })
		pass(sweepRetryMin<<(want-1) - leaseInterval)
		if n := tries(); n != want {
			t.Fatalf("%d tries, want %d: the wait did not grow", n, want)
		}
	}
	if st := lr.status(); !st.SweepsStuck || !strings.Contains(st.SweepsFailing[0], "502") {
		t.Fatalf("status %+v, want old stuck, failing with the 502", st)
	}
	if _, ok := l.runs()["a"]; !ok {
		t.Fatal("a stopped meanwhile")
	}

	// Once it succeeds, it is forgotten, and no longer failing.
	l.mu.Lock()
	l.removeErr = nil
	l.mu.Unlock()
	pass(leaseInterval)
	waitFor(t, "the sweep that succeeds", func() bool { _, f := l.removals(); return len(f) == 1 })
	waitFor(t, "the failure cleared", func() bool { return len(lr.status().SweepsFailing) == 0 })
}
