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

	"github.com/firerunnerhq/firerunner/internal/core"
)

// removals records the runners a Remover was asked to deregister.
type removals struct {
	mu    sync.Mutex
	names []string
	err   error
}

func (r *removals) RemoveRunner(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	return r.err
}

func (r *removals) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

// endingProv launches VMs that end on their own when told on end, after
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
// boot failed after registering too. Drain waits for the removal.
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
			s.Drain()
			if got := rm.got(); !slices.Equal(got, []string{"runner-1"}) {
				t.Fatalf("removed %v, want [runner-1]", got)
			}
		})
	}
}

// TestMachineEndingAfterItsJobKeepsItsRunner: a runner that took a job is
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
	if got := rm.got(); len(got) != 0 {
		t.Fatalf("removed %v, want none", got)
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
	if got := rm.got(); !slices.Equal(got, []string{"runner-1"}) {
		t.Fatalf("removed %v, want [runner-1] once", got)
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
	if got := rm.got(); len(got) != 0 {
		t.Fatalf("removed %v, want none", got)
	}
}

// TestMachineEndingWithAJobAssignedSaysSo: GitHub refusing the removal
// because it had assigned the runner a job before the guest took it is the
// case this exists for: the log says what happens to that job.
func TestMachineEndingWithAJobAssignedSaysSo(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("remove runner %q: %w", "runner-1", core.ErrRunnerBusy), "GitHub had assigned a job to a runner whose machine ended"},
		{errors.New("look up runner: 502"), "the offline-runner sweep removes it later"},
		{nil, "deregistered the runner of a machine that ended before taking a job"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var buf safeBuffer
			entered, end := make(chan struct{}, 4), make(chan struct{})
			rm := &removals{err: tc.err}
			s := New(Options{Max: 2, Provisioner: endingProv(entered, end, false, nil), JIT: &jitSeq{}, Remover: rm,
				Logger: slog.New(slog.NewTextHandler(&buf, nil))})
			s.Reconcile(context.Background(), 1)
			waitEntered(t, entered, 1)
			close(end)
			s.Drain()
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("log lacks %q:\n%s", tc.want, buf.String())
			}
		})
	}
}
