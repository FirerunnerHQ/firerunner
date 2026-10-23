package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solcreek/firerunner/internal/core"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type provFunc func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error

func (p provFunc) Launch(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
	return p(ctx, name, jit, spec, onBusy)
}
func (provFunc) Name() string { return "fake" }

// slotProv is a provFunc that also reports a shared-pool free count, like the
// Firecracker provisioner does.
type slotProv struct {
	provFunc
	free int
}

func (p slotProv) FreeSlots() int { return p.free }

type jitStub struct{}

func (jitStub) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner", "jit", nil
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name                        string
		desired, live, running, max int
		want                        int
	}{
		{"none wanted", 0, 0, 0, 4, 0},
		{"scale from zero", 3, 0, 0, 4, 3},
		{"bounded by max", 10, 0, 0, 4, 4},
		{"partial capacity", 10, 3, 3, 4, 1},
		{"at capacity", 4, 4, 4, 4, 0},
		{"desired below running", 1, 3, 3, 4, 0},
		{"over capacity running", 10, 5, 5, 4, 0},
		// A launch in backoff or a VM tearing down is in running but not live:
		// demand still gets a fresh launch, within the concurrency bound.
		{"demand ignores a launch in backoff", 1, 0, 1, 4, 1},
		{"but running still bounds the attempts", 3, 0, 4, 4, 0},
		{"room left after dead entries", 3, 1, 2, 4, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plan(tc.desired, tc.live, tc.running, tc.max); got != tc.want {
				t.Fatalf("plan(%d,%d,%d,%d)=%d want %d", tc.desired, tc.live, tc.running, tc.max, got, tc.want)
			}
		})
	}
}

func TestReconcileRespectsMaxAndDrains(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan string, 16)
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- name
		<-release
		return nil
	})
	s := New(Options{Max: 2, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(context.Background(), 5) // demand 5, capacity 2
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2", got)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for launch to enter")
		}
	}
	// Further reconcile at capacity launches nothing.
	s.Reconcile(context.Background(), 5)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 after saturated reconcile", got)
	}

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

func TestCapacityIsRunningPlusFreeSlotsCappedAtMax(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	launch := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	prov := &slotProv{provFunc: launch, free: 1}
	s := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	// Nothing running, one slot free on the host: only one job can be held.
	if got := s.Capacity(); got != 1 {
		t.Fatalf("capacity=%d want 1 (0 running + 1 free)", got)
	}

	s.Reconcile(context.Background(), 2)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for launch to enter")
		}
	}
	// Two in flight (each holds a job) and the pool is now dry: capacity is
	// exactly what is already assigned, so GitHub should send nothing more.
	prov.free = 0
	if got := s.Capacity(); got != 2 {
		t.Fatalf("capacity=%d want 2 (2 running + 0 free)", got)
	}
	// The pool recovering never lifts capacity above the tier's own max.
	prov.free = 10
	if got := s.Capacity(); got != 4 {
		t.Fatalf("capacity=%d want 4 (min(max=4, 2+10))", got)
	}

	close(release)
	s.Drain()
	if got := s.Capacity(); got != 4 {
		t.Fatalf("capacity=%d want 4 after drain with a full pool", got)
	}
}

// gateJIT blocks Generate until released, holding a launch in the window
// between Reconcile committing to it and the provisioner being entered.
type gateJIT struct{ release chan struct{} }

func (g gateJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	<-g.release
	return "runner", "jit", nil
}

type failJIT struct{}

func (failJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "", "", errors.New("jit unavailable")
}

func TestCapacityExcludesPendingLaunchesFromFreeSlots(t *testing.T) {
	jitGate := make(chan struct{})
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	prov := &slotProv{free: 2, provFunc: func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-release
		return nil
	}}
	s := New(Options{Max: 6, Provisioner: prov, JIT: gateJIT{jitGate}, Logger: testLogger()})

	s.Reconcile(context.Background(), 2)
	// Both launches are committed (running=2) but stuck in JIT generation, so
	// the provisioner still reports both slots free. Counting them in both
	// places would advertise 4; the honest figure is 2.
	if got := s.Capacity(); got != 2 {
		t.Fatalf("capacity=%d want 2 while 2 launches are pending against 2 free slots", got)
	}

	close(jitGate)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for launch to enter")
		}
	}
	// Entering the provisioner settles the pending count; the fake pool is
	// then updated the way the real one would be by the acquire.
	if got := s.opts.Pending.Count(); got != 0 {
		t.Fatalf("pending=%d want 0 once both launches entered the provisioner", got)
	}
	prov.free = 0
	if got := s.Capacity(); got != 2 {
		t.Fatalf("capacity=%d want 2 with 2 running and a dry pool", got)
	}
	close(release)
	s.Drain()
}

func TestCapacityWhenMoreIsPendingThanFree(t *testing.T) {
	jitGate := make(chan struct{})
	release := make(chan struct{})
	prov := &slotProv{free: 1, provFunc: func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		<-release
		return nil
	}}
	var pending PendingLaunches
	a := New(Options{Max: 4, Provisioner: prov, JIT: gateJIT{jitGate}, Logger: testLogger(), Pending: &pending})
	b := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger(), Pending: &pending})

	a.Reconcile(context.Background(), 3)
	// Three launches committed against one free slot: only one can ever get
	// a slot, so the tier can hold one job, not three.
	if got := a.Capacity(); got != 1 {
		t.Fatalf("tier A capacity=%d want 1 (3 running + 1 free - 3 pending)", got)
	}
	// And the sibling tier sees a pool that is already oversubscribed.
	if got := b.Capacity(); got != 0 {
		t.Fatalf("tier B capacity=%d want 0 (0 running + 1 free - 3 pending, floored)", got)
	}

	close(jitGate)
	close(release)
	a.Drain()
	b.Drain()
}

