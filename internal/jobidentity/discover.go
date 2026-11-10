package jobidentity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Candidates remembers the jobs GitHub offered this deployment's scale sets
// (JobAvailable and JobAssigned messages: repository, run, event), so that
// when an instance reports its job started, Discover can find which of them
// runs on its runner. JobStarted names the runner too, but GitHub has been
// seen to deliver it with JobCompleted, after the job ended (#28), too late
// for a cache step at the start of the job. Safe for concurrent use.
type Candidates struct {
	mu   sync.Mutex
	runs map[string]candidate // repository#run
	now  func() time.Time
}

type candidate struct {
	job  Job
	seen time.Time
}

// candidateTTL bounds how long an offered run stays a candidate; a job
// starts well within it, or is cancelled.
const candidateTTL = 30 * time.Minute

// maxCandidates bounds a Discover's lookups: one API call per run.
const maxCandidates = 32

// Offer records a job GitHub offered. Only its repository, run and event
// matter: the run's jobs are looked up by runner name.
func (c *Candidates) Offer(j Job) {
	if !ValidRepository(j.Repository) || j.RunID <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]candidate{}
	}
	now := c.clock()
	for k, v := range c.runs {
		if now.Sub(v.seen) > candidateTTL {
			delete(c.runs, k)
		}
	}
	c.runs[fmt.Sprintf("%s#%d", j.Repository, j.RunID)] = candidate{job: j, seen: now}
}

// recent returns the candidate runs, newest first, at most maxCandidates.
func (c *Candidates) recent() []Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	var cs []candidate
	for _, v := range c.runs {
		if now.Sub(v.seen) <= candidateTTL {
			cs = append(cs, v)
		}
	}
	sort.Slice(cs, func(i, k int) bool { return cs[i].seen.After(cs[k].seen) })
	if len(cs) > maxCandidates {
		cs = cs[:maxCandidates]
	}
	out := make([]Job, len(cs))
	for i, v := range cs {
		out[i] = v.job
	}
	return out
}

func (c *Candidates) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// RunnerFinder tells which job of a run, if any, is in progress on a runner:
// GitHub's own record, so a run named by anyone else proves nothing. The job
// must name both the runner and its GitHub ID, runnerID: a name alone could be
// shared by a runner registered in another scope.
type RunnerFinder interface {
	RunnerJob(ctx context.Context, repository string, runID int64, runner string, runnerID int64) (jobID int64, found bool, err error)
}

// discoverTries and discoverPause bound how long Discover looks: GitHub may
// record the runner on its job a moment after the runner prints that it
// started.
const (
	discoverTries = 4
	discoverPause = 2 * time.Second
)

// refusalWarnEvery spaces warnings about one repository.
const refusalWarnEvery = time.Hour

// Discover finds the job runner started among the candidates, by asking
// GitHub which job of each candidate run is in progress on that runner, and
// resolves its identity as JobStarted would (Start). It does nothing when the
// runner has no lease or its identity is already being resolved; when no
// candidate matches, JobStarted still can.
func (r *Registry) Discover(ctx context.Context, runner string, cands *Candidates, finder RunnerFinder, resolver Resolver) {
	if cands == nil || finder == nil {
		return
	}
	pause := r.discoverPause
	if pause == 0 {
		pause = discoverPause
	}
	runnerID := r.runnerID(runner)
	if runnerID == 0 {
		// Without the runner's ID only its name could match a job, and a
		// runner registered elsewhere could share that: JobStarted, from the
		// scale set's own session, resolves it instead.
		r.log.Info("cache identity: the runner's GitHub ID is unknown; waiting for JobStarted", "runner", runner)
		return
	}
	for try := 0; try < discoverTries; try++ {
		if !r.awaitsIdentity(runner) || ctx.Err() != nil {
			return
		}
		for _, j := range cands.recent() {
			if !r.awaitsIdentity(runner) {
				return // released, or JobStarted got there first
			}
			id, found, err := finder.RunnerJob(ctx, j.Repository, j.RunID, runner, runnerID)
			if rateLimited(err) {
				// Resolution shares the credential: spend no more of it here.
				r.log.Warn("cache identity: GitHub refused the lookup; waiting for JobStarted", "runner", runner, "err", err)
				return
			}
			if errors.Is(err, errTokenRefused) && r.firstRefusal(j.Repository) {
				// Candidates are shared, so this runner may have nothing to
				// do with the repository: the line names only the repository.
				r.log.Warn("cache identity: cannot read a candidate run's jobs", "repository", j.Repository, "err", err)
				continue
			}
			if err != nil {
				r.log.Debug("look up a candidate run's jobs", "runner", runner, "repository", j.Repository, "run", j.RunID, "err", err)
				continue
			}
			if found {
				j.Runner, j.JobID = runner, strconv.FormatInt(id, 10)
				r.log.Info("cache identity: found the runner's job", "runner", runner, "repository", j.Repository, "run", j.RunID, "job", id)
				r.Start(ctx, j, resolver)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
	r.log.Info("cache identity: no candidate run has the runner's job; waiting for JobStarted", "runner", runner)
}

// firstRefusal reports whether a refusal for repo is due a warning. Every runner
// that sees its candidates would otherwise repeat it.
func (r *Registry) firstRefusal(repo string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.refused[repo]; ok && time.Since(last) < refusalWarnEvery {
		return false
	}
	if r.refused == nil {
		r.refused = make(map[string]time.Time)
	}
	r.refused[repo] = time.Now()
	return true
}

// awaitsIdentity reports whether runner has a lease whose identity nothing
// is resolving yet.
func (r *Registry) awaitsIdentity(runner string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.leases[runner]
	return l != nil && l.cancel == nil
}
