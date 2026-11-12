package scheduler

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

// holds records what a scheduler reports through OnHold.
type holds struct {
	mu   sync.Mutex
	jobs int
	why  string
	n    int
}

func (h *holds) record(jobs int, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.jobs, h.why = jobs, why
	h.n++
}

func (h *holds) get() (int, string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.jobs, h.why, h.n
}

// tenantLab is two tiers of one tenant, each allowed 4 runners, sharing a
// tenant bound of limit.
func tenantLab(t *testing.T, limit int) (context.Context, *Tenant, *jobProv, [2]*Scheduler, [2]*holds) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ten := NewTenant("acme", limit)
	prov := newJobProv()
	jit := &jitSeq{}
	var scheds [2]*Scheduler
	var hs [2]*holds
	for i := range scheds {
		hs[i] = &holds{}
		scheds[i] = New(Options{Max: 4, Provisioner: prov, JIT: jit, Logger: testLogger(), Tenant: ten, OnHold: hs[i].record})
	}
	return ctx, ten, prov, scheds, hs
}

// A tenant's runners are bounded across its tiers, the launches it holds back
// are reported with the reason, and a runner that ends frees its place for a
// job waiting on another tier, without waiting for that tier's next message.
func TestTenantBoundsRunnersAcrossTiers(t *testing.T) {
	ctx, ten, prov, s, hs := tenantLab(t, 2)

	s[0].Reconcile(ctx, 3)
	a1, a2 := prov.next(t), prov.next(t)
	if got := s[0].Running(); got != 2 {
		t.Fatalf("tier a running %d, want 2: the tenant's limit", got)
	}
	jobs, why, _ := hs[0].get()
	if jobs != 1 || !strings.Contains(why, `tenant "acme" is at its limit of 2 runner(s)`) {
		t.Fatalf("tier a holds %d (%q), want 1 with the reason", jobs, why)
	}

	s[1].Reconcile(ctx, 1)
	if got := s[1].Running(); got != 0 {
		t.Fatalf("tier b launched %d while its tenant is at its limit", got)
	}
	if jobs, why, _ := hs[1].get(); jobs != 1 || why == "" {
		t.Fatalf("tier b holds %d (%q), want 1", jobs, why)
	}
	prov.noMore(t)

	// a1's job ends (GitHub reports it, then its runner exits): one place
	// frees, and one held job, on either tier, takes it now.
	message(ctx, s[0], 2, a1)
	prov.finish(a1)
	third := prov.next(t)
	waitUntil(t, "two runners again", func() bool { return ten.Running() == 2 })
	if s[0].Running()+s[1].Running() != 2 {
		t.Fatalf("running %d + %d, want 2", s[0].Running(), s[1].Running())
	}
	prov.noMore(t)

	// The next end lets the last held job go, and nothing is held after.
	message(ctx, s[0], 1, a2)
	prov.finish(a2)
	fourth := prov.next(t)
	waitUntil(t, "no launch held", func() bool {
		j0, _, _ := hs[0].get()
		j1, _, _ := hs[1].get()
		return j0 == 0 && j1 == 0
	})
	if s[0].Running() != 1 || s[1].Running() != 1 {
		t.Fatalf("running %d + %d, want 1 + 1: both tiers' jobs got runners", s[0].Running(), s[1].Running())
	}
	prov.finish(third)
	prov.finish(fourth)
	s[0].Drain()
	s[1].Drain()
	if ten.Running() != 0 {
		t.Fatalf("tenant counts %d runners after both tiers drained", ten.Running())
	}
}

// Places go to the tiers waiting in turn: a tier with a queue does not keep
// every place its own runners free while another tier's job waits. Tier a
// waits first and gets the first place; then tier b, though a's queue has a
// job left and it was a's runner that ended.
func TestTiersWaitingTakeTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ten := NewTenant("acme", 1)
	jit := &jitSeq{}
	provA, provB := newJobProv(), newJobProv()
	a := New(Options{Max: 4, Provisioner: provA, JIT: jit, Logger: testLogger(), Tenant: ten})
	b := New(Options{Max: 4, Provisioner: provB, JIT: jit, Logger: testLogger(), Tenant: ten})
	a.Reconcile(ctx, 3) // one runs, two wait
	a1 := provA.next(t)
	b.Reconcile(ctx, 1) // one waits, behind a
	if n, _ := b.Held(); n != 1 {
		t.Fatalf("tier b holds %d", n)
	}

	message(ctx, a, 2, a1)
	provA.finish(a1)
	a2 := provA.next(t) // a waited first
	waitUntil(t, "the place taken", func() bool { return ten.Running() == 1 })
	provB.noMore(t)

	message(ctx, a, 1, a2)
	provA.finish(a2)
	b1 := provB.next(t) // now b's turn, though a still has a job queued
	waitUntil(t, "the place taken", func() bool { return ten.Running() == 1 })
	provA.noMore(t)
	if n, _ := a.Held(); n != 1 {
		t.Fatalf("tier a holds %d, want its last job", n)
	}

	message(ctx, b, 0, b1)
	provB.finish(b1)
	a3 := provA.next(t) // and a's again
	provA.finish(a3)
	a.Drain()
	b.Drain()
}

