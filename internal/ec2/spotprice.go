package ec2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PriceAPI is what the spot price book calls. The AWS client has both; an
// API without them launches in the configured subnet order.
type PriceAPI interface {
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSpotPriceHistory(context.Context, *ec2.DescribeSpotPriceHistoryInput, ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
}

// Spot price book timings. Prices are fetched in the background every
// priceEvery, from the first tier's start and whether or not anything
// launches, so a launch never waits on them and a burst after a quiet night
// finds them fresh; prices older than priceStale (every refresh since
// failed) are not used at all.
const (
	priceEvery    = 5 * time.Minute
	priceStale    = 30 * time.Minute
	priceTimeout  = 20 * time.Second
	priceProduct  = "Linux/UNIX"
	priceSpread   = 0.03 // zones within 3% of the cheapest take turns
	priceMaxTypes = 100  // types per DescribeSpotPriceHistory call
)

// priceBook orders a spot launch's subnets, within each instance type, by
// the current spot price in their zones, cheapest first. Spot prices differ
// between zones by 10-30% and change by the hour, so a fixed order pays for
// the first zone whatever it costs, and puts every instance in it. Subnets
// whose zones are within priceSpread of the cheapest take turns, so that
// near-equal zones share the launches and one zone's interruptions are not
// every runner's. A subnet with no known price for the type (its zone does
// not offer it, or it is not fetched yet) goes after the priced ones, in
// the configured order; with no prices at all, the configured order stands.
type priceBook struct {
	api PriceAPI
	log *slog.Logger
	now func() time.Time
	// every is how often the book refreshes: priceEvery, shorter in tests.
	every   time.Duration
	ticking sync.Once

	mu       sync.Mutex
	zone     map[string]string // subnet -> availability zone
	types    map[string]bool   // instance types launches asked about
	subnets  map[string]bool   // subnets launches asked about
	price    map[[2]string]float64
	fetched  time.Time // when price was last filled
	tried    time.Time // when a refresh last started
	running  bool
	failing  bool              // the last price fetch failed
	badZones string            // the subnets whose zones the last lookup could not find
	turn     map[string]uint64 // per instance type, for taking turns
	refreshc chan struct{}     // tests: closed after each refresh, then renewed
}

func newPriceBook(api PriceAPI, log *slog.Logger) *priceBook {
	return &priceBook{
		api: api, log: log, now: time.Now, every: priceEvery,
		zone: map[string]string{}, types: map[string]bool{}, subnets: map[string]bool{},
		price: map[[2]string]float64{}, turn: map[string]uint64{}, refreshc: make(chan struct{}),
	}
}

// order returns subnets for one spot launch of itype, cheapest zone first,
// and starts a background refresh when the prices are due.
func (b *priceBook) order(itype string, subnets []string) []string {
	out := slices.Clone(subnets)
	if b == nil {
		return out
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.want(itype, subnets)
	b.kick(false)
	if b.now().Sub(b.fetched) > priceStale {
		return out
	}
	priced := func(s string) (float64, bool) {
		z, ok := b.zone[s]
		if !ok {
			return 0, false
		}
		p, ok := b.price[[2]string{itype, z}]
		return p, ok
	}
	slices.SortStableFunc(out, func(x, y string) int {
		px, okx := priced(x)
		py, oky := priced(y)
		switch {
		case okx && oky:
			if px < py {
				return -1
			}
			if px > py {
				return 1
			}
			return 0
		case okx:
			return -1
		case oky:
			return 1
		}
		return 0
	})
	cheapest, ok := priced(out[0])
	if !ok {
		return out
	}
	near := 1
	for near < len(out) {
		p, ok := priced(out[near])
		if !ok || p > cheapest*(1+priceSpread) {
			break
		}
		near++
	}
	if near > 1 {
		k := int(b.turn[itype] % uint64(near))
		b.turn[itype]++
		slices.Reverse(out[:k])
		slices.Reverse(out[k:near])
		slices.Reverse(out[:near])
	}
	return out
}

// register adds a spot tier's types and subnets to what the book fetches,
// fetches them at once, so that the tier's first launch finds them, and
// starts the book's refreshes on the first call. The refreshes last as long
// as the process, as the client does.
func (b *priceBook) register(itypes, subnets []string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	for _, it := range itypes {
		b.want(it, subnets)
	}
	b.kick(false)
	b.mu.Unlock()
	b.ticking.Do(func() {
		go func() {
			for range time.Tick(b.every) {
				b.mu.Lock()
				b.kick(true)
				b.mu.Unlock()
			}
		}()
	})
}

// kick starts a background refresh when none is running and one is due, or
// at once on the ticker's beat: a launch's refresh between two beats must
// not push the next beat's to the one after. Called with b.mu held.
func (b *priceBook) kick(beat bool) {
	if !b.running && (beat || b.now().Sub(b.tried) >= b.every) {
		b.running, b.tried = true, b.now()
		go b.refresh()
	}
}

// want records what the next refresh fetches. Called with b.mu held.
func (b *priceBook) want(itype string, subnets []string) {
	if !b.types[itype] {
		b.types[itype] = true
		b.tried = time.Time{} // a new type is fetched at once
	}
	for _, s := range subnets {
		if !b.subnets[s] {
			b.subnets[s] = true
			b.tried = time.Time{}
		}
	}
}

// Price returns the spot price of itype in subnet's zone as last fetched,
// or "" when unknown.
func (b *priceBook) Price(itype, subnet string) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	z, ok := b.zone[subnet]
	if !ok || b.now().Sub(b.fetched) > priceStale {
		return ""
	}
	p, ok := b.price[[2]string{itype, z}]
	if !ok {
		return ""
	}
	return strconv.FormatFloat(p, 'f', -1, 64)
}

func (b *priceBook) refresh() {
	b.mu.Lock()
	itypes := sortedKeys(b.types)
	var unzoned []string
	for s := range b.subnets {
		if _, ok := b.zone[s]; !ok {
			unzoned = append(unzoned, s)
		}
	}
	slices.Sort(unzoned)
	b.mu.Unlock()

	// Each lookup has its own deadline, so slow subnet lookups cannot use up
	// the price fetch's.
	zctx, zcancel := context.WithTimeout(context.Background(), priceTimeout)
	zones, zerr := b.zones(zctx, unzoned)
	zcancel()
	pctx, pcancel := context.WithTimeout(context.Background(), priceTimeout)
	prices, perr := b.prices(pctx, itypes)
	pcancel()

	b.mu.Lock()
	for s, z := range zones {
		b.zone[s] = z
	}
	if perr == nil {
		b.price, b.fetched = prices, b.now()
	}
	switch {
	case perr != nil && !b.failing:
		b.failing = true
		b.log.Warn("spot prices unavailable; launching in the configured subnet order until they are", "err", perr)
	case perr == nil && b.failing:
		b.failing = false
		b.log.Info("spot prices available again; launching in the cheapest zone first")
	}
	// Warned once per set of subnets: an error's text carries a request ID
	// that differs on every call.
	var missing []string
	for _, s := range unzoned {
		if _, ok := zones[s]; !ok {
			missing = append(missing, s)
		}
	}
	bad := strings.Join(missing, ",")
	if zerr == nil {
		bad = ""
	}
	if bad != b.badZones {
		b.badZones = bad
		if bad != "" {
			b.log.Warn("cannot find the zone of a subnet; spot launches try it after the priced ones", "err", zerr)
		}
	}
	b.running = false
	close(b.refreshc)
	b.refreshc = make(chan struct{})
	b.mu.Unlock()
}

func (b *priceBook) zones(ctx context.Context, subnets []string) (map[string]string, error) {
	out := map[string]string{}
	if len(subnets) == 0 {
		return out, nil
	}
	res, err := b.api.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: subnets})
	if err == nil {
		for _, s := range res.Subnets {
			out[aws.ToString(s.SubnetId)] = aws.ToString(s.AvailabilityZone)
		}
		return out, nil
	}
	if len(subnets) == 1 {
		return out, fmt.Errorf("subnet %s: %w", subnets[0], err)
	}
	// One unknown subnet fails the whole call: look the rest up one by one,
	// so that it does not hide every other subnet's zone.
	var errs []error
	for _, s := range subnets {
		got, err := b.zones(ctx, []string{s})
		maps.Copy(out, got)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// prices fetches the current spot price of each type in each zone: with
// StartTime and EndTime both now, EC2 returns the price in force then.
func (b *priceBook) prices(ctx context.Context, itypes []string) (map[[2]string]float64, error) {
	out := map[[2]string]float64{}
	at := map[[2]string]time.Time{}
	now := b.now()
	for chunk := range slices.Chunk(itypes, priceMaxTypes) {
		in := &ec2.DescribeSpotPriceHistoryInput{
			InstanceTypes:       make([]types.InstanceType, len(chunk)),
			ProductDescriptions: []string{priceProduct},
			StartTime:           aws.Time(now),
			EndTime:             aws.Time(now),
		}
		for i, t := range chunk {
			in.InstanceTypes[i] = types.InstanceType(t)
		}
		for {
			res, err := b.api.DescribeSpotPriceHistory(ctx, in)
			if err != nil {
				return nil, err
			}
			for _, sp := range res.SpotPriceHistory {
				p, err := strconv.ParseFloat(aws.ToString(sp.SpotPrice), 64)
				if err != nil || p <= 0 {
					continue
				}
				k := [2]string{string(sp.InstanceType), aws.ToString(sp.AvailabilityZone)}
				// The newest entry per pool wins; the call may return an
				// older one alongside.
				ts := aws.ToTime(sp.Timestamp)
				if prev, seen := at[k]; !seen || ts.After(prev) {
					out[k], at[k] = p, ts
				}
			}
			if aws.ToString(res.NextToken) == "" {
				break
			}
			in.NextToken = res.NextToken
		}
	}
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