func TestCapacityPendingIsSharedAcrossTiers(t *testing.T) {
	jitGate := make(chan struct{})
	release := make(chan struct{})
	prov := &slotProv{free: 3, provFunc: func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		<-release
		return nil
	}}
	var pending PendingLaunches
	a := New(Options{Max: 4, Provisioner: prov, JIT: gateJIT{jitGate}, Logger: testLogger(), Pending: &pending})
	b := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger(), Pending: &pending})

	a.Reconcile(context.Background(), 2)
	// Tier B has nothing running, but two of the three free slots are already
	// spoken for by tier A's pending launches.
	if got := b.Capacity(); got != 1 {
		t.Fatalf("tier B capacity=%d want 1 (3 free - 2 pending elsewhere)", got)
	}
	if got := a.Capacity(); got != 3 {
		t.Fatalf("tier A capacity=%d want 3 (2 running + (3 free - 2 pending))", got)
	}

	close(jitGate)
	close(release)
	a.Drain()
	b.Drain()
}

func TestPendingSettlesWhenLaunchBailsBeforeProvisioner(t *testing.T) {
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch must not be reached after a JIT failure")
		return nil
	})
	var pending PendingLaunches
	s := New(Options{Max: 2, Provisioner: prov, JIT: failJIT{}, Logger: testLogger(), Pending: &pending})
	s.Reconcile(context.Background(), 1)
	s.Drain()
	if got := pending.Count(); got != 0 {
		t.Fatalf("pending=%d want 0 after the launch bailed on JIT generation", got)
	}
}

func TestCapacityExcludesCancelledVMsWhileTheyTearDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	exit := make(chan struct{})
	prov := &slotProv{free: 1, provFunc: func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done()
		<-exit // a cancelled VM lingers in teardown until the test lets it go
		return nil
	}}
	s := New(Options{Max: 4, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	if got := s.Capacity(); got != 4 {
		t.Fatalf("capacity=%d want 4 (3 running + 1 free)", got)
	}

	s.Reconcile(ctx, 1) // two idle VMs are cancelled but have not exited yet
	if got := s.Running(); got != 3 {
		t.Fatalf("running=%d want 3 while cancelled VMs are still tearing down", got)
	}
	// Those two can neither take a job nor have they returned their slots.
	if got := s.Capacity(); got != 2 {
		t.Fatalf("capacity=%d want 2 (1 live + 1 free; 2 cancelled excluded)", got)
	}

	close(exit)
	waitRunning(t, s, 1)
	prov.free = 3
	if got := s.Capacity(); got != 4 {
		t.Fatalf("capacity=%d want 4 once the cancelled VMs have exited and freed their slots", got)
	}
	cancel()
	s.Drain()
}

func TestCapacityExcludesLaunchesInBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := make(chan struct{}, 8)
	prov := &slotProv{free: 0, provFunc: func(context.Context, string, string, core.RunnerSpec, func()) error {
		failed <- struct{}{}
		return errors.New("no free network slot (max 4 microVMs)")
	}}
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	for i := 0; i < 2; i++ {
		select {
		case <-failed:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never attempted")
		}
	}
	// Both goroutines are now sleeping in backoff (first delay 500ms), still
	// counted in running for pacing, with no VM and no slot behind them.
	waitCapacity(t, s, 0)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 while the failed launches back off", got)
	}
	cancel()
	s.Drain()
}

func TestCapacityCountsFailedLaunchOnceUnderScaleDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	failed := make(chan struct{}, 8)
	var attempts int32
	prov := &slotProv{free: 2, provFunc: func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		if atomic.AddInt32(&attempts, 1) == 1 {
			failed <- struct{}{}
			return errors.New("boot failed") // the first launch dies and backs off
		}
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	}}
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("failing launch never attempted")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy launch never entered")
	}
	// running=2: one live VM, one goroutine in backoff. Demand drops to 0, so
	// scaleDown cancels every idle handle it can find — the failed launch must
	// not be among them, or it would be discounted for backoff and again for
	// cancellation, dragging capacity below the free slots the host has.
	s.Reconcile(ctx, 0)
	waitRunning(t, s, 1)
	waitCapacity(t, s, 2) // 0 live (the survivor was cancelled) + 2 free

	cancel()
	s.Drain()
}

func TestPendingSettlesBeforeJITFailureBackoff(t *testing.T) {
	prov := &slotProv{free: 3, provFunc: func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch must not be reached after a JIT failure")
		return nil
	}}
	var pending PendingLaunches
	s := New(Options{Max: 4, Provisioner: prov, JIT: failJIT{}, Logger: testLogger(), Pending: &pending})
	s.Reconcile(context.Background(), 1)
	// The goroutine sleeps in backoff for 500ms. Its reservation must be
	// released as soon as JIT fails, not when the goroutine exits, otherwise
	// the free slot it will never use is hidden from every tier meanwhile.
	deadline := time.After(200 * time.Millisecond)
	for pending.Count() != 0 {
		select {
		case <-deadline:
			t.Fatalf("pending=%d want 0 while the JIT failure backs off", pending.Count())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// backoff takes the lock a moment after enter() releases the reservation,
	// so wait for the count to settle rather than sampling once.
	waitCapacity(t, s, 3) // 0 live + min(Max-1 running, 3 free)
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1: the goroutine should still be in backoff", got)
	}
	s.Drain()
}

func waitCapacity(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if got := s.Capacity(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("capacity=%d want %d", s.Capacity(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestReconcileServesDemandPastALaunchInBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	entered := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return errors.New("boot failed") // first attempt dies and backs off
		}
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 1)
	waitCapacity(t, s, 3) // the failed launch is in backoff: 0 live, room 3
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 (the launch in backoff)", got)
	}

	// GitHub's next callback still reports one assigned job. Before, plan
	// measured it against running and launched nothing, leaving the job to
	// wait out the backoff; it must get a fresh VM now.
	s.Reconcile(ctx, 1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("demand was not served while a failed launch backed off")
	}
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 (one live, one in backoff)", got)
	}
	cancel()
	s.Drain()
}

