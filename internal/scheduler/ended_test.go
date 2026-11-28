package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// removals records the runners a Remover was asked to deregister, answering
// each with the next of errs (the last repeats; none is nil). When gate is
// set, each call waits for it.
type removals struct {
	mu      sync.Mutex
	names   []string
	errs    []error
	gate    chan struct{}
	inside  int
	maxSeen int
}

func (r *removals) RemoveRunner(_ context.Context, name string) error {
	r.mu.Lock()
	r.inside++
	r.maxSeen = max(r.maxSeen, r.inside)
	gate := r.gate
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inside--
	r.names = append(r.names, name)
	var err error
	if n := len(r.names) - 1; len(r.errs) > 0 {
		err = r.errs[min(n, len(r.errs)-1)]
	}
	return err
}

func (r *removals) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

// waitFor polls cond until it holds, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitRemoved waits until the Remover has answered for exactly want.
func waitRemoved(t *testing.T, rm *removals, want ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("removals %v", want), func() bool { return len(rm.got()) >= len(want) })
	if got := rm.got(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
}

// asked is how many ended runners the scheduler ever queued for removal.
// Every queueing happens in the launch's goroutine before Drain returns, so
// after Drain it is final.
func asked(s *Scheduler) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endedAsked
}

// endingProv launches VMs that end on their own when end is closed, after
// taking a job first when busy is set, or with err. Each launch reports on
// entered.
func endingProv(entered chan<- struct{}, end <-chan struct{}, busy bool, err error) provFunc {
	return func(vmCtx context.Context, name, jit string, spec core.RunnerSpec, onBusy func()) error {
		entered <- struct{}{}
		if busy {
			onBusy()
		}
		select {
		case <-end:
			return err
		case <-vmCtx.Done():
			return nil
		}
	}
}

// TestMachineEndingBeforeAJobDeregistersItsRunner: a VM that ends on its own
// while its runner waits for a job (a spot instance AWS took back) has its
// runner deregistered at once, so GitHub cannot assign it a job; one whose
// boot failed after registering too.
func TestMachineEndingBeforeAJobDeregistersItsRunner(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ended", nil},
		{"failed", errors.New("instance terminated: Server.SpotInstanceTermination")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, end := make(chan struct{}, 4), make(chan struct{})
			rm := &removals{}
			s := New(Options{Max: 2, Provisioner: endingProv(entered, end, false, tc.err), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
			s.Reconcile(context.Background(), 1)
			waitEntered(t, entered, 1)
			close(end)
			waitRemoved(t, rm, "runner-1")
			s.Drain()
		})
	}
}

