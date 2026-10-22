package ec2

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// launchN creates n pending instances in f and returns their IDs.
func launchN(t *testing.T, f *fakeEC2, n int) []string {
	t.Helper()
	var ids []string
	for range n {
		out, err := f.RunInstances(context.Background(), &ec2.RunInstancesInput{
			InstanceType: types.InstanceTypeC7aXlarge, SubnetId: aws.String("subnet-a"),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, *out.Instances[0].InstanceId)
	}
	return ids
}

func next(t *testing.T, ch <-chan types.InstanceStateName) types.InstanceStateName {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no state published")
		return ""
	}
}

func TestWatcher_BatchesAllInstancesIntoOneCall(t *testing.T) {
	f := newFakeEC2()
	ids := launchN(t, f, 3)
	w := newWatcher(f, 5*time.Millisecond, quietLog())
	var chans []<-chan types.InstanceStateName
	for _, id := range ids {
		ch, stop := w.watch(id)
		defer stop()
		chans = append(chans, ch)
	}
	f.setState(ids[1], types.InstanceStateNameRunning)
	for i, ch := range chans {
		want := types.InstanceStateNamePending
		if i == 1 {
			want = types.InstanceStateNameRunning
		}
		// Drain until the state settles on what the fake reports.
		deadline := time.After(2 * time.Second)
		for s := next(t, ch); s != want; s = next(t, ch) {
			select {
			case <-deadline:
				t.Fatalf("instance %d state %q, want %q", i, s, want)
			default:
			}
		}
	}
	// Every poll after all three were watched covers them in a single call
	// (a tick racing the setup above may have seen fewer).
	f.mu.Lock()
	defer f.mu.Unlock()
	if last := f.describeIDs[len(f.describeIDs)-1]; len(last) != 3 {
		t.Fatalf("latest poll described %d IDs, want all 3 in one call", len(last))
	}
}

func TestWatcher_NotFoundFallsBackPerInstance(t *testing.T) {
	// A just-launched instance unknown to DescribeInstances must not hide the
	// state of the others in its batch.
	f := newFakeEC2()
	ids := launchN(t, f, 2)
	f.invisible[ids[0]] = true
	f.setState(ids[1], types.InstanceStateNameStopped)
	w := newWatcher(f, 5*time.Millisecond, quietLog())
	young, stopYoung := w.watch(ids[0])
	defer stopYoung()
	old, stopOld := w.watch(ids[1])
	defer stopOld()
	if s := next(t, old); s != types.InstanceStateNameStopped {
		t.Fatalf("visible instance state %q, want stopped", s)
	}
	select {
	case s := <-young:
		t.Fatalf("invisible instance published state %q", s)
	default:
	}
	// Once it becomes visible, its state flows.
	f.mu.Lock()
	delete(f.invisible, ids[0])
	f.mu.Unlock()
	if s := next(t, young); s != types.InstanceStateNamePending {
		t.Fatalf("young instance state %q, want pending", s)
	}
}

func TestWatcher_StopsWhenNothingWatched(t *testing.T) {
	f := newFakeEC2()
	ids := launchN(t, f, 1)
	w := newWatcher(f, 5*time.Millisecond, quietLog())
	ch, stop := w.watch(ids[0])
	next(t, ch)
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.mu.Lock()
		running := w.running
		w.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher still polling with nothing watched")
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.describeCalls
	}
	before := calls()
	time.Sleep(30 * time.Millisecond)
	if n := calls() - before; n != 0 {
		t.Fatalf("describe called %d more times after the last unwatch", n)
	}
	// And it restarts for a new watch.
	ch2, stop2 := w.watch(ids[0])
	defer stop2()
	next(t, ch2)
}

func TestWatcher_DescribeErrorKeepsPolling(t *testing.T) {
	f := newFakeEC2()
	ids := launchN(t, f, 1)
	f.describeErr = apiErr("RequestLimitExceeded")
	w := newWatcher(f, 5*time.Millisecond, quietLog())
	ch, stop := w.watch(ids[0])
	defer stop()
	time.Sleep(30 * time.Millisecond)
	f.mu.Lock()
	f.describeErr = nil
	f.mu.Unlock()
	if s := next(t, ch); s != types.InstanceStateNamePending {
		t.Fatalf("state after throttling cleared = %q", s)
	}
}

func TestWatcher_SplitsLargeBatches(t *testing.T) {
	f := newFakeEC2()
	ids := launchN(t, f, describeBatch+5)
	w := newWatcher(f, 5*time.Millisecond, quietLog())
	for _, id := range ids {
		_, stop := w.watch(id)
		defer stop()
	}
	// Wait for a full poll: the calls together cover every watched ID.
	covered := func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		seen := map[string]bool{}
		for _, call := range f.describeIDs {
			for _, id := range call {
				seen[id] = true
			}
		}
		return len(seen) == len(ids)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !covered() {
		if time.Now().After(deadline) {
			t.Fatal("no poll covered every watched instance")
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.describeIDs) < 2 {
		t.Fatalf("%d DescribeInstances calls for %d IDs, want them split", len(f.describeIDs), len(ids))
	}
	for _, call := range f.describeIDs {
		if len(call) > describeBatch {
			t.Fatalf("one call described %d IDs, cap is %d", len(call), describeBatch)
		}
	}
}