func TestReconcileServesDemandWhileCancelledVMsTearDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	exit := make(chan struct{})
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done()
		<-exit
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	<-entered
	<-entered
	s.Reconcile(ctx, 0) // both cancelled, both stuck in teardown
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 while the cancelled VMs tear down", got)
	}

	// Demand returns before they have exited: neither can serve it.
	s.Reconcile(ctx, 1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("demand was not served while cancelled VMs tore down")
	}
	close(exit)
	cancel()
	s.Drain()
}

func TestScaleDownDoesNotCancelALiveVMForALaunchInBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int32
	entered := make(chan struct{}, 8)
	cancelled := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return errors.New("boot failed")
		}
		entered <- struct{}{}
		<-vmCtx.Done()
		cancelled <- struct{}{}
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	<-entered
	waitCapacity(t, s, 3) // 1 live + room 2 (running=2 with one in backoff)

	// Demand holds at 1: the live VM is exactly what is needed. Measured
	// against running (2) it would look like excess and be cancelled.
	s.Reconcile(ctx, 1)
	select {
	case <-cancelled:
		t.Fatal("the only live VM was cancelled to make room for a launch in backoff")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	s.Drain()
}

func TestCapacityWithoutSlotReporterIsBoundedOnlyByMax(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-release
		return nil
	})
	s := New(Options{Max: 3, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})
	if got := s.Capacity(); got != 3 {
		t.Fatalf("capacity=%d want Max(3) when the provisioner has no shared pool", got)
	}
	s.Reconcile(context.Background(), 2)
	for i := 0; i < 2; i++ {
		<-entered
	}
	if got := s.Capacity(); got != 3 {
		t.Fatalf("capacity=%d want 3 (2 live + 1 room)", got)
	}
	close(release)
	s.Drain()
}

func TestCapacityWithoutSlotReporterStillDiscountsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		return errors.New("boot failed")
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})
	s.Reconcile(ctx, 1)
	// No pool to constrain it, but the launch in backoff still holds one of
	// the Max places plan will hand out, so capacity says 3, not 4, until the
	// backoff ends and the goroutine exits.
	waitCapacity(t, s, 3)
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 (the launch in backoff)", got)
	}
	cancel()
	s.Drain()
}

func TestReconcileZeroDoesNothing(t *testing.T) {
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch should not be called for desired=0")
		return nil
	})
	s := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})
	s.Reconcile(context.Background(), 0)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0", got)
	}
}

func TestMaintainsMinimumAfterExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	launched := make(chan string, 32)
	block := make(chan struct{})
	var count int32
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		launched <- name
		if atomic.AddInt32(&count, 1) == 1 {
			return nil // first microVM finishes its job and exits immediately
		}
		<-block // replacement stays "running" so we can observe it
		return nil
	})
	s := New(Options{Max: 4, Min: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 0) // no jobs: a warm pool of 1; that VM exits and must be replenished

	for i := 0; i < 2; i++ {
		select {
		case <-launched:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected a replacement launch after exit; got %d launches", i)
		}
	}
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 (pool refilled to Min)", got)
	}

	// Shutdown must stop replenishment, not fight the drain.
	cancel()
	close(block)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestBusyVMOutlivesCancel is the core of the graceful-drain fix: a SIGTERM
// (modelled by cancelling the parent context) must not cancel the context of a
// microVM that is running its job, or the provisioner's exec.CommandContext
// would SIGKILL Firecracker mid-job.
func TestBusyVMOutlivesCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan context.Context, 1)
	busyCh := make(chan func(), 1)
	release := make(chan struct{})
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- vmCtx
		busyCh <- onBusy
		<-release
		return nil
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 1)

	var vmCtx context.Context
	select {
	case vmCtx = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch never started")
	}

	(<-busyCh)() // guest console reported "Running job:" — mark the VM busy
	cancel()     // simulate SIGTERM while the job is in flight

	select {
	case <-vmCtx.Done():
		t.Fatal("busy microVM context was cancelled by shutdown; the job would be killed mid-run")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestIdleVMCancelledOnShutdown verifies the other half: an idle warm-pool VM
// (no job assigned) is cancelled at once on shutdown so it never blocks the
// drain waiting for a job that will never come.
func TestIdleVMCancelledOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 1)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done() // stays "running" until cancelled
		return nil
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 1)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("launch never started")
	}

	cancel() // shutdown with an idle warm VM in the pool

	done := make(chan struct{})
	go func() { s.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle warm VM was not cancelled on shutdown; drain hung")
	}
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestReconcileAfterShutdownLaunchesNothing verifies we stop accepting new work
// once shutdown has begun: a Reconcile racing in after the context is cancelled
// must not boot a fresh microVM into a draining host.
func TestReconcileAfterShutdownLaunchesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		t.Fatal("Launch must not run once shutdown has begun")
		return nil
	})
	s := New(Options{Max: 4, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0", got)
	}
}

// jitSeq hands out a unique runner name per call so each microVM gets its own
// entry in the scheduler's active registry (the shared-name jitStub would
// collide for multi-VM scenarios).
type jitSeq struct{ n atomic.Int64 }

func (j *jitSeq) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	return "runner-" + strconv.FormatInt(j.n.Add(1), 10), "jit", nil
}

