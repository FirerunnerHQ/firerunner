package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

// removedRecord is tenant id as the job store keeps it once removed.
func removedRecord(id string) jobstore.Removed {
	entry, _ := json.Marshal(listed(id, 2, "x", "y"))
	return jobstore.Removed{ID: id, Entry: entry, RemovedAt: time.Unix(1_800_000_000, 0)}
}

// gated makes the lab's removals wait until the returned func is called,
// or their context ends, and report which.
func gated(l *setLab) (release func(), ended chan error) {
	gate, ended := make(chan struct{}), make(chan error, 4)
	l.mu.Lock()
	l.removing = func(ctx context.Context) error {
		l.mu.Lock()
		l.removeCtx = ctx
		l.mu.Unlock()
		select {
		case <-gate:
			ended <- nil
			return nil
		case <-ctx.Done():
			err := context.Cause(ctx)
			ended <- err
			return err
		}
	}
	l.mu.Unlock()
	return func() { close(gate) }, ended
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

// A sweep deletes what the tenant left under the lease's deadline, and
// forgets its removal once all of it is gone; a run on its account listed
// again waits for it to end.
func TestTenantSetSweep(t *testing.T) {
	l, c := leaseLab(t)
	release, ended := gated(l)
	gone := listed("gone", 2, "x")
	if !l.set.sweep(gone, time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second)) {
		t.Fatal("sweep refused")
	}
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })
	if got := l.set.sweeping(); !slices.Equal(got, []string{"gone"}) {
		t.Fatalf("sweeping %v", got)
	}
	if got := l.set.removing(); !slices.Equal(got, []string{acct("gone")}) {
		t.Fatalf("removing %v, want gone's account: its lease is kept", got)
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
	l.started("gone")
	if _, forgot := l.removals(); !slices.Equal(forgot, []string{"gone"}) {
		t.Fatalf("forgot %v, want gone", forgot)
	}
	if got := l.set.sweeping(); len(got) != 0 {
		t.Fatalf("sweeping %v once done", got)
	}
	// Refused while its account is served here.
	if l.set.sweep(gone, time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second)) {
		t.Fatal("swept an account served here")
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
	if !l.set.sweep(old, time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second)) {
		t.Fatal("sweep refused")
	}
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
// fails is not forgotten either.
func TestTenantSetSweepUnderItsLease(t *testing.T) {
	l, c := leaseLab(t)
	release, ended := gated(l)
	defer release()
	l.set.sweep(listed("gone", 2, "x"), time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })

	c.advance(15 * time.Second)
	l.set.renewRemoving(map[string]time.Time{acct("gone"): c.now().Add(20 * time.Second)})
	c.advance(10 * time.Second) // past the first deadline, not the renewed one
	l.mu.Lock()
	rctx := l.removeCtx
	l.mu.Unlock()
	if err := context.Cause(rctx); err != nil {
		t.Fatalf("removal ended at a deadline renewed: %v", err)
	}
	c.advance(10 * time.Second)
	waitErr(t, ended, errLeaseLapsed)
	l.set.wg.Wait()
	if _, forgot := l.removals(); len(forgot) != 0 {
		t.Fatalf("forgot %v after the lease lapsed", forgot)
	}

	// A renewal after the deadline revives nothing.
	l.set.renewRemoving(map[string]time.Time{acct("gone"): c.now().Add(20 * time.Second)})

	l.mu.Lock()
	l.removing, l.removeErr = nil, errors.New("502")
	l.mu.Unlock()
	l.set.sweep(listed("other", 2, "x"), time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second))
	l.set.wg.Wait()
	if _, forgot := l.removals(); len(forgot) != 0 {
		t.Fatalf("forgot %v after a removal failed", forgot)
	}
}

// A removing run whose deadline has passed is not revived by a renewal.
func TestRenewRemovingRevivesNothingLapsed(t *testing.T) {
	l, c := leaseLab(t)
	release, _ := gated(l)
	defer release()
	l.set.sweep(listed("gone", 2, "x"), time.Unix(1_800_000_000, 0), c.now().Add(20*time.Second))
	waitFor(t, "the sweep's removal", func() bool { r, _ := l.removals(); return len(r) == 1 })
	c.mu.Lock()
	c.t = c.t.Add(21 * time.Second) // past it, its timer not fired yet
	c.mu.Unlock()
	l.set.renewRemoving(map[string]time.Time{acct("gone"): c.now().Add(20 * time.Second)})
	l.set.mu.Lock()
	defer l.set.mu.Unlock()
	for _, r := range l.set.ending {
		if !r.expired(c.now()) {
			t.Fatal("a lapsed removal was revived")
		}
	}
}

// The assigner plans to sweep a removed tenant whose account no shard
// holds, asks for its lease, and hands it to the tenant set once held. It
// plans none whose account is held, drops a plan once the removal is
// forgotten or the account taken, and plans nothing once it no longer
// assigns.
func TestLeaserPlansSweeps(t *testing.T) {
	l, _, store, lr := leaserLab(t)
	lr.joined = true
	store.shards = live("s1")
	store.set(jobstore.AssignerLease)
	lr.apply(nil)
	store.removed = []jobstore.Removed{removedRecord("gone"), removedRecord("held"), removedRecord("busy")}
	store.holders = map[string]string{acct("held"): "s2"}
	step := func() {
		t.Helper()
		if err := lr.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	step()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"busy", "gone"}) {
		t.Fatalf("planned %v, want busy and gone", st.SweepsPlanned)
	}
	store.grant = append(store.grant, acct("gone")) // busy's lease is not free
	step()
	if got := store.lastAsked(); !slices.Contains(got, acct("gone")) || !slices.Contains(got, acct("busy")) {
		t.Fatalf("asked for %v, want both planned accounts", got)
	}
	waitFor(t, "gone's sweep", func() bool { r, _ := l.removals(); return len(r) == 1 })
	l.set.wg.Wait()
	if removed, forgot := l.removals(); removed[0] != "gone x,y" || !slices.Equal(forgot, []string{"gone"}) {
		t.Fatalf("removed %v, forgot %v", removed, forgot)
	}
	store.mu.Lock() // as forgetting it in the job store does
	store.removed = slices.DeleteFunc(store.removed, func(r jobstore.Removed) bool { return r.ID == "gone" })
	store.mu.Unlock()
	if st := lr.status(); !slices.Equal(st.SweepsPlanned, []string{"busy"}) {
		t.Fatalf("planned %v, want busy alone", st.SweepsPlanned)
	}

	// busy's account taken by another shard: its plan is dropped.
	store.holders[acct("busy")] = "s2"
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 {
		t.Fatalf("planned %v once busy's account was held elsewhere", st.SweepsPlanned)
	}

	// A plan whose removal was forgotten meanwhile is dropped.
	delete(store.holders, acct("busy"))
	step()
	store.mu.Lock()
	store.removed = nil
	store.mu.Unlock()
	step()
	if st := lr.status(); len(st.SweepsPlanned) != 0 {
		t.Fatalf("planned %v once the removal was forgotten", st.SweepsPlanned)
	}

	// No longer assigning: what it planned is dropped, and not asked for.
	store.mu.Lock()
	store.removed = []jobstore.Removed{removedRecord("late")}
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
