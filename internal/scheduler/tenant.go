package scheduler

import (
	"fmt"
	"slices"
	"sync"
)

// Tenant bounds the runners of every tier one tenant is served: the
// schedulers that share it launch at most Max runners at once between them.
// A launch counts from the moment a scheduler commits to it until it leaves
// that scheduler's running count, as Max counts for one tier, so launches in
// backoff and runners tearing down count too.
//
// When a runner ends and frees a place, the tenant asks the schedulers whose
// launches it held to plan again, so a job waiting on another tier's runner
// starts when that runner ends rather than at its own tier's next message.
//
// It is safe for concurrent use.
type Tenant struct {
	name string
	max  int

	mu      sync.Mutex
	running int
	scheds  []*Scheduler
}

// NewTenant returns the bound for one tenant's tiers: at most max runners at
// once, 0 for no bound. name says whose runners they are in reasons and
// logs; empty is the install's own.
func NewTenant(name string, max int) *Tenant {
	return &Tenant{name: name, max: max}
}

// who names the tenant in a reason.
func (t *Tenant) who() string {
	if t.name == "" {
		return "the install"
	}
	return fmt.Sprintf("tenant %q", t.name)
}

// join adds a scheduler to the ones it may wake.
func (t *Tenant) join(s *Scheduler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scheds = append(t.scheds, s)
}

// take grants up to n launches and returns how many it granted and, when it
// granted fewer, why the rest wait.
func (t *Tenant) take(n int) (int, string) {
	if n <= 0 {
		return 0, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	granted := n
	if t.max > 0 {
		granted = max(0, min(n, t.max-t.running))
	}
	t.running += granted
	if granted < n {
		return granted, t.atLimit()
	}
	return granted, ""
}

func (t *Tenant) atLimit() string {
	return fmt.Sprintf("%s is at its limit of %d runner(s) at once", t.who(), t.max)
}

// release returns one launch taken earlier, and wakes the tenant's
// schedulers, without waiting for them: the caller may hold its own
// scheduler's lock, which a woken scheduler takes in turn (the lock order is
// a scheduler's, then the tenant's). A scheduler holding no launch wakes to
// nothing.
func (t *Tenant) release() {
	t.mu.Lock()
	t.running--
	var wake []*Scheduler
	if t.max > 0 {
		wake = slices.Clone(t.scheds)
	}
	t.mu.Unlock()
	for _, s := range wake {
		go s.retry()
	}
}

// free is how many more launches the tenant would grant now; -1 is no bound.
func (t *Tenant) free() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.max == 0 {
		return -1
	}
	return max(0, t.max-t.running)
}

// Holding returns why a job assigned to one of the tenant's tiers now would
// wait for a runner, or "" when it would not: the tenant is at its limit. A
// nil Tenant holds nothing.
func (t *Tenant) Holding() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.max > 0 && t.running >= t.max {
		return t.atLimit()
	}
	return ""
}

// Running is how many launches the tenant has granted that have not ended;
// 0 for a nil Tenant.
func (t *Tenant) Running() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