func waitRunning(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if got := s.Running(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("running=%d want %d", s.Running(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestScaleDownCancelsIdle verifies that when demand drops below the number
// running, the surplus idle VMs are cancelled rather than left squatting slots.
func TestScaleDownCancelsIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done() // idle VM stays running until cancelled
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	waitRunning(t, s, 3)

	s.Reconcile(ctx, 1) // demand falls to 1; two idle VMs must be cancelled
	waitRunning(t, s, 1)
}

// TestScaleDownLeavesBusy verifies a busy VM (running a job) is never cancelled
// by scale-down; only idle VMs are reaped.
func TestScaleDownLeavesBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	busyCh := make(chan func(), 8)
	release := make(chan struct{})
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		busyCh <- onBusy
		select {
		case <-vmCtx.Done():
		case <-release:
		}
		return nil
	})
	s := New(Options{Max: 4, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	var onBusy1 func()
	select {
	case onBusy1 = <-busyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("first launch never started")
	}
	select {
	case <-busyCh: // second VM's onBusy (unused)
	case <-time.After(2 * time.Second):
		t.Fatal("second launch never started")
	}
	waitRunning(t, s, 2)

	onBusy1()            // one VM dequeues a job
	s.Reconcile(ctx, 0)  // demand drops to 0: only the idle VM may be cancelled
	waitRunning(t, s, 1) // the busy VM survives

	close(release)
	s.Drain()
	if got := s.Running(); got != 0 {
		t.Fatalf("running=%d want 0 after drain", got)
	}
}

// TestScaleDownPreservesMin verifies scale-to-zero still keeps the warm-pool
// floor (Min) of pre-booted VMs.
func TestScaleDownPreservesMin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	})
	s := New(Options{Max: 4, Min: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2) // two jobs plus Min=1
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	waitRunning(t, s, 3)

	s.Reconcile(ctx, 0) // demand 0, but Min=1 keeps one warm
	waitRunning(t, s, 1)
}

// removerFunc adapts a function to RunnerRemover.
type removerFunc func(ctx context.Context, name string) error

func (f removerFunc) RemoveRunner(ctx context.Context, name string) error { return f(ctx, name) }

// idleProv launches VMs that stay idle until cancelled, reporting each launch
// on entered.
func idleProv(entered chan<- struct{}) provFunc {
	return func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	}
}

func startIdle(t *testing.T, ctx context.Context, s *Scheduler, entered <-chan struct{}, n int) {
	t.Helper()
	s.Reconcile(ctx, n)
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("launch never started")
		}
	}
	waitRunning(t, s, n)
}

// TestScaleDownDeregistersBeforeCancelling verifies that with a Remover, an
// idle VM is cancelled only after GitHub has deregistered its runner.
func TestScaleDownDeregistersBeforeCancelling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	removing := make(chan string, 8)
	allow := make(chan struct{})
	rm := removerFunc(func(_ context.Context, name string) error {
		removing <- name
		<-allow
		return nil
	})
	s := New(Options{Max: 4, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 2)

	s.Reconcile(ctx, 1)
	select {
	case <-removing:
	case <-time.After(2 * time.Second):
		t.Fatal("scale-down never deregistered a runner")
	}
	// While the removal is in flight the VM must still be running.
	time.Sleep(50 * time.Millisecond)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2 before GitHub confirmed the removal", got)
	}
	close(allow)
	waitRunning(t, s, 1)
}

// TestScaleDownSparesRunnerGitHubAssigned verifies that when GitHub refuses
// the removal because a job is assigned, the VM survives and is treated as
// busy, so later scale-downs leave it alone.
func TestScaleDownSparesRunnerGitHubAssigned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	var calls atomic.Int64
	rm := removerFunc(func(context.Context, string) error {
		calls.Add(1)
		return fmt.Errorf("remove: %w", core.ErrRunnerBusy)
	})
	s := New(Options{Max: 4, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)

	s.Reconcile(ctx, 0)
	waitCalls(t, &calls, 1)
	waitIdleBusy(t, s)

	s.Reconcile(ctx, 0) // a busy VM is never picked again
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("remover called %d times, want 1", got)
	}
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1: the assigned runner was cancelled", got)
	}
}

// TestScaleDownRetriesAfterRemovalError verifies an unexplained removal
// failure leaves the VM running and idle, so the next scale-down retries it.
func TestScaleDownRetriesAfterRemovalError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	var calls atomic.Int64
	rm := removerFunc(func(context.Context, string) error {
		if calls.Add(1) == 1 {
			return errors.New("503")
		}
		return nil
	})
	s := New(Options{Max: 4, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)

	s.Reconcile(ctx, 0)
	waitCalls(t, &calls, 1)
	waitNotRemoving(t, s)
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 after a failed removal", got)
	}

	s.Reconcile(ctx, 0)
	waitRunning(t, s, 0)
}

// TestScaleDownAppliesASlowRemovalAndNeverDuplicatesIt verifies a removal that
// answers late is still acted on (its VM is cancelled, not left idle while
// deregistered), and that repeated scale-downs meanwhile start no second
// attempt for the same VM.
func TestScaleDownAppliesASlowRemovalAndNeverDuplicatesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int64
	rm := removerFunc(func(context.Context, string) error {
		calls.Add(1)
		<-release
		return nil
	})
	s := New(Options{Max: 4, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)

	for i := 0; i < 3; i++ {
		s.Reconcile(ctx, 0)
	}
	waitCalls(t, &calls, 1)
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("remover called %d times while one removal was in flight, want 1", got)
	}
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1 before the removal answered", got)
	}

	close(release)
	waitRunning(t, s, 0)
}

// TestDemandRiseReclaimsQueuedRemovals verifies that when demand comes back
// while removals are still queued, those VMs return to the idle pool instead of
// being deregistered one by one; only the removal already running proceeds.
func TestDemandRiseReclaimsQueuedRemovals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int64
	rm := removerFunc(func(context.Context, string) error {
		calls.Add(1)
		<-release
		return nil
	})
	s := New(Options{Max: 3, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 3)

	s.Reconcile(ctx, 0) // one removal runs, two queue
	waitCalls(t, &calls, 1)
	s.Reconcile(ctx, 3) // demand is back before the queue drains

	s.mu.Lock()
	live, queued := s.live(), len(s.removeQ)
	s.mu.Unlock()
	if live != 2 || queued != 0 {
		t.Fatalf("live=%d queued=%d want 2 reclaimed and an empty queue", live, queued)
	}

	close(release)
	waitRunning(t, s, 2) // only the in-flight removal went through
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("remover called %d times, want 1", got)
	}
}

