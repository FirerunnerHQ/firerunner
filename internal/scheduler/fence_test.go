package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// fenceAt is a fence that closes once its reason is set.
type fenceAt struct{ why atomic.Pointer[string] }

func (f *fenceAt) close(why string) { f.why.Store(&why) }

func (f *fenceAt) fence() string {
	if p := f.why.Load(); p != nil {
		return *p
	}
	return ""
}

// A closed fence holds every launch with its reason and kind, and stops no
// runner already launched: the next owner's reaper ends those once their
// jobs are done.
func TestFenceHoldsLaunchesAndStopsNothing(t *testing.T) {
	ctx, ten, prov, s, stops := suspendLab(t)
	f := &fenceAt{}
	ten.SetFence(f.fence)
	s[0].Reconcile(ctx, 1)
	a := recv(t, prov.entered, "a launch while the fence is open")

	f.close("the lease ended at 12:00:20")
	s[0].Reconcile(ctx, 2)
	s[1].Reconcile(ctx, 1)
	n, h := s[0].Holding()
	if n != 1 || h.Kind != HoldFenced || !strings.Contains(h.Why, "the lease ended at 12:00:20") {
		t.Fatalf("held %d %+v", n, h)
	}
	if n, h := s[1].Holding(); n != 1 || h.Kind != HoldFenced {
		t.Fatalf("tier b held %d %+v", n, h)
	}
	if r0, r1 := s[0].Running(), s[1].Running(); r0 != 1 || r1 != 0 {
		t.Fatalf("running %d and %d behind a closed fence, want 1 and 0", r0, r1)
	}
	if len(*stops) != 0 || prov.cause(a) != nil {
		t.Fatalf("the fence stopped a runner: %+v %v", *stops, prov.cause(a))
	}
}

// A launch granted before the fence closed, still getting its registration,
// is stopped before the provisioner asks for its machine.
func TestFenceStopsALaunchOnItsWay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ten := NewTenant("acme", 0)
	f := &fenceAt{}
	ten.SetFence(f.fence)
	prov := newCauseProv()
	jit := enterJIT{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(Options{Max: 1, Provisioner: prov, JIT: jit, Logger: testLogger(), Tenant: ten})
	s.Reconcile(ctx, 1)
	<-jit.entered // granted, and getting its JIT config
	f.close("the lease ended")
	close(jit.release)
	r := recv(t, prov.entered, "the launch")
	recv(t, prov.ended, "the launch to end at once")
	if err := prov.cause(r); !errors.Is(err, errFenced) {
		t.Fatalf("cause %v, want the fence", err)
	}
}

// enterJIT says when a registration begins, and waits to be released.
type enterJIT struct{ entered, release chan struct{} }

func (j enterJIT) Generate(context.Context, core.RunnerSpec) (string, string, error) {
	close(j.entered)
	<-j.release
	return "runner", "jit", nil
}

// A fence that closes between the grant and the registration stops the
// launch before it registers a runner with GitHub.
func TestFenceStopsALaunchBeforeItRegisters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ten := NewTenant("acme", 0)
	var asked atomic.Int32
	// Open for the grant, closed from the next question on.
	ten.SetFence(func() string {
		if asked.Add(1) == 1 {
			return ""
		}
		return "the lease ended"
	})
	prov := newCauseProv()
	jit := &jitSeq{}
	s := New(Options{Max: 1, Provisioner: prov, JIT: jit, Logger: testLogger(), Tenant: ten})
	s.Reconcile(ctx, 1)
	waitUntil(t, "the launch to end", func() bool { return s.Running() == 0 && asked.Load() >= 2 })
	if n := jit.n.Load(); n != 0 {
		t.Fatalf("registered %d runner(s) behind a closed fence", n)
	}
	if ten.Running() != 0 {
		t.Fatalf("the tenant still counts %d", ten.Running())
	}
}
