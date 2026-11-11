package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// causeProv is a provisioner whose runners run until cancelled, and which
// keeps each one's cancel cause, as the EC2 provisioner logs it.
type causeProv struct {
	entered chan string
	mu      sync.Mutex
	causes  map[string]error
	ended   chan string
}

func newCauseProv() *causeProv {
	return &causeProv{entered: make(chan string, 16), ended: make(chan string, 16), causes: map[string]error{}}
}

func (p *causeProv) Launch(ctx context.Context, name, _ string, _ core.RunnerSpec, _ func()) error {
	p.entered <- name
	<-ctx.Done()
	p.mu.Lock()
	p.causes[name] = context.Cause(ctx)
	p.mu.Unlock()
	p.ended <- name
	return nil
}

func (*causeProv) Name() string { return "fake" }

func (p *causeProv) cause(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.causes[name]
}

func recv(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

// maxJobLab is a one-runner tier with a job limit, its timers captured.
func maxJobLab(t *testing.T) (context.Context, *Scheduler, *causeProv, *stopTimer, *[]Stop) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	prov := newCauseProv()
	var stops []Stop
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger(),
		MaxJob: 30 * time.Minute, OnStop: func(st Stop) { stops = append(stops, st) }})
	st := &stopTimer{}
	st.install(s)
	return ctx, s, prov, st, &stops
}

// A runner whose job runs past the limit is ended, with the reason as its
// cause, and is reported; its place goes to the next job.
func TestJobPastTheLimitIsStopped(t *testing.T) {
	ctx, s, prov, st, stops := maxJobLab(t)
	s.Reconcile(ctx, 2) // one job runs, one waits behind the tier's max
	a := recv(t, prov.entered, "a launch")
	if len(st.fns) != 0 {
		t.Fatal("a timer for a runner with no job yet")
	}
	s.MarkBusy(a)
	s.MarkBusy(a) // GitHub's JobStarted after the guest's report: one job, one timer
	if len(st.fns) != 1 || st.grace != 30*time.Minute {
		t.Fatalf("timers %d for %s, want 1 for the limit", len(st.fns), st.grace)
	}
	st.fns[0]()
	if got := recv(t, prov.ended, "the runner to end"); got != a {
		t.Fatalf("ended %s, want %s", got, a)
	}
	var sc StopCause
	if err := prov.cause(a); !errors.As(err, &sc) || sc.Kind != StopJobDuration || sc.Reason != "its job ran past the limit of 30m0s" {
		t.Fatalf("cause = %v, want the job limit", err)
	}
	if len(*stops) != 1 || (*stops)[0].Runner != a || (*stops)[0].Kind != StopJobDuration {
		t.Fatalf("stops = %+v", *stops)
	}
	// The stop frees the place: the waiting job gets a runner once GitHub's
	// next count (the stopped job failed, one still queued) arrives.
	message(ctx, s, 1, a)
	recv(t, prov.entered, "the next launch")
}

// A job that completed before the limit is not stopped by it: its runner is
// shutting down, and the completion grace covers one that does not.
func TestCompletedJobIsNotStoppedByTheLimit(t *testing.T) {
	ctx, s, prov, st, stops := maxJobLab(t)
	s.Reconcile(ctx, 1)
	a := recv(t, prov.entered, "a launch")
	s.MarkBusy(a)
	s.MarkCompleted(a)
	if len(st.fns) != 2 {
		t.Fatalf("timers %d, want the limit's and the completion grace's", len(st.fns))
	}
	st.fns[0]() // the limit's
	select {
	case got := <-prov.ended:
		t.Fatalf("the limit ended %s after its job completed", got)
	default:
	}
	if len(*stops) != 0 {
		t.Fatalf("stops = %+v", *stops)
	}
}

// Without a limit, a job starting arms nothing.
func TestNoJobLimitArmsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := newCauseProv()
	s := New(Options{Max: 1, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger()})
	st := &stopTimer{}
	st.install(s)
	s.Reconcile(ctx, 1)
	a := recv(t, prov.entered, "a launch")
	s.MarkBusy(a)
	if len(st.fns) != 0 {
		t.Fatalf("timers %d with no job limit", len(st.fns))
	}
}