// TestUnconfirmedRemovalStaysRemovingUntilSettled verifies an
// ErrRemovalUnknown answer keeps the VM out of live and retries until GitHub
// gives a definite one.
func TestUnconfirmedRemovalStaysRemovingUntilSettled(t *testing.T) {
	old := removeRetryDelay
	removeRetryDelay = 10 * time.Millisecond
	defer func() { removeRetryDelay = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{}, 8)
	settle := make(chan struct{})
	var calls atomic.Int64
	rm := removerFunc(func(context.Context, string) error {
		calls.Add(1)
		select {
		case <-settle:
			return nil
		default:
			return fmt.Errorf("remove: %w", core.ErrRemovalUnknown)
		}
	})
	s := New(Options{Max: 4, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)

	s.Reconcile(ctx, 0)
	waitCalls(t, &calls, 3) // retried, not abandoned
	s.mu.Lock()
	live := s.live()
	s.mu.Unlock()
	if live != 0 {
		t.Fatalf("live=%d want 0 while the removal is unconfirmed", live)
	}

	close(settle)
	waitRunning(t, s, 0)
}

// TestRemoveLoopStopsOnShutdown verifies shutdown ends a removal loop that is
// retrying an unconfirmed removal, instead of retrying with no delay. The VM
// is busy, so the drain's idle cancel leaves it alone — the case that used to
// spin.
func TestRemoveLoopStopsOnShutdown(t *testing.T) {
	old := removeRetryDelay
	removeRetryDelay = time.Hour // only shutdown can end the wait
	defer func() { removeRetryDelay = old }()

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	busyCh := make(chan func(), 1)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		busyCh <- onBusy
		select {
		case <-vmCtx.Done():
		case <-release:
		}
		return nil
	})
	var calls atomic.Int64
	var onBusy func()
	rm := removerFunc(func(context.Context, string) error {
		if calls.Add(1) == 1 {
			onBusy() // the guest dequeues a job while the removal is in flight
		}
		return fmt.Errorf("remove: %w", core.ErrRemovalUnknown)
	})
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(ctx, 1)
	onBusy = <-busyCh
	waitRunning(t, s, 1)

	s.Reconcile(ctx, 0)
	waitCalls(t, &calls, 1)
	cancel()
	waitRemoverStopped(t, s)
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("remover called %d times across shutdown, want 1", got)
	}

	close(release)
	s.Drain()
}

// TestUnconfirmedRemovalOfABusyVMIsNotRetried verifies that once the guest
// has dequeued a job, an unconfirmed removal is dropped rather than retried:
// the runner is plainly still registered.
func TestUnconfirmedRemovalOfABusyVMIsNotRetried(t *testing.T) {
	old := removeRetryDelay
	removeRetryDelay = 10 * time.Millisecond
	defer func() { removeRetryDelay = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busyCh := make(chan func(), 1)
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		busyCh <- onBusy
		<-vmCtx.Done()
		return nil
	})
	var calls atomic.Int64
	var onBusy func()
	rm := removerFunc(func(context.Context, string) error {
		calls.Add(1)
		onBusy()
		return fmt.Errorf("remove: %w", core.ErrRemovalUnknown)
	})
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(ctx, 1)
	onBusy = <-busyCh
	waitRunning(t, s, 1)

	s.Reconcile(ctx, 0)
	waitRemoverStopped(t, s)
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("remover called %d times for a busy VM, want 1", got)
	}
	if got := s.Running(); got != 1 {
		t.Fatalf("running=%d want 1: the busy VM must survive", got)
	}
}

func waitRemoverStopped(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		active := s.removerActive
		s.mu.Unlock()
		if !active {
			return
		}
		select {
		case <-deadline:
			t.Fatal("removeLoop never stopped")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func waitCalls(t *testing.T, n *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for n.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("calls=%d want %d", n.Load(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func waitNotRemoving(t *testing.T, s *Scheduler) {
	t.Helper()
	waitHandles(t, s, func(h *vmHandle) bool { return !h.removing })
}

func waitIdleBusy(t *testing.T, s *Scheduler) {
	t.Helper()
	waitHandles(t, s, func(h *vmHandle) bool { return !h.removing && h.busy && !h.cancelled })
}

func waitHandles(t *testing.T, s *Scheduler, ok func(*vmHandle) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		all := len(s.active) > 0
		for _, h := range s.active {
			all = all && ok(h)
		}
		s.mu.Unlock()
		if all {
			return
		}
		select {
		case <-deadline:
			t.Fatal("handles never reached the expected state")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 500 * time.Millisecond},
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{100, 30 * time.Second}, // capped
	}
	for _, tc := range cases {
		if got := backoffDelay(tc.failures); got != tc.want {
			t.Errorf("backoffDelay(%d)=%s want %s", tc.failures, got, tc.want)
		}
	}
}

// TestFailedBootBacksOff proves a golden that always fails to boot is retried at
// a paced (exponentially backing off) rate rather than in a tight loop.
func TestFailedBootBacksOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var launches int32
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		atomic.AddInt32(&launches, 1)
		return errors.New("boot failed")
	})
	s := New(Options{Max: 4, Min: 1, Provisioner: prov, JIT: jitStub{}, Logger: testLogger()})

	s.Reconcile(ctx, 0) // the warm runner alone keeps failing
	time.Sleep(1200 * time.Millisecond)
	cancel()
	s.Drain()

	// With 500ms/1s/2s backoff, only a handful of attempts fit in 1.2s; without
	// backoff this loop would run thousands of times.
	if n := atomic.LoadInt32(&launches); n < 1 || n > 6 {
		t.Fatalf("expected a small paced number of boot attempts, got %d", n)
	}
}

// slowJIT generates configs after a fixed delay, concurrently.
type slowJIT struct {
	delay time.Duration
	n     atomic.Int64
}

func (j *slowJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	time.Sleep(j.delay)
	return fmt.Sprintf("runner-%d", j.n.Add(1)), "jit", nil
}

func TestLaunchLogsPhaseTimings(t *testing.T) {
	var buf safeBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	started := make(chan struct{}, 1)
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		started <- struct{}{}
		<-ctx.Done()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	s := New(Options{Max: 1, Provisioner: prov, JIT: &slowJIT{delay: 20 * time.Millisecond}, Logger: log})
	s.Reconcile(ctx, 1)
	<-started
	cancel()
	s.Drain()
	out := buf.String()
	for _, want := range []string{`msg="launching runner"`, "jit=", "wait=", "sinceDecided="} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q:\n%s", want, out)
		}
	}
}