// A tier that stops leaves its tenant's line, so it is never ahead of a tier
// still serving.
func TestDrainedTierLeavesTheLine(t *testing.T) {
	ctx, ten, prov, s, _ := tenantLab(t, 1)
	s[0].Reconcile(ctx, 1)
	a := prov.next(t)
	s[1].Reconcile(ctx, 1)
	ten.mu.Lock()
	inLine := slices.Contains(ten.line, s[1])
	ten.mu.Unlock()
	if !inLine {
		t.Fatal("a tier with a launch held back is not in line")
	}
	s[1].Drain()
	ten.mu.Lock()
	inLine = slices.Contains(ten.line, s[1])
	ten.mu.Unlock()
	if inLine {
		t.Fatal("a drained tier is still in line")
	}
	prov.finish(a)
	s[0].Drain()
}

// The warm floor's top-up is bounded by the tenant too, and comes after a job
// waiting on another tier.
func TestWarmTopUpIsBoundedByTheTenant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ten := NewTenant("", 1)
	prov := newJobProv()
	s := New(Options{Max: 4, Provisioner: prov, JIT: &jitSeq{}, Logger: testLogger(), Tenant: ten})
	s.SetMin(ctx, 2) // before any message: the top-up fills the floor
	a := prov.next(t)
	if got := s.Running(); got != 1 {
		t.Fatalf("running %d, want 1: the install's limit", got)
	}
	prov.noMore(t)
	cancel() // no top-up after the exit
	prov.finish(a)
	s.Drain()
}

// A hold clears when the jobs it held are no longer wanted.
func TestTenantHoldClearsWhenDemandFalls(t *testing.T) {
	ctx, _, prov, s, hs := tenantLab(t, 1)
	s[0].Reconcile(ctx, 2)
	a := prov.next(t)
	if jobs, _, _ := hs[0].get(); jobs != 1 {
		t.Fatalf("held %d, want 1", jobs)
	}
	s[0].Reconcile(ctx, 1) // the queued job was cancelled
	if jobs, why, n := hs[0].get(); jobs != 0 || why != "" || n != 2 {
		t.Fatalf("hold = %d %q after %d reports; want it cleared", jobs, why, n)
	}
	prov.finish(a)
	s[0].Drain()
	prov.noMore(t)
}

// A tier advertises its own room, whatever its tenant has left: a job past
// the tenant's limit is then assigned here and held with the reason, rather
// than kept in GitHub's queue where nothing says why it waits.
func TestTenantDoesNotCutCapacity(t *testing.T) {
	ctx, _, prov, s, hs := tenantLab(t, 1)
	s[1].Reconcile(ctx, 1)
	b := prov.next(t)
	if got := s[0].Capacity(); got != 4 {
		t.Fatalf("capacity %d with the tenant at its limit, want the tier's 4", got)
	}
	s[0].Reconcile(ctx, 1) // GitHub assigned the job: it is held, with the reason
	if jobs, why, _ := hs[0].get(); jobs != 1 || why == "" {
		t.Fatalf("held %d %q", jobs, why)
	}
	prov.finish(b)
	recv(t, prov.entered, "the held job's runner")
}

// Without a bound the tenant holds nothing.
func TestTenantWithoutLimitHoldsNothing(t *testing.T) {
	ctx, _, prov, s, hs := tenantLab(t, 0)
	s[0].Reconcile(ctx, 4)
	var names []string
	for range 4 {
		names = append(names, prov.next(t))
	}
	if _, _, n := hs[0].get(); n != 0 {
		t.Fatalf("a tenant with no limit reported a hold")
	}
	if got := s[0].Capacity(); got != 4 {
		t.Fatalf("capacity %d, want the tier's own 4", got)
	}
	for _, n := range names {
		prov.finish(n)
	}
	s[0].Drain()
}

// A raised bound launches for the jobs held at the old one at once, without
// waiting for a runner to end or another message; a lowered one stops
// nothing, and holds launches until enough runners have ended.
func TestTenantSetMax(t *testing.T) {
	ctx, ten, prov, s, hs := tenantLab(t, 1)
	s[0].Reconcile(ctx, 3)
	a := prov.next(t)
	if jobs, _, _ := hs[0].get(); jobs != 2 {
		t.Fatalf("held %d, want 2", jobs)
	}
	if ten.SetMax(1) {
		t.Fatal("SetMax to the same bound reported a change")
	}

	if !ten.SetMax(3) || ten.Max() != 3 {
		t.Fatalf("SetMax(3): max %d", ten.Max())
	}
	b, c := prov.next(t), prov.next(t)
	waitUntil(t, "nothing held", func() bool { j, _, _ := hs[0].get(); return j == 0 })

	// Lowered below what runs: every runner keeps going, and the next job
	// waits until two have ended.
	ten.SetMax(1)
	if got := s[0].Running(); got != 3 {
		t.Fatalf("running %d after lowering the bound, want 3: it stops nothing", got)
	}
	message(ctx, s[0], 3, a)
	prov.finish(a)
	waitUntil(t, "a gone", func() bool { return ten.Running() == 2 })
	s[0].Reconcile(ctx, 3)
	prov.noMore(t)
	if jobs, why, _ := hs[0].get(); jobs != 1 || !strings.Contains(why, "limit of 1 runner(s)") {
		t.Fatalf("held %d (%q), want 1 at the new limit", jobs, why)
	}

	// No bound: everything held launches.
	ten.SetMax(0)
	d := prov.next(t)
	for _, n := range []string{b, c, d} {
		prov.finish(n)
	}
	s[0].Drain()
	s[1].Drain()
}
