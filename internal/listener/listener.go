// Package listener yields the desired runner count over time by wrapping
// github.com/actions/scaleset — the official GitHub Actions Runner Scale Set
// client — which long-polls GitHub for assigned jobs. It is the single external
// dependency of firerunner.
package listener

import "context"

// DesiredFunc is invoked whenever GitHub reports a new desired runner count. It
// returns the number of runners currently in flight, which is reported back to
// GitHub's scale-set protocol.
type DesiredFunc func(ctx context.Context, desired int) (running int)

// BusyFunc is invoked with a runner's name when GitHub reports that runner has
// started a job, so the shutdown drain lets it finish instead of reaping it as
// an idle warm-pool VM. It is a second, independent busy signal alongside the
// guest console marker; either source flipping a VM to busy is enough.
type BusyFunc func(runnerName string)

// CompletedFunc is invoked with a runner's name when GitHub reports that
// runner's job completed. It comes before the desired count of the same
// message, which no longer counts the job.
type CompletedFunc func(runnerName string)

// CapacityFunc returns how many jobs the tier can hold concurrently right now.
// It is advertised to GitHub on every poll so that jobs beyond it stay queued
// at GitHub — where a sibling scale set on another host can take them — rather
// than being assigned to a host that cannot start them.
type CapacityFunc func() int

// Listener drives the scheduler from GitHub's assigned-job signal.
type Listener interface {
	// Run blocks until ctx is cancelled, calling onDesired with the latest
	// desired runner count each time GitHub reports one, onBusy with a
	// runner's name each time GitHub reports it started a job, and onCompleted
	// each time GitHub reports its job completed. capacity, when non-nil, is
	// re-read after every onDesired and reported to GitHub as the scale set's
	// current capacity. onBusy, onCompleted and capacity may be nil.
	Run(ctx context.Context, onDesired DesiredFunc, onBusy BusyFunc, onCompleted CompletedFunc, capacity CapacityFunc) error
}
