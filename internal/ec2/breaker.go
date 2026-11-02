package ec2

import (
	"sort"
	"sync"
	"time"
)

// quotaCodes are RunInstances errors that mean the account's vCPU quota is
// used up in that market for the type's quota group (one covers every
// Standard type: A, C, D, H, I, M, R, T, Z). The quota is regional, so no
// other subnet helps. Another market may, as may a type in another group or a
// smaller type that fits the vCPUs left, so the walk goes on by type.
var quotaCodes = []string{
	"MaxSpotInstanceCountExceeded", // spot vCPU quota for the group
	"VcpuLimitExceeded",            // on-demand vCPU quota for the group
	"InstanceLimitExceeded",        // the older per-type instance count limit
}

func isQuotaErr(err error) bool {
	for _, c := range quotaCodes {
		if isCode(err, c) {
			return true
		}
	}
	return false
}

// Breaker backoff: a refused option goes last for breakerBase, doubling with
// each refusal after a backoff ended, up to breakerMax. An option quiet for
// breakerQuiet past its backoff starts over.
const (
	breakerBase  = 30 * time.Second
	breakerMax   = 10 * time.Minute
	breakerQuiet = 30 * time.Minute
)

// option is one launch choice. An empty subnet stands for every subnet: a
// quota is regional.
type option struct {
	market, itype, subnet string
}

type trip struct {
	failures int
	since    time.Time
	until    time.Time
	reason   string
}

// breakers remember the launch options EC2 refused lately, shared by every
// tier of a Client: capacity and quota belong to the account, not to a tier.
// A launch tries the options that are not backing off first, so a run of jobs
// does not walk the same refusals one by one, each paying for the calls.
type breakers struct {
	mu   sync.Mutex
	now  func() time.Time
	open map[option]*trip
}

func newBreakers() *breakers {
	return &breakers{now: time.Now, open: map[option]*trip{}}
}

// blocked reports whether the option, or its market and type in every
// subnet, is backing off.
func (b *breakers) blocked(o option) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for _, k := range []option{o, {o.market, o.itype, ""}} {
		if t := b.open[k]; t != nil && now.Before(t.until) {
			return true
		}
	}
	return false
}

// fail records a refusal of o and backs it off. Refusals while it backs off
// (concurrent launches that chose it before it opened) do not count again:
// one capacity event is one refusal, however many jobs met it.
func (b *breakers) fail(o option, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	t := b.open[o]
	if t != nil && now.Before(t.until) {
		t.reason = reason
		return
	}
	if t == nil || now.After(t.until.Add(breakerQuiet)) {
		t = &trip{since: now}
		b.open[o] = t
	}
	t.failures++
	t.reason = reason
	wait := breakerMax
	if t.failures <= 6 { // 30s << 5 = 16m, past the cap
		wait = min(breakerBase<<(t.failures-1), breakerMax)
	}
	t.until = now.Add(wait)
}

// succeeded closes o's breaker and its market and type's quota breaker.
func (b *breakers) succeeded(o option) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.open, o)
	delete(b.open, option{o.market, o.itype, ""})
}

// Breaker is one launch option backing off, as Client.Breakers reports it.
type Breaker struct {
	Market string
	Type   string
	// Subnet is empty for a quota: every subnet backs off.
	Subnet   string
	Reason   string
	Failures int
	Since    time.Time
	Until    time.Time
}

// Breakers returns the launch options backing off now, oldest first.
func (c *Client) Breakers() []Breaker {
	b := c.breakers
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []Breaker
	for o, t := range b.open {
		if !now.Before(t.until) {
			continue
		}
		out = append(out, Breaker{Market: o.market, Type: o.itype, Subnet: o.subnet, Reason: t.reason, Failures: t.failures, Since: t.since, Until: t.until})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].Market+out[i].Type+out[i].Subnet < out[j].Market+out[j].Type+out[j].Subnet
	})
	return out
}
