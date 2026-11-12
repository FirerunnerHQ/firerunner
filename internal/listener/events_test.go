package listener

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

type eventLog struct {
	mu  sync.Mutex
	got []jobevent.Event
}

func (l *eventLog) Record(e jobevent.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, e)
}

func TestJobEvents(t *testing.T) {
	queued := time.Unix(100, 0)
	base := scaleset.JobMessageBase{
		JobID: "job-1", OwnerName: "owner", RepositoryName: "repo", JobWorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/heads/main",
		JobDisplayName: "build", WorkflowRunID: 7, EventName: "push", RequestLabels: []string{"tier-a"},
		QueueTime: queued, ScaleSetAssignTime: time.Unix(101, 0), RunnerAssignTime: time.Unix(120, 0), FinishTime: time.Unix(150, 0),
	}
	events := &eventLog{}

	f := newFakeSession(0, false)
	ts := trackedSession{session: f, events: events, tier: "tier-a"}
	f.msgs <- &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base}}}
	if _, err := ts.GetMessage(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}

	a := &scaler{events: events, tier: "tier-a", log: slog.New(slog.DiscardHandler)}
	if err := a.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: "runner-1", JobMessageBase: base}); err != nil {
		t.Fatal(err)
	}
	if err := a.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: "runner-1", Result: "succeeded", JobMessageBase: base}); err != nil {
		t.Fatal(err)
	}

	if len(events.got) != 3 {
		t.Fatalf("events = %+v", events.got)
	}
	want := []struct {
		kind   jobevent.Kind
		at     int64
		runner string
		result string
	}{
		{jobevent.Assigned, 101, "", ""},
		{jobevent.Started, 120, "runner-1", ""},
		{jobevent.Completed, 150, "runner-1", "succeeded"},
	}
	for i, w := range want {
		e := events.got[i]
		if e.Kind != w.kind || e.At.Unix() != w.at || e.Runner != w.runner || e.Result != w.result {
			t.Errorf("event %d = %+v, want %+v", i, e, w)
		}
		if e.Tier != "tier-a" || e.JobID != "job-1" || e.Repository != "owner/repo" || e.RunID != 7 ||
			e.EventName != "push" || e.JobName != "build" || !e.QueuedAt.Equal(queued) || e.Workflow == "" || len(e.Labels) != 1 {
			t.Errorf("event %d lost GitHub's fields: %+v", i, e)
		}
	}
}

// The jobs a message assigns that its plan could not start, by its tenant's
// limit, are recorded as held with the reason: the newest ones, as many as
// the tier holds back. With nothing held, none is.
func TestJobHeldWithReason(t *testing.T) {
	job := func(id string) *scaleset.JobAssigned {
		return &scaleset.JobAssigned{JobMessageBase: scaleset.JobMessageBase{JobID: id, OwnerName: "owner", RepositoryName: "repo"}}
	}
	events := &eventLog{}
	held, why := 0, `tenant "acme" is at its limit of 2 runner(s) at once`
	var planned []int
	tracker := health.NewTracker("tier-a")
	a := &scaler{events: events, tier: "tier-a", log: slog.New(slog.DiscardHandler), stats: &statsRecorder{}, health: tracker,
		held: func() (int, string, string) { return held, "limit", why },
		onDesired: func(_ context.Context, n int) int {
			planned = append(planned, n)
			held = 1 // the plan launches one and holds one back
			return 1
		}}
	msg := &scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2},
		JobAssignedMessages: []*scaleset.JobAssigned{job("job-1"), nil, job("job-2")}}
	if err := a.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(planned) != 1 || planned[0] != 2 {
		t.Fatalf("planned %v: the hold must be read after the plan", planned)
	}
	if len(events.got) != 1 {
		t.Fatalf("events = %+v", events.got)
	}
	if h := events.got[0]; h.Kind != jobevent.Held || h.Reason != why || h.JobID != "job-2" || h.Tier != "tier-a" || h.Repository != "owner/repo" {
		t.Fatalf("held event = %+v", h)
	}
	if got := tracker.Snapshot().HeldJobs; len(got) != 1 || got["limit"] != 1 {
		t.Fatalf("held jobs in the tier's health = %v, want 1 by its limit", got)
	}

	held, events.got = 0, nil
	a.onDesired = func(context.Context, int) int { return 1 }
	if err := a.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(events.got) != 0 {
		t.Fatalf("recorded held with nothing held: %+v", events.got)
	}
}
