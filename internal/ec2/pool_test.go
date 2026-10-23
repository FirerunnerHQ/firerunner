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

	"github.com/firerunnerhq/firerunner/internal/config"
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
	// Claimed: tagged, told where to fetch its config, started, and then its
	// spot request cancelled.
	if in.tags[TagPool] != PoolClaimed || in.tags[TagRunner] != "runner-1" {
		t.Fatalf("claimed tags = %v", in.tags)
	}
	if !strings.HasPrefix(in.userData, "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit") {
		t.Fatalf("claimed user-data = %q", in.userData)
	}
	eventually(t, "the spot request cancelled after the start", func() bool { return len(h.f.openSpotRequests()) == 0 })
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
	h.f.startStaysStopped = true // describe keeps reporting stopped after the start
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	eventually(t, "the start", func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return len(h.f.started) == 1 })
	time.Sleep(30 * time.Millisecond) // several polls of the stale "stopped"
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
	// owner and tier, finds the ready instances instead of launching more.
	// The one a crash left claimed is not the pool's any more: the reaper
	// decides its fate, after asking GitHub whether it still runs a job.
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

	eventually(t, "the ready one rediscovered", func() bool { r, _ := p2.pool.Ready(); return r == 1 })
	if s := h.f.state(ids[1]); s != types.InstanceStateNameStopped {
		t.Fatalf("the pool touched a claimed leftover: state %q", s)
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

func TestLaunch_SpotCancelRetriedBeforeTerminate(t *testing.T) {
	// The cancel after the start fails: the request must still be cancelled
	// before the instance is terminated, or AWS would replace it.
	h := poolHarness(t, 1, config.MarketSpot)
	h.runPool(t)
	eventually(t, "a warming instance", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "a ready instance", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	id := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.cancelErrs = 1
	h.f.mu.Unlock()

	errc := h.launch(context.Background())
	eventually(t, "the start", func() bool { h.f.mu.Lock(); defer h.f.mu.Unlock(); return len(h.f.started) == 1 })
	h.f.setState(id, types.InstanceStateNameRunning)
	time.Sleep(20 * time.Millisecond)
	h.f.setState(id, types.InstanceStateNameStopped) // the job ends
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if len(h.f.termWithOpenRequest) != 0 {
		t.Fatalf("terminated %v while its spot request was open", h.f.termWithOpenRequest)
	}
	if !h.f.cancelled[h.f.instances[id].spotReq] {
		t.Fatal("the spot request was never cancelled")
	}
}

// readyPool returns a harness whose pool holds n ready instances.
func readyPool(t *testing.T, n int) *harness {
	t.Helper()
	h := poolHarness(t, n, config.MarketSpot)
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == n })
	h.stopWarming()
	eventually(t, "ready", func() bool { r, _ := h.p.pool.Ready(); return r == n })
	return h
}

