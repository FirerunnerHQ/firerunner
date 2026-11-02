package ec2

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
)

func TestBreakers_BackOffDoublesToTheCapAndSuccessCloses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := newBreakers()
	b.now = func() time.Time { return now }
	o := option{"spot", "c8a.xlarge", "subnet-a"}
	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		b.fail(o, "InsufficientInstanceCapacity")
		if got := b.open[o].until.Sub(now); got != want {
			t.Fatalf("refusal %d: backs off %v, want %v", i+1, got, want)
		}
	}
	if !b.blocked(o) {
		t.Fatal("not blocked while backing off")
	}
	now = now.Add(10 * time.Minute)
	if b.blocked(o) {
		t.Fatal("still blocked once the backoff passed")
	}
	b.succeeded(o)
	b.fail(o, "InsufficientInstanceCapacity")
	if got := b.open[o].until.Sub(now); got != breakerBase {
		t.Fatalf("after a success the next refusal backs off %v, want %v", got, breakerBase)
	}
}

func TestBreakers_QuotaBlocksEverySubnet(t *testing.T) {
	b := newBreakers()
	b.fail(option{"spot", "c8a.xlarge", ""}, "MaxSpotInstanceCountExceeded")
	for _, o := range []option{{"spot", "c8a.xlarge", "subnet-a"}, {"spot", "c8a.xlarge", "subnet-b"}} {
		if !b.blocked(o) {
			t.Fatalf("%v not blocked by its type's quota", o)
		}
	}
	for _, o := range []option{{"on-demand", "c8a.xlarge", "subnet-a"}, {"spot", "c7a.xlarge", "subnet-a"}} {
		if b.blocked(o) {
			t.Fatalf("%v blocked by another market's or type's quota", o)
		}
	}
	b.succeeded(option{"spot", "c8a.xlarge", "subnet-b"})
	if b.blocked(option{"spot", "c8a.xlarge", "subnet-a"}) {
		t.Fatal("a launch of the type did not lift its quota breaker")
	}
}

// launchOnce runs one job launch to its end on h's provisioner, as name.
func launchOnce(t *testing.T, h *harness, name string) {
	t.Helper()
	before := len(h.f.launched())
	errc := make(chan error, 1)
	go func() { errc <- h.p.Launch(context.Background(), name, testJIT, core.RunnerSpec{}, nil) }()
	var id string
	eventually(t, "an instance for "+name, func() bool {
		l := h.f.launched()
		if len(l) <= before {
			return false
		}
		id = l[before].id
		return true
	})
	h.f.setState(id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("launch %s: %v", name, err)
	}
}

func TestLaunch_RefusedOptionIsTriedLast(t *testing.T) {
	h := newHarness(t, nil)
	h.f.runErrs["c8a.xlarge/subnet-a/*"] = apiErr("InsufficientInstanceCapacity")
	launchOnce(t, h, "runner-1")
	h.f.mu.Lock()
	h.f.attempts = nil
	h.f.mu.Unlock()

	// Still refused: the next launch starts with what worked, not the refusal.
	launchOnce(t, h, "runner-2")
	if got, want := h.f.attemptLog(), "c8a.xlarge/subnet-b/on-demand"; got != want {
		t.Fatalf("second launch attempts = %s, want %s", got, want)
	}
	if bs := h.p.c.Breakers(); len(bs) != 1 || bs[0].Type != "c8a.xlarge" || bs[0].Subnet != "subnet-a" || bs[0].Reason != "InsufficientInstanceCapacity" {
		t.Fatalf("breakers = %+v, want c8a.xlarge in subnet-a", bs)
	}
}

func TestLaunch_EveryOptionBackingOffIsStillTried(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.EC2.Market = config.MarketSpot })
	h.f.runErrs["*/*/*"] = apiErr("InsufficientInstanceCapacity")
	if err := waitErr(t, h.launch(context.Background())); err == nil {
		t.Fatal("launch succeeded with no capacity anywhere")
	}
	delete(h.f.runErrs, "*/*/*") // capacity is back, every option still backing off
	h.f.mu.Lock()
	h.f.attempts = nil
	h.f.mu.Unlock()
	launchOnce(t, h, "runner-2")
	if got := h.f.attemptLog(); got != "c8a.xlarge/subnet-a/spot" {
		t.Fatalf("attempts = %s, want the first option despite its breaker", got)
	}
	bs := h.p.c.Breakers()
	if len(bs) != 3 {
		t.Fatalf("breakers = %+v, want the three options not launched since", bs)
	}
	for _, b := range bs {
		if b.Type == "c8a.xlarge" && b.Subnet == "subnet-a" {
			t.Fatalf("breakers = %+v: the option that launched is still backing off", bs)
		}
	}
}
