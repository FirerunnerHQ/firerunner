package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// suspendLab is two tiers of one tenant with no runner bound, whose runners
// run until cancelled, keeping their cancel causes.
func suspendLab(t *testing.T) (context.Context, *Tenant, *causeProv, [2]*Scheduler, *[]Stop) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ten := NewTenant("acme", 0)
	prov := newCauseProv()
	jit := &jitSeq{}
	var stops []Stop
	var scheds [2]*Scheduler
	for i := range scheds {
		scheds[i] = New(Options{Max: 4, Provisioner: prov, JIT: jit, Logger: testLogger(), Tenant: ten,
			OnStop: func(st Stop) { stops = append(stops, st) }})
	}
	return ctx, ten, prov, scheds, &stops
}

// Suspending a tenant stops every runner of every tier it has at once, busy
// or idle, with the operator's reason as the cause the provisioner ends the
// machine with; then holds its launches with the reason, and a resume
// launches for the jobs that waited.
func TestSuspendStopsEveryRunnerAndHolds(t *testing.T) {
	ctx, ten, prov, s, stops := suspendLab(t)
	s[0].Reconcile(ctx, 2)
	s[1].Reconcile(ctx, 1)
	a1, a2 := recv(t, prov.entered, "a launch"), recv(t, prov.entered, "a launch")
	b1 := recv(t, prov.entered, "a launch")
	s[0].MarkBusy(a1) // one busy runner: a suspension ends its job too

	ten.Suspend("abuse report")
	ended := map[string]bool{}
	for range 3 {
		ended[recv(t, prov.ended, "a runner to end")] = true
	}
	for _, r := range []string{a1, a2, b1} {
		var sc StopCause
		if err := prov.cause(r); !ended[r] || !errors.As(err, &sc) || sc.Kind != StopSuspended || !strings.Contains(sc.Reason, `tenant "acme" is suspended: abuse report`) {
			t.Fatalf("%s: ended %v, cause %v", r, ended[r], err)
		}
	}
	if len(*stops) != 3 {
		t.Fatalf("stops = %+v", *stops)
	}
	if susp, why := ten.State(); !susp || !strings.Contains(why, "abuse report") {
		t.Fatalf("state = %v %q", susp, why)
	}

	// GitHub's next counts still want runners: none launches, and the reason
	// is what a held job records.
	s[0].Reconcile(ctx, 2)
	s[1].Reconcile(ctx, 1)
	if n, why := s[0].Held(); n != 2 || !strings.Contains(why, "is suspended: abuse report") {
		t.Fatalf("held %d %q", n, why)
	}
	if got := s[0].Capacity(); got != 0 {
		t.Fatalf("capacity %d while suspended", got)
	}
	select {
	case r := <-prov.entered:
		t.Fatalf("launched %s while suspended", r)
	default:
	}

	ten.Resume()
	for range 3 {
		recv(t, prov.entered, "a launch after the resume")
	}
	waitUntil(t, "no launch held", func() bool {
		n0, _ := s[0].Held()
		n1, _ := s[1].Held()
		return n0 == 0 && n1 == 0
	})
	if susp, why := ten.State(); susp || why != "" {
		t.Fatalf("state after resume = %v %q", susp, why)
	}
}

// A launch decided before the suspension, still getting its registration,
// is stopped as it starts: nothing it boots outlives the suspension.
func TestSuspendStopsALaunchOnItsWay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ten := NewTenant("acme", 0)
	prov := newCauseProv()
	gate := make(chan struct{})
	s := New(Options{Max: 1, Provisioner: prov, JIT: gateJIT{gate}, Logger: testLogger(), Tenant: ten})
	s.Reconcile(ctx, 1) // committed, waiting for its JIT config
	ten.Suspend("abuse")
	close(gate)
	r := recv(t, prov.entered, "the launch")
	recv(t, prov.ended, "the launch to end at once")
	var sc StopCause
	if err := prov.cause(r); !errors.As(err, &sc) || sc.Kind != StopSuspended {
		t.Fatalf("cause %v", err)
	}
}

// A hold (the tenant's state not read yet) refuses launches but stops
// nothing; a resume lets them go.
func TestHoldStopsNothing(t *testing.T) {
	ctx, ten, prov, s, stops := suspendLab(t)
	s[0].Reconcile(ctx, 1)
	a := recv(t, prov.entered, "a launch")
	ten.Hold("its state has not been read yet")
	s[0].Reconcile(ctx, 2)
	if n, why := s[0].Held(); n != 1 || !strings.Contains(why, `tenant "acme" is held: its state has not been read yet`) {
		t.Fatalf("held %d %q", n, why)
	}
	if len(*stops) != 0 || prov.cause(a) != nil {
		t.Fatalf("a hold stopped a runner: %+v", *stops)
	}
	ten.Resume()
	recv(t, prov.entered, "the held launch")
}
