package ec2

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// polledAlone counts the DescribeInstances calls for id alone: the
// watcher's polls of that one instance, not the pool's own listings.
func (f *fakeEC2) polledAlone(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ids := range f.describeIDs {
		if len(ids) == 1 && ids[0] == id {
			n++
		}
	}
	return n
}

// interruptions records what Options.Interrupted is told.
type interruptions struct {
	mu        sync.Mutex
	duringJob []bool
}

func (r *interruptions) record(duringJob bool) {
	r.mu.Lock()
	r.duringJob = append(r.duringJob, duringJob)
	r.mu.Unlock()
}

func (r *interruptions) got() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.duringJob...)
}

// TestLaunch_SpotInterruption: a spot instance AWS terminates under its runner
// is reported (during the job or before it) and backs its option off, but is
// no launch failure: the scheduler must not back off for AWS's doing.
func TestLaunch_SpotInterruption(t *testing.T) {
	for _, c := range []struct {
		name    string
		reports []string
		during  bool
	}{
		{"during the job", []string{"phase=listening", "phase=job"}, true},
		{"before the job", []string{"phase=listening"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var seen interruptions
			h := newHarness(t, func(o *Options) {
				o.EC2.Market = config.MarketSpot
				o.Interrupted = seen.record
			})
			errc := h.launch(context.Background())
			inst := h.instance(t)
			if code, _ := h.fetch(inst.ip); code != http.StatusOK {
				t.Fatalf("fetch = %d", code)
			}
			for _, q := range c.reports {
				h.report(t, inst.ip, q)
			}
			h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Server.SpotInstanceTermination")
			if err := waitErr(t, errc); err != nil {
				t.Fatalf("Launch = %v, want nil: an interruption is not the runner failing", err)
			}
			if got := seen.got(); len(got) != 1 || got[0] != c.during {
				t.Fatalf("interruptions reported = %v, want [%v]", got, c.during)
			}
			bs := h.p.c.Breakers()
			if len(bs) != 1 || bs[0].Market != "spot" || bs[0].Type != inst.itype || bs[0].Subnet != inst.subnet || bs[0].Reason != "Server.SpotInstanceTermination" {
				t.Fatalf("breakers = %+v, want the interrupted option", bs)
			}
		})
	}
}

