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
		out = append(out, s.ClaimPool())
	}
	return out
}

func TestNoPolicyAlwaysTakesThePool(t *testing.T) {
	s := New(nil)
	if !s.ClaimPool() || !(*Jobs)(nil).ClaimPool() {
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
	if s.ClaimPool() {
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
	if s.ClaimPool() || s.Mix().Jobs != 0 {
		t.Fatal("a job past keepFor still counted")
	}
}
