// Package scheduler reconciles the number of running ephemeral microVMs to the
// demand reported by GitHub, bounded by a per-host capacity.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
)

// JITSource generates a just-in-time runner registration for a new microVM.
// The actions/scaleset listener implements this against the GitHub API.
type JITSource interface {
	Generate(ctx context.Context, spec core.RunnerSpec) (name, jitConfig string, err error)
}

// RunnerRemover deregisters an idle runner from GitHub before scale-down kills
// its microVM. GitHub may already have assigned the runner a job in the few
// seconds before its guest logs "Running job:"; killing the VM then strands
// that job on a dead runner until GitHub reaps the session (~5 min) and
// re-queues it. Deregistering first closes the race: GitHub refuses with
// core.ErrRunnerBusy while a job is assigned, and after a successful removal it
// can assign the runner nothing.
//
// RemoveRunner must return in bounded time on its own; the scheduler waits for
// its answer, since only that answer says whether the VM may be cancelled. It
// returns core.ErrRemovalUnknown when it cannot tell whether GitHub removed
// the runner; the scheduler then keeps the VM removing and retries. Any other
// error must mean the runner is still registered.
type RunnerRemover interface {
	RemoveRunner(ctx context.Context, name string) error
}

// Options configures a Scheduler.
type Options struct {
	Max         int
	Min         int
	Spec        core.RunnerSpec
	Provisioner provisioner.Provisioner
	JIT         JITSource
	// Remover, when set, deregisters each idle VM with GitHub before
	// scale-down cancels it. nil cancels at once, with the race described on
	// RunnerRemover.
	Remover RunnerRemover
	Logger  *slog.Logger
	// Pending, when set, is shared by every Scheduler on the host so that each
	// tier's Capacity accounts for launches the other tiers have committed to
	// but the provisioner has not yet drawn a slot for. nil gives the
	// Scheduler a private counter.
	Pending *PendingLaunches
	// CompletedGrace is how long a runner may keep running after GitHub
	// reports its job completed before the scheduler stops it. The guest is
	// meant to shut itself down at once, but the job ran as root in it and
	// can stop that, holding the slot (or the instance) until its lifetime
	// ends. 0 means DefaultCompletedGrace; negative never stops it.
	CompletedGrace time.Duration
	// Tenant, when set, bounds this tier's runners together with the other
	// tiers of its tenant (see Tenant).
	Tenant *Tenant
	// Tier names this tier to its Tenant, whose plan may leave it out (see
	// Tenant.SetPlan).
	Tier string
	// MaxJob, when positive, ends a runner whose job has run this long: the
	// job fails, and the runner's machine goes. The job's start is the first
	// MarkBusy: the guest's own report, or GitHub's JobStarted.
	MaxJob time.Duration
	// OnStop, when set, is told of each runner the scheduler ends for a
	// tenant's limit. It is called with the scheduler's lock held.
	OnStop func(Stop)
	// OnHold, when set, is told how many launches the tenant holds back and
	// why, each time that changes; 0 and "" when none are. It is called with
	// the scheduler's lock held, so it must not call back into it.
	OnHold func(jobs int, reason string)
}

// DefaultCompletedGrace is Options.CompletedGrace's default: far longer than
// a runner takes to report and exit after its job.
const DefaultCompletedGrace = 2 * time.Minute

// PendingLaunches counts microVMs a Scheduler has added to its running total
// whose Launch has not yet been entered, so they are in neither the
// provisioner's free-slot count nor its in-use count. Capacity subtracts them
// from the free count to keep the two numbers a consistent snapshot: between
// Reconcile and the provisioner acquiring a slot (a JIT registration round
// trip to GitHub) a launching VM would otherwise be counted twice.
type PendingLaunches struct{ n atomic.Int64 }

func (p *PendingLaunches) add(n int) { p.n.Add(int64(n)) }
func (p *PendingLaunches) done()     { p.n.Add(-1) }

// Count returns the number of launches committed but not yet entered.
func (p *PendingLaunches) Count() int { return int(p.n.Load()) }

// Scheduler launches one ephemeral microVM per assigned job, up to Max
// concurrently.
type Scheduler struct {
	opts      Options
	afterFunc func(time.Duration, func())
	// removeRetryDelay paces a removal whose outcome GitHub could not
	// confirm (defaultRemoveRetryDelay). A field set at construction, not a
	// package variable, so a test can shorten it without racing a removeLoop
	// another test left running.
	removeRetryDelay time.Duration
	mu               sync.Mutex
	running          int
	wg               sync.WaitGroup
	// active tracks in-flight microVMs by runner name so a shutdown drain can
	// tell an idle warm-pool VM (cancel it immediately) from one running a job
	// (let it finish). MarkBusy flips a VM to busy when its guest dequeues a job.
	active map[string]*vmHandle
	// failures counts consecutive unhealthy microVM boots so the warm-pool
	// replenish path can back off exponentially instead of hot-looping a broken
	// golden (which burns a JIT registration per attempt). Reset on any healthy
	// launch. Guarded by mu.
	failures int
	// backingOff counts launch goroutines sleeping in backoff after a failed
	// boot: still in running (so the pacing and replenish bookkeeping hold)
	// but with no VM behind them. live leaves them out. Guarded by mu.
	backingOff int
	// pending is this tier's share of opts.Pending: launches in running that
	// have not yet drawn a slot. Guarded by mu.
	pending int
	// removeQ holds VMs marked removing that removeLoop has not started on,
	// oldest first; removerActive is true while a removeLoop runs. Guarded by
	// mu.
	removeQ       []string
	removerActive bool
	// lastJobs and lastCtx are the job count and context of the latest
	// Reconcile, kept so a runner that finishes its job, or a change of Min,
	// can re-run the plan (see replan, SetMin). hasDesired is false until the
	// first Reconcile. Guarded by mu.
	lastJobs   int
	lastCtx    context.Context
	hasDesired bool
	// reconciles counts Reconcile calls, so an exit can tell whether a
	// desired count arrived after its job's completion was seen (see
	// MarkCompleted). Guarded by mu.
	reconciles uint64
	// draining is set by Drain: from then on nothing launches, so the wait
	// cannot be extended by a launch it has to wait for in turn. Guarded by mu.
	draining bool
	// held is how many launches the latest plan wanted that the tenant held
	// back, and hold why. Guarded by mu.
	held int
	hold Hold
}

