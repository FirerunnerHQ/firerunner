package ec2

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/solcreek/firerunner/internal/config"
)

// poolHarness is a harness whose tier keeps a pool of size n.
func poolHarness(t *testing.T, n int, market string) *harness {
	t.Helper()
	h := newHarness(t, func(o *Options) {
		o.EC2.Pool = n
		o.EC2.Market = market
	})
	h.p.pool.tick = 5 * time.Millisecond
	return h
}

// stopWarming moves every warming fake instance to stopped, as each pool
// instance's boot script powers it off after its warm-up.
func (h *harness) stopWarming() {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for _, i := range h.f.instances {
		if i.tags[TagPool] == PoolWarming && (i.state == types.InstanceStateNamePending || i.state == types.InstanceStateNameRunning) {
			i.state = types.InstanceStateNameStopped
		}
	}
}

func (h *harness) runPool(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.p.pool.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func TestPool_WarmsToTarget(t *testing.T) {
	for _, mkt := range []string{config.MarketSpot, config.MarketOnDemand} {
		t.Run(mkt, func(t *testing.T) {
			h := poolHarness(t, 2, mkt)
			h.runPool(t)
			eventually(t, "two warming instances", func() bool { _, w := h.p.pool.Ready(); return w == 2 })
			for _, i := range h.f.launched() {
				if i.userData != "" {
					t.Errorf("pool instance %s has user-data %q; it must boot with none", i.id, i.userData)
				}
				if i.tags[TagRunner] != "" || i.tags[TagPool] != PoolWarming || i.tags[TagOwner] != "test-owner" {
					t.Errorf("pool instance tags = %v", i.tags)
				}
				switch mkt {
				case config.MarketSpot:
					if i.spotReq == "" || i.shutdown != "" {
						t.Errorf("spot pool instance: request %q shutdown %q, want persistent and no shutdown behaviour", i.spotReq, i.shutdown)
					}
				default:
					if i.shutdown != types.ShutdownBehaviorStop {
						t.Errorf("on-demand pool instance shutdown = %q, want stop", i.shutdown)
					}
				}
			}
			h.stopWarming()
			eventually(t, "two ready", func() bool { r, w := h.p.pool.Ready(); return r == 2 && w == 0 })
			for _, i := range h.f.launched() {
				if i.tags[TagPool] != PoolReady {
					t.Errorf("stopped pool instance tag = %q, want ready", i.tags[TagPool])
				}
			}
			// The target is kept: no more launches.
			time.Sleep(30 * time.Millisecond)
			if n := len(h.f.launched()); n != 2 {
				t.Fatalf("%d instances launched for a pool of 2", n)
			}
		})
	}
}

func TestLaunch_ClaimsFromPool(t *testing.T) {
	h := poolHarness(t, 1, config.MarketSpot)
	h.runPool(t)
	eventually(t, "a warming instance", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "a ready instance", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	pooledID := h.f.launched()[0].id

	errc := h.launch(context.Background())
	eventually(t, "the pooled instance to start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	in := h.f.instance(pooledID)
	// Claimed: tagged, its spot request cancelled before the start, and
	// told where to fetch its config.
	if in.tags[TagPool] != PoolClaimed || in.tags[TagRunner] != "runner-1" {
		t.Fatalf("claimed tags = %v", in.tags)
	}
	if !strings.HasPrefix(in.userData, "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit") {
		t.Fatalf("claimed user-data = %q", in.userData)
	}
	if open := h.f.openSpotRequests(); len(open) != 0 {
		t.Fatalf("spot requests still open after the claim: %v", open)
	}
	// The config was registered before the start, so the first fetch works.
	if code, body := h.fetch(in.ip); code != http.StatusOK || body != testJIT {
		t.Fatalf("fetch = %d %q", code, body)
	}
	// The instance runs its job, powers off and stops; Launch terminates it.
	h.f.setState(pooledID, types.InstanceStateNameRunning)
	time.Sleep(20 * time.Millisecond)
	h.f.setState(pooledID, types.InstanceStateNameStopped)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 1 || ids[0] != pooledID {
		t.Fatalf("terminated %v, want the pooled instance", ids)
	}
	// No cold launch happened for this job: only the pool's own launches.
	for _, i := range h.f.launched() {
		if i.tags[TagRunner] == "runner-1" && i.id != pooledID {
			t.Fatalf("a cold instance %s was launched although the pool had one", i.id)
		}
	}
	// The pool refills behind the claim.
	eventually(t, "a replacement warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
}

func TestLaunch_IgnoresStoppedBeforeTheStart(t *testing.T) {
	// The first state seen after StartInstances can still be "stopped";
	// that must not end the job before it begins.
	h := poolHarness(t, 1, config.MarketOnDemand)
	h.runPool(t)
	eventually(t, "a warming instance", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "a ready instance", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	id := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.startErr = nil
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	eventually(t, "the start", func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return len(h.f.started) == 1 })
	h.f.setState(id, types.InstanceStateNameStopped) // a stale read before the start shows
	time.Sleep(30 * time.Millisecond)
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v on a stale stopped state", ids)
	}
	h.f.setState(id, types.InstanceStateNameRunning)
	time.Sleep(20 * time.Millisecond)
	h.f.setState(id, types.InstanceStateNameStopped)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestLaunch_ColdWhenPoolEmpty(t *testing.T) {
	h := poolHarness(t, 1, config.MarketOnDemand)
	// The pool is not running, so it has nothing ready.
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if inst.tags[TagRunner] != "runner-1" || inst.tags[TagPool] != "" {
		t.Fatalf("cold instance tags = %v", inst.tags)
	}
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestLaunch_PoolStartFailureFallsBackCold(t *testing.T) {
	h := poolHarness(t, 1, config.MarketSpot)
	h.runPool(t)
	eventually(t, "a warming instance", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "a ready instance", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	pooledID := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.startErr = apiErr("InsufficientInstanceCapacity")
	h.f.mu.Unlock()

	errc := h.launch(context.Background())
	var cold *fakeInstance
	eventually(t, "a cold instance for the job", func() bool {
		for _, i := range h.f.launched() {
			if i.tags[TagRunner] == "runner-1" && i.tags[TagPool] == "" {
				cold = i
				return h.jit.Pending(netip.MustParseAddr(i.ip))
			}
		}
		return false
	})
	if ids := h.f.terminatedIDs(); len(ids) == 0 || ids[0] != pooledID {
		t.Fatalf("terminated %v, want the pooled instance that could not start", ids)
	}
	if open := h.f.openSpotRequests(); len(open) != 0 {
		t.Fatalf("spot requests left open: %v", open)
	}
	h.f.setState(cold.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestPool_RediscoversAfterRestart(t *testing.T) {
	// A first firerunner warms a pool and exits; a second, with the same
	// owner and tier, finds the ready instances instead of launching more,
	// and terminates the one a crash left claimed.
	h := poolHarness(t, 2, config.MarketSpot)
	cancel := h.runPool(t)
	eventually(t, "two warming", func() bool { _, w := h.p.pool.Ready(); return w == 2 })
	h.stopWarming()
	eventually(t, "two ready", func() bool { r, _ := h.p.pool.Ready(); return r == 2 })
	cancel()

	ids := []string{h.f.launched()[0].id, h.f.launched()[1].id}
	// The first run died mid-claim on the second instance.
	h.f.mu.Lock()
	h.f.instances[ids[1]].tags[TagPool] = PoolClaimed
	h.f.mu.Unlock()

	c := NewClient(h.f, nil, 5*time.Millisecond, quietLog())
	p2 := c.Tier(h.p.o)
	p2.pool.tick = 5 * time.Millisecond
	ctx, cancel2 := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p2.pool.Run(ctx); close(done) }()
	defer func() { cancel2(); <-done }()

	eventually(t, "the ready one rediscovered", func() bool { r, _ := p2.pool.Ready(); return r >= 1 })
	if s := h.f.state(ids[1]); s != types.InstanceStateNameShuttingDown {
		t.Fatalf("claimed leftover state %q, want terminated", s)
	}
	if open := h.f.openSpotRequests(); len(open) != 0 {
		t.Fatalf("open spot requests behind discarded instances: %v", open)
	}
	// It refills the one it discarded, rather than relaunching both.
	eventually(t, "one replacement warming", func() bool { _, w := p2.pool.Ready(); return w == 1 })
	if n := len(h.f.launched()); n != 3 {
		t.Fatalf("%d instances launched in total, want 2 + 1 replacement", n)
	}
}

func TestPool_WarmFailureBacksOff(t *testing.T) {
	h := poolHarness(t, 1, config.MarketSpot)
	h.f.runErrs["*/*/*"] = apiErr("VcpuLimitExceeded")
	h.runPool(t)
	time.Sleep(60 * time.Millisecond) // ~12 ticks
	h.f.mu.Lock()
	attempts := len(h.f.attempts)
	h.f.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("%d RunInstances attempts in 60ms after a quota error, want 1 (backoff)", attempts)
	}
}

func TestPool_ClaimDiscardsAnUnpreparableInstance(t *testing.T) {
	h := poolHarness(t, 2, config.MarketSpot)
	h.runPool(t)
	eventually(t, "two warming", func() bool { _, w := h.p.pool.Ready(); return w == 2 })
	h.stopWarming()
	eventually(t, "two ready", func() bool { r, _ := h.p.pool.Ready(); return r == 2 })
	// The next instance the pool would hand out has been started by someone
	// else: its user data can no longer be modified.
	h.p.pool.mu.Lock()
	first := h.p.pool.ready[0].id
	h.p.pool.mu.Unlock()
	h.f.setState(first, types.InstanceStateNameRunning)
	in, err := h.p.pool.claim(context.Background(), "runner-x")
	if err != nil || in.id == first {
		t.Fatalf("claim = %+v, %v; want the second instance", in, err)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 1 || ids[0] != first {
		t.Fatalf("terminated %v, want the unpreparable instance", ids)
	}
	if _, err := h.p.pool.claim(context.Background(), "runner-y"); !errors.Is(err, errPoolEmpty) {
		t.Fatalf("claim on an empty pool = %v", err)
	}
}