// TestConcurrentLaunchesDoNotSerialize guards the scheduler itself: with a
// JIT source of fixed latency that does not serialize and a provisioner that
// starts at once, 20 launches decided together all reach the provisioner
// within 1.5x of a single launch's time. (#49's serialization was in the JIT
// client; listener's TestJITSource_ConcurrentGenerateDoesNotSerialize covers
// that.)
func TestConcurrentLaunchesDoNotSerialize(t *testing.T) {
	const delay = 100 * time.Millisecond
	measure := func(n int) []time.Duration {
		var mu sync.Mutex
		var lat []time.Duration
		start := time.Now()
		all := make(chan struct{})
		prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
			mu.Lock()
			lat = append(lat, time.Since(start))
			if len(lat) == n {
				close(all)
			}
			mu.Unlock()
			<-ctx.Done()
			return nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		s := New(Options{Max: n, Provisioner: prov, JIT: &slowJIT{delay: delay}, Logger: testLogger()})
		s.Reconcile(ctx, n)
		select {
		case <-all:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d launches reached the provisioner", len(lat), n)
		}
		cancel()
		s.Drain()
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		return lat
	}
	single := measure(1)[0]
	lat := measure(20)
	p95 := lat[len(lat)*95/100-1]
	if limit := single * 3 / 2; p95 > limit {
		t.Fatalf("p95 of 20 concurrent launches = %v, single = %v; want <= %v", p95, single, limit)
	}
}

// safeBuffer is a bytes.Buffer safe for the concurrent writes of a logger.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestIdleRunnersAndCancelLost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan string, 4)
	release := make(chan struct{}) // ends a busy runner's job; shutdown leaves busy runners alone
	prov := provFunc(func(ctx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		select {
		case <-ctx.Done():
			cancelled <- name
		case <-release:
		}
		return nil
	})
	s := New(Options{Max: 2, Provisioner: prov, JIT: &slowJIT{}, Logger: testLogger()})
	s.Reconcile(ctx, 2)
	deadline := time.Now().Add(2 * time.Second)
	for len(s.IdleRunners(0)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("runners never tracked")
		}
		time.Sleep(time.Millisecond)
	}
	if got := s.IdleRunners(time.Hour); len(got) != 0 {
		t.Fatalf("IdleRunners(1h) = %v, want none that old", got)
	}
	idle := s.IdleRunners(0)
	s.MarkBusy(idle[0])
	if got := s.IdleRunners(0); len(got) != 1 || got[0] != idle[1] {
		t.Fatalf("IdleRunners after MarkBusy = %v, want only %s", got, idle[1])
	}
	if s.CancelLost(idle[0]) {
		t.Fatal("cancelled a busy runner")
	}
	if !s.CancelLost(idle[1]) {
		t.Fatal("did not cancel the lost idle runner")
	}
	if s.CancelLost(idle[1]) {
		t.Fatal("cancelled the same runner twice")
	}
	if got := <-cancelled; got != idle[1] {
		t.Fatalf("cancelled %s, want %s", got, idle[1])
	}
	if got := s.IdleRunners(0); len(got) != 0 {
		t.Fatalf("IdleRunners after CancelLost = %v", got)
	}
	cancel()
	close(release)
	s.Drain()
}

// jobProv is a provisioner whose runners take a job at once (onBusy, as the
// guest console marker or GitHub's JobStarted would) and then run until the
// test ends that job with finish. Each Launch sends its runner name on
// entered.
//
// With bootIdle set, a runner takes no job by itself: it stays idle (booting)
// until the test calls MarkBusy, and a cancel ends it, reported on cancelled.
type jobProv struct {
	entered   chan string
	cancelled chan string
	bootIdle  bool
	mu        sync.Mutex
	done      map[string]chan struct{}
}

func newJobProv() *jobProv {
	return &jobProv{entered: make(chan string, 64), cancelled: make(chan string, 64), done: map[string]chan struct{}{}}
}

func (p *jobProv) Launch(ctx context.Context, name, _ string, _ core.RunnerSpec, onBusy func()) error {
	ch := make(chan struct{})
	p.mu.Lock()
	p.done[name] = ch
	p.mu.Unlock()
	if !p.bootIdle {
		onBusy()
		p.entered <- name
		<-ch
		return nil
	}
	p.entered <- name
	select {
	case <-ch:
	case <-ctx.Done():
		p.cancelled <- name
	}
	return nil
}

func (*jobProv) Name() string { return "fake" }

// finish ends the named runner's job; its Launch then returns.
func (p *jobProv) finish(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.done[name])
}

func (p *jobProv) next(t *testing.T) string {
	t.Helper()
	select {
	case name := <-p.entered:
		return name
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a launch")
		return ""
	}
}

// noMore asserts that nothing has entered Launch. Call it only once every
// launch that could have happened has been decided, e.g. after Drain.
func (p *jobProv) noMore(t *testing.T) {
	t.Helper()
	select {
	case name := <-p.entered:
		t.Fatalf("unexpected launch %s", name)
	default:
	}
}

