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
	// receive: the time to apply it, one GitHub lookup and a transaction.
	// Its group is kept after it is deleted. One not deleted by then is
	// delivered again; the queue's redrive policy moves one received too
	// often to its dead-letter queue.
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
	// now and sleep are the clock, and leaseCheck how often a KeepAll
	// looks whether this shard is still the one; tests replace them.
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration)
	leaseCheck time.Duration

	mu       sync.Mutex
	status   WorkerStatus
	lastKeep time.Time
}

// WorkerStatus is what the worker has done, for status and doctor.
type WorkerStatus struct {
	Active  bool `json:"active"`
	Applied int  `json:"applied"`
	// Dropped are malformed deliveries, deleted: they never apply.
	Dropped int `json:"dropped"`
	// Rejected are deliveries whose signature does not hold, left on the
	// queue for its dead-letter queue: a webhook secret rotated on GitHub
	// before --onboard-webhook-secret-file makes GitHub's own fail, and
	// they can be replayed from there.
	Rejected     int       `json:"rejected"`
	LastRejectAt time.Time `json:"last_reject_at,omitzero"`
	// Failed are deliveries that failed to apply, left for a retry.
	Failed       int       `json:"failed"`
	LastDelivery time.Time `json:"last_delivery,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	// ReceiveError is the queue's last failure, cleared by a receive that
	// succeeds.
	ReceiveError   string    `json:"receive_error,omitempty"`
	ReceiveErrorAt time.Time `json:"receive_error_at,omitzero"`
	KeptAllAt      time.Time `json:"kept_all_at,omitzero"`
	KeepAllError   string    `json:"keep_all_error,omitempty"`
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
// if due, then receives and applies one delivery. One at a time, so the
// visibility timeout bounds one delivery's work, not a batch's.
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
		w.keepAll(ctx)
	}
	out, err := w.Queue.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(w.QueueURL),
		MaxNumberOfMessages: 1,
		WaitTimeSeconds:     20,
		VisibilityTimeout:   int32(visibility / time.Second),
	})
	if err != nil {
		if ctx.Err() == nil {
			w.Log.Warn("onboarding: receiving from the queue", "err", err)
			w.count(func(s *WorkerStatus) { s.ReceiveError, s.ReceiveErrorAt = err.Error(), w.clock() })
			w.wait(ctx, idleWait)
		}
		return
	}
	w.count(func(s *WorkerStatus) { s.ReceiveError, s.ReceiveErrorAt = "", time.Time{} })
	for _, m := range out.Messages {
		if ctx.Err() != nil || !w.Active() {
			// Not deleted: delivered again, to whichever shard works.
			return
		}
		w.apply(ctx, m)
	}
}

// keepAll keeps every installation's group, and stops once this shard is
// no longer the one to work.
func (w *Worker) keepAll(ctx context.Context) {
	kctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	every := w.leaseCheck
	if every == 0 {
		every = idleWait
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case <-kctx.Done():
				return
			case <-time.After(every):
				if !w.Active() {
					cancel()
					return
				}
			}
		}
	}()
	err := w.Keeper.KeepAll(kctx)
	w.mu.Lock()
	w.lastKeep = w.clock()
	w.status.KeptAllAt, w.status.KeepAllError = w.lastKeep, ""
	if err != nil && kctx.Err() == nil {
		w.status.KeepAllError = err.Error()
	}
	w.mu.Unlock()
	if err != nil && kctx.Err() == nil {
		w.Log.Warn("onboarding: keeping the runner groups", "err", err)
	}
}

// apply applies one message, and deletes it once it is applied, or will
// never apply. Its runner group is kept after: a failure there is kept by
// the next KeepAll, and the delivery is applied either way.
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
	case errors.Is(err, ErrSignature):
		// Left for the dead-letter queue: GitHub's own fail while the
		// secret here is not the App's, and replay from there once it is.
		w.Log.Warn("onboarding: delivery rejected: its signature does not hold against --onboard-webhook-secret-file; left for the dead-letter queue", "delivery", env.Delivery, "event", env.Event)
		w.count(func(s *WorkerStatus) { s.Rejected++; s.LastRejectAt = w.clock() })
		return
	case errors.Is(err, ErrMalformed):
		// Never applies: not what GitHub sends.
		w.Log.Warn("onboarding: delivery dropped", "delivery", env.Delivery, "event", env.Event, "err", err)
		w.count(func(s *WorkerStatus) { s.Dropped++ })
	case err != nil:
		if ctx.Err() == nil { // stopping is no failure: delivered again
			w.failed(fmt.Errorf("delivery %s: %w", env.Delivery, err))
		}
		return
	default:
		w.Log.Info("onboarding: delivery applied", "delivery", out.Delivery, "event", out.Event, "action", out.Action,
			"installation", out.Installation, "account", out.Account, "tenant", out.Tenant, "result", out.Result, "reason", out.Reason)
		w.count(func(s *WorkerStatus) { s.Applied++; s.LastDelivery = w.clock() })
	}
	if _, err := w.Queue.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.QueueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
		// Delivered again, and applied once: the job store says duplicate.
		w.Log.Warn("onboarding: deleting a delivery from the queue", "delivery", env.Delivery, "err", err)
	}
	if out.KeepGroup && ctx.Err() == nil {
		if res, kept, err := w.Keeper.Keep(ctx, out.Installation); err != nil {
			w.Log.Warn("onboarding: keeping a runner group", "installation", out.Installation, "err", err)
		} else if kept {
			w.Log.Info("onboarding: runner group kept", "installation", out.Installation, "state", res.State, "reason", res.Reason,
				"group", res.GroupID, "all", res.All, "served", res.Served, "public", len(res.Public))
		}
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
