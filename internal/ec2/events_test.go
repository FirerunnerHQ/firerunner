package ec2

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
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

// An instance's job report reaches the tier that launched it, which is how a
// shard's tenants each resolve only their own runners.
func TestLaunch_ToldWhenItsRunnerReportsAJob(t *testing.T) {
	for name, cached := range map[string]bool{"uncached": false, "cached": true} {
		t.Run(name, func(t *testing.T) {
			heard := make(chan string, 1)
			h := newHarness(t, func(o *Options) {
				o.OnRunnerJob = func(runner string) { heard <- runner }
				if cached {
					o.EC2.Cache = true
					o.Cache = cacheOpts(t, &fakeJobs{bound: map[string]string{}})
				}
			})
			errc := h.launch(context.Background())
			inst := h.instance(t)
			if code, _ := h.fetch(inst.ip); code != http.StatusOK {
				t.Fatalf("fetch = %d", code)
			}
			h.report(t, inst.ip, "phase=job")
			select {
			case r := <-heard:
				if r != "runner-1" {
					t.Fatalf("heard %q", r)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the job report was not heard")
			}
			h.report(t, inst.ip, "phase=exited&rc=0")
			h.f.setStateFor(inst.id, types.InstanceStateNameTerminated, "Client.UserInitiatedShutdown")
			waitErr(t, errc)
		})
	}
}

// A job's instance reports what it is, each stage at its own time, and why it
// went, so usage and billing can be built from these alone.
func TestLaunch_RecordsEvents(t *testing.T) {
	events := &eventLog{}
	h := newHarness(t, func(o *Options) { o.Events = events })
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, inst.ip, "phase=listening")
	h.report(t, inst.ip, "phase=job")
	h.report(t, inst.ip, "phase=exited&rc=0")
	h.f.setStateFor(inst.id, types.InstanceStateNameTerminated, "Client.UserInitiatedShutdown")
	waitErr(t, errc)

	events.mu.Lock()
	defer events.mu.Unlock()
	var kinds []jobevent.Kind
	for _, e := range events.got {
		kinds = append(kinds, e.Kind)
		if e.Tier != "cloud-4c" || e.Runner != "runner-1" || e.Instance != inst.id || e.InstanceType != "c8a.xlarge" ||
			e.Market != "on-demand" || e.Subnet != "subnet-a" || e.At.IsZero() {
			t.Errorf("event lost the instance: %+v", e)
		}
	}
	want := []jobevent.Kind{jobevent.Launched, jobevent.InstanceGone, jobevent.Served, jobevent.Listening, jobevent.Running, jobevent.Exited}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if events.got[0].Path != PathCold {
		t.Errorf("path = %q", events.got[0].Path)
	}
	h.f.mu.Lock()
	asked := h.f.runAt
	h.f.mu.Unlock()
	assertBefore(t, events.got[0], asked)
	if events.got[1].Reason != "Client.UserInitiatedShutdown" {
		t.Errorf("reason = %q", events.got[1].Reason)
	}
}

// A launch dates from before EC2 was asked, or the config's fetch could precede it
// and show a negative boot time.
func assertBefore(t *testing.T, e jobevent.Event, asked time.Time) {
	t.Helper()
	if asked.IsZero() || e.At.After(asked) {
		t.Errorf("launched at %v, EC2 was asked at %v", e.At, asked)
	}
}

// An instance that outlives its termination may still run and bill, so it is not
// recorded as gone.
func TestStuckInstanceIsNotRecordedAsGone(t *testing.T) {
	events := &eventLog{}
	h := newHarness(t, func(o *Options) { o.Events = events })
	h.f.terminateErr = apiErr("RequestLimitExceeded")
	ctx, cancel := context.WithCancel(context.Background())
	errc := h.launch(ctx)
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	cancel()
	if err := waitErr(t, errc); err == nil {
		t.Fatal("the launch should give up")
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	for _, e := range events.got {
		if e.Kind == jobevent.InstanceGone {
			t.Fatalf("recorded as gone: %+v", e)
		}
	}
}

// A spot interruption's own reason stands: AWS's fault injection shows on the
// instance as the user's own shutdown, which would read as a normal end.
func TestInterruptionReasonIsRecorded(t *testing.T) {
	events := &eventLog{}
	h := newHarness(t, func(o *Options) {
		o.Events = events
		o.EC2.Market = config.MarketSpot
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.mu.Lock()
	h.f.instances[inst.id].spotStatus = "instance-terminated-by-experiment"
	h.f.mu.Unlock()
	h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Client.UserInitiatedShutdown")
	waitErr(t, errc)
	events.mu.Lock()
	defer events.mu.Unlock()
	gone := events.got[len(events.got)-1]
	if gone.Kind != jobevent.InstanceGone || gone.Reason != "instance-terminated-by-experiment" || gone.Market != config.MarketSpot {
		t.Fatalf("gone = %+v", gone)
	}
}

// A pool instance reports its own path, and its market.
func TestPoolLaunch_RecordsEvents(t *testing.T) {
	events := &eventLog{}
	h := poolHarness(t, 1, config.MarketSpot)
	h.p.o.Events = events
	h.runPool(t)
	eventually(t, "a warming instance", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "a ready instance", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	errc := h.launch(context.Background())
	eventually(t, "an event", func() bool {
		events.mu.Lock()
		defer events.mu.Unlock()
		return len(events.got) > 0
	})
	events.mu.Lock()
	first := events.got[0]
	events.mu.Unlock()
	if first.Kind != jobevent.Launched || first.Path != PathPool || first.Market != config.MarketSpot {
		t.Fatalf("first = %+v", first)
	}
	h.f.mu.Lock()
	asked := h.f.startAt
	h.f.mu.Unlock()
	assertBefore(t, first, asked)
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
}

// A busy runner left to the next process at shutdown keeps its instance, and
// records its runner's start, which the next process cannot.
func TestLaunch_HandOffLeavesTheInstance(t *testing.T) {
	events := &eventLog{}
	h := newHarness(t, func(o *Options) { o.Events = events })
	ctx, cancel := context.WithCancelCause(context.Background())
	errc := h.launch(ctx)
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	h.report(t, inst.ip, "phase=job")
	cancel(core.ErrHandedOff)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v, want nil", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v; a handed-off instance runs its job on", ids)
	}
	if s := h.f.state(inst.id); s != types.InstanceStateNameRunning {
		t.Fatalf("instance state %q, want running", s)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	var running bool
	for _, e := range events.got {
		running = running || e.Kind == jobevent.Running
		if e.Kind == jobevent.InstanceGone {
			t.Fatalf("recorded the instance gone: %+v", e)
		}
	}
	if !running {
		t.Fatalf("the runner's start is not recorded: %+v", events.got)
	}
}
