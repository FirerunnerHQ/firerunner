package listener

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/google/uuid"

	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/health"
	"github.com/solcreek/firerunner/internal/scheduler"
)

// fakeSession is one message session. While failing is set, GetMessage
// errors as the library does once its retries give up; otherwise it hands
// out queued messages, or an empty long poll.
type fakeSession struct {
	assigned int // initial TotalAssignedJobs
	failing  atomic.Bool
	msgs     chan *scaleset.RunnerScaleSetMessage
	closed   atomic.Bool
	gets     atomic.Int32
}

func newFakeSession(assigned int, failing bool) *fakeSession {
	f := &fakeSession{assigned: assigned, msgs: make(chan *scaleset.RunnerScaleSetMessage, 8)}
	f.failing.Store(failing)
	return f
}

func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{
		SessionID:  uuid.New(),
		Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: f.assigned},
	}
}

func (f *fakeSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	f.gets.Add(1)
	if f.failing.Load() {
		return nil, errors.New(`request GET https://pipelines/messages failed(status="503 Service Unavailable")`)
	}
	select {
	case m := <-f.msgs:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return nil, nil // empty long poll
	}
}

func (f *fakeSession) DeleteMessage(context.Context, int) error { return nil }
func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	return ids, nil
}
func (f *fakeSession) Close(context.Context) error { f.closed.Store(true); return nil }

// message builds a message whose statistics assign n jobs.
func message(id, n int) *scaleset.RunnerScaleSetMessage {
	return &scaleset.RunnerScaleSetMessage{MessageID: id, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: n}}
}

// opener hands out sessions in order; when it runs out it keeps returning
// the last one's error, or a fresh healthy session.
type opener struct {
	mu       sync.Mutex
	results  []openResult
	calls    int
	sessions []*fakeSession
}

type openResult struct {
	sess *fakeSession
	err  error
}

func (o *opener) open(context.Context) (session, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if len(o.results) == 0 {
		return nil, errors.New(`status="503 Service Unavailable"`)
	}
	r := o.results[0]
	if len(o.results) > 1 {
		o.results = o.results[1:]
	}
	if r.err != nil {
		return nil, r.err
	}
	o.sessions = append(o.sessions, r.sess)
	return r.sess, nil
}

// opened returns how many sessions were opened.
func (o *opener) opened() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.sessions)
}

func fastReconnect(t *testing.T) {
	t.Helper()
	oldMin, oldMax, oldReset := reconnectMin, reconnectMax, reconnectReset
	reconnectMin, reconnectMax, reconnectReset = time.Millisecond, 5*time.Millisecond, time.Hour
	t.Cleanup(func() { reconnectMin, reconnectMax, reconnectReset = oldMin, oldMax, oldReset })
}

func testScaleSet(name string, first *fakeSession, o *opener, tr *health.Tracker) *ScaleSet {
	return &ScaleSet{
		cfg:        Config{Name: name, MaxRunners: 4},
		log:        testLogger(),
		session:    first,
		open:       o.open,
		health:     tr,
		scaleSetID: 1,
	}
}

// fakeProv is a provisioner whose runners live until their context is
// cancelled or release is closed.
type fakeProv struct {
	mu        sync.Mutex
	launched  int
	cancelled int
	release   chan struct{}
}

func newFakeProv() *fakeProv { return &fakeProv{release: make(chan struct{})} }

func (p *fakeProv) Launch(ctx context.Context, _, _ string, _ core.RunnerSpec, _ func()) error {
	p.mu.Lock()
	p.launched++
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		p.mu.Lock()
		p.cancelled++
		p.mu.Unlock()
	case <-p.release:
	}
	return nil
}
func (p *fakeProv) Name() string { return "fake" }
func (p *fakeProv) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launched, p.cancelled
}

type fakeJIT struct{ n atomic.Int32 }

func (j *fakeJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner-" + string(rune('a'+j.n.Add(1))), "jit", nil
}

// tier wires a ScaleSet to a real scheduler, as main does.
type tier struct {
	ss    *ScaleSet
	sched *scheduler.Scheduler
	prov  *fakeProv
	done  chan error
}

