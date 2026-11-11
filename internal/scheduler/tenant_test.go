package scheduler

import (
	"context"
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
	if got := ten.Holding(); !strings.Contains(got, "at its limit") {
		t.Fatalf("Holding() = %q while at the limit", got)
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
	if ten.Holding() != "" && ten.Running() < 2 {
		t.Fatalf("Holding() = %q below the limit", ten.Holding())
	}
	prov.finish(third)
	prov.finish(fourth)
	s[0].Drain()
	s[1].Drain()
	if ten.Running() != 0 {
		t.Fatalf("tenant counts %d runners after both tiers drained", ten.Running())
	}
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

// A tier advertises no more room than its tenant has left, so GitHub keeps
// the jobs a limit would hold queued at GitHub.
func TestTenantBoundsCapacity(t *testing.T) {
	ctx, _, prov, s, _ := tenantLab(t, 2)
	if got := s[0].Capacity(); got != 2 {
		t.Fatalf("capacity %d, want 2: the tenant's limit, below the tier's max of 4", got)
	}
	s[1].Reconcile(ctx, 1)
	b := prov.next(t)
	if got := s[0].Capacity(); got != 1 {
		t.Fatalf("capacity %d with the other tier using one place, want 1", got)
	}
	if got := s[1].Capacity(); got != 2 {
		t.Fatalf("capacity %d for the tier holding the runner, want 2 (its runner and one more)", got)
	}
	prov.finish(b)
	s[1].Drain()
}

// Without a bound the tenant holds nothing.
func TestTenantWithoutLimitHoldsNothing(t *testing.T) {
	ctx, ten, prov, s, hs := tenantLab(t, 0)
	s[0].Reconcile(ctx, 4)
	var names []string
	for range 4 {
		names = append(names, prov.next(t))
	}
	if _, _, n := hs[0].get(); n != 0 || ten.Holding() != "" {
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
