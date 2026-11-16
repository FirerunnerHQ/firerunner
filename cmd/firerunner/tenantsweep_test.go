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

// sweepLab is a leaser that assigns, with a list of tenant a, and a sweep
// that records each tenant it is given and returns once the test lets it,
// or its context ends.
type sweepLab struct {
	*testing.T
	l       *setLab
	store   *fakeLeases
	lr      *tenantLeaser
	started chan string
	ended   chan error
	finish  chan struct{}
}

func newSweepLab(t *testing.T) *sweepLab {
	l, _, store, lr := leaserLab(t)
	sl := &sweepLab{T: t, l: l, store: store, lr: lr,
		started: make(chan string, 4), ended: make(chan error, 4), finish: make(chan struct{})}
	lr.sweep = func(ctx context.Context, c config.Tenant) {
		sl.started <- c.ID
		select {
		case <-sl.finish:
			sl.ended <- nil
		case <-ctx.Done():
			sl.ended <- ctx.Err()
		}
	}
	lr.joined = true
	store.shards = live("s1")
	store.set(jobstore.AssignerLease, acct("a"))
	lr.apply([]config.Tenant{listed("a", 2, "x")})
	t.Cleanup(func() {
		select {
		case <-sl.finish:
		default:
			close(sl.finish)
		}
		lr.sweepWG.Wait()
	})
	return sl
}

// removed adds a tenant to those removed, as the tenants table's trigger
// does.
func (sl *sweepLab) removed(id string) jobstore.Removed {
	entry, _ := json.Marshal(listed(id, 2, "x", "y"))
	r := jobstore.Removed{ID: id, Entry: entry, RemovedAt: time.Unix(1_800_000_000, 0)}
	sl.store.mu.Lock()
	sl.store.removed = append(sl.store.removed, r)
	sl.store.mu.Unlock()
	return r
}

func (sl *sweepLab) step() {
	sl.Helper()
	if err := sl.lr.step(context.Background()); err != nil {
		sl.Fatal(err)
	}
}

func (sl *sweepLab) sweepStarted(want string) {
	sl.Helper()
	select {
	case id := <-sl.started:
		if id != want {
			sl.Fatalf("swept %s, want %s", id, want)
		}
	case <-time.After(5 * time.Second):
		sl.Fatalf("%s never swept", want)
	}
}

func (sl *sweepLab) sweepEnded(want error) {
	sl.Helper()
	select {
	case err := <-sl.ended:
		if !errors.Is(err, want) {
			sl.Fatalf("sweep ended with %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		sl.Fatal("sweep never ended")
	}
}

func (sl *sweepLab) forgotten() []string {
	sl.lr.sweepWG.Wait()
	sl.store.mu.Lock()
	defer sl.store.mu.Unlock()
	return slices.Clone(sl.store.forgotten)
}

// A tenant removed while no shard held its account's lease is swept by
// the assigner, under that lease, which it asks for and holds throughout;
// then its removal is forgotten.
func TestLeaserSweepsATenantRemovedWhileUnserved(t *testing.T) {
	sl := newSweepLab(t)
	sl.removed("gone")
	sl.store.grant = append(sl.store.grant, acct("gone"))
	sl.step() // plans the sweep, and asks for the lease at once (moved: a)
	sl.step()
	sl.sweepStarted("gone")
	if st := sl.lr.status(); !slices.Equal(st.Sweeping, []string{"gone"}) {
		t.Fatalf("status %+v, want gone being swept", st)
	}
	sl.step() // a renewal while it runs keeps it and starts no other
	if got := sl.store.lastAsked(); !slices.Contains(got, acct("gone")) {
		t.Fatalf("asked for %v while sweeping, want gone's account", got)
	}
	close(sl.finish)
	sl.sweepEnded(nil)
	if got := sl.forgotten(); !slices.Equal(got, []string{"gone"}) {
		t.Fatalf("forgotten %v, want gone", got)
	}
	sl.step()
	if got := sl.store.lastAsked(); slices.Contains(got, acct("gone")) {
		t.Fatalf("asked for %v once swept, want gone's account no more", got)
	}
}

// One whose account a shard holds is left to it: that shard is removing
// it, or serves the account. One listed again is forgotten, not swept.
func TestLeaserSweepsNothingHeldOrListedAgain(t *testing.T) {
	sl := newSweepLab(t)
	sl.removed("held")
	sl.removed("a") // listed again under its id
	sl.store.holders = map[string]string{acct("held"): "s2"}
	sl.store.grant = append(sl.store.grant, acct("held"))
	sl.step()
	sl.step()
	select {
	case id := <-sl.started:
		t.Fatalf("swept %s", id)
	default:
	}
	if got := sl.forgotten(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("forgotten %v, want a alone", got)
	}
	if st := sl.lr.status(); len(st.Sweeping) != 0 {
		t.Fatalf("status %+v, want nothing swept", st)
	}

	// No holder read, but the lease not free: planned, asked for, and not
	// started until it is held.
	sl.removed("busy")
	sl.step()
	sl.step()
	sl.lr.mu.Lock()
	s := sl.lr.sweeps[acct("busy")]
	sl.lr.mu.Unlock()
	if s == nil || s.cancel != nil {
		t.Fatalf("sweep %+v, want it planned and not started without the lease", s)
	}
	if got := sl.store.lastAsked(); !slices.Contains(got, acct("busy")) {
		t.Fatalf("asked for %v, want busy's account", got)
	}
}

// A sweep stops once this shard no longer holds the account's lease, or
// cannot renew to know, and its removal stays to sweep again.
func TestLeaserSweepStopsWithItsLease(t *testing.T) {
	for _, how := range []string{"taken", "renewal failed"} {
		sl := newSweepLab(t)
		sl.removed("gone")
		sl.store.grant = append(sl.store.grant, acct("gone"))
		sl.step()
		sl.step()
		sl.sweepStarted("gone")
		if how == "taken" {
			sl.store.mu.Lock()
			sl.store.grant = slices.DeleteFunc(sl.store.grant, func(a string) bool { return a == acct("gone") })
			sl.store.holders = map[string]string{acct("gone"): "s2"} // whoever took it
			sl.store.mu.Unlock()
			sl.step()
		} else {
			sl.store.fail(errors.New("connection refused"))
			_ = sl.lr.step(context.Background())
		}
		sl.sweepEnded(context.Canceled)
		if got := sl.forgotten(); len(got) != 0 {
			t.Fatalf("%s: forgotten %v, want the removal kept", how, got)
		}
		if st := sl.lr.status(); len(st.Sweeping) != 0 {
			t.Fatalf("%s: status %+v, want no sweep", how, st)
		}
	}
}
