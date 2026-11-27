package ec2

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
)

// pricedEC2 is the fake EC2 with spot prices: zones maps subnets to their
// zones, and hist is the price history DescribeSpotPriceHistory returns,
// newest last, filtered by type, pageSize entries a page.
type pricedEC2 struct {
	*fakeEC2
	pmu        sync.Mutex
	zones      map[string]string
	hist       []types.SpotPrice
	priceErr   error
	priceCalls int
	pageSize   int
	block      chan struct{} // when set, price calls wait for it to close
}

func (f *pricedEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range in.SubnetIds {
		z, ok := f.zones[id]
		if !ok {
			return nil, apiErr("InvalidSubnetID.NotFound")
		}
		out.Subnets = append(out.Subnets, types.Subnet{SubnetId: aws.String(id), AvailabilityZone: aws.String(z)})
	}
	return out, nil
}

func (f *pricedEC2) DescribeSpotPriceHistory(_ context.Context, in *ec2.DescribeSpotPriceHistoryInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	f.pmu.Lock()
	block := f.block
	f.pmu.Unlock()
	if block != nil {
		<-block
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.priceCalls++
	if f.priceErr != nil {
		return nil, f.priceErr
	}
	if !slices.Equal(in.ProductDescriptions, []string{priceProduct}) {
		return nil, apiErr("InvalidParameterValue")
	}
	var match []types.SpotPrice
	for _, sp := range f.hist {
		if slices.Contains(in.InstanceTypes, sp.InstanceType) {
			match = append(match, sp)
		}
	}
	items, next := page(match, in.NextToken, f.pageSize)
	return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: items, NextToken: next}, nil
}

func (f *pricedEC2) setPrice(itype, zone, price string, at time.Time) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.hist = append(f.hist, types.SpotPrice{
		InstanceType: types.InstanceType(itype), AvailabilityZone: aws.String(zone),
		SpotPrice: aws.String(price), Timestamp: aws.Time(at), ProductDescription: types.RIProductDescription(priceProduct),
	})
}

func (f *pricedEC2) calls() int {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return f.priceCalls
}

// testClock is a settable clock for the price book.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

// newPricedHarness is newHarness on a spot tier whose subnets a, b and c
// are in zones 1a, 1b and 1c.
func newPricedHarness(t *testing.T, mut func(*Options)) (*harness, *pricedEC2, *testClock) {
	t.Helper()
	f := &pricedEC2{fakeEC2: newFakeEC2(t), zones: map[string]string{
		"subnet-a": "us-east-1a", "subnet-b": "us-east-1b", "subnet-c": "us-east-1c",
	}}
	srv := jitserver.New(time.Minute, nil)
	o := Options{
		Tier: "cloud-4c", Owner: "test-owner",
		EC2: config.EC2Tier{
			InstanceTypes: []string{"c8a.xlarge", "c7a.xlarge"}, AMI: "ami-0abc",
			Subnets: []string{"subnet-a", "subnet-b", "subnet-c"}, SecurityGroups: []string{"sg-1"},
			Market: config.MarketSpot,
		},
		JIT: srv, JITURL: "http://10.0.0.5:8098/jit", Logger: quietLog(),
	}
	if mut != nil {
		mut(&o)
	}
	c := NewClient(f, nil, 5*time.Millisecond, quietLog())
	if c.prices == nil {
		t.Fatal("a client over an API with spot prices has no price book")
	}
	clk := &testClock{t: t0}
	c.prices.now = clk.now
	started := refreshed(c.prices)
	p := c.Tier(o)
	waitRefresh(t, started) // the tier's start fetches its prices
	p.tick = 5 * time.Millisecond
	p.termWait, p.termRetry = 300*time.Millisecond, 20*time.Millisecond
	return &harness{f: f.fakeEC2, jit: o.JIT, p: p}, f, clk
}

// refreshed returns a channel closed when b's next refresh ends.
func refreshed(b *priceBook) <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshc
}

func waitRefresh(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the background refresh did not finish")
	}
}

// prime fetches the tier's prices now, as the first launch would in the
// background, so the launch after it sees them.
func prime(t *testing.T, b *priceBook, o config.EC2Tier) {
	t.Helper()
	b.mu.Lock()
	for _, it := range o.InstanceTypes {
		b.want(it, o.Subnets)
	}
	b.running, b.tried = true, b.now()
	b.mu.Unlock()
	b.refresh()
}

// launchOnce launches one runner, lets its instance end, and returns it.
func launchPriced(t *testing.T, h *harness) *fakeInstance {
	t.Helper()
	before := len(h.f.launched())
	errc := h.launch(context.Background())
	var inst *fakeInstance
	eventually(t, "a new instance", func() bool {
		l := h.f.launched()
		if len(l) > before {
			inst = l[len(l)-1]
			return true
		}
		return false
	})
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	return inst
}

