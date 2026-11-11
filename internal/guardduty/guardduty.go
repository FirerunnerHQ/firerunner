// Package guardduty reads AWS GuardDuty's findings on the EC2 instances an
// install launched, and flags for review the tenant whose machine each one
// names, by the instance's firerunner:owner tag. A flag never suspends: an
// operator reviews it, then suspends the tenant or clears the flag.
//
// The owner tag is not signed (the runner MAC on a job instance is not among
// the tags a finding carries), so whoever may tag instances in the account
// could make a finding name another owner. That costs an operator a review,
// never a tenant its service.
package guardduty

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/guardduty"
	"github.com/aws/aws-sdk-go-v2/service/guardduty/types"

	"github.com/firerunnerhq/firerunner/internal/tenantstate"
)

// API is the subset of the GuardDuty client this package calls, so tests can
// substitute a fake.
type API interface {
	ListDetectors(context.Context, *guardduty.ListDetectorsInput, ...func(*guardduty.Options)) (*guardduty.ListDetectorsOutput, error)
	ListFindings(context.Context, *guardduty.ListFindingsInput, ...func(*guardduty.Options)) (*guardduty.ListFindingsOutput, error)
	GetFindings(context.Context, *guardduty.GetFindingsInput, ...func(*guardduty.Options)) (*guardduty.GetFindingsOutput, error)
}

// Tag keys a finding's instance carries; ec2.TagOwner and its kin.
const (
	tagOwner  = "firerunner:owner"
	tagTier   = "firerunner:tier"
	tagRunner = "firerunner:runner"
)

const (
	// DefaultMinSeverity is GuardDuty's Medium: Low findings (below 4) are
	// mostly probes of the instance from outside, which say nothing of
	// what its job did.
	DefaultMinSeverity = 4.0
	// DefaultInterval is how often the daemon reads findings. GuardDuty
	// reports a finding minutes after the activity, so a shorter interval
	// gains little.
	DefaultInterval = 2 * time.Minute
	// FirstLookback is how far back the first read of an empty store goes.
	FirstLookback = 24 * time.Hour
	// Settle is how long after its update time a finding is still read
	// again, in case GuardDuty lists it late or the clocks differ.
	Settle = 15 * time.Minute
	// maxSeen bounds the cursor's findings, so that it stays within one
	// SSM parameter (4 KB). Past it, the cursor moves past the oldest
	// (see advance).
	maxSeen = 40
	// pageSize is the most ids ListFindings returns and GetFindings takes.
	pageSize = 50
)

// Finding is one GuardDuty finding on an instance with an owner tag.
type Finding struct {
	ID        string
	Detector  string
	Type      string
	Severity  float64
	UpdatedAt time.Time
	// Count is how many times GuardDuty saw the activity.
	Count    int
	Instance string
	Owner    string
	Tier     string
	Runner   string
}

// Query chooses the findings List returns.
type Query struct {
	// Since is the oldest update time.
	Since time.Time
	// MinSeverity is the least severity.
	MinSeverity float64
	// Account, when set, is the only account whose findings count: a
	// GuardDuty administrator's detector lists its members' findings too,
	// whose instances may carry any tag.
	Account string
}

