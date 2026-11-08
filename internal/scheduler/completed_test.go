package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// stopTimer captures what MarkCompleted schedules, so a test runs it when it
// chooses instead of waiting out the grace.
type stopTimer struct {
	grace time.Duration
	fns   []func()
}

func (st *stopTimer) install(s *Scheduler) {
	s.afterFunc = func(d time.Duration, f func()) { st.grace = d; st.fns = append(st.fns, f) }
}

// syncBuffer is a log sink safe for the scheduler's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// waitUntil polls cond until it holds, failing after two seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// completedLab is a one-slot scheduler whose runners return from Launch
// only when cancelled or finished, with its stops captured and its log kept.
func completedLab(t *testing.T, grace time.Duration) (context.Context, *Scheduler, *jobProv, *stopTimer, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	prov := newJobProv()
	prov.bootIdle = true
	var logs syncBuffer
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, CompletedGrace: grace,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	st := &stopTimer{}
	st.install(s)
	return ctx, s, prov, st, &logs
}

// A guest that keeps running after its job completed (its job ran as root
// and stopped the shutdown) is stopped once the grace passes, rather than
// holding its slot until its lifetime ends. Its exit is healthy churn, so
// the next queued job gets a runner at once.
func TestCompletedRunnerStillRunningIsStopped(t *testing.T) {
	ctx, s, prov, st, _ := completedLab(t, 0)
	s.Reconcile(ctx, 2) // one job running, one queued
	a := prov.next(t)
	s.MarkBusy(a)
	s.MarkCompleted(a)
	s.MarkCompleted(a) // a repeated report schedules nothing more
	if len(st.fns) != 1 || st.grace != DefaultCompletedGrace {
		t.Fatalf("scheduled %d stops after %s, want 1 after %s", len(st.fns), st.grace, DefaultCompletedGrace)
	}
	s.Reconcile(ctx, 1) // the message's desired count no longer counts a's job
	st.fns[0]()
	select {
	case got := <-prov.cancelled:
		if got != a {
			t.Fatalf("cancelled %s, want %s", got, a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner still running after the grace")
	}
	b := prov.next(t) // the queued job's runner, with no backoff first
	if b == a {
		t.Fatalf("relaunched %s", a)
	}
}

// A runner whose job was cancelled before it started is reported completed
// but may still take the next job, so it is not stopped.
func TestCompletedRunnerThatNeverStartedIsNotStopped(t *testing.T) {
	ctx, s, prov, st, _ := completedLab(t, 0)
	s.Reconcile(ctx, 1)
	s.MarkCompleted(prov.next(t))
	if len(st.fns) != 0 {
		t.Fatalf("scheduled %d stops for a runner that never started its job", len(st.fns))
	}
}

// A runner that exits within the grace is left alone when the stop fires.
func TestCompletedRunnerThatExitedIsNotStopped(t *testing.T) {
	ctx, s, prov, st, logs := completedLab(t, 0)
	s.Reconcile(ctx, 1)
	a := prov.next(t)
	s.MarkBusy(a)
	s.MarkCompleted(a)
	prov.finish(a)
	waitUntil(t, "the runner to exit", func() bool { return !s.Tracks(a) })
	st.fns[0]()
	if strings.Contains(logs.String(), "stopping a runner") {
		t.Fatalf("stopped a runner that had already exited:\n%s", logs)
	}
}

// A negative grace never stops a runner.
func TestCompletedGraceNegativeNeverStops(t *testing.T) {
	ctx, s, prov, st, _ := completedLab(t, -1)
	s.Reconcile(ctx, 1)
	a := prov.next(t)
	s.MarkBusy(a)
	s.MarkCompleted(a)
	if len(st.fns) != 0 {
		t.Fatalf("scheduled %d stops with a negative grace", len(st.fns))
	}
}