func TestSpotPrice_CheapestZoneFirstIsNoFallback(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	var fellBack []string
	h, f, _ := newPricedHarness(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil))
		o.FellBack = func(kind string) { fellBack = append(fellBack, kind) }
	})
	f.setPrice("c8a.xlarge", "us-east-1a", "0.0761", t0)
	f.setPrice("c8a.xlarge", "us-east-1b", "0.0681", t0)
	f.setPrice("c8a.xlarge", "us-east-1c", "0.0714", t0)
	prime(t, h.p.c.prices, h.p.o.EC2)

	inst := launchPriced(t, h)
	if inst.subnet != "subnet-b" || inst.itype != "c8a.xlarge" {
		t.Fatalf("launched %s in %s, want c8a.xlarge in subnet-b, the cheapest zone", inst.itype, inst.subnet)
	}
	if len(fellBack) != 0 {
		t.Fatalf("the cheapest zone counted as a fallback: %v", fellBack)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "spotPrice=0.0681") {
		t.Fatalf("the launch log does not name the price paid:\n%s", logs.String())
	}
}

func TestSpotPrice_CapacityWalksZonesByPriceThenTheNextType(t *testing.T) {
	h, f, _ := newPricedHarness(t, nil)
	for _, p := range [][3]string{
		{"c8a.xlarge", "us-east-1a", "0.07"}, {"c8a.xlarge", "us-east-1b", "0.05"}, {"c8a.xlarge", "us-east-1c", "0.09"},
		{"c7a.xlarge", "us-east-1a", "0.08"}, {"c7a.xlarge", "us-east-1b", "0.09"}, {"c7a.xlarge", "us-east-1c", "0.06"},
	} {
		f.setPrice(p[0], p[1], p[2], t0)
	}
	prime(t, h.p.c.prices, h.p.o.EC2)
	h.f.runErrs["c8a.xlarge/*/*"] = apiErr("InsufficientInstanceCapacity")
	inst := launchPriced(t, h)
	want := "c8a.xlarge/subnet-b/spot c8a.xlarge/subnet-a/spot c8a.xlarge/subnet-c/spot c7a.xlarge/subnet-c/spot"
	if got := h.f.attemptLog(); got != want {
		t.Fatalf("attempts:\n got %s\nwant %s", got, want)
	}
	if inst.subnet != "subnet-c" {
		t.Fatalf("launched in %s, want subnet-c, c7a's cheapest zone", inst.subnet)
	}
}

func TestSpotPrice_OnDemandKeepsTheConfiguredOrder(t *testing.T) {
	h, f, _ := newPricedHarness(t, func(o *Options) { o.EC2.Market = config.MarketSpotThenOnDemand })
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0)
	f.setPrice("c8a.xlarge", "us-east-1c", "0.05", t0)
	prime(t, h.p.c.prices, h.p.o.EC2)
	h.f.runErrs["*/*/spot"] = apiErr("InsufficientInstanceCapacity")
	h.f.runErrs["c8a.xlarge/subnet-a/on-demand"] = apiErr("InsufficientInstanceCapacity")
	inst := launchPriced(t, h)
	if !strings.HasPrefix(h.f.attemptLog(), "c8a.xlarge/subnet-c/spot ") {
		t.Fatalf("spot did not start at the cheapest zone: %s", h.f.attemptLog())
	}
	if inst.spot || inst.subnet != "subnet-b" {
		t.Fatalf("launched spot=%v in %s, want on-demand in subnet-b, the next configured subnet", inst.spot, inst.subnet)
	}
}

func TestSpotPrice_NearEqualZonesTakeTurns(t *testing.T) {
	_, f, _ := newPricedHarness(t, nil)
	b := newPriceBook(f, quietLog())
	clk := &testClock{t: t0}
	b.now = clk.now
	f.setPrice("c8a.xlarge", "us-east-1a", "0.0610", t0) // within 3% of 1b
	f.setPrice("c8a.xlarge", "us-east-1b", "0.0600", t0)
	f.setPrice("c8a.xlarge", "us-east-1c", "0.0620", t0) // 3.3% over: not near
	subnets := []string{"subnet-a", "subnet-b", "subnet-c"}
	prime(t, b, config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: subnets})
	var firsts []string
	for range 4 {
		got := b.order("c8a.xlarge", subnets)
		if got[2] != "subnet-c" {
			t.Fatalf("order %v: the zone 3.3%% over the cheapest took a turn", got)
		}
		firsts = append(firsts, got[0])
	}
	if want := []string{"subnet-b", "subnet-a", "subnet-b", "subnet-a"}; !slices.Equal(firsts, want) {
		t.Fatalf("first subnets %v, want %v", firsts, want)
	}
	// Another type keeps its own turns, and an unpriced type keeps the
	// configured order.
	if got := b.order("c7a.xlarge", subnets); !slices.Equal(got, subnets) {
		t.Fatalf("an unpriced type's order = %v, want the configured %v", got, subnets)
	}
}