// TestMachineEndingAfterACancelledJobDeregistersItsRunner: a job cancelled
// before it started is reported completed, which marks its runner busy;
// the runner never began a job and may still be handed one.
func TestMachineEndingAfterACancelledJobDeregistersItsRunner(t *testing.T) {
	entered, end := make(chan struct{}, 4), make(chan struct{})
	rm := &removals{}
	s := New(Options{Max: 2, Provisioner: endingProv(entered, end, false, nil), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(context.Background(), 1)
	waitEntered(t, entered, 1)
	s.MarkCompleted("runner-1")
	close(end)
	waitRemoved(t, rm, "runner-1")
	s.Drain()
}

// TestMachineEndingAfterItsJobKeepsItsRunner: a runner that began a job is
// left to GitHub, which removes an ephemeral runner after its job and fails
// a job whose machine went away mid-run; asking would cost a call per job.
func TestMachineEndingAfterItsJobKeepsItsRunner(t *testing.T) {
	entered, end := make(chan struct{}, 4), make(chan struct{})
	rm := &removals{}
	s := New(Options{Max: 2, Provisioner: endingProv(entered, end, true, nil), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(context.Background(), 1)
	waitEntered(t, entered, 1)
	close(end)
	s.Drain()
	if n := asked(s); n != 0 {
		t.Fatalf("queued %d removals, want none", n)
	}
}

// TestScaleDownRemovesARunnerOnce: scale-down deregisters an idle runner
// before cancelling its VM, and that VM's end asks nothing more.
func TestScaleDownRemovesARunnerOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 4)
	rm := &removals{}
	s := New(Options{Max: 2, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)
	s.Reconcile(ctx, 0)
	waitRunning(t, s, 0)
	s.Drain()
	if got, n := rm.got(), asked(s); !slices.Equal(got, []string{"runner-1"}) || n != 0 {
		t.Fatalf("removed %v and queued %d more, want [runner-1] and none", got, n)
	}
}

// TestRunnerEndingWhileScaleDownRemovesItIsNotAskedTwice: GitHub deleting
// the runner can end its VM before scale-down's removal has answered; that
// end queues no second removal.
func TestRunnerEndingWhileScaleDownRemovesItIsNotAskedTwice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, end := make(chan struct{}, 4), make(chan struct{})
	rm := &removals{gate: make(chan struct{})}
	s := New(Options{Max: 2, Provisioner: endingProv(entered, end, false, nil), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(ctx, 1)
	waitEntered(t, entered, 1)
	s.Reconcile(ctx, 0)
	waitFor(t, "scale-down's removal", func() bool {
		rm.mu.Lock()
		defer rm.mu.Unlock()
		return rm.inside == 1
	})
	close(end) // the VM ends while GitHub is answering
	waitRunning(t, s, 0)
	close(rm.gate)
	waitFor(t, "scale-down's removal to answer", func() bool { return len(rm.got()) == 1 })
	s.Drain()
	if got, n := rm.got(), asked(s); !slices.Equal(got, []string{"runner-1"}) || n != 0 {
		t.Fatalf("removed %v and queued %d more, want [runner-1] and none", got, n)
	}
}

// TestShutdownLeavesIdleRunnersAlone: shutdown cancels an idle VM without
// deregistering it, as before.
func TestShutdownLeavesIdleRunnersAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 4)
	rm := &removals{}
	s := New(Options{Max: 2, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)
	cancel()
	s.Drain()
	if n := asked(s); n != 0 {
		t.Fatalf("queued %d removals, want none", n)
	}
}

// TestCancelledIdleRunnerIsNotQueued: a VM the scheduler cancels itself
// without deregistering (here, one whose runner GitHub no longer lists) has
// its own reason, and queues no removal.
func TestCancelledIdleRunnerIsNotQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 4)
	rm := &removals{}
	s := New(Options{Max: 2, Provisioner: idleProv(entered), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	startIdle(t, ctx, s, entered, 1)
	if !s.CancelLost("runner-1") {
		t.Fatal("CancelLost refused an idle runner")
	}
	waitRunning(t, s, 0)
	s.Drain()
	if n := asked(s); n != 0 {
		t.Fatalf("queued %d removals, want none", n)
	}
}

// TestEndedRunnersAreRemovedOneAtATime: machines that end together (a burst
// of spot interruptions) are deregistered in turn, not all at once.
func TestEndedRunnersAreRemovedOneAtATime(t *testing.T) {
	entered, end := make(chan struct{}, 4), make(chan struct{})
	rm := &removals{gate: make(chan struct{})}
	s := New(Options{Max: 3, Provisioner: endingProv(entered, end, false, nil), JIT: &jitSeq{}, Remover: rm, Logger: testLogger()})
	s.Reconcile(context.Background(), 3)
	waitEntered(t, entered, 3)
	close(end)
	s.Drain() // all three have ended and queued
	if n := asked(s); n != 3 {
		t.Fatalf("queued %d removals, want 3", n)
	}
	close(rm.gate)
	waitFor(t, "three removals", func() bool { return len(rm.got()) == 3 })
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.maxSeen != 1 {
		t.Fatalf("%d removals ran at once, want 1", rm.maxSeen)
	}
}

// TestEndedRunnerRemovalLogs: the log says what GitHub's answer means, and
// an answer GitHub could not confirm is asked once more.
func TestEndedRunnerRemovalLogs(t *testing.T) {
	unknown := fmt.Errorf("remove runner %q: %w", "runner-1", core.ErrRemovalUnknown)
	for _, tc := range []struct {
		errs  []error
		calls int
		want  string
	}{
		{nil, 1, "made sure GitHub holds no runner for a machine that ended before taking a job"},
		{[]error{fmt.Errorf("remove runner %q: %w", "runner-1", core.ErrRunnerBusy)}, 1, "GitHub holds a job for the runner of a machine that ended"},
		{[]error{errors.New("look up runner: 502")}, 1, "could not deregister the runner of a machine that ended"},
		{[]error{unknown, nil}, 2, "made sure GitHub holds no runner"},
		{[]error{unknown, unknown}, 2, "could not deregister the runner of a machine that ended"},
	} {
		t.Run(fmt.Sprint(tc.calls, tc.want), func(t *testing.T) {
			var buf safeBuffer
			entered, end := make(chan struct{}, 4), make(chan struct{})
			rm := &removals{errs: tc.errs}
			s := New(Options{Max: 2, Provisioner: endingProv(entered, end, false, nil), JIT: &jitSeq{}, Remover: rm,
				Logger: slog.New(slog.NewTextHandler(&buf, nil))})
			s.removeRetryDelay = time.Millisecond
			s.Reconcile(context.Background(), 1)
			waitEntered(t, entered, 1)
			close(end)
			waitFor(t, "the removal's log line", func() bool { return strings.Contains(buf.String(), tc.want) })
			s.Drain()
			if got := rm.got(); len(got) != tc.calls {
				t.Fatalf("asked GitHub %d times, want %d", len(got), tc.calls)
			}
		})
	}
}
