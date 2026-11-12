package scheduler

import (
	"fmt"
	"maps"
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
// also stops every runner it has (see Suspend). Its plan can narrow its bound
// and leave tiers out (see SetPlan).
//
// It is safe for concurrent use.
type Tenant struct {
	name string

	mu sync.Mutex
	// max is the bound the tenant was given (SetMax), and plan the one its
	// plan sets (SetPlan); the narrower holds, 0 for none.
	max, plan int
	// tiers, when not nil, is the only tiers the plan lets launch.
	tiers   map[string]bool
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

// HoldKind says what holds a tier's launches back.
type HoldKind string

// Hold kinds.
const (
	// HoldLimit: the tenant is at its own runner limit.
	HoldLimit HoldKind = "limit"
	// HoldPlanLimit: the tenant is at its plan's runner limit, narrower
	// than its own.
	HoldPlanLimit HoldKind = "plan_limit"
	// HoldPlanTier: the tenant's plan leaves the tier out.
	HoldPlanTier HoldKind = "plan_tier"
	// HoldHeld: the tenant is held (Tenant.Hold).
	HoldHeld HoldKind = "held"
	// HoldSuspended: the tenant is suspended (Tenant.Suspend).
	HoldSuspended HoldKind = "suspended"
)

// Hold is why a tier's launches wait: its kind, and the reason in words.
// The zero Hold holds nothing.
type Hold struct {
	Kind HoldKind
	Why  string
}

// take grants s up to n launches and returns how many it granted and, when
// it granted fewer, why the rest wait. The places free go first to the
// schedulers ahead of s in line; one not in line is behind them all. A
// scheduler in line that is granted a place moves to the back.
func (t *Tenant) take(s *Scheduler, n int) (int, Hold) {
	if n <= 0 {
		return 0, Hold{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.heldWhy != "" {
		if t.suspended {
			return 0, Hold{HoldSuspended, t.heldWhy}
		}
		return 0, Hold{HoldHeld, t.heldWhy}
	}
	if !t.allows(s) {
		return 0, Hold{HoldPlanTier, fmt.Sprintf("%s's plan does not include tier %q", t.who(), s.opts.Tier)}
	}
	granted := n
	if limit := t.limit(); limit > 0 {
		// Only a tier the plan lets launch is ahead: one it leaves out
		// would wait in line for ever.
		ahead := 0
		for _, x := range t.line {
			if x == s {
				break
			}
			if t.allows(x) {
				ahead++
			}
		}
		granted = max(0, min(n, limit-t.running-ahead))
	}
	t.running += granted
	if i := slices.Index(t.line, s); granted > 0 && i >= 0 {
		t.line = append(slices.Delete(t.line, i, i+1), s)
	}
	if granted < n {
		return granted, t.atLimit()
	}
	return granted, Hold{}
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

func (t *Tenant) atLimit() Hold {
	limit := t.limit()
	if limit == t.plan && limit != t.max {
		return Hold{HoldPlanLimit, fmt.Sprintf("%s is at its plan's limit of %d runner(s) at once", t.who(), limit)}
	}
	return Hold{HoldLimit, fmt.Sprintf("%s is at its limit of %d runner(s) at once", t.who(), limit)}
}

// limit is the bound in force: the narrower of the tenant's and its plan's,
// 0 for none. Callers must hold t.mu.
func (t *Tenant) limit() int {
	switch {
	case t.max == 0:
		return t.plan
	case t.plan == 0:
		return t.max
	}
	return min(t.max, t.plan)
}

// allows says whether the plan lets s's tier launch. Callers must hold t.mu.
func (t *Tenant) allows(s *Scheduler) bool {
	return t.tiers == nil || t.tiers[s.opts.Tier]
}

// raised says whether a bound changed from was to now lets more launch.
func raised(was, now int) bool {
	return now != was && (now == 0 || (was != 0 && now > was))
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
	if t.limit() > 0 && t.heldWhy == "" {
		wake = slices.Clone(t.scheds)
	}
	t.mu.Unlock()
	for _, s := range wake {
		go s.retry()
	}
}

// SetMax changes the tenant's bound to at most max runners at once, 0 for
// none; its plan's, when narrower, still holds. A lower bound stops no
// runner: launches wait until enough have ended. A higher one wakes the
// tenant's schedulers to launch for the jobs that waited. It reports whether
// the bound changed.
func (t *Tenant) SetMax(max int) bool {
	t.mu.Lock()
	if t.max == max {
		t.mu.Unlock()
		return false
	}
	was := t.limit()
	t.max = max
	t.wakeIf(raised(was, t.limit()))
	return true
}

// Plan is what a tenant's plan lets it run, beyond its own bound.
type Plan struct {
	// Runners, when positive, bounds its runners at once; the narrower of
	// this and the tenant's own bound holds.
	Runners int
	// Tiers, when not nil, is the only tiers that launch; a job on another
	// waits, with the reason.
	Tiers []string
}

// SetPlan sets what the tenant's plan lets it run. Like SetMax, it stops no
// runner: a tier left out keeps its runners, and launches nothing more. Any
// change wakes the tenant's schedulers, unless it is held: one that lets more
// launch frees places, and one that leaves a tier out moves the tiers behind
// it up the line and changes why its own jobs wait. Set a hold before a
// plan, so that no launch slips in between. It reports whether anything
// changed.
func (t *Tenant) SetPlan(p Plan) bool {
	var tiers map[string]bool
	if p.Tiers != nil {
		tiers = make(map[string]bool, len(p.Tiers))
		for _, n := range p.Tiers {
			tiers[n] = true
		}
	}
	runners := max(0, p.Runners)
	t.mu.Lock()
	if t.plan == runners && (t.tiers == nil) == (tiers == nil) && maps.Equal(t.tiers, tiers) {
		t.mu.Unlock()
		return false
	}
	t.plan, t.tiers = runners, tiers
	t.wakeIf(true)
	return true
}

// wakeIf unlocks t.mu, which the caller holds, and wakes the tenant's
// schedulers when more may launch and the tenant is not held.
func (t *Tenant) wakeIf(more bool) {
	var wake []*Scheduler
	if more && t.heldWhy == "" {
		wake = slices.Clone(t.scheds)
	}
	t.mu.Unlock()
	for _, s := range wake {
		go s.retry()
	}
}

// Plan is the plan last set, its tiers sorted.
func (t *Tenant) Plan() Plan {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := Plan{Runners: t.plan}
	if t.tiers != nil {
		p.Tiers = slices.AppendSeq(make([]string, 0, len(t.tiers)), maps.Keys(t.tiers))
		slices.Sort(p.Tiers)
	}
	return p
}

// Max is the bound in force: at most this many runners at once, the
// narrower of the tenant's own and its plan's, 0 for none.
func (t *Tenant) Max() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.limit()
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
