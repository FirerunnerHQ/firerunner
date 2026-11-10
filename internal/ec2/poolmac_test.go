package ec2

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// testKey is a pool key, as a deployment's secret holds one.
var testKey = []byte(strings.Repeat("k", MinPoolKeyBytes))

// poolTags are the tags firerunner gives a pool instance of the harness's
// tier, before its MAC.
func poolTags(state string) map[string]string {
	return map[string]string{TagOwner: "test-owner", TagTier: "cloud-4c", TagPool: state}
}

// restart runs a second process's pool for the harness's tier over the same
// account: with key set, as the deployment's key; nil, with none configured.
func (h *harness) restart(t *testing.T, key []byte) *Provisioner {
	t.Helper()
	c := NewClient(h.f, nil, 5*time.Millisecond, quietLog())
	if key != nil {
		if err := c.SetPoolKey(key); err != nil {
			t.Fatal(err)
		}
	}
	p := c.Tier(h.p.o)
	p.tick = 5 * time.Millisecond
	p.pool.tick = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.pool.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p
}

// keyedPool is a harness whose client holds key, with one ready instance
// warmed by its pool, which then exits.
func keyedPool(t *testing.T, key []byte) (*harness, string) {
	t.Helper()
	h := poolHarness(t, 1, config.MarketSpot)
	if err := h.p.c.SetPoolKey(key); err != nil {
		t.Fatal(err)
	}
	cancel := h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	h.stopWarming()
	eventually(t, "ready", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	cancel()
	return h, h.f.launched()[0].id
}

func TestPoolMAC_NewPoolInstanceIsSigned(t *testing.T) {
	h, id := keyedPool(t, testKey)
	got := h.f.instance(id).tags[TagPoolMAC]
	if got == "" || got != h.p.c.poolMAC("test-owner", "cloud-4c", id) {
		t.Fatalf("%s = %q, want the MAC of its owner, tier and ID", TagPoolMAC, got)
	}
}

func TestPoolMAC_BindsOwnerTierAndInstance(t *testing.T) {
	c := NewClient(newFakeEC2(t), nil, 0, quietLog())
	if err := c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	inst := func(id, mac string) types.Instance {
		return types.Instance{InstanceId: aws.String(id), Tags: []types.Tag{{Key: aws.String(TagPoolMAC), Value: aws.String(mac)}}}
	}
	good := c.poolMAC("own", "tier", "i-1")
	if err := c.verifyPoolMAC(inst("i-1", good), "own", "tier"); err != nil {
		t.Fatalf("genuine: %v", err)
	}
	for _, tc := range []struct {
		name, id, owner, tier, mac string
	}{
		{"copied to another instance", "i-2", "own", "tier", good},
		{"another owner", "i-1", "other", "tier", good},
		{"another tier", "i-1", "own", "other", good},
		{"no MAC", "i-1", "own", "tier", ""},
		{"upper-case hex", "i-1", "own", "tier", strings.ToUpper(good)},
		{"truncated", "i-1", "own", "tier", good[:len(good)-1]},
		{"another key", "i-1", "own", "tier", NewClient(newFakeEC2(t), nil, 0, quietLog()).poolMAC("own", "tier", "i-1")},
	} {
		err := c.verifyPoolMAC(inst(tc.id, tc.mac), tc.owner, tc.tier)
		if want := errBadPoolMAC; tc.mac == "" && !errors.Is(err, errNoPoolMAC) || tc.mac != "" && !errors.Is(err, want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The fields are length-prefixed: moving a boundary changes the MAC.
	if c.poolMAC("ab", "c", "i-1") == c.poolMAC("a", "bc", "i-1") || c.poolMAC("own", "tier", "i-1") == c.poolMAC("own", "tie", "ri-1") {
		t.Fatal("two different triples share a MAC")
	}
}

func TestPoolMAC_ShortKeyRefused(t *testing.T) {
	c := NewClient(newFakeEC2(t), nil, 0, quietLog())
	if !c.PoolKeyEphemeral() {
		t.Fatal("a new client's key is not reported ephemeral")
	}
	if err := c.SetPoolKey(testKey[:MinPoolKeyBytes-1]); err == nil {
		t.Fatal("a short key was accepted")
	}
	if !c.PoolKeyEphemeral() {
		t.Fatal("a refused key replaced the random one")
	}
	if err := c.SetPoolKey(testKey); err != nil || c.PoolKeyEphemeral() {
		t.Fatalf("SetPoolKey = %v, ephemeral %v", err, c.PoolKeyEphemeral())
	}
}

func TestPoolMAC_ForgedInstancesAreNeverStartedOrRegistered(t *testing.T) {
	// Someone in the account launched stopped instances with the right owner,
	// tier and pool tags, and the runner security group. Rediscovery finds
	// them first; none may be told a JIT config or started. Each is
	// terminated, its persistent request cancelled first.
	h := poolHarness(t, 4, config.MarketSpot)
	if err := h.p.c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	// Listed before the genuine one, so a pool that took them would hand
	// one out first.
	forged := map[string]string{ // id -> how
		h.f.put(poolTags(PoolReady), types.InstanceStateNameStopped, old, true): "no MAC",
		h.f.put(poolTags(PoolReady), types.InstanceStateNameStopped, old, true): "wrong MAC",
		h.f.put(poolTags(PoolReady), types.InstanceStateNameStopped, old, true): "MAC copied from the genuine instance",
		// Launched just now: not left to warmMax to end.
		h.f.put(poolTags(PoolWarming), types.InstanceStateNameRunning, time.Now(), true): "warming, no MAC",
	}
	genuine := h.f.put(poolTags(PoolReady), types.InstanceStateNameStopped, old, true)
	h.f.signPool(h.p.c, genuine)
	h.f.mu.Lock()
	for id, how := range forged {
		switch how {
		case "wrong MAC":
			h.f.instances[id].tags[TagPoolMAC] = strings.Repeat("ab", 32)
		case "MAC copied from the genuine instance":
			h.f.instances[id].tags[TagPoolMAC] = h.f.instances[genuine].tags[TagPoolMAC]
		}
	}
	h.f.mu.Unlock()

	h.runPool(t)
	eventually(t, "the forged instances terminated", func() bool {
		for id := range forged {
			if h.f.state(id) != types.InstanceStateNameShuttingDown {
				return false
			}
		}
		return true
	})
	if r, _ := h.p.pool.Ready(); r != 1 {
		t.Fatalf("ready = %d, want only the genuine instance", r)
	}
	// A job takes the pool's one ready instance: the genuine one.
	errc := h.launch(context.Background())
	eventually(t, "a pool instance started", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	h.f.mu.Lock()
	started := slices.Clone(h.f.started)
	h.f.mu.Unlock()
	if started[0] != genuine {
		t.Fatalf("started %v, want the genuine instance %s", started, genuine)
	}
	for id, how := range forged {
		in := h.f.instance(id)
		if in.userData != "" || in.tags[TagPool] == PoolClaimed {
			t.Errorf("%s (%s): claimed, user-data %q", id, how, in.userData)
		}
		if code, _ := h.fetch(in.ip); code == http.StatusOK {
			t.Errorf("%s (%s): its address was served a JIT config", id, how)
		}
		if ip := netip.MustParseAddr(in.ip); h.jit.Pending(ip) || h.jit.Served(ip) {
			t.Errorf("%s (%s): a JIT config was registered for its address", id, how)
		}
	}
	h.f.setState(genuine, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestPoolMAC_RejectLeavesAnotherOwnersInstance(t *testing.T) {
	// The pool's filters find only its owner's instances; were one to slip
	// through, it is not used, and not the pool's to terminate either.
	h := poolHarness(t, 1, config.MarketSpot)
	tags := poolTags(PoolReady)
	tags[TagOwner] = "someone-else"
	id := h.f.put(tags, types.InstanceStateNameStopped, old, true)
	out, err := h.f.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	var inst types.Instance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				inst = i
			}
		}
	}
	h.p.pool.markReady(context.Background(), inst)
	if r, _ := h.p.pool.Ready(); r != 0 {
		t.Fatal("an unsigned instance joined the pool")
	}
	if s := h.f.state(id); s != types.InstanceStateNameStopped {
		t.Fatalf("another owner's instance is %s", s)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if h.f.cancelled["sir-"+id] {
		t.Fatal("another owner's spot request was cancelled")
	}
}

func TestPoolMAC_SameKeyReusesAfterRestart(t *testing.T) {
	h, id := keyedPool(t, testKey)
	p := h.restart(t, testKey)
	eventually(t, "the instance rediscovered", func() bool { r, _ := p.pool.Ready(); return r == 1 })
	if n := len(h.f.launched()); n != 1 {
		t.Fatalf("%d instances launched, want the one reused", n)
	}
	in, err := p.pool.claim(context.Background(), "runner-x")
	if err != nil || in.id != id {
		t.Fatalf("claim = %+v, %v; want %s", in, err, id)
	}
	if s := h.f.state(id); s == types.InstanceStateNameShuttingDown {
		t.Fatal("the genuine instance was terminated")
	}
	p.c.untrack(in.id)
	p.pool.discard(context.Background(), in.id, in.spotReq)
}

func TestPoolMAC_NewKeyOrNoKeyDiscardsThePool(t *testing.T) {
	for name, key := range map[string][]byte{
		"new key": []byte(strings.Repeat("n", MinPoolKeyBytes)),
		"no key":  nil, // a random key of the process's own
	} {
		t.Run(name, func(t *testing.T) {
			h, id := keyedPool(t, testKey)
			p := h.restart(t, key)
			if (key == nil) != p.c.PoolKeyEphemeral() {
				t.Fatalf("ephemeral = %v", p.c.PoolKeyEphemeral())
			}
			eventually(t, "the old instance discarded", func() bool { return h.f.state(id) == types.InstanceStateNameShuttingDown })
			eventually(t, "a replacement warming", func() bool { _, w := p.pool.Ready(); return w == 1 })
			if r, _ := p.pool.Ready(); r != 0 {
				t.Fatalf("ready = %d: the old instance joined the pool", r)
			}
			fresh := h.f.launched()[1]
			if fresh.tags[TagPoolMAC] != p.c.poolMAC("test-owner", "cloud-4c", fresh.id) {
				t.Fatal("the replacement is not signed with the new key")
			}
		})
	}
}

func TestPoolMAC_UnsignedWarmingInstanceIsNotTakenWhenItStops(t *testing.T) {
	// refresh, too, verifies before an instance joins the pool.
	h := poolHarness(t, 1, config.MarketSpot)
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	id := h.f.launched()[0].id
	h.f.mu.Lock()
	delete(h.f.instances[id].tags, TagPoolMAC)
	h.f.mu.Unlock()
	h.stopWarming()
	eventually(t, "the unsigned instance discarded", func() bool { return h.f.state(id) == types.InstanceStateNameShuttingDown })
	eventually(t, "a replacement warming", func() bool { return len(h.f.launched()) == 2 })
	if r, _ := h.p.pool.Ready(); r != 0 {
		t.Fatalf("ready = %d: the unsigned instance joined the pool", r)
	}
}

func TestPoolMAC_UnsignableLaunchIsDiscarded(t *testing.T) {
	// The MAC could not be tagged: no later process would trust the
	// instance, so it goes at once rather than warm for nothing.
	h := poolHarness(t, 1, config.MarketSpot)
	h.f.mu.Lock()
	h.f.tagErr = apiErr("RequestLimitExceeded") // not retried
	h.f.mu.Unlock()
	h.runPool(t)
	eventually(t, "the launch discarded", func() bool {
		l := h.f.launched()
		return len(l) >= 1 && h.f.state(l[0].id) == types.InstanceStateNameShuttingDown
	})
	id := h.f.launched()[0].id
	h.p.pool.mu.Lock()
	_, followed := h.p.pool.warming[id]
	h.p.pool.mu.Unlock()
	if followed {
		t.Fatal("the unsigned instance is followed as warming")
	}
}

func TestPoolMAC_SignRetriesAnInstanceEC2DoesNotKnowYet(t *testing.T) {
	// Right after RunInstances, CreateTags on the new ID may answer
	// InvalidInstanceID.NotFound for a few seconds: that is no reason to
	// discard it.
	old := signBackoff
	signBackoff = time.Millisecond
	t.Cleanup(func() { signBackoff = old })
	h := poolHarness(t, 1, config.MarketSpot)
	h.f.mu.Lock()
	h.f.tagUnknown = signAttempts - 1
	h.f.mu.Unlock()
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	in := h.f.launched()[0]
	if in.tags[TagPoolMAC] != h.p.c.poolMAC("test-owner", "cloud-4c", in.id) || in.state == types.InstanceStateNameShuttingDown {
		t.Fatalf("tags %v, state %s; want it signed and kept", in.tags, in.state)
	}
}
