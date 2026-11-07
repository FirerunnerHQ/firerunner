package listener

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/actions/scaleset"
)

// Scale does what actions/scaleset v0.4.0's listener did with a message:
// acquires the jobs it offers, tells of starts and completions, and sizes the
// tier from its statistics; an empty long poll resizes from the last ones.
func TestScaleActsOnAMessage(t *testing.T) {
	var acquired []int64
	var busy, done []string
	var desired []int
	a := &scaler{
		log: testLogger(), stats: &statsRecorder{},
		acquire: func(_ context.Context, ids []int64) ([]int64, error) {
			acquired = append(acquired, ids...)
			return ids, nil
		},
		onBusy:      func(r string) { busy = append(busy, r) },
		onCompleted: func(r string) { done = append(done, r) },
		onDesired:   func(_ context.Context, n int) int { desired = append(desired, n); return n },
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
	if fmt.Sprint(acquired, busy, done, desired) != "[7 8] [r-1] [r-0] [3]" {
		t.Fatalf("acquired %v, busy %v, completed %v, desired %v", acquired, busy, done, desired)
	}
	if err := a.Scale(context.Background(), nil); err != nil || fmt.Sprint(desired) != "[3 3]" {
		t.Fatalf("an empty poll sized the tier from %v, %v", desired, err)
	}
}

// A message whose jobs cannot be acquired fails, so the listener keeps it for
// another delivery instead of deleting it.
func TestScaleFailsWhenJobsCannotBeAcquired(t *testing.T) {
	a := &scaler{log: testLogger(), stats: &statsRecorder{},
		acquire:   func(context.Context, []int64) ([]int64, error) { return nil, errors.New("503") },
		onDesired: func(context.Context, int) int { return 0 },
	}
	msg := &scaleset.RunnerScaleSetMessage{MessageID: 1,
		JobAvailableMessages: []*scaleset.JobAvailable{{JobMessageBase: scaleset.JobMessageBase{RunnerRequestID: 7}}}}
	if err := a.Scale(context.Background(), msg); err == nil {
		t.Fatal("a failed acquisition passed")
	}
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