func TestSpotPrice_UnpricedSubnetGoesLast(t *testing.T) {
	_, f, _ := newPricedHarness(t, nil)
	b := newPriceBook(f, quietLog())
	b.now = (&testClock{t: t0}).now
	f.setPrice("c8a.xlarge", "us-east-1b", "0.08", t0)
	f.setPrice("c8a.xlarge", "us-east-1c", "0.06", t0)
	subnets := []string{"subnet-a", "subnet-b", "subnet-c"}
	prime(t, b, config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: subnets})
	if got, want := b.order("c8a.xlarge", subnets), []string{"subnet-c", "subnet-b", "subnet-a"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v: the zone with no price last", got, want)
	}
}

func TestSpotPrice_NewestPriceWinsAcrossPages(t *testing.T) {
	_, f, _ := newPricedHarness(t, nil)
	f.pageSize = 1
	b := newPriceBook(f, quietLog())
	b.now = (&testClock{t: t0}).now
	f.setPrice("c8a.xlarge", "us-east-1a", "0.05", t0.Add(-time.Hour))
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0.Add(-time.Minute))
	f.setPrice("c8a.xlarge", "us-east-1a", "0.04", t0.Add(-2*time.Hour))
	f.setPrice("c8a.xlarge", "us-east-1b", "0.07", t0.Add(-time.Hour))
	subnets := []string{"subnet-a", "subnet-b"}
	before := f.calls()
	prime(t, b, config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: subnets})
	if got := b.Price("c8a.xlarge", "subnet-a"); got != "0.09" {
		t.Fatalf("price in 1a = %q, want the newest, 0.09", got)
	}
	if got, want := b.order("c8a.xlarge", subnets), []string{"subnet-b", "subnet-a"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if n := f.calls() - before; n != 4 {
		t.Fatalf("%d price calls, want 4 (one per page)", n)
	}
}

func TestSpotPrice_LaunchNeverWaitsForPrices(t *testing.T) {
	h, f, clk := newPricedHarness(t, nil)
	f.setPrice("c8a.xlarge", "us-east-1c", "0.05", t0)
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0)
	f.pmu.Lock()
	f.block = make(chan struct{})
	f.pmu.Unlock()
	done := refreshed(h.p.c.prices)

	// The tier's start fetched no prices (there were none yet). A launch
	// once they are due starts the fetch and goes on in the configured
	// order.
	clk.add(priceEvery)
	if inst := launchPriced(t, h); inst.subnet != "subnet-a" {
		t.Fatalf("first launch in %s, want subnet-a while prices are being fetched", inst.subnet)
	}
	f.pmu.Lock()
	close(f.block)
	f.block = nil
	f.pmu.Unlock()
	waitRefresh(t, done)
	if inst := launchPriced(t, h); inst.subnet != "subnet-c" {
		t.Fatalf("launch after the fetch in %s, want subnet-c", inst.subnet)
	}
	// Within priceEvery, no launch fetches again; after it, one does.
	calls := f.calls()
	clk.add(priceEvery - time.Second)
	launchPriced(t, h)
	if f.calls() != calls {
		t.Fatalf("prices fetched again %v after the last fetch", priceEvery-time.Second)
	}
	clk.add(time.Second)
	done = refreshed(h.p.c.prices)
	launchPriced(t, h)
	waitRefresh(t, done)
	if f.calls() == calls {
		t.Fatalf("no fetch %v after the last", priceEvery)
	}
}

func TestSpotPrice_TierStartFetchesPricesBeforeAnyLaunch(t *testing.T) {
	f := &pricedEC2{fakeEC2: newFakeEC2(t), zones: map[string]string{"subnet-a": "us-east-1a", "subnet-b": "us-east-1b"}}
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0)
	f.setPrice("c8a.xlarge", "us-east-1b", "0.05", t0)
	c := NewClient(f, nil, time.Second, quietLog())
	c.prices.now = (&testClock{t: t0}).now
	done := refreshed(c.prices)
	c.Tier(Options{Tier: "t", Owner: "o", EC2: config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: []string{"subnet-a", "subnet-b"}, Market: config.MarketSpotThenOnDemand}})
	waitRefresh(t, done)
	if got := c.prices.Price("c8a.xlarge", "subnet-b"); got != "0.05" {
		t.Fatalf("price after the tier's start = %q, want 0.05 fetched before any launch", got)
	}
	// An on-demand tier fetches nothing.
	calls := f.calls()
	c.Tier(Options{Tier: "od", Owner: "o", EC2: config.EC2Tier{InstanceTypes: []string{"m8a.xlarge"}, Subnets: []string{"subnet-a"}, Market: config.MarketOnDemand}})
	c.prices.mu.Lock()
	asked := c.prices.types["m8a.xlarge"]
	c.prices.mu.Unlock()
	if asked || f.calls() != calls {
		t.Fatalf("an on-demand tier registered its types for spot prices")
	}
}