// vmHandle is the shutdown-relevant state of one in-flight microVM.
type vmHandle struct {
	cancel context.CancelCauseFunc
	busy   bool
	// since is when the runner was handed to the provisioner.
	since time.Time
	// cancelled guards against a scale-down (or shutdown) cancelling the same
	// idle VM twice across rapid Reconcile calls before its launch goroutine has
	// removed it from the registry.
	cancelled bool
	// removing marks an idle VM scale-down has chosen for deregistration with
	// GitHub, queued or in flight. It is not live: scale-down has already
	// counted it as gone. It returns to the pool (busy or idle) if GitHub
	// refuses the removal, or if demand rises while it is still queued.
	removing bool
	// deregistering is set once removeLoop has asked GitHub to remove the
	// runner, and stays set while the answer is unknown: GitHub may have
	// deleted the runner, so its runner may exit non-zero because of us. A
	// definite answer that left the runner registered clears it.
	deregistering bool
	// completed is set when GitHub reported the runner's job completed, while
	// the VM was still tracked. completedAt is s.reconciles at that moment:
	// a Reconcile after it carried a desired count that no longer counts the
	// job. Only then may the VM's exit re-plan (see replan).
	completed   bool
	completedAt uint64
	// jobStarted is when the runner was first marked busy: its job began.
	jobStarted time.Time
}

// Stop is a runner the scheduler ended because of its tenant's limits.
type Stop struct {
	Runner string
	// Kind is StopJobDuration or StopSuspended.
	Kind   string
	Reason string
}

// Kinds of Stop.
const (
	// StopJobDuration is a runner whose job ran past Options.MaxJob.
	StopJobDuration = "job_duration"
	// StopSuspended is a runner of a tenant suspended (Tenant.Suspend).
	StopSuspended = "suspended"
)

// StopCause is the cause a runner's context is cancelled with when the
// scheduler stops it for its tenant's limits: a provisioner may give its
// reason when it ends the machine.
type StopCause struct{ Stop }

func (c StopCause) Error() string { return c.Reason }

// New returns a Scheduler.
func New(o Options) *Scheduler {
	if o.Pending == nil {
		o.Pending = &PendingLaunches{}
	}
	if o.CompletedGrace == 0 {
		o.CompletedGrace = DefaultCompletedGrace
	}
	s := &Scheduler{
		opts:             o,
		active:           make(map[string]*vmHandle),
		afterFunc:        afterFunc,
		removeRetryDelay: defaultRemoveRetryDelay,
	}
	if o.Tenant != nil {
		o.Tenant.join(s)
	}
	return s
}

// afterFunc is time.AfterFunc without its *Timer; tests replace it to run
// the function when they choose.
func afterFunc(d time.Duration, f func()) { time.AfterFunc(d, f) }

// plan returns how many new runners to launch given the desired count reported
// by GitHub, the number of microVMs able to take a job (live), the number of
// launch goroutines in flight (running, which also counts VMs tearing down and
// launches sleeping in backoff), and the capacity limit. Demand is measured
// against live, so a job is never left waiting on a VM that cannot serve it;
// the launch is bounded by max - running, so a failing golden can hold at most
// max attempts in backoff at once. It returns only the scale-up amount (never
// negative); scaling down when desired drops is handled separately by
// scaleDown, which cancels idle VMs rather than relying solely on them
// self-terminating.
func plan(desired, live, running, max int) int {
	want := desired - live
	if want < 0 {
		want = 0
	}
	if avail := max - running; want > avail {
		want = avail
	}
	if want < 0 {
		want = 0
	}
	return want
}

// live returns how many in-flight microVMs can take a job: running minus the
// launches sleeping in backoff (no VM behind them), the VMs scaleDown has
// cancelled (tearing down) or is deregistering, and the VMs whose job GitHub
// reported completed (shutting down; each runs one job). Counting a completed
// VM made scaleDown see one VM too many when the completion message's desired
// count still held a queued job, and remove the idle VM booting for it.
// Callers must hold s.mu.
func (s *Scheduler) live() int {
	n := s.running - s.backingOff
	for _, h := range s.active {
		if h.cancelled || h.removing || h.completed {
			n--
		}
	}
	return n
}

