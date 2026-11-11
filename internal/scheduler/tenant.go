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
// A tier whose launches it holds back waits in line, and places go to the
// tiers in line in turn: a tier given a place goes to the back while it still
// waits, and a tier not in line comes after every tier that is. So a job
// held on one tier is never passed over for long by another tier's queue,
// nor by a warm-runner top-up. When a runner ends, the tenant wakes its
// schedulers, so a job held on another tier starts when that runner ends
// rather than at its own tier's next message.
//
// A tenant can also be held, which refuses every launch, or suspended, which
// also stops every runner it has (see Suspend).
//
// It is safe for concurrent use.
type Tenant struct {
	name string
	max  int

	mu      sync.Mutex
	running int
	scheds  []*Scheduler
	// line is the schedulers whose launches the tenant holds back, in the
	// order places go to them.
	line []*Scheduler
	// heldWhy, when set, refuses every launch, for that reason; suspended
	// says whether a suspension set it.
	heldWhy   string
	suspended bool
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

// take grants s up to n launches and returns how many it granted and, when
// it granted fewer, why the rest wait. The places free go first to the
// schedulers ahead of s in line; one not in line is behind them all. A
// scheduler in line that is granted a place moves to the back.
func (t *Tenant) take(s *Scheduler, n int) (int, string) {
	if n <= 0 {
		return 0, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.heldWhy != "" {
		return 0, t.heldWhy
	}
	granted := n
	if t.max > 0 {
		ahead := slices.Index(t.line, s)
		if ahead < 0 {
			ahead = len(t.line)
		}
		granted = max(0, min(n, t.max-t.running-ahead))
	}
	t.running += granted
	if i := slices.Index(t.line, s); granted > 0 && i >= 0 {
		t.line = append(slices.Delete(t.line, i, i+1), s)
	}
	if granted < n {
		return granted, t.atLimit()
	}
	return granted, ""
}

// wait puts s in line, at the back, while it has launches held back, and
// takes it out when it has none. A scheduler already in line keeps its place.
func (t *Tenant) wait(s *Scheduler, waiting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	i := slices.Index(t.line, s)
	switch {
	case waiting && i < 0:
		t.line = append(t.line, s)
	case !waiting && i >= 0:
		t.line = slices.Delete(t.line, i, i+1)
	}
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
	if t.max > 0 && t.heldWhy == "" {
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
	if t.heldWhy != "" {
		return 0
	}
	if t.max == 0 {
		return -1
	}
	return max(0, t.max-t.running)
}

// Suspend refuses every launch of the tenant's tiers and stops every runner
// they have, busy or not, so its machines end now: a job running fails. A
// launch already on its way is stopped as it starts. The reason is the
// operator's; Resume lifts it.
//
// It reports whether it changed anything: suspending again for the same
// reason does not.
func (t *Tenant) Suspend(reason string) bool {
	why := fmt.Sprintf("%s is suspended: %s", t.who(), reason)
	t.mu.Lock()
	if t.suspended && t.heldWhy == why {
		t.mu.Unlock()
		return false
	}
	t.heldWhy, t.suspended = why, true
	scheds := slices.Clone(t.scheds)
	t.mu.Unlock()
	for _, s := range scheds {
		s.stopAll(Stop{Kind: StopSuspended, Reason: why})
	}
	return true
}

// Hold refuses every launch of the tenant's tiers, for why, and stops
// nothing; Resume lifts it.
func (t *Tenant) Hold(why string) bool {
	held := fmt.Sprintf("%s is held: %s", t.who(), why)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.heldWhy == held {
		return false
	}
	t.heldWhy, t.suspended = held, false
	return true
}

// Resume lifts a suspension or a hold, and wakes the tenant's schedulers to
// launch for the jobs that waited.
// It reports whether there was anything to lift.
func (t *Tenant) Resume() bool {
	t.mu.Lock()
	was := t.heldWhy != ""
	t.heldWhy, t.suspended = "", false
	wake := slices.Clone(t.scheds)
	t.mu.Unlock()
	if !was {
		return false
	}
	for _, s := range wake {
		go s.retry()
	}
	return true
}

// State says whether the tenant is suspended, or held, and why.
func (t *Tenant) State() (suspended bool, why string) {
	if t == nil {
		return false, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.suspended, t.heldWhy
}

// suspension is why the tenant is suspended, or "".
func (t *Tenant) suspension() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.suspended {
		return ""
	}
	return t.heldWhy
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
