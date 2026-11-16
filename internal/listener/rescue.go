package listener

import (
	"slices"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// rescueFor is how long a stranded job adds a runner to its tier: the
// runner's boot and its job's pickup take about a minute, and a job that was
// not stranded after all (cancelled by its user during the outage) costs one
// idle runner until then.
const rescueFor = 10 * time.Minute

// rescues are the jobs of one tier that GitHub stopped assigning to the scale
// set while no session was open, each with when its rescue ends.
//
// GitHub assigns a job to a scale set even with no session open. If none
// opens within about 4 minutes, it cancels that assignment: the next session
// receives the JobAssigned and, in the same message, a JobCompleted with
// result "canceled" and no runner. The job itself stays queued, and GitHub
// counts it nowhere in the scale set's statistics, so sizing from them
// launches nothing for it. It runs when an idle runner of the scale set comes
// online (measured 2026-11-16: within 55 s of the restart that launched one),
// or when the installation queues another job, which may be hours away. So
// each stranded job adds one runner to its tier until GitHub assigns or starts
// it again, or rescueFor passes.
//
// They live on the ScaleSet, not the session: a reconnect keeps them. Safe
// for concurrent use.
type rescues struct {
	mu   sync.Mutex
	now  func() time.Time
	jobs map[string]time.Time // job ID -> when its rescue ends
}

// add starts a rescue for job id and reports whether it is new: a message
// delivered twice starts none.
func (r *rescues) add(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	r.pruneLocked(now)
	if _, ok := r.jobs[id]; ok {
		return false
	}
	if r.jobs == nil {
		r.jobs = map[string]time.Time{}
	}
	r.jobs[id] = now.Add(rescueFor)
	return true
}

// forget ends job id's rescue, if any, and reports whether one ended.
func (r *rescues) forget(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[id]; !ok {
		return false
	}
	delete(r.jobs, id)
	return true
}

// count is how many rescues are on.
func (r *rescues) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock())
	return len(r.jobs)
}

func (r *rescues) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *rescues) pruneLocked(now time.Time) {
	for id, end := range r.jobs {
		if !now.Before(end) {
			delete(r.jobs, id)
		}
	}
}

// stranded returns the assignments of the jobs msg shows stranded: a
// JobCompleted with result "canceled" and no runner, for a job the same
// message assigned, and assigned before the session opened. A job assigned and cancelled within one
// long poll of a live session is not: it was assigned after the session
// opened. When GitHub leaves out the assignment time, only the session's
// first message with jobs counts (first).
func stranded(msg *scaleset.RunnerScaleSetMessage, opened time.Time, first bool) []*scaleset.JobAssigned {
	if msg == nil || len(msg.JobCompletedMessages) == 0 {
		return nil
	}
	assigned := map[string]*scaleset.JobAssigned{}
	for _, a := range msg.JobAssignedMessages {
		if a != nil && a.JobID != "" {
			assigned[a.JobID] = a
		}
	}
	var out []*scaleset.JobAssigned
	for _, c := range msg.JobCompletedMessages {
		if c == nil || c.RunnerName != "" || c.Result != "canceled" || c.JobID == "" {
			continue
		}
		a := assigned[c.JobID]
		if a == nil || slices.Contains(out, a) {
			continue
		}
		if at := a.ScaleSetAssignTime; at.IsZero() && !first || !at.IsZero() && !at.Before(opened) {
			continue
		}
		out = append(out, a)
	}
	return out
}