// Reconcile launches microVMs to meet the desired count, bounded by capacity,
// and cancels idle VMs when desired drops below the number running. It is safe
// for concurrent use; each launched runner handles exactly one job.
//
// jobs is the count GitHub asks for; the desired count adds Min, the warm
// runners kept with no job queued, under the same lock SetMin changes it
// with, so a schedule moving Min never races a message.
//
// The listener calls it on every message from GitHub. Between messages, a
// runner that finishes its job re-runs the plan with the desired count given
// here (see replan), so a job queued at Max starts when a slot frees rather
// than at the next message.
func (s *Scheduler) Reconcile(ctx context.Context, jobs int) {
	s.mu.Lock()
	s.lastJobs, s.lastCtx, s.hasDesired = jobs, ctx, true
	s.reconciles++
	act := s.reconcileLocked(ctx)
	s.mu.Unlock()
	act()
}

// reconcileLocked plans for the last job count plus Min and returns what to
// do once s.mu is released: log, launch, start the remover. Callers must hold
// s.mu.
func (s *Scheduler) reconcileLocked(ctx context.Context) func() {
	desired := s.desired()
	reclaimed := s.reclaimQueued(desired)
	n := 0
	if !s.draining {
		n = s.grant(plan(desired, s.live(), s.running, s.opts.Max))
	}
	s.commit(n)
	// Counted under the lock, so a Drain that begins once it is released
	// waits for these launches rather than racing their Add.
	s.wg.Add(n)
	running := s.running
	floor := s.opts.Min
	stopped := 0
	startRemover := false
	if n == 0 {
		stopped, startRemover = s.scaleDown(desired)
	}
	return func() {
		if reclaimed > 0 {
			s.opts.Logger.Info("demand rose: kept runners queued for removal", "desired", desired, "reclaimed", reclaimed)
		}
		if n > 0 {
			s.opts.Logger.Info("scaling up", "desired", desired, "launching", n, "running", running, "max", s.opts.Max)
			for i := 0; i < n; i++ {
				go s.launchOne(ctx, time.Now())
			}
			return
		}
		if stopped > 0 {
			s.opts.Logger.Info("scaling down", "desired", desired, "cancelling", stopped, "running", running, "min", floor)
		}
		if startRemover {
			go s.removeLoop(ctx)
		}
	}
}

// grant asks the tenant for the launches a plan wants, records any it holds
// back, and returns how many may go. Callers must hold s.mu.
func (s *Scheduler) grant(want int) int {
	if s.opts.Tenant == nil {
		return want
	}
	n, h := s.opts.Tenant.take(s, want)
	s.setHeld(want-n, h)
	return n
}

// setHeld records that the tenant holds n wanted launches back, and why, and
// reports a change. Callers must hold s.mu.
func (s *Scheduler) setHeld(n int, h Hold) {
	if n <= 0 || h.Why == "" {
		n, h = 0, Hold{}
	}
	if n == s.held && h == s.hold {
		return
	}
	s.held, s.hold = n, h
	if s.opts.Tenant != nil {
		s.opts.Tenant.wait(s, n > 0)
	}
	if n > 0 {
		s.opts.Logger.Info("launches held", "jobs", n, "kind", h.Kind, "reason", h.Why)
	} else {
		s.opts.Logger.Info("launches no longer held")
	}
	if s.opts.OnHold != nil {
		s.opts.OnHold(n, h.Why)
	}
}

// Held returns how many launches the tenant holds back, and why.
func (s *Scheduler) Held() (int, string) {
	n, h := s.Holding()
	return n, h.Why
}

// Holding returns how many launches the tenant holds back, and the hold.
func (s *Scheduler) Holding() (int, Hold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held, s.hold
}

// retry plans again with the last desired count, when a place among its
// tenant's runners has freed or a hold was lifted, if the tier has launches
// waiting: ones the tenant held back, or the re-plan an exit whose job
// completed asked for (see replan). It does what a Reconcile with the same
// count would.
func (s *Scheduler) retry() {
	s.mu.Lock()
	if s.held > 0 && (s.draining || (s.hasDesired && s.lastCtx.Err() != nil)) {
		// The tier stopped: it no longer waits in its tenant's line.
		s.setHeld(0, Hold{})
	}
	if s.held == 0 || !s.hasDesired || s.draining || s.lastCtx.Err() != nil {
		s.mu.Unlock()
		return
	}
	act := s.reconcileLocked(s.lastCtx)
	s.mu.Unlock()
	act()
}

// desired is the last job count plus Min. Callers must hold s.mu.
func (s *Scheduler) desired() int { return s.lastJobs + s.opts.Min }

// commit adds n launches to running and to the pending counts, before their
// goroutines start. Callers must hold s.mu.
func (s *Scheduler) commit(n int) {
	s.running += n
	s.pending += n
	s.opts.Pending.add(n)
}