// message models one listener message: GitHub reports the named runners'
// jobs completed, then the desired count that no longer counts them.
func message(ctx context.Context, s *Scheduler, desired int, completed ...string) {
	for _, n := range completed {
		s.MarkCompleted(n)
	}
	s.Reconcile(ctx, desired)
}

// TestRunnerExitAtMaxLaunchesForQueuedJob is issue #79: with the tier at Max
// and jobs queued behind it, GitHub reports a job completed while its VM is
// still live, so that message launches nothing; when the VM exits, the queued
// job must get a runner then, not at the listener's next message.
func TestRunnerExitAtMaxLaunchesForQueuedJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 4) // two jobs running, two queued
	a, b := prov.next(t), prov.next(t)
	message(ctx, s, 3, a) // a's job done: b's and the two queued remain
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2: a's VM is still live, at Max", got)
	}

	prov.finish(a) // no Reconcile follows
	c := prov.next(t)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2: the re-plan replaces the exited runner, no more", got)
	}
	message(ctx, s, 2, b) // c's job and the one still queued
	prov.finish(b)
	d := prov.next(t)

	cancel()
	prov.finish(c)
	prov.finish(d)
	s.Drain()
	prov.noMore(t)
}

// TestCompletedRunnerIsNotLiveForScaleDown is issue #92: at Max 2, runner a has
// taken a job and b is still booting for the job queued behind it. The message
// reporting a's job completed carries desired 1 (the queued job). a can never
// take another job, so it must not count as live: counting it made scaleDown
// see one VM too many and cancel b, the runner the queued job needed.
func TestCompletedRunnerIsNotLiveForScaleDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	prov.bootIdle = true
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 2)
	a, b := prov.next(t), prov.next(t)
	s.MarkBusy(a)

	message(ctx, s, 1, a) // a's job done; b's job is the one still queued
	// scaleDown decides under the lock inside Reconcile: a VM it picked is
	// marked cancelled before Reconcile returns, and IdleRunners leaves it out.
	if got := s.IdleRunners(0); len(got) != 1 || got[0] != b {
		t.Fatalf("idle runners = %v, want [%s]: the completed runner was counted as live", got, b)
	}
	if got := s.Capacity(); got != 1 {
		t.Fatalf("capacity=%d want 1: b can take the queued job, a cannot take another", got)
	}

	cancel()
	prov.finish(a)
	s.Drain()
	if got := <-prov.cancelled; got != b {
		t.Fatalf("cancelled %s at shutdown, want %s", got, b)
	}
	prov.noMore(t)
}

// TestRunnerExitBeforeItsCompletionLaunchesNothing: a VM can exit before the
// message reporting its job completed. The last desired count still counts
// that job, so a re-plan would launch a runner for a job that is over. The
// exit must launch nothing; the completion message's own Reconcile then
// launches exactly what is queued.
func TestRunnerExitBeforeItsCompletionLaunchesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3) // two running, one queued
	a, b := prov.next(t), prov.next(t)

	prov.finish(a) // exits; GitHub has not reported a's completion yet
	// The re-plan is decided in the critical section that takes the exit out
	// of running, so once running reads 1 it has launched nothing.
	waitRunning(t, s, 1)
	prov.noMore(t)

	message(ctx, s, 2, a) // completion arrives: b's job and the queued one
	c := prov.next(t)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2: one launch for the one queued job", got)
	}

	cancel()
	prov.finish(b)
	prov.finish(c)
	s.Drain()
	prov.noMore(t)
}

// TestCompletionWithoutAReconcileDoesNotReplan: a VM that exits between its
// completion and the desired count of the same message would re-plan with
// the count before, which still counts its job. It must wait for that
// Reconcile instead.
func TestCompletionWithoutAReconcileDoesNotReplan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	s := New(Options{Max: 2, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	a, b := prov.next(t), prov.next(t)
	s.MarkCompleted(a) // the message's desired count has not been handled yet
	prov.finish(a)
	waitRunning(t, s, 1)
	prov.noMore(t)

	cancel()
	prov.finish(b)
	s.Drain()
	prov.noMore(t)
}

// TestRunnerExitWithDemandMetLaunchesNothing: when the runners still live
// cover the last desired count, an exit launches nothing.
func TestRunnerExitWithDemandMetLaunchesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	s := New(Options{Max: 4, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	a, b, c := prov.next(t), prov.next(t), prov.next(t)
	message(ctx, s, 2, a) // a's job completed; its VM has not exited yet

	prov.finish(a)
	waitRunning(t, s, 2)
	prov.noMore(t)

	cancel()
	prov.finish(b)
	prov.finish(c)
	s.Drain()
	prov.noMore(t)
}

// TestManyRunnersExitingAtOnceReplaceOnlyWhatIsQueued: concurrent exits each
// commit against the count the others left, so the tier never goes past Max
// and launches only for the jobs queued.
func TestManyRunnersExitingAtOnceReplaceOnlyWhatIsQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newJobProv()
	s := New(Options{Max: 4, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 6) // four running, two queued
	first := []string{prov.next(t), prov.next(t), prov.next(t), prov.next(t)}
	message(ctx, s, 2, first...) // the four complete: only the two queued remain

	for _, n := range first {
		go prov.finish(n)
	}
	second := []string{prov.next(t), prov.next(t)}
	waitRunning(t, s, 2) // all four exits settled: two replacements, no more
	prov.noMore(t)

	cancel()
	for _, n := range second {
		prov.finish(n)
	}
	s.Drain()
	prov.noMore(t)
}

// TestReplanLaunchesAtMostOnePerExit: launches that failed without a job hold
// the tier at Max through backoff, then leave it below Max with demand unmet
// (they wait for the next message). One runner exiting frees one place, so
// its re-plan launches one, not the whole shortfall.
func TestReplanLaunchesAtMostOnePerExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := newJobProv()
	var attempts atomic.Int32
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		if attempts.Add(1) <= 2 {
			return errors.New("boot failed") // no job: backs off, no re-plan
		}
		return jobs.Launch(vmCtx, name, jit, spec, onBusy)
	})
	s := New(Options{Max: 4, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 4)
	a, b := jobs.next(t), jobs.next(t)
	// The two failed launches are in their 500 ms backoff, still in running,
	// so this message (a's job done, three left) launches nothing.
	message(ctx, s, 4, a)
	if got := s.Running(); got != 4 {
		t.Fatalf("running=%d want 4: the message should find the tier at Max", got)
	}
	waitRunningWithin(t, s, 2, 5*time.Second) // both backoffs ended

	jobs.finish(a) // live 1, running 1, desired 4: plan alone would launch 3
	c := jobs.next(t)
	if got := s.Running(); got != 2 {
		t.Fatalf("running=%d want 2: one exit, one launch", got)
	}

	cancel()
	jobs.finish(b)
	jobs.finish(c)
	s.Drain()
	jobs.noMore(t)
}

