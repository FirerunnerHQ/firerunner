// Package assigned keeps the jobs GitHub has assigned to a tier's scale set
// that have not started yet. On a JIT scale set every job arrives as a
// JobAssigned message, which actions/scaleset parses but never passes on: the
// listener's session wrapper reads them here instead, before the library acts
// on the same message's statistics.
//
// The jobs shape supply, not placement. GitHub hands a queued job to any idle
// runner of the scale set, so a fast runner launched "for" a pull request may
// run the push job queued before it. Policy decides only which launches may
// spend a pool instance.
package assigned

import (
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// keepFor drops a job never seen to start or complete: GitHub cancels a job
// still queued after 24 hours.
const keepFor = 24 * time.Hour

// Job is one assigned job's attributes.
type Job struct {
	Repository  string
	WorkflowRef string
	Name        string // the job's display name
	Event       string
	Labels      []string
	QueuedAt    time.Time
	AssignedAt  time.Time // when GitHub assigned it to the scale set
	seen        time.Time
	claimed     bool
}

// Policy names the jobs a pool instance is kept for: those whose event is in
// Events or whose display name is in Jobs.
type Policy struct {
	Events []string
	Jobs   []string
}

// Matches reports whether the policy keeps the pool for j.
func (p Policy) Matches(j Job) bool {
	return slices.Contains(p.Events, j.Event) || slices.Contains(p.Jobs, j.Name)
}

// Jobs is one tier's assigned-but-not-started jobs. It is safe for concurrent
// use.
type Jobs struct {
	policy *Policy
	now    func() time.Time

	mu   sync.Mutex
	jobs map[string]*Job
}

// New returns an empty set. A nil policy lets every launch take the pool.
func New(policy *Policy) *Jobs {
	return &Jobs{policy: policy, now: time.Now, jobs: map[string]*Job{}}
}

// key identifies a job across its messages: the runner request, else the job.
func key(b scaleset.JobMessageBase) string {
	if b.RunnerRequestID != 0 {
		return "r" + strconv.FormatInt(b.RunnerRequestID, 10)
	}
	return "j" + b.JobID
}

// Message records a message's assignments and forgets the jobs it reports
// started or completed. It reports whether the set changed. A nil set
// records nothing.
func (s *Jobs) Message(m *scaleset.RunnerScaleSetMessage) bool {
	if s == nil || m == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := s.pruneLocked(now)
	for _, a := range m.JobAssignedMessages {
		if a == nil {
			continue
		}
		k := key(a.JobMessageBase)
		if _, ok := s.jobs[k]; ok {
			continue // a re-delivered message keeps its claim
		}
		s.jobs[k] = &Job{
			Repository: a.OwnerName + "/" + a.RepositoryName, WorkflowRef: a.JobWorkflowRef,
			Name: a.JobDisplayName, Event: a.EventName, Labels: a.RequestLabels,
			QueuedAt: a.QueueTime, AssignedAt: a.ScaleSetAssignTime, seen: now,
		}
		changed = true
	}
	for _, st := range m.JobStartedMessages {
		if st != nil {
			changed = s.forgetLocked(key(st.JobMessageBase)) || changed
		}
	}
	for _, c := range m.JobCompletedMessages {
		if c != nil {
			changed = s.forgetLocked(key(c.JobMessageBase)) || changed
		}
	}
	return changed
}

func (s *Jobs) forgetLocked(k string) bool {
	if _, ok := s.jobs[k]; !ok {
		return false
	}
	delete(s.jobs, k)
	return true
}

func (s *Jobs) pruneLocked(now time.Time) bool {
	changed := false
	for k, j := range s.jobs {
		if now.Sub(j.seen) > keepFor {
			delete(s.jobs, k)
			changed = true
		}
	}
	return changed
}

// ClaimPool reports whether a launch may take a pool instance. Without a
// policy it always may. With one, each pending job the policy matches lets
// one launch take the pool, oldest first; other launches cold-launch and
// leave the pool for such jobs.
func (s *Jobs) ClaimPool() bool {
	if s == nil || s.policy == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	var oldest *Job
	for _, j := range s.jobs {
		if !j.claimed && s.policy.Matches(*j) && (oldest == nil || j.seen.Before(oldest.seen)) {
			oldest = j
		}
	}
	if oldest == nil {
		return false
	}
	oldest.claimed = true
	return true
}

// Mix summarizes the pending jobs.
type Mix struct {
	Jobs int
	// ByEvent counts jobs per event name.
	ByEvent map[string]int
	// Kept is how many the policy keeps the pool for.
	Kept int
}

// Mix returns the pending jobs' summary.
func (s *Jobs) Mix() Mix {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Mix{Jobs: len(s.jobs), ByEvent: map[string]int{}}
	for _, j := range s.jobs {
		m.ByEvent[j.Event]++
		if s.policy != nil && s.policy.Matches(*j) {
			m.Kept++
		}
	}
	return m
}