// replan re-runs Reconcile's plan with the last desired count after a runner
// whose job completed has exited, and reports how many launches it committed
// (0 or 1) along with the context to launch them under. GitHub sends a message
// when a job completes, but the VM that ran it still holds its place in
// running then, so at Max plan launches nothing for the jobs queued behind it;
// the VM goes away seconds later and, without this, nothing looks again until
// the next message, up to a long poll (~50 s) later. Callers must hold s.mu,
// in the same critical section that took the exited launch out of running.
//
// It launches only what Reconcile would launch for the same count, with the
// same accounting, and never more than the one place the exit freed. It
// returns a queued removal to the pool before booting anew, as Reconcile does,
// and it never scales down: an exit only lowers live, so there is no new
// excess to cancel.
//
// Only the exit of a VM whose job completion GitHub reported, followed by a
// Reconcile, triggers it (launchOne checks; see vmHandle.completed). Then the
// desired count it re-plans with already leaves that job out, so a launch can
// only serve a job still queued. A VM that exits before its completion is
// reported does not re-plan: the desired count still counts its job, and
// re-planning would launch a runner for a job that is over. The completion
// message runs its own Reconcile, which sees the VM gone, so that order
// costs neither a delay nor a runner.
//
// The desired count is then stale only by jobs that left the queue between
// messages: taken by a sibling scale set, or cancelled. A runner launched for
// one is idle, and the next message (a long poll returns at least every ~50
// s) scales it down. That costs at most one runner per exit: the cap of one
// holds however many runners exit at once, since each exit commits under the
// lock against the count the previous ones left.
//
// A failed boot holds its place in running through backoff and replans, if
// its job completed, only when that ends, so backoff paces it; an idle VM
// that exits by itself (a lost runner, a lifetime cap) waits for the next
// message like before, so a VM that exits at once without a job cannot spin
// a launch loop. Shutdown (ctx cancelled) and Drain stop it.
func (s *Scheduler) replan(ctx context.Context) (int, context.Context) {
	if !s.hasDesired || s.draining || ctx.Err() != nil || s.lastCtx.Err() != nil {
		return 0, nil
	}
	if s.reclaimQueued(s.desired()) > 0 {
		s.opts.Logger.Info("runner exited: kept a runner queued for removal", "desired", s.desired())
		return 0, nil
	}
	want := plan(s.desired(), s.live(), s.running, s.opts.Max)
	n := min(1, want)
	if s.opts.Tenant != nil {
		// The place this exit freed is the tenant's: a tier ahead in its
		// line gets it first (see Tenant).
		granted, h := s.opts.Tenant.take(s, n)
		switch {
		case h.Why != "":
			s.setHeld(want-granted, h)
		case granted == want:
			s.setHeld(0, Hold{})
		}
		// Granted one of several: the rest wait for the next message, as
		// without a tenant, and the hold stays as the last plan left it.
		n = granted
	}
	s.commit(n)
	if n > 0 {
		s.opts.Logger.Info("runner exited: scaling up", "desired", s.desired(), "launching", n, "running", s.running, "max", s.opts.Max)
	}
	return n, s.lastCtx
}

// scaleDown picks idle (non-busy, not already cancelling or deregistering)
// microVMs so the live count converges toward max(desired, Min), returning how
// many it picked. Without a Remover it cancels them at once; with one it only
// marks them removing and queues them for removeLoop, reporting whether the
// caller must start that loop. Callers must hold s.mu; the cancel is invoked
// under the lock (a context.CancelFunc is non-blocking and re-enters the
// scheduler only asynchronously) so a MarkBusy cannot slip between the busy
// check and the cancel. Busy VMs are never cancelled — they are running a job
// and left to finish and self-terminate; the warm-pool floor (Min) is always
// preserved so a scale-to-zero still keeps pre-booted capacity.
func (s *Scheduler) scaleDown(desired int) (int, bool) {
	floor := desired
	if floor < s.opts.Min {
		floor = s.opts.Min
	}
	excess := s.live() - floor
	if excess <= 0 {
		return 0, false
	}
	picked := 0
	for name, h := range s.active {
		if excess <= 0 {
			break
		}
		if h.busy || h.cancelled || h.removing {
			continue
		}
		if s.opts.Remover != nil {
			h.removing = true
			s.removeQ = append(s.removeQ, name)
		} else {
			h.cancelled = true
			h.cancel(nil)
		}
		picked++
		excess--
	}
	start := len(s.removeQ) > 0 && !s.removerActive
	if start {
		s.removerActive = true
	}
	return picked, start
}

// reclaimQueued returns VMs still waiting in removeQ to the idle pool when
// demand has risen past what is live, so a quick reversal keeps its warm VMs
// instead of deregistering them one by one and booting replacements. The
// removal removeLoop is running cannot be recalled; it is already off the
// queue. Nor can one GitHub may already have done (an unconfirmed removal
// waiting to retry): its runner may be gone, so it stays queued. Returns how
// many it reclaimed. Callers must hold s.mu.
func (s *Scheduler) reclaimQueued(desired int) int {
	need := max(desired, s.opts.Min) - s.live()
	reclaimed := 0
	var asked []string
	for need > 0 && len(s.removeQ) > 0 {
		last := len(s.removeQ) - 1
		name := s.removeQ[last]
		s.removeQ = s.removeQ[:last]
		h := s.active[name]
		switch {
		case h == nil || !h.removing || h.cancelled:
		case h.deregistering:
			asked = append(asked, name)
		default:
			h.removing = false
			need--
			reclaimed++
		}
	}
	slices.Reverse(asked)
	s.removeQ = append(s.removeQ, asked...)
	return reclaimed
}

// defaultRemoveRetryDelay is how long removeLoop waits before retrying a
// removal whose outcome GitHub could not confirm.
const defaultRemoveRetryDelay = 2 * time.Second