func TestPool_PoolWideClaimErrorKeepsThePool(t *testing.T) {
	// Missing permissions or throttling would fail for every instance: the
	// pool must not be emptied (and re-warmed) for each job. The job cold-
	// launches; the instance stays in the pool.
	h := readyPool(t, 2)
	h.f.mu.Lock()
	h.f.tagErr = apiErr("UnauthorizedOperation")
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	var cold *fakeInstance
	eventually(t, "a cold launch", func() bool {
		for _, i := range h.f.launched() {
			if i.tags[TagRunner] == "runner-1" && i.tags[TagPool] == "" {
				cold = i
				return true
			}
		}
		return false
	})
	if r, _ := h.p.pool.Ready(); r != 2 {
		t.Fatalf("ready = %d after a pool-wide error, want both kept", r)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v", ids)
	}
	h.f.mu.Lock()
	h.f.tagErr = nil
	h.f.mu.Unlock()
	eventually(t, "the cold instance registered", func() bool { return h.jit.Pending(netip.MustParseAddr(cold.ip)) })
	h.f.setState(cold.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestPool_ClaimWithCancelledContextKeepsThePool(t *testing.T) {
	h := readyPool(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.p.pool.claim(ctx, "runner-x"); err == nil {
		t.Fatal("claim with a cancelled context succeeded")
	}
	if r, _ := h.p.pool.Ready(); r != 2 {
		t.Fatalf("ready = %d after a cancelled claim, want 2", r)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v on a cancelled claim", ids)
	}
}

func TestPool_DiscardNeverTerminatesWithAnOpenRequest(t *testing.T) {
	h := readyPool(t, 1)
	in := h.f.launched()[0]
	h.f.mu.Lock()
	h.f.cancelErrs = 3 // every retry fails
	h.f.mu.Unlock()
	if h.p.pool.discard(context.Background(), in.id, in.spotReq) {
		t.Fatal("discard reported success with the spot request still open")
	}
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v with its request open", ids)
	}
	// A later attempt, once cancelling works, terminates it.
	if !h.p.pool.discard(context.Background(), in.id, in.spotReq) {
		t.Fatal("discard failed once cancelling works")
	}
}

func TestPool_StuckWarmingIsDiscardedAndRefilled(t *testing.T) {
	old := warmMax
	warmMax = 30 * time.Millisecond
	t.Cleanup(func() { warmMax = old })
	h := poolHarness(t, 1, config.MarketSpot)
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	stuck := h.f.launched()[0].id
	h.f.setState(stuck, types.InstanceStateNameRunning) // its warm-up never powers off
	eventually(t, "the stuck instance discarded", func() bool { return h.f.state(stuck) == types.InstanceStateNameShuttingDown })
	if open := h.f.openSpotRequests(); len(open) != 0 {
		t.Fatalf("spot requests left open: %v", open)
	}
	eventually(t, "a replacement warming", func() bool { return len(h.f.launched()) == 2 })
}

func TestPool_RequeueAfterTaggingClearsTheRunner(t *testing.T) {
	// The claim tagged the instance, then failed pool-wide (throttling on
	// the user-data write): it goes back ready, no longer naming the runner
	// the job now cold-launches as.
	h := readyPool(t, 1)
	id := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.modifyErr = apiErr("RequestLimitExceeded")
	h.f.mu.Unlock()
	if _, err := h.p.pool.claim(context.Background(), "runner-x"); err == nil {
		t.Fatal("claim succeeded")
	}
	in := h.f.instance(id)
	if in.tags[TagPool] != PoolReady || in.tags[TagRunner] != "" {
		t.Fatalf("tags after a failed claim = %v, want ready and no runner", in.tags)
	}
	if r, _ := h.p.pool.Ready(); r != 1 {
		t.Fatalf("ready = %d, want the instance back", r)
	}
	if h.p.c.isLive(id) {
		t.Fatal("a requeued instance is still tracked as in use")
	}
}

func TestPool_ShrinkTerminatesReadyBeforeReturning(t *testing.T) {
	h := readyPool(t, 3)
	h.p.pool.SetTarget(context.Background(), 1)
	// No eventually: the instances are gone when SetTarget returns, so a
	// schedule can shrink the pool before it lowers its hot runners.
	if r, w := h.p.pool.Ready(); r != 1 || w != 0 {
		t.Fatalf("ready %d warming %d after shrinking to 1", r, w)
	}
	if got := len(h.f.terminatedIDs()); got != 2 {
		t.Fatalf("terminated %d, want 2", got)
	}
	// And it stays at 1: nothing refills what was trimmed.
	time.Sleep(50 * time.Millisecond)
	if n := len(h.f.launched()); n != 3 {
		t.Fatalf("launched %d after the shrink, want the original 3", n)
	}
}

func TestPool_GrowFills(t *testing.T) {
	h := readyPool(t, 1)
	h.p.pool.SetTarget(context.Background(), 3)
	eventually(t, "two more warming", func() bool { r, w := h.p.pool.Ready(); return r == 1 && w == 2 })
	if h.p.pool.Target() != 3 {
		t.Fatalf("target %d", h.p.pool.Target())
	}
}

func TestPool_WarmingAboveTargetEndsOnceReady(t *testing.T) {
	h := poolHarness(t, 2, config.MarketSpot)
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 2 })
	h.p.pool.SetTarget(context.Background(), 0)
	if n := len(h.f.terminatedIDs()); n != 0 {
		t.Fatalf("terminated %d warming instances mid-boot", n)
	}
	h.stopWarming()
	eventually(t, "both trimmed once ready", func() bool {
		r, w := h.p.pool.Ready()
		return r == 0 && w == 0 && len(h.f.terminatedIDs()) == 2
	})
}

func TestPool_ShrinkKeepsAnInstanceWhoseRequestWillNotCancel(t *testing.T) {
	h := readyPool(t, 2)
	h.f.mu.Lock()
	h.f.cancelErrs = 3 // every retry of one discard
	h.f.mu.Unlock()
	h.p.pool.SetTarget(context.Background(), 1)
	if r, _ := h.p.pool.Ready(); r != 2 {
		t.Fatalf("ready %d: an instance whose request is open must stay pooled", r)
	}
	if n := len(h.f.terminatedIDs()); n != 0 {
		t.Fatalf("terminated %d with its spot request open", n)
	}
	eventually(t, "trimmed on a later pass", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
}

func TestPool_ScheduledPoolStartsEmpty(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Pooled = true; o.EC2.Market = config.MarketSpot })
	if h.p.pool == nil {
		t.Fatal("a Pooled tier with pool 0 has no pool")
	}
	h.p.pool.tick = 5 * time.Millisecond
	h.runPool(t)
	time.Sleep(30 * time.Millisecond)
	if n := len(h.f.launched()); n != 0 {
		t.Fatalf("launched %d at target 0", n)
	}
	h.p.pool.SetTarget(context.Background(), 1)
	eventually(t, "one warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
}

func TestPool_ShrinkKeepsReadyWhileOthersWarm(t *testing.T) {
	// After a busy window: 2 ready, 3 still warming (refills after claims).
	// Shrinking to 2 must not end the ready ones for the warming ones' sake,
	// or jobs cold-launch until they finish booting.
	h := readyPool(t, 2)
	h.p.pool.SetTarget(context.Background(), 5)
	eventually(t, "three warming", func() bool { _, w := h.p.pool.Ready(); return w == 3 })
	h.p.pool.SetTarget(context.Background(), 2)
	if r, _ := h.p.pool.Ready(); r != 2 {
		t.Fatalf("ready %d after shrinking to 2 while 3 warm, want the 2 kept", r)
	}
	if n := len(h.f.terminatedIDs()); n != 0 {
		t.Fatalf("terminated %d ready instances", n)
	}
	h.stopWarming()
	eventually(t, "the warmed surplus trimmed", func() bool {
		r, w := h.p.pool.Ready()
		return r == 2 && w == 0 && len(h.f.terminatedIDs()) == 3
	})
}
