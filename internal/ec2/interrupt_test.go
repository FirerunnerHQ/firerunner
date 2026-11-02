package ec2

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
)

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
	h.f.mu.Lock()
	n := h.f.describeCalls
	h.f.mu.Unlock()
	eventually(t, "polls of the running instance", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return h.f.describeCalls >= n+3
	})
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
	n := h.f.describeCalls
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	eventually(t, "stale stopped reads after the start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1 && h.f.describeCalls >= n+3
	})
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