// removeLoop deregisters queued VMs one at a time, so each removal runs only
// when it can, a queued one can still be reclaimed, and a VM never has two
// attempts in flight. It waits for each answer rather than racing a deadline:
// an abandoned call could still succeed and leave a deregistered VM counted as
// live. The Remover bounds each attempt (see RunnerRemover).
//
// Shutdown ends the loop: the drain cancels idle VMs itself and lets busy ones
// finish, so a removal still queued or awaiting a retry has nothing left to
// protect, and retrying it without the delay would only hammer GitHub.
func (s *Scheduler) removeLoop(ctx context.Context) {
	for {
		s.mu.Lock()
		name, ok := "", false
		if ctx.Err() == nil {
			name, ok = s.nextRemoval()
		}
		if !ok {
			s.removeQ = nil
			s.removerActive = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		err := s.opts.Remover.RemoveRunner(context.WithoutCancel(ctx), name)
		if s.settleRemoval(name, err) {
			select {
			case <-ctx.Done():
			case <-time.After(s.removeRetryDelay):
			}
		}
	}
}

// nextRemoval pops the next queued VM that is still waiting to be removed. A
// VM whose guest dequeued a job while queued leaves the queue for good: its
// runner is plainly still registered and busy. Callers must hold s.mu.
func (s *Scheduler) nextRemoval() (string, bool) {
	for len(s.removeQ) > 0 {
		name := s.removeQ[0]
		s.removeQ = s.removeQ[1:]
		h := s.active[name]
		if h == nil || !h.removing || h.cancelled {
			continue
		}
		if h.busy {
			h.removing = false
			continue
		}
		h.deregistering = true
		return name, true
	}
	return "", false
}

// settleRemoval acts on GitHub's answer for a VM removeLoop tried to remove:
// cancel it once GitHub can no longer hand it a job; mark it busy if GitHub
// refused because a job is already assigned; return it to idle on a failure
// that left the runner registered; and requeue it, still removing, when GitHub
// could not say whether the removal happened. Reports whether it requeued.
func (s *Scheduler) settleRemoval(name string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil || h.cancelled {
		return false // exited, or shutdown reaped it, while we waited
	}
	if errors.Is(err, core.ErrRemovalUnknown) && !h.busy {
		s.opts.Logger.Warn("runner removal unconfirmed; retrying", "runner", name, "err", err)
		s.removeQ = append(s.removeQ, name)
		return true
	}
	h.removing = false
	if err != nil {
		h.deregistering = false // GitHub answered: the runner is still registered
	}
	switch {
	case errors.Is(err, core.ErrRunnerBusy):
		h.busy = true
		s.opts.Logger.Info("scale-down spared runner: GitHub already assigned it a job", "runner", name)
	case err != nil:
		s.opts.Logger.Warn("deregister runner before scale-down", "runner", name, "err", err)
	case h.busy:
		// The guest dequeued a job meanwhile.
	default:
		h.cancelled = true
		h.cancel(nil)
	}
	return false
}

// launchOne launches one runner. queuedAt is when the launch was decided, so
// the log can split the time to a running VM into its phases.
func (s *Scheduler) launchOne(ctx context.Context, queuedAt time.Time) {
	defer s.wg.Done()
	// jobDone is set once the VM has exited if GitHub reported its job
	// completed and a Reconcile has run since: its exit frees a place a
	// queued job may be waiting for (see replan).
	jobDone := false
	finished := false
	replanned, replanCtx := 0, context.Context(nil)
	// finish takes this launch out of running and, if its job is done,
	// re-plans in the same critical section (callers hold s.mu), so a
	// concurrent Reconcile, exit or Running sees both or neither.
	finish := func() {
		if !finished {
			finished = true
			s.running--
			if s.opts.Tenant != nil {
				s.opts.Tenant.release()
			}
			if jobDone {
				replanned, replanCtx = s.replan(ctx)
			}
		}
	}
	// startReplanned starts what finish committed, once s.mu is released.
	// Every call is before wg.Done, so the WaitGroup counter stays >=1 while
	// it Adds, avoiding a concurrent-Add-during-Drain race.
	startReplanned := func() {
		for ; replanned > 0; replanned-- {
			s.wg.Add(1)
			go s.launchOne(replanCtx, time.Now())
		}
	}
	// This runs before wg.Done above (defers are LIFO), so the WaitGroup counter
	// stays >=1 while maintainMinimum may Add a replacement too.
	defer func() {
		s.mu.Lock()
		finish()
		s.mu.Unlock()
		startReplanned()
		s.maintainMinimum(ctx)
	}()

	// The launch stops being pending once it is about to enter the provisioner
	// (Firecracker.Launch draws its slot as its first act) or when we bail
	// before getting there.
	entered := false
	enter := func() {
		if !entered {
			entered = true
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
			s.opts.Pending.done()
		}
	}
	defer enter()

	// Once shutdown has begun the listener has stopped accepting work and Drain
	// is waiting to reap; don't boot a brand-new microVM into a draining host.
	if ctx.Err() != nil {
		return
	}

	jitStart := time.Now()
	name, jit, err := s.opts.JIT.Generate(context.WithoutCancel(ctx), s.opts.Spec)
	jitDur := time.Since(jitStart)
	if err != nil {
		s.opts.Logger.Error("generate JIT config", "err", err)
		// A failed JIT generation is a GitHub API error; without pacing, the
		// maintainMinimum defer relaunches at once and spins a tight retry loop.
		// Settle the reservation first: backoff already keeps this launch out of
		// Capacity, and a slot it will never draw must not stay spoken for.
		enter()
		s.backoff(ctx)
		return
	}

	// Detach the microVM's lifetime from ctx: once a runner is booting or running
	// its single job we must let it finish rather than kill it mid-job. The
	// provisioner boots Firecracker via exec.CommandContext, which SIGKILLs the
	// VMM the instant its context is cancelled — so if we passed ctx straight
	// through, a SIGTERM would abort every in-flight job.
	//
	// A watcher still cancels the VM on shutdown, but ONLY while it is idle (no
	// job assigned yet): an idle warm-pool VM has nothing to lose and must not
	// block the drain, whereas a busy VM (MarkBusy flipped it when the guest
	// dequeued its job) is left to finish and is reaped by Drain when it
	// self-destructs.
	// systemd's TimeoutStopSec (with KillMode=mixed) is the final backstop for a
	// job that overruns the stop timeout.
	vmCtx, vmCancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer vmCancel(nil)
	// Settle the reservation before the handle becomes visible to scaleDown:
	// a handle that is both pending and cancelled would be discounted twice.
	enter()
	s.track(name, vmCancel)
	defer s.untrack(name)

	go func() {
		select {
		case <-ctx.Done():
			s.cancelIfIdle(name)
		case <-vmCtx.Done():
		}
	}()

	// Phase timing up to the provisioner; it logs its own phases from here.
	s.opts.Logger.Info("launching runner", "runner", name,
		"wait", jitStart.Sub(queuedAt).Round(time.Millisecond),
		"jit", jitDur.Round(time.Millisecond),
		"sinceDecided", time.Since(queuedAt).Round(time.Millisecond))
	launchErr := s.opts.Provisioner.Launch(vmCtx, name, jit, s.opts.Spec, func() { s.MarkBusy(name) })
	// The VM is gone and its slot is back in the pool. Settle the count and
	// drop the handle in one critical section, so the two never disagree and a
	// scale-down during the backoff below cannot mark a dead launch cancelled.
	// The provisioner released the slot just before returning, so a Capacity
	// sample between that release and this lock still sees the VM and its
	// freed slot together; the window is a few instructions and closing it
	// would tie the provisioner's slot lifecycle to this lock. A failed launch
	// keeps its place in running through backoff so plan bounds the retries.
	s.mu.Lock()
	// A VM whose runner scale-down asked GitHub to remove may exit non-zero
	// because GitHub deleted it, before an unconfirmed removal settles and
	// cancels the VM. Its error is then ours, not a failed boot. A runner
	// with a job is never deleted, so a busy one's error still counts.
	deregistered := false
	if h := s.active[name]; h != nil {
		jobDone = h.completed && s.reconciles > h.completedAt
		deregistered = h.deregistering && !h.busy
	}
	delete(s.active, name)
	failed := launchErr != nil && vmCtx.Err() == nil && !deregistered
	if !failed {
		finish()
	}
	s.mu.Unlock()
	startReplanned()
	switch {
	case vmCtx.Err() != nil:
		// We cancelled this VM ourselves (scale-down or shutdown); any error it
		// returns is expected, so treat it as healthy churn.
		s.resetBackoff()
	case launchErr != nil && deregistered:
		// Neither a failure nor proof of a healthy boot: leave the count.
		s.opts.Logger.Info("runner ended while GitHub was removing it", "runner", name, "err", launchErr)
	case launchErr != nil:
		s.opts.Logger.Error("launch microVM", "runner", name, "err", launchErr)
		s.backoff(ctx)
	default:
		s.resetBackoff()
	}
}

// backoff paces warm-pool replenishment after an unhealthy microVM boot so a
// persistently failing golden (or a GitHub API error during JIT generation)
// cannot spin a tight relaunch loop that burns a runner registration per
// attempt. The delay grows exponentially with the consecutive-failure count and
// returns early if shutdown begins, so it never delays Drain.
func (s *Scheduler) backoff(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.failures++
	s.backingOff++
	n := s.failures
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.backingOff--
		s.mu.Unlock()
	}()
	d := backoffDelay(n)
	s.opts.Logger.Warn("microVM boot failed; backing off before replenishing", "consecutiveFailures", n, "delay", d)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// resetBackoff clears the consecutive-failure count after a healthy launch.
func (s *Scheduler) resetBackoff() {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
}

const (
	backoffBase = 500 * time.Millisecond
	backoffMax  = 30 * time.Second
)

// backoffDelay returns the delay for the nth consecutive failure: backoffBase
// doubled per failure, capped at backoffMax (a loop, not a shift, so a long
// failure streak cannot overflow the duration).
func backoffDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := backoffBase
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// track registers an in-flight microVM so a shutdown drain can find and, if it
// is still idle, cancel it. It starts life idle (busy=false).
//
// A runner of a suspended tenant is stopped as it is tracked: its launch was
// decided before the suspension.
func (s *Scheduler) track(name string, cancel context.CancelCauseFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := &vmHandle{cancel: cancel, since: time.Now()}
	s.active[name] = h
	if s.opts.Tenant == nil {
		return
	}
	if why := s.opts.Tenant.suspension(); why != "" {
		s.stopLocked(name, h, Stop{Kind: StopSuspended, Reason: why})
	}
}

// stopAll stops every runner the scheduler has, busy or not, for stop: its
// tenant is suspended.
func (s *Scheduler) stopAll(stop Stop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for name, h := range s.active {
		if !h.cancelled {
			s.stopLocked(name, h, stop)
			n++
		}
	}
	if n > 0 {
		s.opts.Logger.Warn("stopped every runner: the tenant is suspended", "runners", n, "reason", stop.Reason)
	}
}

// stopLocked cancels one runner for stop, with the stop as its cause, and
// reports it. Callers must hold s.mu.
func (s *Scheduler) stopLocked(name string, h *vmHandle, stop Stop) {
	stop.Runner = name
	h.cancelled = true
	h.cancel(StopCause{stop})
	if s.opts.OnStop != nil {
		s.opts.OnStop(stop)
	}
}

// untrack removes a microVM from the registry once it has exited.
func (s *Scheduler) untrack(name string) {
	s.mu.Lock()
	delete(s.active, name)
	s.mu.Unlock()
}

// MarkBusy records that the named runner has dequeued a job, so a shutdown
// drain lets it finish instead of cancelling it as an idle warm-pool VM. It is
// driven by the guest console "Running job:" marker (see the provisioner) and
// no-ops for a runner that has already exited.
//
// The first mark is the job's start: with Options.MaxJob set, the runner is
// stopped once its job has run that long (see stopOverlong).
func (s *Scheduler) MarkBusy(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil {
		return
	}
	h.busy = true
	if h.jobStarted.IsZero() && !h.completed {
		h.jobStarted = time.Now()
		if s.opts.MaxJob > 0 {
			s.afterFunc(s.opts.MaxJob, func() { s.stopOverlong(name, h) })
		}
	}
}

// stopOverlong ends a runner whose job has run Options.MaxJob and is still
// running. Its job fails at GitHub: the limit is the tenant's, and the
// machine must not outlive it. A job that completed meanwhile is left to
// stopCompleted.
func (s *Scheduler) stopOverlong(name string, h *vmHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[name] != h || h.cancelled || h.completed {
		return
	}
	s.opts.Logger.Warn("stopping a runner: its job ran past the limit", "runner", name, "limit", s.opts.MaxJob)
	s.stopLocked(name, h, Stop{Kind: StopJobDuration, Reason: fmt.Sprintf("its job ran past the limit of %s", s.opts.MaxJob)})
}

// MarkCompleted records that GitHub reported the named runner's job completed.
// It arrives in the same message as, and just before, the desired count that
// no longer counts the job; once that Reconcile has run, the VM's exit may
// re-plan for the jobs queued behind it (see replan). A completed runner is
// also busy: it ran a job and will not take another. It no-ops for a runner
// that has already exited, whose exit then did not re-plan.
//
// A runner that had started its job and is still running CompletedGrace
// later is stopped: see stopCompleted.
func (s *Scheduler) MarkCompleted(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil || h.completed {
		return
	}
	// Only a runner that started the job is stopped: a job cancelled before
	// it started is reported completed too, and its idle runner may still get
	// the next one.
	started := h.busy
	h.busy = true
	h.completed = true
	h.completedAt = s.reconciles
	if started && s.opts.CompletedGrace > 0 {
		s.afterFunc(s.opts.CompletedGrace, func() { s.stopCompleted(name, h) })
	}
}

// stopCompleted cancels a runner whose job completed CompletedGrace ago and
// that is still running. Its job is over, so nothing is lost; the guest
// should have shut itself down, and a job that stops that would otherwise
// keep the slot, or the instance, until its lifetime ends.
func (s *Scheduler) stopCompleted(name string, h *vmHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[name] != h || h.cancelled {
		return
	}
	h.cancelled = true
	h.cancel(nil)
	s.opts.Logger.Warn("stopping a runner still running after its job completed", "runner", name, "grace", s.opts.CompletedGrace)
}

// IdleRunners returns the runners launched more than olderThan ago that have
// not taken a job and are not being removed: the ones a reconciler checks
// against GitHub.
func (s *Scheduler) IdleRunners(olderThan time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	cutoff := time.Now().Add(-olderThan)
	for name, h := range s.active {
		if !h.busy && !h.cancelled && !h.removing && h.since.Before(cutoff) {
			out = append(out, name)
		}
	}
	return out
}

// Tracks reports whether the named runner is one of this scheduler's live
// launches. A name is generated (and registered at GitHub) shortly before
// its launch is tracked, so callers must allow for that window.
func (s *Scheduler) Tracks(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[name] != nil
}

// CancelLost cancels an idle runner GitHub no longer knows: it can never get
// a job, and would hold its slot (and its instance) until its lifetime ends.
// A busy runner is left alone. It reports whether it cancelled.
func (s *Scheduler) CancelLost(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil || h.busy || h.cancelled || h.removing {
		return false
	}
	h.cancelled = true
	h.cancel(nil)
	s.opts.Logger.Warn("cancelling a runner GitHub no longer lists", "runner", name)
	return true
}

// cancelIfIdle cancels the named microVM's context only if no job has been
// assigned to it, so shutdown reaps idle warm-pool VMs at once while leaving
// busy VMs to finish their job.
func (s *Scheduler) cancelIfIdle(name string) {
	// Cancel under the lock (the CancelFunc only closes a channel, so it never
	// blocks) so a MarkBusy cannot slip between the busy check and the cancel
	// and get an in-flight job SIGKILLed — the same invariant scaleDown holds.
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.active[name]
	if h == nil || h.busy || h.cancelled {
		return
	}
	h.cancelled = true
	h.cancel(nil)
}

// SetMin changes the warm-pool floor (Min) while the scheduler runs, as a
// tier's schedule moves between windows, and re-plans in the same critical
// section: a higher floor launches runners now, a lower one scales idle
// runners down (busy ones are never cancelled). Before any message from
// GitHub, a higher floor is filled as after a runner exits.
func (s *Scheduler) SetMin(ctx context.Context, n int) {
	s.mu.Lock()
	old := s.opts.Min
	if n == old {
		s.mu.Unlock()
		return
	}
	s.opts.Min = n
	var act func()
	if s.hasDesired && s.lastCtx.Err() == nil {
		act = s.reconcileLocked(s.lastCtx)
	}
	s.mu.Unlock()
	s.opts.Logger.Info("warm-pool floor changed", "from", old, "to", n)
	if act != nil {
		act()
	} else if n > old {
		s.maintainMinimum(ctx)
	}
}

// Min is the current warm-pool floor.
func (s *Scheduler) Min() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts.Min
}

