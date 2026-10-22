package ec2

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// describeBatch caps the instance IDs per DescribeInstances call.
const describeBatch = 100

// errNotFound is the code EC2 returns for an instance ID it does not know yet:
// a just-launched instance can be missing from DescribeInstances for a few
// seconds (eventual consistency).
const errNotFound = "InvalidInstanceID.NotFound"

// watcher polls the state of every instance some Launch is waiting on with one
// DescribeInstances call per interval, rather than one poller per instance,
// and hands each waiter its instance's latest state. It runs only while
// something is watched.
type watcher struct {
	api      API
	interval time.Duration
	log      *slog.Logger

	mu      sync.Mutex
	subs    map[string]chan types.InstanceStateName
	running bool
}

func newWatcher(api API, interval time.Duration, log *slog.Logger) *watcher {
	return &watcher{api: api, interval: interval, log: log, subs: map[string]chan types.InstanceStateName{}}
}

// watch returns a channel that receives id's state after each poll (only the
// latest is kept if the reader falls behind) and a func to stop watching.
func (w *watcher) watch(id string) (<-chan types.InstanceStateName, func()) {
	ch := make(chan types.InstanceStateName, 1)
	w.mu.Lock()
	w.subs[id] = ch
	if !w.running {
		w.running = true
		go w.loop()
	}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		if w.subs[id] == ch {
			delete(w.subs, id)
		}
		w.mu.Unlock()
	}
}

func (w *watcher) loop() {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for range t.C {
		w.mu.Lock()
		ids := make([]string, 0, len(w.subs))
		for id := range w.subs {
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()
		for start := 0; start < len(ids); start += describeBatch {
			w.poll(ids[start:min(start+describeBatch, len(ids))])
		}
	}
}

// poll describes ids and publishes their states. When the batch fails because
// one ID is not visible yet, it falls back to describing each ID alone so one
// young instance does not blind the watcher to all the others.
func (w *watcher) poll(ids []string) {
	// Polls must not be cut short by any one Launch's cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	states, err := w.describe(ctx, ids)
	if isCode(err, errNotFound) && len(ids) > 1 {
		states, err = map[string]types.InstanceStateName{}, nil
		for _, id := range ids {
			s, err1 := w.describe(ctx, []string{id})
			switch {
			case err1 == nil:
				states[id] = s[id]
			case !isCode(err1, errNotFound):
				err = err1
			}
		}
	}
	if err != nil && !isCode(err, errNotFound) {
		w.log.Warn("describe instances", "count", len(ids), "err", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, s := range states {
		ch, ok := w.subs[id]
		if !ok {
			continue
		}
		select { // keep only the latest state
		case <-ch:
		default:
		}
		ch <- s
	}
}

func (w *watcher) describe(ctx context.Context, ids []string) (map[string]types.InstanceStateName, error) {
	out, err := w.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, err
	}
	states := make(map[string]types.InstanceStateName, len(ids))
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if i.InstanceId != nil && i.State != nil {
				states[*i.InstanceId] = i.State.Name
			}
		}
	}
	return states, nil
}

// isCode reports whether err is an AWS API error with the given code.
func isCode(err error, code string) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == code
}