func TestSpotPrice_IdleBookKeepsRefreshing(t *testing.T) {
	_, f, _ := newPricedHarness(t, nil)
	b := newPriceBook(f, quietLog())
	b.every = 5 * time.Millisecond // and the real clock
	b.register([]string{"c8a.xlarge"}, []string{"subnet-a"})
	eventually(t, "three refreshes with no launch", func() bool { return f.calls() >= 3 })
}

func TestSpotPrice_UnknownSubnetHidesNoOtherZone(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	_, f, clk := newPricedHarness(t, nil)
	b := newPriceBook(f, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	b.now = clk.now
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0)
	f.setPrice("c8a.xlarge", "us-east-1b", "0.05", t0)
	subnets := []string{"subnet-gone", "subnet-a", "subnet-b"}
	o := config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: subnets}
	prime(t, b, o)
	clk.add(priceEvery)
	prime(t, b, o)
	if got, want := b.order("c8a.xlarge", subnets), []string{"subnet-b", "subnet-a", "subnet-gone"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "cannot find the zone of a subnet"); n != 1 || !strings.Contains(logs.String(), "subnet-gone") {
		t.Fatalf("want one warning naming subnet-gone:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "spot prices unavailable") {
		t.Fatalf("an unknown subnet was reported as prices unavailable:\n%s", logs.String())
	}
}

func TestSpotPrice_StalePricesFallBackAndFailureLogsOnce(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	_, f, _ := newPricedHarness(t, nil)
	b := newPriceBook(f, slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, nil)))
	clk := &testClock{t: t0}
	b.now = clk.now
	f.setPrice("c8a.xlarge", "us-east-1b", "0.05", t0)
	f.setPrice("c8a.xlarge", "us-east-1a", "0.09", t0)
	subnets := []string{"subnet-a", "subnet-b"}
	o := config.EC2Tier{InstanceTypes: []string{"c8a.xlarge"}, Subnets: subnets}
	prime(t, b, o)

	f.pmu.Lock()
	f.priceErr = apiErr("UnauthorizedOperation")
	f.pmu.Unlock()
	for range 3 {
		clk.add(priceEvery)
		prime(t, b, o)
	}
	// 15 minutes old: still used.
	if got := b.order("c8a.xlarge", subnets); got[0] != "subnet-b" {
		t.Fatalf("order with 15-minute-old prices = %v, want them used", got)
	}
	clk.add(priceStale)
	prime(t, b, o)
	if got := b.order("c8a.xlarge", subnets); !slices.Equal(got, subnets) {
		t.Fatalf("order with stale prices = %v, want the configured %v", got, subnets)
	}
	if got := b.Price("c8a.xlarge", "subnet-b"); got != "" {
		t.Fatalf("stale price reported: %q", got)
	}
	f.pmu.Lock()
	f.priceErr = nil
	f.pmu.Unlock()
	clk.add(priceEvery)
	prime(t, b, o)
	if got := b.order("c8a.xlarge", subnets); got[0] != "subnet-b" {
		t.Fatalf("order after recovery = %v, want the cheapest first", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "spot prices unavailable"); n != 1 {
		t.Fatalf("failure logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "UnauthorizedOperation") || !strings.Contains(logs.String(), "spot prices available again") {
		t.Fatalf("logs lack the cause or the recovery:\n%s", logs.String())
	}
}

func TestSpotPrice_NoPriceAPIKeepsTheConfiguredOrder(t *testing.T) {
	c := NewClient(newFakeEC2(t), nil, time.Second, quietLog())
	if c.prices != nil {
		t.Fatal("a client over an API without spot prices has a price book")
	}
	subnets := []string{"subnet-a", "subnet-b"}
	if got := c.prices.order("c8a.xlarge", subnets); !slices.Equal(got, subnets) {
		t.Fatalf("order = %v, want %v", got, subnets)
	}
	if got := c.prices.Price("c8a.xlarge", "subnet-a"); got != "" {
		t.Fatalf("price = %q, want none", got)
	}
}