// TestFailedBootWithAJobReplansOnlyAfterBackoff: a launch that fails keeps its
// place through backoff, so the re-plan its exit triggers is paced by the
// backoff rather than retrying at once.
func TestFailedBootWithAJobReplansOnlyAfterBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan string, 16)
	crash := make(chan struct{})
	prov := provFunc(func(_ context.Context, name, _ string, _ core.RunnerSpec, onBusy func()) error {
		onBusy()
		entered <- name
		<-crash // the job completes, then the VM dies with an error
		return errors.New("vm crashed")
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	message(ctx, s, 2, <-entered)
	failedAt := time.Now()
	crash <- struct{}{}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-plan never came")
	}
	// A timer never fires early, so this lower bound holds exactly.
	if gap := time.Since(failedAt); gap < backoffDelay(1) {
		t.Fatalf("re-plan after a failed boot came %s later, inside its %s backoff", gap, backoffDelay(1))
	}
	cancel()
	close(crash)
	s.Drain()
}

// TestFailedBootWithoutAJobDoesNotReplan: only a runner whose job completed
// re-plans on exit; a boot that failed before getting one waits for the next
// message, as before.
func TestFailedBootWithoutAJobDoesNotReplan(t *testing.T) {
	var attempts atomic.Int32
	prov := provFunc(func(context.Context, string, string, core.RunnerSpec, func()) error {
		attempts.Add(1)
		return errors.New("boot failed")
	})
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(context.Background(), 3)
	waitRunningWithin(t, s, 0, 5*time.Second) // through backoff to the exit
	s.Drain()
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1", got)
	}
}

// TestNoReplanAfterShutdown: once the scheduler's context is cancelled, a
// runner finishing its job launches nothing, whatever was queued.
func TestNoReplanAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	prov := newJobProv()
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	a := prov.next(t)
	message(ctx, s, 2, a)
	cancel()
	prov.finish(a)
	// Drain waits for every launch a re-plan could have started: one would
	// have been added before the exiting goroutine finished.
	s.Drain()
	prov.noMore(t)
}

// TestNoReplanDuringDrain: Drain stops re-plans even while the context is
// still live, or a tier with queued jobs could never finish draining.
func TestNoReplanDuringDrain(t *testing.T) {
	ctx := context.Background()
	prov := newJobProv()
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})

	s.Reconcile(ctx, 3)
	a := prov.next(t)
	message(ctx, s, 2, a)
	drained := make(chan struct{})
	go func() { s.Drain(); close(drained) }()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		d := s.draining
		s.mu.Unlock()
		if d {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Drain never began")
		case <-time.After(5 * time.Millisecond):
		}
	}
	prov.finish(a)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not finish: a re-plan launched during it")
	}
	prov.noMore(t)
}

func waitRunningWithin(t *testing.T, s *Scheduler, want int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		if got := s.Running(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("running=%d want %d", s.Running(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestSetMinRaisesAndLowersTheFloor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 16)
	busy := make(chan struct{})
	var launches int32
	prov := provFunc(func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		// The first runner takes a job and keeps it; the rest stay idle.
		if atomic.AddInt32(&launches, 1) == 1 {
			onBusy()
			entered <- struct{}{}
			<-busy
			return nil
		}
		entered <- struct{}{}
		<-vmCtx.Done()
		return nil
	})
	s := New(Options{Max: 6, Min: 0, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})
	defer func() { close(busy); cancel(); s.Drain() }()

	// One job; no floor yet.
	startIdle(t, ctx, s, entered, 1)

	// A window opens: 3 hot runners on top of the job.
	s.SetMin(ctx, 3)
	waitEntered(t, entered, 3)
	waitRunning(t, s, 4)
	if s.Min() != 3 {
		t.Fatalf("Min = %d", s.Min())
	}
	// The next message still counts one job; the floor stays added.
	s.Reconcile(ctx, 1)
	waitRunning(t, s, 4)

	// The window closes: the idle hot runners go, the busy one stays.
	s.SetMin(ctx, 0)
	waitRunning(t, s, 1)
	if got := atomic.LoadInt32(&launches); got != 4 {
		t.Fatalf("launches = %d, want 4", got)
	}
}

func TestSetMinBeforeAnyMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 8)
	s := New(Options{Max: 4, Min: 0, Provisioner: idleProv(entered), JIT: &jitSeq{}, Logger: testLogger()})
	defer func() { cancel(); s.Drain() }()
	s.SetMin(ctx, 2)
	waitEntered(t, entered, 2)
	waitRunning(t, s, 2)
	// Without a desired count to re-plan, lowering only moves the floor;
	// the next message scales down.
	s.SetMin(ctx, 0)
	s.Reconcile(ctx, 0)
	waitRunning(t, s, 0)
}

func waitEntered(t *testing.T, entered <-chan struct{}, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d launches entered the provisioner", i, n)
		}
	}
}
