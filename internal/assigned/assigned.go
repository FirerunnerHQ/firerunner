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
	keys        []string
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

	mu sync.Mutex
	// jobs indexes each job under every key it has (see keys), so a message
	// that carries only one of them still finds it.
	jobs map[string]*Job
	// unknown counts assigned jobs a new session reported that no message
	// described (they were assigned before this session); each may take the
	// pool once, as it would without a policy. unknownClaimed of them have.
	unknown, unknownClaimed int
	generation              int
}

// New returns an empty set. A nil policy lets every launch take the pool.
func New(policy *Policy) *Jobs {
	return &Jobs{policy: policy, now: time.Now, jobs: map[string]*Job{}}
}

// keys identify a job across its messages: its runner request and its job ID,
// whichever it carries.
func keys(b scaleset.JobMessageBase) []string {
	var ks []string
	if b.RunnerRequestID != 0 {
		ks = append(ks, "r"+strconv.FormatInt(b.RunnerRequestID, 10))
	}
	if b.JobID != "" {
		ks = append(ks, "j"+b.JobID)
	}
	return ks
}

// Session records a new message session's statistics. The jobs assigned
// but not running that no message described were assigned before this
// session (a restart or a reconnect), and each may take the pool once: their
// attributes are unknown, and a pull request waiting through a deploy is what
// the pool is kept for.
func (s *Jobs) Session(st *scaleset.RunnerScaleSetStatistic) {
	if s == nil || st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	s.unknown = s.waitingUnknownLocked(st)
	s.unknownClaimed = 0
	s.generation++ // releases of the old session's claims no longer count
}

// waitingUnknownLocked is how many jobs st says wait for a runner beyond
// those a message described. (GitHub counts running jobs among the assigned.)
func (s *Jobs) waitingUnknownLocked(st *scaleset.RunnerScaleSetStatistic) int {
	return max(0, st.TotalAssignedJobs-st.TotalRunningJobs-len(s.uniqueLocked()))
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
		ks := keys(a.JobMessageBase)
		if len(ks) == 0 || slices.ContainsFunc(ks, func(k string) bool { return s.jobs[k] != nil }) {
			continue // unidentifiable, or re-delivered: the job keeps its claim
		}
		j := &Job{
			Repository: a.OwnerName + "/" + a.RepositoryName, WorkflowRef: a.JobWorkflowRef,
			Name: a.JobDisplayName, Event: a.EventName, Labels: a.RequestLabels,
			QueuedAt: a.QueueTime, AssignedAt: a.ScaleSetAssignTime, keys: ks, seen: now,
		}
		for _, k := range ks {
			s.jobs[k] = j
		}
		changed = true
	}
	for _, st := range m.JobStartedMessages {
		if st != nil {
			changed = s.forgetLocked(st.JobMessageBase) || changed
		}
	}
	for _, c := range m.JobCompletedMessages {
		if c != nil {
			changed = s.forgetLocked(c.JobMessageBase) || changed
		}
	}
	// The earlier jobs only ever start or go: their count only shrinks.
	if m.Statistics != nil {
		s.unknown = min(s.unknown, s.waitingUnknownLocked(m.Statistics))
		s.unknownClaimed = min(s.unknownClaimed, s.unknown)
	}
	return changed
}

func (s *Jobs) forgetLocked(b scaleset.JobMessageBase) bool {
	for _, k := range keys(b) {
		if j := s.jobs[k]; j != nil {
			s.deleteLocked(j)
			return true
		}
	}
	return false
}

func (s *Jobs) deleteLocked(j *Job) {
	for _, k := range j.keys {
		delete(s.jobs, k)
	}
}

func (s *Jobs) pruneLocked(now time.Time) bool {
	changed := false
	for _, j := range s.uniqueLocked() {
		if now.Sub(j.seen) > keepFor {
			s.deleteLocked(j)
			changed = true
		}
	}
	return changed
}

// uniqueLocked returns each job once.
func (s *Jobs) uniqueLocked() []*Job {
	seen := map[*Job]bool{}
	var out []*Job
	for _, j := range s.jobs {
		if !seen[j] {
			seen[j] = true
			out = append(out, j)
		}
	}
	return out
}

// ClaimPool reports whether a launch may take a pool instance. Without a
// policy it always may. With one, each pending job the policy matches (and
// each job assigned before the session, see Session) lets one launch take
// the pool, earliest queued first; other launches cold-launch and leave the
// pool for such jobs. A launch that ends up not using the pool calls release,
// so the job's claim is not spent.
func (s *Jobs) ClaimPool() (ok bool, release func()) {
	if s == nil || s.policy == nil {
		return true, func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	var first *Job
	for _, j := range s.uniqueLocked() {
		if !j.claimed && s.policy.Matches(*j) && (first == nil || earlier(j, first)) {
			first = j
		}
	}
	if first != nil {
		first.claimed = true
		return true, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			first.claimed = false
		}
	}
	if s.unknownClaimed < s.unknown {
		s.unknownClaimed++
		gen := s.generation
		return true, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if gen == s.generation {
				s.unknownClaimed = max(0, s.unknownClaimed-1)
			}
		}
	}
	return false, func() {}
}

func earlier(a, b *Job) bool {
	if !a.QueuedAt.Equal(b.QueuedAt) {
		return a.QueuedAt.Before(b.QueuedAt)
	}
	return a.keys[0] < b.keys[0]
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
	s.pruneLocked(s.now())
	js := s.uniqueLocked()
	m := Mix{Jobs: len(js), ByEvent: map[string]int{}}
	for _, j := range js {
		m.ByEvent[j.Event]++
		if s.policy != nil && s.policy.Matches(*j) {
			m.Kept++
		}
	}
	return m
}