// maintainMinimum tops the warm pool back up to Min after a microVM exits. GitHub
// only pushes a desired-count message when job demand changes, so without this a
// burst that drains the pool would leave it below Min until the next job arrives —
// exactly when the following burst is most likely and the warm-pool latency win
// matters most. It only ever launches (never cancels): it must not trigger the
// scale-down path, or every VM exit during a scale-down would keep collapsing the
// pool toward Min instead of the listener's actual desired count. It no-ops
// during shutdown (ctx cancelled) so it never relaunches VMs that Drain is
// trying to reap.
func (s *Scheduler) maintainMinimum(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.opts.Min <= 0 {
		s.mu.Unlock()
		return
	}
	// The floor is measured against running rather than live on purpose: a
	// launch sleeping in backoff is the pool's pending replenishment, and
	// counting it keeps a failing golden from being retried faster than backoff
	// allows. Demand-driven launches (Reconcile) are the ones that must not
	// wait on it.
	n := 0
	if !s.draining {
		n = plan(s.opts.Min, s.running, s.running, s.opts.Max)
	}
	if s.opts.Tenant != nil && n > 0 {
		// Behind every tier in its tenant's line, and not in line itself:
		// the floor is refilled by the next plan once a place is free.
		n, _ = s.opts.Tenant.take(s, n)
	}
	s.commit(n)
	running, floor := s.running, s.opts.Min
	s.mu.Unlock()
	if n == 0 {
		return
	}
	s.opts.Logger.Info("replenishing warm pool", "min", floor, "launching", n, "running", running)
	for i := 0; i < n; i++ {
		s.wg.Add(1)
		go s.launchOne(ctx, time.Now())
	}
}