func startTier(ctx context.Context, ss *ScaleSet, max int) *tier {
	prov := newFakeProv()
	sched := scheduler.New(scheduler.Options{
		Max: max, Spec: core.RunnerSpec{}, Provisioner: prov, JIT: &fakeJIT{}, Logger: testLogger(),
	})
	t := &tier{ss: ss, sched: sched, prov: prov, done: make(chan error, 1)}
	go func() {
		t.done <- ss.Run(ctx,
			func(ctx context.Context, desired int) int { sched.Reconcile(ctx, desired); return sched.Running() },
			sched.MarkBusy,
			sched.Capacity)
	}()
	return t
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReconnect_TierIsolation(t *testing.T) {
	// Tier A's session fails repeatedly and its reconnects fail too; tier B
	// keeps taking work throughout. A recovers once GitHub does, and neither
	// tier is drained or cancelled.
	fastReconnect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trA := health.NewTracker("a")
	healed := newFakeSession(1, false)
	oA := &opener{results: []openResult{
		{err: errors.New(`status="503 Service Unavailable"`)},
		{err: errors.New(`status="502 Bad Gateway"`)},
		{sess: healed},
	}}
	firstA := newFakeSession(0, true)
	a := startTier(ctx, testScaleSet("a", firstA, oA, trA), 4)

	firstB := newFakeSession(0, false)
	b := startTier(ctx, testScaleSet("b", firstB, &opener{}, health.NewTracker("b")), 4)
	firstB.msgs <- message(1, 2)

	eventually(t, "tier B to launch 2 runners", func() bool { l, _ := b.prov.counts(); return l == 2 })
	eventually(t, "tier A to reconnect", func() bool { return trA.Snapshot().Reconnects == 1 })
	if !firstA.closed.Load() {
		t.Fatal("tier A's failed session was not closed")
	}
	// The healed session's initial statistics (1 assigned job) drive A.
	eventually(t, "tier A to launch from its new session's statistics", func() bool { l, _ := a.prov.counts(); return l == 1 })
	if s := trA.Snapshot(); s.State != health.Connected || s.LastError == "" {
		t.Fatalf("tier A health = %+v, want connected with the last error kept", s)
	}
	// B keeps working after A's outage.
	firstB.msgs <- message(2, 3)
	eventually(t, "tier B to launch a third runner", func() bool { l, _ := b.prov.counts(); return l == 3 })

	for name, tr := range map[string]*tier{"a": a, "b": b} {
		select {
		case err := <-tr.done:
			t.Fatalf("tier %s Run returned %v; a reconnect must not end it", name, err)
		default:
		}
		if _, c := tr.prov.counts(); c != 0 {
			t.Fatalf("tier %s had %d runners cancelled", name, c)
		}
	}
	cancel()
	for _, tr := range []*tier{a, b} {
		if err := <-tr.done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run after cancel = %v", err)
		}
	}
}