// TestLaunch_OtherEndsAreNoInterruption: an instance that powers off after
// its job, or one firerunner terminates, reports no interruption.
func TestLaunch_OtherEndsAreNoInterruption(t *testing.T) {
	var seen interruptions
	h := newHarness(t, func(o *Options) {
		o.EC2.Market = config.MarketSpot
		o.Interrupted = seen.record
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.mu.Lock()
	h.f.instances[inst.id].spotStatus = "instance-terminated-by-user"
	h.f.mu.Unlock()
	h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Client.InstanceInitiatedShutdown")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 0 || len(h.p.c.Breakers()) != 0 {
		t.Fatalf("interruptions %v, breakers %+v; want none for a runner powering off", got, h.p.c.Breakers())
	}
}

// TestLaunch_PooledSpotInterruptionStops: a pool instance keeps the stop
// behaviour of its persistent request, so AWS stops it rather than
// terminating it; that is an interruption too, and firerunner then
// terminates the instance as for any stop.
func TestLaunch_PooledSpotInterruptionStops(t *testing.T) {
	var seen interruptions
	h := readyPool(t, 1)
	h.p.o.Interrupted = seen.record
	pooledID := h.f.launched()[0].id
	errc := h.launch(context.Background())
	eventually(t, "the pooled instance to start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	in := h.f.instance(pooledID)
	if code, _ := h.fetch(in.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, in.ip, "phase=job")
	h.f.setState(pooledID, types.InstanceStateNameRunning)
	// Let the waiter see it running, so a stop is no longer the one from
	// before the start.
	n := h.f.polledAlone(pooledID)
	eventually(t, "polls of the running instance", func() bool { return h.f.polledAlone(pooledID) >= n+2 })
	h.f.setStateFor(pooledID, types.InstanceStateNameStopped, "Server.SpotInstanceShutdown")
	eventually(t, "the stopped instance terminated", func() bool {
		for _, id := range h.f.terminatedIDs() {
			if id == pooledID {
				return true
			}
		}
		return false
	})
	h.f.setState(pooledID, types.InstanceStateNameShuttingDown)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 1 || !got[0] {
		t.Fatalf("interruptions reported = %v, want [true]", got)
	}
	if bs := h.p.c.Breakers(); len(bs) != 1 || bs[0].Reason != "Server.SpotInstanceShutdown" || bs[0].Subnet != in.subnet {
		t.Fatalf("breakers = %+v, want the pool instance's option", bs)
	}
}

// TestLaunch_PooledStaleInterruptionIgnored: a pool instance AWS stopped
// during its warm-up keeps that state reason while stopped and ready. The
// stale stop seen right after a job starts it is not this job's
// interruption.
func TestLaunch_PooledStaleInterruptionIgnored(t *testing.T) {
	var seen interruptions
	h := readyPool(t, 1)
	h.p.o.Interrupted = seen.record
	pooledID := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.instances[pooledID].stateReason = "Server.SpotInstanceShutdown"
	h.f.startStaysStopped = true // the first reads after the start are stale
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	eventually(t, "stale stopped reads after the start", func() bool { return h.f.polledAlone(pooledID) >= 2 })
	in := h.f.instance(pooledID)
	if code, _ := h.fetch(in.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.f.setStateFor(pooledID, types.InstanceStateNameShuttingDown, "Client.InstanceInitiatedShutdown")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 0 {
		t.Fatalf("interruptions reported = %v, want none: the stop predates this job", got)
	}
}

// TestLaunch_StaleReasonWhileRunningIgnored: a running pool instance may
// still carry the reason of an interruption during its warm-up; only a stop
// or termination's reason says why it ends.
func TestLaunch_StaleReasonWhileRunningIgnored(t *testing.T) {
	var seen interruptions
	h := readyPool(t, 1)
	h.p.o.Interrupted = seen.record
	pooledID := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.instances[pooledID].stateReason = "Server.SpotInstanceShutdown"
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	eventually(t, "the pooled instance to start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	h.f.setState(pooledID, types.InstanceStateNameRunning) // the reason stays
	n := h.f.polledAlone(pooledID)
	eventually(t, "polls of the running instance", func() bool { return h.f.polledAlone(pooledID) >= n+2 })
	h.f.setStateFor(pooledID, types.InstanceStateNameShuttingDown, "Client.InstanceInitiatedShutdown")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 0 {
		t.Fatalf("interruptions reported = %v, want none for a stale reason on a running instance", got)
	}
}

// TestLaunch_InterruptionAfterTheJobEnded: a runner that reported its exit
// had finished its job; an interruption before the instance is seen going
// is not one during a job.
func TestLaunch_InterruptionAfterTheJobEnded(t *testing.T) {
	var seen interruptions
	h := newHarness(t, func(o *Options) {
		o.EC2.Market = config.MarketSpot
		o.Interrupted = seen.record
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	for _, q := range []string{"phase=listening", "phase=job", "phase=exited&rc=0"} {
		h.report(t, inst.ip, q)
	}
	h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Server.SpotInstanceTermination")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 1 || got[0] {
		t.Fatalf("interruptions reported = %v, want [false]: the job had ended", got)
	}
}

// TestLaunch_InterruptionFromTheSpotRequest: an interruption FIS sends
// leaves Client.UserInitiatedShutdown on the instance; its spot request's
// status says what happened (seen on AWS, 2026-11-02).
func TestLaunch_InterruptionFromTheSpotRequest(t *testing.T) {
	var seen interruptions
	h := newHarness(t, func(o *Options) {
		o.EC2.Market = config.MarketSpot
		o.Interrupted = seen.record
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, inst.ip, "phase=job")
	h.f.mu.Lock()
	h.f.instances[inst.id].spotStatus = "instance-terminated-by-experiment"
	h.f.mu.Unlock()
	h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Client.UserInitiatedShutdown")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 1 || !got[0] {
		t.Fatalf("interruptions reported = %v, want [true]", got)
	}
	if bs := h.p.c.Breakers(); len(bs) != 1 || bs[0].Reason != "instance-terminated-by-experiment" {
		t.Fatalf("breakers = %+v, want one with the request's status", bs)
	}
}

func TestInterruptionStatus(t *testing.T) {
	for code, want := range map[string]bool{
		"marked-for-termination":                      true,
		"marked-for-stop":                             true,
		"instance-terminated-by-price":                true,
		"instance-terminated-no-capacity":             true,
		"instance-terminated-capacity-oversubscribed": true,
		"instance-stopped-no-capacity":                true,
		"instance-terminated-by-experiment":           true,
		"instance-terminated-by-user":                 false,
		"instance-stopped-by-user":                    false,
		"fulfilled":                                   false,
		"request-canceled-and-instance-running":       false,
		"":                                            false,
	} {
		if got := interruptionStatus(code); got != want {
			t.Errorf("interruptionStatus(%q) = %v, want %v", code, got, want)
		}
	}
}

// TestLaunch_NoRequestReadAfterAReportedExit: a runner that reported its exit
// powers its instance off itself; that end costs no DescribeSpotInstanceRequests
// call, and a stale interruption status is not read.
func TestLaunch_NoRequestReadAfterAReportedExit(t *testing.T) {
	var seen interruptions
	h := newHarness(t, func(o *Options) {
		o.EC2.Market = config.MarketSpot
		o.Interrupted = seen.record
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.fetch(inst.ip); code != http.StatusOK {
		t.Fatalf("fetch = %d", code)
	}
	h.report(t, inst.ip, "phase=exited&rc=0")
	h.f.mu.Lock()
	h.f.instances[inst.id].spotStatus = "instance-terminated-by-experiment" // would count if read
	h.f.mu.Unlock()
	h.f.setStateFor(inst.id, types.InstanceStateNameShuttingDown, "Client.InstanceInitiatedShutdown")
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if got := seen.got(); len(got) != 0 {
		t.Fatalf("interruptions reported = %v, want none: the spot request was not to be read", got)
	}
}
