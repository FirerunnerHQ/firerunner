// Package reconcile compares GitHub's view of a scale set with firerunner's
// and repairs the gaps it can, using only the statistics GitHub sends with
// every message and the runner lookups firerunner is already allowed to make.
//
// Three drifts are watched:
//   - Jobs left available but never acquired. actions/scaleset deletes a
//     message before it acquires the message's jobs, so a crash in between can
//     leave them waiting. The reconciler recycles the session, so a fresh one
//     reports, and may re-offer, them.
//   - Runners firerunner tracks that GitHub no longer lists. They can never
//     get a job, so they are cancelled, which frees their slot and instance.
//   - More runners registered at GitHub than firerunner tracks. Reported only:
//     firerunner cannot list a scale set's runners by name. The instance
//     reaper finds their machines by tag.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/solcreek/firerunner/internal/health"
)

// Stats is a scale set's statistics as GitHub reported them.
type Stats struct {
	Available, Acquired, Assigned, Running, Registered, Busy, Idle int
}

// ScaleSet is the listener side: GitHub's latest statistics, and a way to
// start a fresh message session.
type ScaleSet interface {
	Stats() (Stats, time.Time, bool)
	// Recycle reports whether a live session was recycled.
	Recycle(reason string) bool
}

// Runners is the scheduler side.
type Runners interface {
	IdleRunners(olderThan time.Duration) []string
	CancelLost(name string) bool
	Running() int
}

// Lookup reports whether GitHub still knows a runner.
type Lookup func(ctx context.Context, name string) (bool, error)

// Options tune a Reconciler; zero values take the defaults.
type Options struct {
	// StuckAfter is how long jobs may stay available, unacquired, before
	// the session is recycled (default 60s).
	StuckAfter time.Duration
	// RecycleEvery bounds how often a session is recycled (default 5m).
	RecycleEvery time.Duration
	// LostAfter is how long a runner stays idle before GitHub is asked
	// whether it still knows it (default 2m).
	LostAfter time.Duration
	// LostEvery paces those lookups (default 1m).
	LostEvery time.Duration
	// RegisteredGrace is how long GitHub may list more runners than
	// firerunner tracks before it is reported (default 3m), measured
	// between the messages that report it.
	RegisteredGrace time.Duration
	// Tick is how often the reconciler looks (default 10s).
	Tick   time.Duration
	Health *health.Tracker
	Logger *slog.Logger
}

// Reconciler watches one tier.
type Reconciler struct {
	ss      ScaleSet
	runners Runners
	lookup  Lookup
	o       Options
	log     *slog.Logger
	now     func() time.Time

	availableSince   time.Time
	lastRecycle      time.Time
	lastLostCheck    time.Time
	overRegisteredAt time.Time
}

// New returns a Reconciler for one tier.
func New(ss ScaleSet, runners Runners, lookup Lookup, o Options) *Reconciler {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.StuckAfter, time.Minute)
	def(&o.RecycleEvery, 5*time.Minute)
	def(&o.LostAfter, 2*time.Minute)
	def(&o.LostEvery, time.Minute)
	def(&o.RegisteredGrace, 3*time.Minute)
	def(&o.Tick, 10*time.Second)
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Reconciler{ss: ss, runners: runners, lookup: lookup, o: o, log: log, now: time.Now}
}

// Run checks every Tick until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Check(ctx)
		}
	}
}

// Check compares once and repairs what it can.
func (r *Reconciler) Check(ctx context.Context) {
	st, at, ok := r.ss.Stats()
	if !ok {
		return
	}
	now := r.now()
	tracked := r.runners.Running()
	var drift []string

	// Jobs GitHub holds for this scale set that nobody acquired.
	if st.Available > 0 {
		if r.availableSince.IsZero() {
			r.availableSince = now
		}
		waited := now.Sub(r.availableSince)
		if waited >= r.o.StuckAfter {
			drift = append(drift, fmt.Sprintf("%d job(s) available but unacquired for %s", st.Available, waited.Round(time.Second)))
			if r.lastRecycle.IsZero() || now.Sub(r.lastRecycle) >= r.o.RecycleEvery {
				// No live session (reconnecting): nothing was recycled, and
				// the next tick tries again.
				if r.ss.Recycle(fmt.Sprintf("%d job(s) available but unacquired for %s", st.Available, waited.Round(time.Second))) {
					r.lastRecycle = now
					if r.o.Health != nil {
						r.o.Health.Recycled()
					}
				}
			}
		}
	} else {
		r.availableSince = time.Time{}
	}

	// GitHub lists runners firerunner does not track. Statistics arrive only
	// with messages, so an idle scale set keeps showing the last ones (a
	// runner that has since deregistered still counted). Only a later message
	// that still shows the gap confirms it: the grace is measured in message
	// time, not wall time.
	if st.Registered > tracked {
		if r.overRegisteredAt.IsZero() {
			r.overRegisteredAt = at
		}
		if at.Sub(r.overRegisteredAt) >= r.o.RegisteredGrace {
			drift = append(drift, fmt.Sprintf("GitHub lists %d registered runner(s), firerunner tracks %d", st.Registered, tracked))
		}
	} else {
		r.overRegisteredAt = time.Time{}
	}

	// Runners firerunner tracks that GitHub no longer knows.
	if r.lookup != nil && now.Sub(r.lastLostCheck) >= r.o.LostEvery {
		r.lastLostCheck = now
		for _, name := range r.runners.IdleRunners(r.o.LostAfter) {
			known, err := r.lookup(ctx, name)
			if err != nil {
				r.log.Warn("look up runner", "runner", name, "err", err)
				continue
			}
			if !known && r.runners.CancelLost(name) && r.o.Health != nil {
				r.o.Health.LostRunner()
			}
		}
	}

	if r.o.Health != nil {
		r.o.Health.GitHubStats(health.GitHubStats{
			Available: st.Available, Acquired: st.Acquired, Assigned: st.Assigned, Running: st.Running,
			Registered: st.Registered, Busy: st.Busy, Idle: st.Idle, At: at, Tracked: tracked,
		})
		r.o.Health.SetDrift(strings.Join(drift, "; "))
	}
	if len(drift) > 0 {
		r.log.Warn("scale set drift", "drift", strings.Join(drift, "; "))
	}
}
