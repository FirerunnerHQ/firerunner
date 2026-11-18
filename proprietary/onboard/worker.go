package onboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// Queue is the SQS calls the worker makes; *sqs.Client satisfies it.
type Queue interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// Envelope is a webhook delivery as the ingress puts it on the queue:
// GitHub's headers, and the body as it arrived, base64, as its signature
// is over those bytes.
type Envelope struct {
	Delivery  string `json:"delivery"`
	Event     string `json:"event"`
	Signature string `json:"signature"`
	Body      string `json:"body"`
}

// Delivery decodes an envelope.
func (e Envelope) delivery() (Delivery, error) {
	body, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		return Delivery{}, fmt.Errorf("%w: the body is not base64: %v", ErrMalformed, err)
	}
	return Delivery{ID: e.Delivery, Event: e.Event, Signature: e.Signature, Body: body}, nil
}

const (
	// visibility is how long a received delivery is hidden from another
	// receive: the time to apply it and keep its group. One not deleted
	// by then is delivered again; the queue's redrive policy moves one
	// received too often to its dead-letter queue.
	visibility = 120 * time.Second
	// idleWait is how often a shard not holding the assigner's lease looks
	// again.
	idleWait = 5 * time.Second
	// keepEvery is how often every installation's runner group is kept, as
	// a webhook can be lost.
	keepEvery = 10 * time.Minute
)

// Worker applies the deliveries on the queue, on the shard that holds the
// assigner's lease (Active), and keeps every installation's runner group
// every so often. Two shards at a handover may both apply a delivery: the
// job store applies each once, one at a time.
type Worker struct {
	Queue    Queue
	QueueURL string
	Config   Config
	Store    Store
	Keeper   Keeper
	// Active reports whether this shard is the one to work.
	Active func() bool
	Log    *slog.Logger
	// now and sleep are the clock; tests replace them.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration)

	mu       sync.Mutex
	status   WorkerStatus
	lastKeep time.Time
}

// WorkerStatus is what the worker has done, for status and doctor.
type WorkerStatus struct {
	Active       bool      `json:"active"`
	Applied      int       `json:"applied"`
	Dropped      int       `json:"dropped"`
	Failed       int       `json:"failed"`
	LastDelivery time.Time `json:"last_delivery,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	KeptAllAt    time.Time `json:"kept_all_at,omitzero"`
	KeepAllError string    `json:"keep_all_error,omitempty"`
}

// Status returns what the worker has done.
func (w *Worker) Status() WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *Worker) wait(ctx context.Context, d time.Duration) {
	if w.sleep != nil {
		w.sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run works until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		w.Step(ctx)
	}
}

// Step works once: idles a while when not active; else keeps every group
// if due, then receives and applies one batch.
func (w *Worker) Step(ctx context.Context) {
	active := w.Active()
	w.mu.Lock()
	w.status.Active = active
	due := w.clock().Sub(w.lastKeep) >= keepEvery
	w.mu.Unlock()
	if !active {
		w.wait(ctx, idleWait)
		return
	}
	if due {
		err := w.Keeper.KeepAll(ctx)
		w.mu.Lock()
		w.lastKeep = w.clock()
		w.status.KeptAllAt, w.status.KeepAllError = w.lastKeep, ""
		if err != nil {
			w.status.KeepAllError = err.Error()
		}
		w.mu.Unlock()
		if err != nil {
			w.Log.Warn("onboarding: keeping the runner groups", "err", err)
		}
	}
	out, err := w.Queue.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(w.QueueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     20,
		VisibilityTimeout:   int32(visibility / time.Second),
	})
	if err != nil {
		if ctx.Err() == nil {
			w.failed(fmt.Errorf("receive: %w", err))
			w.wait(ctx, idleWait)
		}
		return
	}
	for _, m := range out.Messages {
		if ctx.Err() != nil || !w.Active() {
			// Not deleted: delivered again, to whichever shard works.
			return
		}
		w.apply(ctx, m)
	}
}

// apply applies one message, and deletes it unless a retry may succeed.
func (w *Worker) apply(ctx context.Context, m sqstypes.Message) {
	var env Envelope
	d, err := func() (Delivery, error) {
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &env); err != nil {
			return Delivery{}, fmt.Errorf("%w: not an envelope: %v", ErrMalformed, err)
		}
		return env.delivery()
	}()
	var out Outcome
	if err == nil {
		out, err = Handle(ctx, w.Config, w.Store, d)
	}
	switch {
	case errors.Is(err, ErrSignature), errors.Is(err, ErrMalformed):
		// Never applies: not GitHub's, or not what it sends.
		w.Log.Warn("onboarding: delivery dropped", "delivery", env.Delivery, "event", env.Event, "err", err)
		w.count(func(s *WorkerStatus) { s.Dropped++ })
	case err != nil:
		w.failed(fmt.Errorf("delivery %s: %w", env.Delivery, err))
		return
	default:
		w.Log.Info("onboarding: delivery applied", "delivery", out.Delivery, "event", out.Event, "action", out.Action,
			"installation", out.Installation, "account", out.Account, "tenant", out.Tenant, "result", out.Result, "reason", out.Reason)
		w.count(func(s *WorkerStatus) { s.Applied++; s.LastDelivery = w.clock() })
		if out.KeepGroup {
			// A failure here is kept by the next KeepAll; the delivery is
			// applied either way.
			if res, kept, err := w.Keeper.Keep(ctx, out.Installation); err != nil {
				w.Log.Warn("onboarding: keeping a runner group", "installation", out.Installation, "err", err)
			} else if kept {
				w.Log.Info("onboarding: runner group kept", "installation", out.Installation, "state", res.State, "reason", res.Reason,
					"group", res.GroupID, "all", res.All, "served", res.Served, "public", len(res.Public))
			}
		}
	}
	if _, err := w.Queue.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.QueueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
		// Delivered again, and applied once: the job store says duplicate.
		w.Log.Warn("onboarding: deleting a delivery from the queue", "delivery", env.Delivery, "err", err)
	}
}

func (w *Worker) failed(err error) {
	w.Log.Warn("onboarding: delivery not applied; retried", "err", err)
	w.count(func(s *WorkerStatus) { s.Failed++; s.LastError, s.LastErrorAt = err.Error(), w.clock() })
}

func (w *Worker) count(f func(*WorkerStatus)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(&w.status)
}
