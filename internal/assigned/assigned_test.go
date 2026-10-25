package assigned

import (
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func base(req int64, event, name string) scaleset.JobMessageBase {
	return scaleset.JobMessageBase{RunnerRequestID: req, OwnerName: "acme", RepositoryName: "app", EventName: event, JobDisplayName: name}
}

func assigned(bs ...scaleset.JobMessageBase) *scaleset.RunnerScaleSetMessage {
	m := &scaleset.RunnerScaleSetMessage{}
	for _, b := range bs {
		m.JobAssignedMessages = append(m.JobAssignedMessages, &scaleset.JobAssigned{JobMessageBase: b})
	}
	return m
}

func claims(s *Jobs, n int) []bool {
	var out []bool
	for range n {
		ok, _ := s.ClaimPool()
		out = append(out, ok)
	}
	return out
}

func TestNoPolicyAlwaysTakesThePool(t *testing.T) {
	ok, _ := New(nil).ClaimPool()
	nilOK, _ := (*Jobs)(nil).ClaimPool()
	if !ok || !nilOK {
		t.Fatal("without a policy every launch takes the pool")
	}
}

func TestPRAndPushMix(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	if !s.Message(assigned(base(1, "push", "build"), base(2, "pull_request", "build"))) {
		t.Fatal("assignments did not change the set")
	}
	// Two launches for two jobs: one pool instance for the PR, one cold.
	if got := claims(s, 2); got[0] != true || got[1] != false {
		t.Fatalf("claims = %v, want [true false]", got)
	}
	if m := s.Mix(); m.Jobs != 2 || m.Kept != 1 || m.ByEvent["push"] != 1 || m.ByEvent["pull_request"] != 1 {
		t.Fatalf("mix = %+v", m)
	}
	// A re-delivered assignment keeps its claim.
	s.Message(assigned(base(2, "pull_request", "build")))
	if ok, _ := s.ClaimPool(); ok {
		t.Fatal("a re-delivered assignment was claimed twice")
	}
	// The PR job starts; a new PR job lets the next launch take the pool.
	s.Message(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: base(2, "pull_request", "build")}}})
	s.Message(assigned(base(3, "pull_request", "lint")))
	if got := claims(s, 2); got[0] != true || got[1] != false {
		t.Fatalf("claims = %v, want [true false]", got)
	}
	s.Message(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{
		{JobMessageBase: base(1, "push", "build")}, {JobMessageBase: base(3, "pull_request", "lint")}}})
	if m := s.Mix(); m.Jobs != 0 {
		t.Fatalf("mix after completion = %+v", m)
	}
}

func TestPushOnlyDemandGoesCold(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	s.Message(assigned(base(1, "push", "a"), base(2, "schedule", "b")))
	if got := claims(s, 2); got[0] || got[1] {
		t.Fatalf("claims = %v, want all cold", got)
	}
}

func TestJobNamesMatch(t *testing.T) {
	s := New(&Policy{Jobs: []string{"required-check"}})
	s.Message(assigned(base(1, "push", "other"), base(2, "push", "required-check")))
	if got := claims(s, 2); got[0] != true || got[1] != false {
		t.Fatalf("claims = %v, want [true false]", got)
	}
}

func TestJobIDKeyWithoutRunnerRequest(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	b := base(0, "pull_request", "x")
	b.JobID = "job-1"
	s.Message(assigned(b))
	s.Message(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: b}}})
	if m := s.Mix(); m.Jobs != 0 {
		t.Fatalf("job keyed by JobID not forgotten: %+v", m)
	}
}

func TestStaleJobsDropped(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Message(assigned(base(1, "pull_request", "x")))
	now = now.Add(keepFor + time.Minute)
	if s.Mix().Jobs != 0 {
		t.Fatal("a job past keepFor still counted in the mix")
	}
	if ok, _ := s.ClaimPool(); ok {
		t.Fatal("a job past keepFor still claimable")
	}
}

func TestReleaseReturnsTheClaim(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	s.Message(assigned(base(1, "pull_request", "x")))
	ok, release := s.ClaimPool()
	if !ok {
		t.Fatal("no claim")
	}
	release()
	if ok, _ := s.ClaimPool(); !ok {
		t.Fatal("a released claim was not available again")
	}
}

func TestEarliestQueuedClaimedFirst(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	t0 := time.Now()
	late, early := base(1, "pull_request", "late"), base(2, "pull_request", "early")
	late.QueueTime, early.QueueTime = t0.Add(time.Minute), t0
	s.Message(assigned(late, early))
	s.ClaimPool()
	// The early job holds the claim: its start leaves the late one claimable.
	s.Message(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: early}}})
	if ok, _ := s.ClaimPool(); !ok {
		t.Fatal("the late job's claim was spent by the first launch")
	}
}

func TestEitherKeyForgetsTheJob(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	b := base(7, "pull_request", "x")
	b.JobID = "job-7"
	s.Message(assigned(b))
	onlyJob := base(0, "pull_request", "x")
	onlyJob.JobID = "job-7"
	if !s.Message(&scaleset.RunnerScaleSetMessage{JobCompletedMessages: []*scaleset.JobCompleted{{JobMessageBase: onlyJob}}}) || s.Mix().Jobs != 0 {
		t.Fatalf("a completion carrying only the job ID left %+v", s.Mix())
	}
}

func TestJobsAssignedBeforeTheSessionMayTakeThePool(t *testing.T) {
	s := New(&Policy{Events: []string{"pull_request"}})
	s.Message(assigned(base(1, "push", "known")))
	// A new session: 3 assigned, 1 known; 2 described by no message.
	s.Session(3)
	if got := claims(s, 3); !got[0] || !got[1] || got[2] {
		t.Fatalf("claims = %v, want two for the unknown jobs", got)
	}
	// One unknown job starts, taking its claim with it: the other keeps its.
	s.Message(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: base(9, "", "")}}})
	if got := claims(s, 1); got[0] {
		t.Fatalf("after a start claims = %v, want none left", got)
	}
	// A new session recounts from scratch: 2 assigned, 1 known.
	s.Session(2)
	if got := claims(s, 2); !got[0] || got[1] {
		t.Fatalf("after a new session claims = %v, want one", got)
	}
}
