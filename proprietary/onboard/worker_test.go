package onboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/firerunnerhq/firerunner/proprietary/jobstore"
)

type fakeQueue struct {
	messages   []sqstypes.Message
	received   int
	deleted    []string
	receiveErr error
	asked      *sqs.ReceiveMessageInput
}

func (q *fakeQueue) ReceiveMessage(_ context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	q.received++
	q.asked = in
	if q.receiveErr != nil {
		return nil, q.receiveErr
	}
	out := &sqs.ReceiveMessageOutput{Messages: q.messages}
	q.messages = nil
	return out, nil
}

func (q *fakeQueue) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	q.deleted = append(q.deleted, aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, nil
}

func message(handle string, d Delivery) sqstypes.Message {
	b, _ := json.Marshal(Envelope{Delivery: d.ID, Event: d.Event, Signature: d.Signature, Body: base64.StdEncoding.EncodeToString(d.Body)})
	return sqstypes.Message{ReceiptHandle: aws.String(handle), Body: aws.String(string(b))}
}

type workerFixture struct {
	w      *Worker
	q      *fakeQueue
	s      *fakeStore
	api    *fakeAPI
	active bool
	now    time.Time
	slept  int
}

func newWorker(t *testing.T) *workerFixture {
	f := &workerFixture{q: &fakeQueue{}, s: newFake(), active: true, now: base}
	api, gh := newAPI(t)
	f.api = api
	api.repos = []Repository{repo(1, "app", "private")}
	// The keeper reads what onboarding wrote.
	gs := &keeperView{s: f.s, owned: map[int64]int64{}}
	f.w = &Worker{Queue: f.q, QueueURL: "https://sqs/q", Config: cfg, Store: f.s,
		Keeper: Keeper{Store: gs, GitHub: gh, Group: "firerunner"},
		Active: func() bool { return f.active }, Log: slog.New(slog.DiscardHandler),
		now:   func() time.Time { return f.now },
		sleep: func(context.Context, time.Duration) { f.slept++ }}
	return f
}

// keeperView reads the fake store's installations, as the job store gives
// the keeper what deliveries wrote.
type keeperView struct {
	s     *fakeStore
	owned map[int64]int64
}

func (v *keeperView) ReadInstallation(_ context.Context, id int64) (jobstore.Installation, bool, error) {
	in, ok := v.s.installations[id]
	return in, ok, nil
}

func (v *keeperView) ActiveInstallations(context.Context) ([]jobstore.Installation, error) {
	var out []jobstore.Installation
	for _, in := range v.s.installations {
		if in.State == "active" {
			out = append(out, in)
		}
	}
	return out, nil
}

func (v *keeperView) LockInstallation(context.Context, int64) (func(), error) { return func() {}, nil }

func (v *keeperView) OwnedGroup(_ context.Context, id int64) (int64, error) { return v.owned[id], nil }

func (v *keeperView) RecordGroup(_ context.Context, id, group int64, _ json.RawMessage) error {
	if group != 0 {
		v.owned[id] = group
	}
	return nil
}

// A delivery is applied, its group kept, and it leaves the queue.
func TestWorkerApplies(t *testing.T) {
	f := newWorker(t)
	f.q.messages = []sqstypes.Message{message("m1", event("created", 7, "acme", "", 0))}
	f.w.Step(context.Background())
	if _, ok := tenant(t, f.s, "acme"); !ok {
		t.Fatal("no tenant")
	}
	if !slices.Equal(f.q.deleted, []string{"m1"}) {
		t.Fatalf("deleted %v", f.q.deleted)
	}
	if len(f.api.groups) != 1 || f.api.groups[0].Name != "firerunner" {
		t.Fatalf("groups %+v", f.api.groups)
	}
	if st := f.w.Status(); st.Applied != 1 || !st.Active || st.KeptAllAt.IsZero() {
		t.Fatalf("status %+v", st)
	}
	if a := f.q.asked; aws.ToInt32(&a.WaitTimeSeconds) != 20 || a.VisibilityTimeout != 120 {
		t.Fatalf("receive %+v", a)
	}
}

// What can never apply leaves the queue, counted; what may yet apply
// stays, to be delivered again.
func TestWorkerDropsOrRetries(t *testing.T) {
	f := newWorker(t)
	good := event("created", 8, "beta", "", 0)
	forged := good
	forged.Signature = "sha256=" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	f.q.messages = []sqstypes.Message{
		message("forged", forged),
		{ReceiptHandle: aws.String("not-json"), Body: aws.String("{")},
		{ReceiptHandle: aws.String("not-base64"), Body: aws.String(`{"delivery":"x","event":"installation","body":"%%%"}`)},
		message("db-down", good),
	}
	f.s.failPut = errors.New("database down")
	f.w.Step(context.Background())
	if !slices.Equal(f.q.deleted, []string{"forged", "not-json", "not-base64"}) {
		t.Fatalf("deleted %v", f.q.deleted)
	}
	if st := f.w.Status(); st.Dropped != 3 || st.Failed != 1 || st.Applied != 0 || st.LastError == "" {
		t.Fatalf("status %+v", st)
	}
}

// A shard not holding the assigner's lease receives nothing; one that
// loses it mid-batch leaves the rest on the queue.
func TestWorkerOnlyWhenActive(t *testing.T) {
	f := newWorker(t)
	f.active = false
	f.w.Step(context.Background())
	if f.q.received != 0 || f.slept != 1 || f.w.Status().Active {
		t.Fatalf("received %d, slept %d", f.q.received, f.slept)
	}
	f.active = true
	calls := 0
	f.w.Active = func() bool { calls++; return calls <= 2 } // the step, then the first message
	f.q.messages = []sqstypes.Message{message("m1", event("created", 9, "acme", "", 0)), message("m2", accountEvent("created", 10, 901, "two", "", 0))}
	f.w.Step(context.Background())
	if !slices.Equal(f.q.deleted, []string{"m1"}) {
		t.Fatalf("deleted %v", f.q.deleted)
	}
	if _, ok := tenant(t, f.s, "two"); ok {
		t.Fatal("applied after the lease was lost")
	}
}

// Every group is kept at the first step and every keepEvery after.
func TestWorkerKeepsAllEverySoOften(t *testing.T) {
	f := newWorker(t)
	f.w.Step(context.Background())
	first := f.w.Status().KeptAllAt
	f.now = f.now.Add(keepEvery - time.Second)
	f.w.Step(context.Background())
	if f.w.Status().KeptAllAt != first {
		t.Fatal("kept again before keepEvery")
	}
	f.now = f.now.Add(time.Second)
	f.w.Step(context.Background())
	if !f.w.Status().KeptAllAt.After(first) {
		t.Fatal("not kept again at keepEvery")
	}
}

// A queue that fails is retried after a wait, and said.
func TestWorkerReceiveFails(t *testing.T) {
	f := newWorker(t)
	f.q.receiveErr = errors.New("throttled")
	f.w.Step(context.Background())
	if st := f.w.Status(); st.Failed != 1 || f.slept != 1 {
		t.Fatalf("status %+v, slept %d", st, f.slept)
	}
}
