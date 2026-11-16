package listener

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/health"
)

func assignedAt(id string, at time.Time) *scaleset.JobAssigned {
	return &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: id, ScaleSetAssignTime: at,
		OwnerName: "o", RepositoryName: "r", JobDisplayName: "build"}}
}

func completed(id, runner, result string) *scaleset.JobCompleted {
	return &scaleset.JobCompleted{RunnerName: runner, Result: result, JobMessageBase: scaleset.JobMessageBase{JobID: id}}
}

// A stranded job is one the message both assigns, from before the session
// opened, and reports cancelled with no runner: what GitHub sends after it
// gave up on an assignment during an outage (measured 2026-11-16).
func TestStranded(t *testing.T) {
	opened := time.Date(2026, 11, 16, 4, 4, 28, 0, time.UTC)
	before, after := opened.Add(-6*time.Minute), opened.Add(time.Minute)
	cases := []struct {
		name     string
		assigned []*scaleset.JobAssigned
		done     []*scaleset.JobCompleted
		first    bool
		want     string
	}{
		{"assigned before the session, cancelled with no runner", []*scaleset.JobAssigned{assignedAt("j1", before)},
			[]*scaleset.JobCompleted{completed("j1", "", "canceled")}, false, "[j1]"},
		{"assigned in this session: cancelled by its user", []*scaleset.JobAssigned{assignedAt("j1", after)},
			[]*scaleset.JobCompleted{completed("j1", "", "canceled")}, true, "[]"},
		{"ran on a runner", []*scaleset.JobAssigned{assignedAt("j1", before)},
			[]*scaleset.JobCompleted{completed("j1", "r-1", "canceled")}, true, "[]"},
		{"failed, not cancelled", []*scaleset.JobAssigned{assignedAt("j1", before)},
			[]*scaleset.JobCompleted{completed("j1", "", "failed")}, true, "[]"},
		{"assigned in an earlier message", nil,
			[]*scaleset.JobCompleted{completed("j1", "", "canceled")}, true, "[]"},
		{"no assignment time, first message", []*scaleset.JobAssigned{assignedAt("j1", time.Time{})},
			[]*scaleset.JobCompleted{completed("j1", "", "canceled")}, true, "[j1]"},
		{"no assignment time, later message", []*scaleset.JobAssigned{assignedAt("j1", time.Time{})},
			[]*scaleset.JobCompleted{completed("j1", "", "canceled")}, false, "[]"},
		{"reported twice, counted once", []*scaleset.JobAssigned{assignedAt("j1", before), assignedAt("j2", before)},
			[]*scaleset.JobCompleted{completed("j1", "", "canceled"), completed("j1", "", "canceled"), completed("j2", "", "canceled")}, false, "[j1 j2]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := &scaleset.RunnerScaleSetMessage{JobAssignedMessages: c.assigned, JobCompletedMessages: c.done}
			var ids []string
			for _, a := range stranded(msg, opened, c.first) {
				ids = append(ids, a.JobID)
			}
			if got := fmt.Sprint(ids); got != c.want {
				t.Fatalf("stranded = %s, want %s", got, c.want)
			}
		})
	}
}

// A stranded job adds a runner to the tier, which GitHub's count leaves it
// out of, once however often its message is delivered, until GitHub takes the
// job up again or the rescue runs out.
func TestScaleRescuesAStrandedJob(t *testing.T) {
	now := time.Date(2026, 11, 16, 4, 4, 28, 0, time.UTC)
	r := &rescues{now: func() time.Time { return now }}
	tr := health.NewTracker("a")
	var desired []int
	a := &scaler{log: testLogger(), stats: &statsRecorder{}, health: tr, rescue: r, opened: now,
		onDesired: func(_ context.Context, n int) int { desired = append(desired, n); return n }}
	strandMsg := &scaleset.RunnerScaleSetMessage{MessageID: 1,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
		JobAssignedMessages:  []*scaleset.JobAssigned{assignedAt("j1", now.Add(-6*time.Minute))},
		JobCompletedMessages: []*scaleset.JobCompleted{completed("j1", "", "canceled")}}
	for range 2 { // delivered again after a crash: one rescue
		if err := a.Scale(context.Background(), strandMsg); err != nil {
			t.Fatal(err)
		}
	}
	if s := tr.Snapshot(); fmt.Sprint(desired) != "[1 1]" || s.Stranded != 1 || s.Rescuing != 1 {
		t.Fatalf("desired %v, stranded %d, rescuing %d; want [1 1], 1, 1", desired, s.Stranded, s.Rescuing)
	}

	// A new session keeps it: the rescue is the tier's, not the session's.
	b := &scaler{log: testLogger(), stats: &statsRecorder{}, health: tr, rescue: r, opened: now.Add(time.Minute),
		onDesired: func(_ context.Context, n int) int { desired = append(desired, n); return n }}
	if err := b.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// GitHub assigns it again, and now counts it itself.
	again := &scaleset.RunnerScaleSetMessage{MessageID: 2,
		Statistics:          &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAssignedMessages: []*scaleset.JobAssigned{assignedAt("j1", now.Add(time.Minute))}}
	if err := b.Scale(context.Background(), again); err != nil {
		t.Fatal(err)
	}
	if s := tr.Snapshot(); fmt.Sprint(desired) != "[1 1 1 1]" || s.Rescuing != 0 || s.Stranded != 1 {
		t.Fatalf("desired %v, rescuing %d, stranded %d; want [1 1 1 1], 0, 1", desired, s.Rescuing, s.Stranded)
	}
}

// A rescue that GitHub never takes up (the job's user cancelled it during the
// outage) ends after rescueFor, and its runner with it.
func TestRescueRunsOut(t *testing.T) {
	now := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	r := &rescues{now: func() time.Time { return now }}
	if !r.add("j1") || r.add("j1") {
		t.Fatal("add starts one rescue per job")
	}
	now = now.Add(rescueFor - time.Second)
	if n := r.count(); n != 1 {
		t.Fatalf("count before rescueFor = %d, want 1", n)
	}
	now = now.Add(time.Second)
	if n := r.count(); n != 0 {
		t.Fatalf("count at rescueFor = %d, want 0", n)
	}
	if r.forget("j1") {
		t.Fatal("an ended rescue is not forgotten twice")
	}
}