func TestReconnect_FatalErrorStopsOnlyThatTier(t *testing.T) {
	fastReconnect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trA := health.NewTracker("a")
	oA := &opener{results: []openResult{
		{err: errors.New(`request POST https://api.github.com/.../sessions failed(status="401 Unauthorized"): bad credentials`)},
	}}
	a := startTier(ctx, testScaleSet("a", newFakeSession(0, true), oA, trA), 4)
	firstB := newFakeSession(0, false)
	b := startTier(ctx, testScaleSet("b", firstB, &opener{}, health.NewTracker("b")), 4)

	select {
	case err := <-a.done:
		if !errors.Is(err, ErrFatal) {
			t.Fatalf("tier A Run = %v, want ErrFatal", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tier A did not stop on a 401")
	}
	if s := trA.Snapshot(); s.State != health.Failed || s.LastError == "" {
		t.Fatalf("tier A health = %+v, want failed", s)
	}
	oA.mu.Lock()
	calls := oA.calls
	oA.mu.Unlock()
	if calls != 1 {
		t.Fatalf("tier A retried a fatal error %d times", calls)
	}
	// Tier B carries on.
	firstB.msgs <- message(1, 1)
	eventually(t, "tier B to launch", func() bool { l, _ := b.prov.counts(); return l == 1 })
	select {
	case err := <-b.done:
		t.Fatalf("tier B stopped: %v", err)
	default:
	}
}

func TestReconnect_BusyRunnersSurvive(t *testing.T) {
	// Runners busy with jobs are untouched by a reconnect, and the desired
	// count after it comes from the new session's statistics: lower demand
	// cancels no busy runner, higher demand launches more.
	for _, c := range []struct {
		name         string
		after        int // TotalAssignedJobs in the new session
		wantLaunched int
	}{
		{"demand drops to 0", 0, 2},
		{"demand rises to 3", 3, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			fastReconnect(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			first := newFakeSession(2, false)
			next := newFakeSession(c.after, false)
			o := &opener{results: []openResult{{sess: next}}}
			tr := startTier(ctx, testScaleSet("a", first, o, health.NewTracker("a")), 4)

			eventually(t, "two runners", func() bool { l, _ := tr.prov.counts(); return l == 2 })
			for _, name := range []string{"runner-b", "runner-c"} {
				tr.sched.MarkBusy(name)
			}
			first.failing.Store(true)
			eventually(t, "the new session to be polled", func() bool { return o.opened() == 1 && next.gets.Load() > 1 })
			eventually(t, "launches from the new statistics", func() bool { l, _ := tr.prov.counts(); return l == c.wantLaunched })

			time.Sleep(20 * time.Millisecond) // let any wrong cancel land
			if _, cancelled := tr.prov.counts(); cancelled != 0 {
				t.Fatalf("%d busy runners cancelled across the reconnect", cancelled)
			}
			if n := tr.sched.Running(); n != c.wantLaunched {
				t.Fatalf("scheduler tracks %d runners, want %d", n, c.wantLaunched)
			}
		})
	}
}

func TestReconnect_BackoffGrowsAndIsCapped(t *testing.T) {
	fastReconnect(t)
	reconnectMin, reconnectMax = time.Millisecond, 4*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := health.NewTracker("a")
	o := &opener{} // every open fails with a 503
	done := make(chan error, 1)
	ss := testScaleSet("a", newFakeSession(0, true), o, tr)
	go func() { done <- ss.Run(ctx, func(context.Context, int) int { return 0 }, nil, nil) }()

	var peak float64
	eventually(t, "several attempts", func() bool {
		if b := tr.Snapshot().BackoffSeconds; b > peak {
			peak = b
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.calls >= 8
	})
	// Capped at reconnectMax plus 20% jitter.
	if limit := (4 * time.Millisecond).Seconds() * 1.2; peak > limit {
		t.Fatalf("backoff reached %vs, cap is %vs", peak, limit)
	}
	if s := tr.Snapshot(); s.State != health.Reconnecting {
		t.Fatalf("state = %s, want reconnecting", s.State)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestIsFatal(t *testing.T) {
	cases := map[string]bool{
		`failed(status="401 Unauthorized"): bad credentials`:                     true,
		`failed(status="403 Forbidden"): resource not accessible by integration`: true,
		`failed(status="404 Not Found"): runner scale set not found`:             false, // restarts the daemon (ErrRestart), not fatal
		`failed(status="403 Forbidden"): API rate limit exceeded`:                false,
		`failed(status="503 Service Unavailable")`:                               false,
		`dial tcp: i/o timeout`:                                                  false,
		`failed(status="409 Conflict")`:                                          false,
	}
	for msg, want := range cases {
		if got := isFatal(errors.New(msg)); got != want {
			t.Errorf("isFatal(%q) = %v, want %v", msg, got, want)
		}
	}
	if isFatal(nil) {
		t.Error("isFatal(nil) = true")
	}
}

func TestReconnect_DeletedScaleSetRestarts(t *testing.T) {
	// The scale set is gone (another instance that created it deleted it on
	// shutdown). Reopening gets 404 and Run returns ErrRestart at once, for
	// the daemon to restart and register it again; it is not retried.
	fastReconnect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := health.NewTracker("a")
	o := &opener{results: []openResult{
		{err: errors.New(`request POST .../sessions failed(status="404 Not Found"): runner scale set not found`)},
	}}
	ss := testScaleSet("a", newFakeSession(0, true), o, tr)
	done := make(chan error, 1)
	go func() { done <- ss.Run(ctx, func(context.Context, int) int { return 0 }, nil, nil) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run kept retrying a deleted scale set")
	}
	if !errors.Is(err, ErrRestart) || errors.Is(err, ErrFatal) {
		t.Fatalf("Run = %v, want ErrRestart (not ErrFatal)", err)
	}
	o.mu.Lock()
	calls := o.calls
	o.mu.Unlock()
	if calls != 1 {
		t.Fatalf("opened %d times, want no retry after a 404", calls)
	}
	if s := tr.Snapshot(); s.State != health.Failed || s.LastError == "" {
		t.Fatalf("health = %+v, want failed with the 404", s)
	}
}

func TestJitterBounds(t *testing.T) {
	for range 1000 {
		if j := jitter(time.Second); j < 800*time.Millisecond || j > 1200*time.Millisecond {
			t.Fatalf("jitter(1s) = %v, outside ±20%%", j)
		}
	}
}

func TestTrackedSessionRecordsMessages(t *testing.T) {
	tr := health.NewTracker("a")
	f := newFakeSession(0, false)
	ts := trackedSession{session: f, health: tr}
	if _, err := ts.GetMessage(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if tr.Snapshot().LastMessage.IsZero() {
		t.Fatal("a successful GetMessage was not recorded")
	}
	f.failing.Store(true)
	before := tr.Snapshot().LastMessage
	_, _ = ts.GetMessage(context.Background(), 0, 1)
	if !tr.Snapshot().LastMessage.Equal(before) {
		t.Fatal("a failed GetMessage counted as a message")
	}
}

func TestRecycleReconnectsAndStatsAreRecorded(t *testing.T) {
	fastReconnect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := health.NewTracker("a")
	first := newFakeSession(0, false)
	next := newFakeSession(2, false)
	o := &opener{results: []openResult{{sess: next}}}
	ss := testScaleSet("a", first, o, tr)
	tier := startTier(ctx, ss, 4)

	if _, _, ok := ss.Stats(); ok {
		t.Fatal("stats reported before any message")
	}
	first.msgs <- &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{
		TotalAvailableJobs: 3, TotalAssignedJobs: 1, TotalRegisteredRunners: 5,
	}}
	eventually(t, "the message's statistics recorded", func() bool {
		st, at, ok := ss.Stats()
		return ok && st.TotalAvailableJobs == 3 && st.TotalRegisteredRunners == 5 && !at.IsZero()
	})

	ss.Recycle("test")
	eventually(t, "a new session after the recycle", func() bool { return o.opened() == 1 && next.gets.Load() > 0 })
	if !first.closed.Load() {
		t.Fatal("the recycled session was not closed")
	}
	// The new session's statistics drive demand as after any reconnect.
	eventually(t, "launches from the new session", func() bool { l, _ := tier.prov.counts(); return l == 2 })
	select {
	case err := <-tier.done:
		t.Fatalf("Run returned %v after a recycle", err)
	default:
	}
}

func TestChaosExitBeforeAcquire(t *testing.T) {
	// The fault switch must exit before AcquireJobs reaches GitHub. Run it in
	// a child process, since it calls os.Exit.
	if os.Getenv("FR_CHAOS_CHILD") == "1" {
		f := newFakeSession(0, false)
		ts := trackedSession{session: f, chaos: chaosExitBeforeAcquire()}
		_, _ = ts.AcquireJobs(context.Background(), []int64{1, 2})
		os.Exit(0) // not reached when chaos is on
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestChaosExitBeforeAcquire$")
	cmd.Env = append(os.Environ(), "FR_CHAOS_CHILD=1", "FR_CHAOS_EXIT_BEFORE_ACQUIRE=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 137 || !strings.Contains(string(out), "exiting before acquiring 2 job(s)") {
		t.Fatalf("child exit = %v, output:\n%s", err, out)
	}
	// Off by default: AcquireJobs passes through.
	f := newFakeSession(0, false)
	ids, err := trackedSession{session: f, chaos: false}.AcquireJobs(context.Background(), []int64{7})
	if err != nil || len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("AcquireJobs = %v, %v", ids, err)
	}
}
