package listener

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"

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

// A job assigned while its tenant is at a limit is recorded as held, with the
// reason, beside its assignment; one assigned with room is not.
func TestJobHeldWithReason(t *testing.T) {
	base := scaleset.JobMessageBase{JobID: "job-1", OwnerName: "owner", RepositoryName: "repo"}
	events := &eventLog{}
	why := `tenant "acme" is at its limit of 2 runner(s) at once`
	f := newFakeSession(0, false)
	ts := trackedSession{session: f, events: events, tier: "tier-a", held: func() string { return why }}
	f.msgs <- &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base}}}
	if _, err := ts.GetMessage(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if len(events.got) != 2 || events.got[0].Kind != jobevent.Assigned || events.got[1].Kind != jobevent.Held {
		t.Fatalf("events = %+v", events.got)
	}
	if h := events.got[1]; h.Reason != why || h.JobID != "job-1" || h.Tier != "tier-a" || h.Repository != "owner/repo" {
		t.Fatalf("held event = %+v", h)
	}

	why = ""
	f.msgs <- &scaleset.RunnerScaleSetMessage{JobAssignedMessages: []*scaleset.JobAssigned{{JobMessageBase: base}}}
	if _, err := ts.GetMessage(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if len(events.got) != 3 || events.got[2].Kind != jobevent.Assigned {
		t.Fatalf("a job assigned with room was recorded as held: %+v", events.got)
	}
}
