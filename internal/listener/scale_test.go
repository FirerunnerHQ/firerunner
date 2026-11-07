package listener

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/health"
)

// Scale does what actions/scaleset v0.4.0's listener did with a message, in its
// order: acquires the jobs it offers, tells of starts, then completions, then
// sizes the tier, which a completion's re-plan relies on. An empty long poll
// resizes from the last statistics.
func TestScaleActsOnAMessage(t *testing.T) {
	var seen []string
	note := func(f string, v ...any) { seen = append(seen, fmt.Sprintf(f, v...)) }
	a := &scaler{
		log: testLogger(), stats: &statsRecorder{},
		acquire: func(_ context.Context, ids []int64) ([]int64, error) {
			note("acquire %v", ids)
			return ids, nil
		},
		onBusy:      func(r string) { note("busy %s", r) },
		onCompleted: func(r string) { note("completed %s", r) },
		onDesired:   func(_ context.Context, n int) int { note("desired %d", n); return n },
	}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID:            1,
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}, {JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 8}}},
		JobStartedMessages:   []*scaleset.JobStarted{{RunnerName: "r-1"}},
		JobCompletedMessages: []*scaleset.JobCompleted{{RunnerName: "r-0"}},
	}
	if err := a.Scale(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := a.Scale(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(seen); got != "[acquire [7 8] busy r-1 completed r-0 desired 3 desired 3]" {
		t.Fatalf("Scale did %s", got)
	}
}

// A message whose jobs cannot be acquired is still acted on and succeeds:
// failing would keep it, and one that always fails would block the tier.
func TestScaleGoesOnWhenJobsCannotBeAcquired(t *testing.T) {
	var desired []int
	var ended error
	a := &scaler{log: testLogger(), stats: &statsRecorder{},
		acquire:    func(context.Context, []int64) ([]int64, error) { return nil, errors.New("503") },
		onDesired:  func(_ context.Context, n int) int { desired = append(desired, n); return n },
		endSession: func(err error) { ended = err },
	}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2},
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}
	if err := a.Scale(context.Background(), msg); err != nil || fmt.Sprint(desired) != "[2]" {
		t.Fatalf("Scale = %v, desired %v", err, desired)
	}
	if ended == nil {
		t.Fatal("the session goes on: a new one would offer the jobs again")
	}
}

// A session ended that way is reopened as after a failure, and its message is
// deleted before it ends.
func TestFailedAcquisitionReopensTheSession(t *testing.T) {
	fastReconnect(t)
	first := newFakeSession(0, false)
	first.failAcquire.Store(true)
	first.msgs <- &scaleset.RunnerScaleSetMessage{MessageID: 9, Statistics: &scaleset.RunnerScaleSetStatistic{},
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}
	tr := health.NewTracker("a")
	o := &opener{results: []openResult{{sess: newFakeSession(0, false)}}}
	ss := testScaleSet("a", first, o, tr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ss.Run(ctx, func(context.Context, int) int { return 0 }, nil, nil, nil) }()
	eventually(t, "a new session", func() bool { return o.opened() >= 1 })
	if got := first.deleted(); fmt.Sprint(got) != "[9]" {
		t.Fatalf("deleted %v: the message must go before the session", got)
	}
	// status and doctor say why, not only that the session ended.
	eventually(t, "the reason in health", func() bool { return strings.Contains(tr.Snapshot().LastError, "acquire jobs") })
}

// A message in hand is acted on in full though the session's context ends.
func TestScaleFinishesAMessageAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var sawCancel bool
	a := &scaler{log: testLogger(), stats: &statsRecorder{},
		acquire: func(ctx context.Context, ids []int64) ([]int64, error) {
			sawCancel = ctx.Err() != nil
			return ids, nil
		},
		onDesired: func(context.Context, int) int { return 0 },
	}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 1,
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}
	if err := a.Scale(ctx, msg); err != nil || sawCancel {
		t.Fatalf("acquired with a cancelled context: %v, %v", sawCancel, err)
	}
}
