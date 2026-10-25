package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/health"
)

type fakeScaleSet struct {
	stats    Stats
	at       time.Time // when the statistics' message arrived
	ok       bool
	noLive   bool // no live session to recycle
	recycles []string
}

func (f *fakeScaleSet) Stats() (Stats, time.Time, bool) { return f.stats, f.at, f.ok }
func (f *fakeScaleSet) Recycle(reason string) bool {
	if f.noLive {
		return false
	}
	f.recycles = append(f.recycles, reason)
	return true
}

type fakeRunners struct {
	idle      []string
	busy      map[string]bool
	cancelled []string
	running   int
	tracked   map[string]bool
}

func (f *fakeRunners) IdleRunners(time.Duration) []string { return f.idle }
func (f *fakeRunners) Running() int                       { return f.running }
func (f *fakeRunners) Tracks(name string) bool            { return f.tracked[name] }
func (f *fakeRunners) CancelLost(name string) bool {
	if f.busy[name] {
		return false
	}
	f.cancelled = append(f.cancelled, name)
	return true
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time                        { return c.t }
func (c *clock) advance(d time.Duration)               { c.t = c.t.Add(d) }
func newClock() *clock                                 { return &clock{t: time.Unix(10_000, 0)} }
func lookupNone(context.Context, string) (bool, error) { return false, nil }

func newRec(ss *fakeScaleSet, rn *fakeRunners, lookup Lookup, c *clock) (*Reconciler, *health.Tracker) {
	tr := health.NewTracker("t")
	r := New(ss, rn, lookup, Options{Health: tr})
	r.now = c.now
	return r, tr
}

func TestUnacquiredJobsRecycleTheSession(t *testing.T) {
	ss := &fakeScaleSet{ok: true, stats: Stats{Available: 2}}
	c := newClock()
	r, tr := newRec(ss, &fakeRunners{}, nil, c)

	r.Check(context.Background())
	c.advance(30 * time.Second)
	r.Check(context.Background())
	if len(ss.recycles) != 0 {
		t.Fatalf("recycled after 30s: %v", ss.recycles)
	}
	c.advance(31 * time.Second) // available for >= 60s
	r.Check(context.Background())
	if len(ss.recycles) != 1 || !strings.Contains(ss.recycles[0], "2 job(s) available but unacquired") {
		t.Fatalf("recycles = %v, want one", ss.recycles)
	}
	s := tr.Snapshot()
	if s.Recycles != 1 || !strings.Contains(s.Drift, "unacquired") || s.GitHub == nil || s.GitHub.Available != 2 {
		t.Fatalf("health = %+v", s)
	}
	// Still stuck: no second recycle within RecycleEvery.
	c.advance(time.Minute)
	r.Check(context.Background())
	if len(ss.recycles) != 1 {
		t.Fatalf("recycled again within 5m: %v", ss.recycles)
	}
	c.advance(5 * time.Minute)
	r.Check(context.Background())
	if len(ss.recycles) != 2 {
		t.Fatalf("recycles = %d after 5m more, want 2", len(ss.recycles))
	}
}

func TestAvailableThatClearsDoesNotRecycle(t *testing.T) {
	// Jobs available briefly (acquired by the next message) are normal.
	ss := &fakeScaleSet{ok: true, stats: Stats{Available: 1}}
	c := newClock()
	r, tr := newRec(ss, &fakeRunners{}, nil, c)
	r.Check(context.Background())
	c.advance(40 * time.Second)
	ss.stats.Available = 0
	r.Check(context.Background())
	c.advance(40 * time.Second)
	ss.stats.Available = 1 // a new job; its clock restarts
	r.Check(context.Background())
	c.advance(40 * time.Second)
	r.Check(context.Background())
	if len(ss.recycles) != 0 || tr.Snapshot().Drift != "" {
		t.Fatalf("recycles %v drift %q for jobs that kept being acquired", ss.recycles, tr.Snapshot().Drift)
	}
}

func TestLostRunnersAreCancelled(t *testing.T) {
	ss := &fakeScaleSet{ok: true}
	rn := &fakeRunners{idle: []string{"gone", "known", "busy-gone"}, busy: map[string]bool{"busy-gone": true}}
	lookup := func(_ context.Context, name string) (bool, error) {
		return name == "known", nil
	}
	c := newClock()
	r, tr := newRec(ss, rn, lookup, c)
	r.Check(context.Background())
	if len(rn.cancelled) != 1 || rn.cancelled[0] != "gone" {
		t.Fatalf("cancelled %v, want only the idle runner GitHub forgot", rn.cancelled)
	}
	if tr.Snapshot().LostRunners != 1 {
		t.Fatalf("lost runners = %d", tr.Snapshot().LostRunners)
	}
	// Lookups are paced: no second pass within LostEvery.
	rn.idle = []string{"gone2"}
	c.advance(10 * time.Second)
	r.Check(context.Background())
	if len(rn.cancelled) != 1 {
		t.Fatalf("looked up again within LostEvery: %v", rn.cancelled)
	}
	c.advance(time.Minute)
	r.Check(context.Background())
	if len(rn.cancelled) != 2 {
		t.Fatalf("cancelled %v after LostEvery", rn.cancelled)
	}
}

func TestLookupErrorsCancelNothing(t *testing.T) {
	rn := &fakeRunners{idle: []string{"a"}}
	r, _ := newRec(&fakeScaleSet{ok: true}, rn, func(context.Context, string) (bool, error) {
		return false, errors.New("503")
	}, newClock())
	r.Check(context.Background())
	if len(rn.cancelled) != 0 {
		t.Fatalf("cancelled %v on a failed lookup", rn.cancelled)
	}
}

func TestOverRegisteredIsReportedAfterGrace(t *testing.T) {
	c := newClock()
	ss := &fakeScaleSet{ok: true, stats: Stats{Registered: 5}, at: c.t}
	rn := &fakeRunners{running: 2}
	r, tr := newRec(ss, rn, lookupNone, c)
	r.Check(context.Background())
	if tr.Snapshot().Drift != "" {
		t.Fatalf("reported before the grace: %q", tr.Snapshot().Drift)
	}
	// No new message for 10 minutes: the stale statistics prove nothing.
	c.advance(10 * time.Minute)
	r.Check(context.Background())
	if tr.Snapshot().Drift != "" {
		t.Fatalf("reported from stale statistics: %q", tr.Snapshot().Drift)
	}
	// A message 3 minutes after the first still shows the gap.
	ss.at = ss.at.Add(3 * time.Minute)
	r.Check(context.Background())
	d := tr.Snapshot().Drift
	if !strings.Contains(d, "GitHub lists 5 registered runner(s), firerunner tracks 2") {
		t.Fatalf("drift = %q", d)
	}
	if len(ss.recycles) != 0 || len(rn.cancelled) != 0 {
		t.Fatal("over-registration must only be reported")
	}
	// Agreement clears it.
	ss.stats.Registered = 2
	r.Check(context.Background())
	if d := tr.Snapshot().Drift; d != "" {
		t.Fatalf("drift after agreement = %q", d)
	}
}

func TestNoStatsNoAction(t *testing.T) {
	rn := &fakeRunners{idle: []string{"a"}}
	r, tr := newRec(&fakeScaleSet{ok: false}, rn, lookupNone, newClock())
	r.Check(context.Background())
	if len(rn.cancelled) != 0 || tr.Snapshot().GitHub != nil {
		t.Fatal("acted before any statistics arrived")
	}
}

func TestRecycleWithNoLiveSessionIsRetried(t *testing.T) {
	// While the listener reconnects there is no session to recycle: that is
	// not counted, and does not hold off the next attempt.
	ss := &fakeScaleSet{ok: true, stats: Stats{Available: 1}, noLive: true}
	c := newClock()
	r, tr := newRec(ss, &fakeRunners{}, nil, c)
	r.Check(context.Background())
	c.advance(61 * time.Second)
	r.Check(context.Background())
	if tr.Snapshot().Recycles != 0 {
		t.Fatalf("counted %d recycles that did nothing", tr.Snapshot().Recycles)
	}
	ss.noLive = false
	c.advance(10 * time.Second)
	r.Check(context.Background())
	if len(ss.recycles) != 1 || tr.Snapshot().Recycles != 1 {
		t.Fatalf("recycles = %v (counted %d), want the retry at once", ss.recycles, tr.Snapshot().Recycles)
	}
}

type fakeOffline struct {
	names     []string
	err       error
	removeErr map[string]error
	removed   []string
}

func (f *fakeOffline) Offline(context.Context) ([]string, error) { return f.names, f.err }
func (f *fakeOffline) Remove(_ context.Context, name string) error {
	if err := f.removeErr[name]; err != nil {
		return err
	}
	f.removed = append(f.removed, name)
	return nil
}

func newOfflineRec(off *fakeOffline, rn *fakeRunners, dry bool, c *clock) (*Reconciler, *health.Tracker) {
	tr := health.NewTracker("t")
	r := New(&fakeScaleSet{ok: true}, rn, nil, Options{Health: tr, Offline: off, OfflineDryRun: dry})
	r.now = c.now
	return r, tr
}

func TestOfflineRunnersRemovedOnTheSecondSighting(t *testing.T) {
	off := &fakeOffline{names: []string{"t-orphan", "t-booting", "t-live"}}
	rn := &fakeRunners{tracked: map[string]bool{"t-live": true}}
	c := newClock()
	r, tr := newOfflineRec(off, rn, false, c)

	r.Check(context.Background())
	if len(off.removed) != 0 {
		t.Fatalf("removed %v on the first sighting", off.removed)
	}
	// Within the interval: no new sweep.
	c.advance(time.Minute)
	r.Check(context.Background())
	if len(off.removed) != 0 {
		t.Fatalf("swept again within OfflineEvery: %v", off.removed)
	}
	// Next sweep: the booting runner has connected (no longer offline), the
	// orphan is still there; the tracked one is never touched.
	off.names = []string{"t-orphan", "t-live"}
	c.advance(5 * time.Minute)
	r.Check(context.Background())
	if fmt.Sprint(off.removed) != "[t-orphan]" {
		t.Fatalf("removed %v, want only the orphan", off.removed)
	}
	if tr.Snapshot().OfflineRemoved != 1 {
		t.Fatalf("health counted %d", tr.Snapshot().OfflineRemoved)
	}
	// A runner that becomes tracked between sightings is kept.
	off.names = []string{"t-new"}
	c.advance(5 * time.Minute)
	r.Check(context.Background())
	rn.tracked["t-new"] = true
	c.advance(5 * time.Minute)
	r.Check(context.Background())
	if len(off.removed) != 1 {
		t.Fatalf("removed %v: a runner tracked by now must stay", off.removed)
	}
}

func TestOfflineDryRunAndErrors(t *testing.T) {
	off := &fakeOffline{names: []string{"t-a"}}
	c := newClock()
	r, _ := newOfflineRec(off, &fakeRunners{}, true, c)
	for range 3 {
		r.Check(context.Background())
		c.advance(5 * time.Minute)
	}
	if len(off.removed) != 0 {
		t.Fatalf("dry run removed %v", off.removed)
	}

	// A failed removal is retried next sweep; a failed listing forgets nothing.
	off = &fakeOffline{names: []string{"t-a"}, removeErr: map[string]error{"t-a": errors.New("503")}}
	r, _ = newOfflineRec(off, &fakeRunners{}, false, c)
	r.Check(context.Background())
	c.advance(5 * time.Minute)
	r.Check(context.Background()) // remove fails
	off.removeErr = nil
	off.err = errors.New("listing down")
	c.advance(5 * time.Minute)
	r.Check(context.Background()) // listing fails
	off.err = nil
	c.advance(5 * time.Minute)
	r.Check(context.Background())
	if fmt.Sprint(off.removed) != "[t-a]" {
		t.Fatalf("removed %v, want the retry to succeed", off.removed)
	}
}