// List returns the findings on EC2 instances that carry an owner tag,
// updated at or after q.Since, not archived (a suppression rule archives what
// an operator chose to ignore), of severity at least q.MinSeverity, in
// q.Account, oldest update first, and how many detectors it read.
func List(ctx context.Context, api API, q Query) ([]Finding, int, error) {
	var detectors []string
	p := guardduty.NewListDetectorsPaginator(api, &guardduty.ListDetectorsInput{})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("list detectors: %w", err)
		}
		detectors = append(detectors, page.DetectorIds...)
	}
	var out []Finding
	for _, d := range detectors {
		fs, err := listDetector(ctx, api, d, q)
		if err != nil {
			return nil, len(detectors), err
		}
		out = append(out, fs...)
	}
	slices.SortStableFunc(out, func(a, b Finding) int {
		if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, len(detectors), nil
}

func listDetector(ctx context.Context, api API, detector string, q Query) ([]Finding, error) {
	criteria := &types.FindingCriteria{Criterion: map[string]types.Condition{
		"resource.resourceType":             {Equals: []string{"Instance"}},
		"resource.instanceDetails.tags.key": {Equals: []string{tagOwner}},
		"service.archived":                  {Equals: []string{"false"}},
		"updatedAt":                         {GreaterThanOrEqual: aws.Int64(q.Since.UnixMilli())},
		"severity":                          {GreaterThanOrEqual: aws.Int64(int64(math.Floor(q.MinSeverity)))},
	}}
	if q.Account != "" {
		criteria.Criterion["accountId"] = types.Condition{Equals: []string{q.Account}}
	}
	var ids []string
	p := guardduty.NewListFindingsPaginator(api, &guardduty.ListFindingsInput{
		DetectorId: aws.String(detector), FindingCriteria: criteria, MaxResults: aws.Int32(pageSize),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list findings: %w", err)
		}
		ids = append(ids, page.FindingIds...)
	}
	var out []Finding
	for batch := range slices.Chunk(ids, pageSize) {
		got, err := api.GetFindings(ctx, &guardduty.GetFindingsInput{DetectorId: aws.String(detector), FindingIds: batch})
		if err != nil {
			return nil, fmt.Errorf("get findings: %w", err)
		}
		for _, f := range got.Findings {
			// The criteria choose these already; read them again, so a
			// criterion GuardDuty ignored never flags.
			fd, ok := finding(f, detector)
			if ok && fd.Severity >= q.MinSeverity && !fd.UpdatedAt.Before(q.Since) &&
				(q.Account == "" || aws.ToString(f.AccountId) == q.Account) {
				out = append(out, fd)
			}
		}
	}
	return out, nil
}

// finding reads what a flag needs of f, and whether it is an unarchived
// finding on an instance with an owner tag.
func finding(f types.Finding, detector string) (Finding, bool) {
	r := f.Resource
	if r == nil || aws.ToString(r.ResourceType) != "Instance" || r.InstanceDetails == nil {
		return Finding{}, false
	}
	if f.Service != nil && aws.ToBool(f.Service.Archived) {
		return Finding{}, false
	}
	tags := map[string]string{}
	for _, t := range r.InstanceDetails.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	updated, err := time.Parse(time.RFC3339Nano, aws.ToString(f.UpdatedAt))
	if err != nil || tags[tagOwner] == "" {
		return Finding{}, false
	}
	fd := Finding{
		ID: aws.ToString(f.Id), Detector: detector, Type: aws.ToString(f.Type), Severity: aws.ToFloat64(f.Severity),
		UpdatedAt: updated.UTC(), Instance: aws.ToString(r.InstanceDetails.InstanceId),
		Owner: tags[tagOwner], Tier: tags[tagTier], Runner: tags[tagRunner],
	}
	if f.Service != nil {
		fd.Count = int(aws.ToInt32(f.Service.Count))
	}
	return fd, true
}

// Owners maps an owner tag to the tenant it names: install's own tag to
// tenantstate.Install, and install/<id> to id, for an id in ids (any valid
// id when ids is nil). It names no other.
func Owners(install string, ids []string) func(owner string) (string, bool) {
	return func(owner string) (string, bool) {
		if owner == install {
			return tenantstate.Install, true
		}
		id, ok := strings.CutPrefix(owner, install+"/")
		if !ok || id == tenantstate.Install || !tenantstate.ValidID(id) {
			return "", false
		}
		if ids != nil && !slices.Contains(ids, id) {
			return "", false
		}
		return id, true
	}
}

// Flagged is a finding that flagged a tenant.
type Flagged struct {
	Tenant string
	Finding
}

// Status is how reading findings goes.
type Status struct {
	Store       string
	MinSeverity float64
	// Detectors is how many the last read found; 0 means GuardDuty is
	// off in the region, and nothing is flagged.
	Detectors   int
	LastPoll    time.Time
	LastError   string
	LastErrorAt time.Time
}

// Poller reads findings now and then, flags the tenants they name in Store,
// and keeps the flags for status. It never suspends a tenant.
type Poller struct {
	API   API
	Store tenantstate.FlagStore
	// Tenant names the tenant an owner tag belongs to; findings on other
	// owners' instances are not this install's.
	Tenant      func(owner string) (string, bool)
	MinSeverity float64
	// Account returns this account's id, so that a GuardDuty
	// administrator's members' findings never flag; nil reads every
	// account's. It is asked until it answers.
	Account  func(ctx context.Context) (string, error)
	Interval time.Duration
	Log      *slog.Logger
	// OnFlag hears each finding, or update of one, that flags a tenant,
	// once its flag is written.
	OnFlag func(Flagged)
	// tick and now replace the ticker and the clock in tests.
	tick <-chan time.Time
	now  func() time.Time

	// polling serialises polls; account and cur are theirs.
	polling sync.Mutex
	account string
	// cur is the cursor as this process last advanced it: a cursor that
	// cannot be written never makes a finding count again.
	cur *tenantstate.Cursor

	mu     sync.Mutex
	flags  tenantstate.Flags
	status Status
}

// Poll reads the cursor, the findings since it, and flags each tenant a new
// finding, or a new update of one, names. It reads the flags again just
// before writing them, so that an operator's clear meanwhile is lost only in
// the moment between, and writes the flags before the cursor: a crash
// between counts a finding twice, rather than missing it.
func (p *Poller) Poll(ctx context.Context) error {
	p.polling.Lock()
	defer p.polling.Unlock()
	start := p.clock().UTC()
	_, stored, err := p.Store.LoadFlags(ctx)
	if err != nil {
		return p.fail(start, fmt.Errorf("read flags from %s: %w", p.Store, err))
	}
	cur := merge(stored, p.cur)
	if cur.Since.IsZero() {
		cur.Since = start.Add(-FirstLookback)
	}
	if p.account == "" && p.Account != nil {
		if p.account, err = p.Account(ctx); err != nil {
			return p.fail(start, fmt.Errorf("this account's id: %w", err))
		}
	}
	found, detectors, err := List(ctx, p.API, Query{Since: cur.Since, MinSeverity: p.MinSeverity, Account: p.account})
	if err != nil {
		return p.fail(start, err)
	}
	byTenant := map[string][]Finding{}
	for _, f := range found {
		id, ok := p.Tenant(f.Owner)
		if !ok {
			continue
		}
		if seen, ok := cur.Seen[f.ID]; ok && !f.UpdatedAt.After(seen) {
			continue
		}
		byTenant[id] = append(byTenant[id], f)
	}
	flags, _, err := p.Store.LoadFlags(ctx)
	if err != nil {
		return p.fail(start, fmt.Errorf("read flags from %s: %w", p.Store, err))
	}
	// An operator's clear shows even when a write below fails.
	p.mu.Lock()
	p.flags = maps.Clone(flags)
	p.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(byTenant)) {
		fl := flags[id]
		for _, f := range byTenant[id] {
			if fl.Count == 0 {
				fl.First = f.UpdatedAt
			}
			fl.Count++
			fl.Type, fl.Tier, fl.Instance, fl.Finding, fl.Last = f.Type, f.Tier, f.Instance, f.ID, f.UpdatedAt
			fl.Severity = max(fl.Severity, f.Severity)
		}
		if err := p.Store.PutFlag(ctx, id, fl); err != nil {
			p.cur = &cur
			return p.fail(start, fmt.Errorf("flag %s in %s: %w", id, p.Store, err))
		}
		flags[id] = fl
		p.mu.Lock()
		p.flags = maps.Clone(flags)
		p.mu.Unlock()
		for _, f := range byTenant[id] {
			cur.Seen[f.ID] = f.UpdatedAt
			p.heard(id, f)
		}
	}
	cur = advance(cur, start)
	p.cur = &cur
	if err := p.Store.PutCursor(ctx, cur); err != nil {
		return p.fail(start, fmt.Errorf("write the cursor to %s: %w", p.Store, err))
	}
	p.mu.Lock()
	p.status.Detectors, p.status.LastPoll = detectors, start
	p.mu.Unlock()
	return nil
}

