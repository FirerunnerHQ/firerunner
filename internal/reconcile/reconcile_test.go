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
	attempts  int
	listed    int
}

func (f *fakeOffline) Offline(context.Context) ([]string, error) { f.listed++; return f.names, f.err }
func (f *fakeOffline) Remove(_ context.Context, name string) error {
	f.attempts++
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

func TestOfflineRunnerRemovedAfterAnInterval(t *testing.T) {
	off := &fakeOffline{names: []string{"t-orphan", "t-booting", "t-live"}}
	rn := &fakeRunners{tracked: map[string]bool{"t-live": true}}
	c := newClock()
	r, tr := newOfflineRec(off, rn, false, c)
	ctx := context.Background()

	r.SweepOffline(ctx)
	c.advance(4 * time.Minute)
	r.SweepOffline(ctx)
	if len(off.removed) != 0 {
		t.Fatalf("removed %v before an interval had passed", off.removed)
	}
	// The booting runner has connected; the orphan is still offline.
	off.names = []string{"t-orphan", "t-live"}
	c.advance(time.Minute)
	r.SweepOffline(ctx)
	if fmt.Sprint(off.removed) != "[t-orphan]" {
		t.Fatalf("removed %v, want only the orphan", off.removed)
	}
	if tr.Snapshot().OfflineRemoved != 1 {
		t.Fatalf("health counted %d", tr.Snapshot().OfflineRemoved)
	}
	// A runner tracked by the next look is kept.
	off.names = []string{"t-new"}
	r.SweepOffline(ctx)
	rn.tracked["t-new"] = true
	c.advance(10 * time.Minute)
	r.SweepOffline(ctx)
	if len(off.removed) != 1 {
		t.Fatalf("removed %v: a runner tracked by now must stay", off.removed)
	}
}

func TestOfflineSightingSurvivesAMissedListing(t *testing.T) {
	// Runners come and go between a listing's pages: one sweep missing the
	// orphan must not restart its clock.
	off := &fakeOffline{names: []string{"t-orphan"}}
	c := newClock()
	r, _ := newOfflineRec(off, &fakeRunners{}, false, c)
	ctx := context.Background()
	r.SweepOffline(ctx)
	off.names = nil
	c.advance(3 * time.Minute)
	r.SweepOffline(ctx)
	off.names = []string{"t-orphan"}
	c.advance(3 * time.Minute)
	r.SweepOffline(ctx)
	if fmt.Sprint(off.removed) != "[t-orphan]" {
		t.Fatalf("removed %v after 6m offline with one listing missing it", off.removed)
	}
	// Gone for over two intervals: forgotten, and its clock starts afresh.
	off.removed, off.names = nil, []string{"t-b"}
	r.SweepOffline(ctx)
	off.names = nil
	c.advance(11 * time.Minute)
	r.SweepOffline(ctx)
	off.names = []string{"t-b"}
	r.SweepOffline(ctx)
	if len(off.removed) != 0 {
		t.Fatalf("removed %v on a sighting after the old one expired", off.removed)
	}
}

func TestOfflineForeignDryRunAndErrors(t *testing.T) {
	ctx := context.Background()
	// Another scale set's runner: asked once, then left alone.
	off := &fakeOffline{names: []string{"t-x"}, removeErr: map[string]error{"t-x": ErrNotOurs}}
	c := newClock()
	r, _ := newOfflineRec(off, &fakeRunners{}, false, c)
	for range 4 {
		r.SweepOffline(ctx)
		c.advance(5 * time.Minute)
	}
	if off.attempts != 1 {
		t.Fatalf("asked %d times about another scale set's runner, want 1", off.attempts)
	}

	// Dry run removes nothing and reports the count.
	off = &fakeOffline{names: []string{"t-a", "t-b"}}
	r, tr := newOfflineRec(off, &fakeRunners{}, true, c)
	r.SweepOffline(ctx)
	c.advance(5 * time.Minute)
	r.SweepOffline(ctx)
	if len(off.removed) != 0 || tr.Snapshot().OfflineWouldRemove != 2 {
		t.Fatalf("dry run: removed %v, would %d", off.removed, tr.Snapshot().OfflineWouldRemove)
	}

	// A failed removal or listing is retried.
	off = &fakeOffline{names: []string{"t-a"}, removeErr: map[string]error{"t-a": errors.New("503")}}
	r, _ = newOfflineRec(off, &fakeRunners{}, false, c)
	r.SweepOffline(ctx)
	c.advance(5 * time.Minute)
	r.SweepOffline(ctx) // remove fails
	off.removeErr, off.err = nil, errors.New("listing down")
	c.advance(5 * time.Minute)
	r.SweepOffline(ctx) // listing fails
	off.err = nil
	c.advance(5 * time.Minute)
	r.SweepOffline(ctx)
	if fmt.Sprint(off.removed) != "[t-a]" {
		t.Fatalf("removed %v, want the retry to succeed", off.removed)
	}
}

func TestCheckDoesNotSweep(t *testing.T) {
	// The sweep has its own loop: a slow listing never holds up a check.
	off := &fakeOffline{names: []string{"t-a"}}
	r, _ := newOfflineRec(off, &fakeRunners{}, false, newClock())
	r.Check(context.Background())
	if off.listed != 0 {
		t.Fatalf("Check listed runners %d time(s)", off.listed)
	}
}