// Running returns the current number of in-flight microVMs.
func (s *Scheduler) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Capacity returns how many jobs this tier can hold concurrently right now.
// It is what the listener advertises to GitHub as the scale set's capacity, so
// it must match what Reconcile can actually serve — otherwise GitHub assigns
// the job here and it queues while a sibling host sits idle. Three parts add
// up, mirroring plan:
//
//   - VMs that hold a slot and can take a job (live minus this tier's pending
//     launches);
//   - pending launches that will find a slot (the shared pool has room for
//     them; those it does not have room for will fail and are not counted);
//   - launches Reconcile could still make: bounded both by Max - running, as
//     plan is, and by the slots the pool has left once every tier's pending
//     launches have drawn theirs.
//
// Launches sleeping in backoff and VMs tearing down after a cancel are in
// running but not in live: they cannot take a job, and their slot is either
// absent or not yet back in the pool. A provisioner without a shared pool has
// no slot constraint, so only Max - running bounds the last term — which is
// still below Max while such entries hold places in running, exactly as plan
// would refuse to launch into them.
//
// Every tier on the host sees the same free count, so the tiers' advertised
// values can sum to more than the pool while several are idle, and a burst
// that lands on two tiers within one poll can still be over-assigned by up to
// the free count. That is deliberate: dividing the free slots between tiers
// would leave a slot unadvertised whenever the tier that wants it has used up
// its share, stranding a job on a single host that has room for it. The
// overlap only lasts until the next poll, and the sum is never larger than the
// static maxima advertised before.
func (s *Scheduler) Capacity() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A tenant's bound does not cut it: GitHub would keep the jobs past the
	// bound in its own queue, where nothing records why they wait. Assigned
	// here, they are held with the reason (see Options.OnHold).
	room := max(0, s.opts.Max-s.running)
	committed := max(0, s.live()-s.pending)
	sr, ok := s.opts.Provisioner.(provisioner.SlotReporter)
	if !ok {
		return committed + s.pending + room
	}
	free := sr.FreeSlots()
	willBoot := min(s.pending, free)
	headroom := max(0, min(room, free-s.opts.Pending.Count()))
	return committed + willBoot + headroom
}

// HandOff cancels each busy runner's launch with core.ErrHandedOff, so a
// Drain after it need not wait for their jobs: the provisioner leaves their
// machines running for the next process to find. Call it only once shutdown
// has begun (idle runners are cancelled then), and only for a provisioner
// that honours the cause. It returns how many it handed off.
func (s *Scheduler) HandOff() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.active {
		if h.busy && !h.cancelled {
			h.cancelled = true
			h.cancel(core.ErrHandedOff)
			n++
		}
	}
	return n
}

// Drain blocks until all in-flight microVMs have exited. Nothing launches once
// it has begun: not Reconcile, not the warm-pool top-up, not a re-plan after
// an exit.
func (s *Scheduler) Drain() {
	s.mu.Lock()
	s.draining = true
	s.setHeld(0, Hold{}) // out of its tenant's line
	s.mu.Unlock()
	s.wg.Wait()
}