// heard logs and reports a finding that flagged tenant id.
func (p *Poller) heard(id string, f Finding) {
	if p.Log != nil {
		p.Log.Warn("GuardDuty finding on a tenant's machine: the tenant is flagged for review, not suspended",
			"tenant", id, "type", f.Type, "severity", f.Severity, "tier", f.Tier, "instance", f.Instance,
			"runner", f.Runner, "finding", f.ID, "updated", f.UpdatedAt, "count", f.Count)
	}
	if p.OnFlag != nil {
		p.OnFlag(Flagged{Tenant: id, Finding: f})
	}
}

// merge joins the stored cursor with this process's own, which a failed
// write may have kept from the store: the later Since, and each finding's
// latest update time.
func merge(stored tenantstate.Cursor, own *tenantstate.Cursor) tenantstate.Cursor {
	out := tenantstate.Cursor{Since: stored.Since, Seen: maps.Clone(stored.Seen)}
	if out.Seen == nil {
		out.Seen = map[string]time.Time{}
	}
	if own == nil {
		return out
	}
	if own.Since.After(out.Since) {
		out.Since = own.Since
	}
	for id, at := range own.Seen {
		if at.After(out.Seen[id]) {
			out.Seen[id] = at
		}
	}
	return out
}

// advance moves the cursor to Settle before now, never back, and keeps the
// findings seen since then. Past maxSeen of them, it moves Since past the
// update time of those it lets go, latest first kept, so that a finding it
// no longer holds is never listed again: in such a burst, a finding listed
// later than Settle allows is missed, rather than one counted on every poll.
func advance(cur tenantstate.Cursor, now time.Time) tenantstate.Cursor {
	if s := now.Add(-Settle); s.After(cur.Since) {
		cur.Since = s
	}
	seen := map[string]time.Time{}
	for id, at := range cur.Seen {
		if !at.Before(cur.Since) {
			seen[id] = at
		}
	}
	if len(seen) > maxSeen {
		ids := slices.SortedFunc(maps.Keys(seen), func(a, b string) int {
			if c := seen[b].Compare(seen[a]); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		cur.Since = seen[ids[maxSeen]].Add(time.Millisecond)
		for id, at := range seen {
			if at.Before(cur.Since) {
				delete(seen, id)
			}
		}
	}
	cur.Seen = seen
	return cur
}

func (p *Poller) fail(at time.Time, err error) error {
	p.mu.Lock()
	p.status.LastError, p.status.LastErrorAt = err.Error(), at
	p.mu.Unlock()
	if p.Log != nil {
		p.Log.Warn("read GuardDuty findings", "err", err)
	}
	return err
}

// Run polls at once and every Interval until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	tick := p.tick
	if tick == nil {
		every := p.Interval
		if every <= 0 {
			every = DefaultInterval
		}
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		_ = p.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

// Flags returns the flags as last read or written.
func (p *Poller) Flags() tenantstate.Flags {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.flags)
}

// Status returns how reading findings goes.
func (p *Poller) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.status
	s.Store, s.MinSeverity = p.Store.String(), p.MinSeverity
	return s
}

func (p *Poller) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
