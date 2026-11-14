package ec2

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// RemoveRunner deregisters a tier's runner at GitHub. It returns nil when the
// runner is gone (removed now, or already unknown), and core.ErrRunnerBusy
// when GitHub refuses because the runner has a job.
type RemoveRunner func(ctx context.Context, tier, runner string) error

// ReaperOptions configure a Reaper.
type ReaperOptions struct {
	// Owner is this install's firerunner:owner tag; nothing else is touched.
	Owner string
	// PooledTiers names the tiers that keep a stopped pool: their ready and
	// warming instances belong to the pool, if their pool MAC holds (see
	// TagPoolMAC). Pool instances of any other tier (its pool was
	// configured away), and ones whose MAC fails, are reaped.
	PooledTiers map[string]bool
	// Remove deregisters a runner before its instance is reaped.
	Remove RemoveRunner
	// MaxLifetime reaps a running instance this old even if its runner is
	// busy; 0 disables.
	MaxLifetime time.Duration
	// Suspended, when set, says why the owner's tenant may run nothing (it
	// is suspended, or removed: see EndAll), or "". While it says so, a
	// running orphan is reaped busy or not: such a tenant's machines end,
	// after a restart too.
	Suspended func() string
	// Grace leaves alone instances younger than this, so a launch between
	// RunInstances and its registration is never mistaken for an orphan
	// (default 2m).
	Grace time.Duration
	// Interval between sweeps (default 5m).
	Interval time.Duration
	// DryRun logs what would be reaped and changes nothing.
	DryRun bool
	Logger *slog.Logger
}

// Reaper ends what this install launched and no longer owns: job instances
// left by a firerunner that crashed or restarted, and persistent spot
// requests left without an instance. It runs at startup and every Interval.
//
// It never ends a job: a running orphan is ended only once GitHub agrees to
// deregister its runner (it has no job), past MaxLifetime, or while its
// tenant is suspended. A busy one is
// left to finish; it powers off, and the next sweep ends it. It deregisters a
// runner only by a runner tag whose TagRunnerMAC holds for the instance.
//
// It ends nothing by its owner tag alone, which anyone who may tag an
// instance can write: an instance must prove this install launched it (see
// launchedHere), and a spot request must carry its TagRequestMAC. What does
// not is left alone, logged, and reported as Unproven.
type Reaper struct {
	c   *Client
	o   ReaperOptions
	log *slog.Logger
	now func() time.Time

	mu   sync.Mutex
	last *ReapReport
}

// Last returns the latest sweep's report, if any.
func (r *Reaper) Last() (ReapReport, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return ReapReport{}, false
	}
	return *r.last, true
}

// ReapReport is what one sweep did (or, in dry run, would have done).
type ReapReport struct {
	At           time.Time
	DryRun       bool
	Instances    []string // terminated
	SpotRequests []string // cancelled
	Busy         []string // orphans left running a job
	// Young are untracked instances inside the grace period (just launched
	// or started); a later sweep looks at them again.
	Young []string
	// Unproven are instances and spot requests with this install's owner
	// tag that nothing proves it launched; they are left alone. One is
	// either retagged by someone else, or one whose signing failed.
	Unproven []string
	Err      string
}

// NewReaper returns a Reaper over the client's account.
func (c *Client) NewReaper(o ReaperOptions) *Reaper {
	if o.Grace <= 0 {
		o.Grace = 2 * time.Minute
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Minute
	}
	log := o.Logger
	if log == nil {
		log = c.log
	}
	return &Reaper{c: c, o: o, log: log.With("component", "reaper", "dryRun", o.DryRun), now: time.Now}
}

// Run sweeps now and every Interval until ctx ends.
func (r *Reaper) Run(ctx context.Context) {
	t := time.NewTicker(r.o.Interval)
	defer t.Stop()
	for {
		rep, err := r.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("reaper sweep", "err", err)
			rep.Err = err.Error()
		}
		r.mu.Lock()
		r.last = &rep
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EndAll ends what the owner's tenant left once it is removed from the
// tenants list: its scale sets, and its runners with them, are gone, so
// nothing of it is kept. A pool instance is reaped as of a tier without a
// pool (o.PooledTiers is ignored), and a running instance busy or not, for
// why (o.Suspended is ignored). What it cannot prove this install launched
// it leaves, as Sweep does. An instance too young to judge, a launch the
// tenant's pool had in flight as it stopped, is judged again once its grace
// has passed, and a sweep that failed is tried again then. It returns the
// last sweep's report, with every instance and request either one ended.
func (c *Client) EndAll(ctx context.Context, o ReaperOptions, why string) (ReapReport, error) {
	o.PooledTiers = nil
	o.Suspended = func() string { return why }
	r := c.NewReaper(o)
	rep, err := r.Sweep(ctx)
	if err == nil && len(rep.Young) == 0 {
		return rep, nil
	}
	t := time.NewTimer(r.o.Grace)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return rep, errors.Join(err, ctx.Err())
	case <-t.C:
	}
	again, err := r.Sweep(ctx)
	again.Instances = append(rep.Instances, again.Instances...)
	again.SpotRequests = append(rep.SpotRequests, again.SpotRequests...)
	return again, err
}

// Sweep reaps once.
func (r *Reaper) Sweep(ctx context.Context) (ReapReport, error) {
	rep := ReapReport{At: r.now(), DryRun: r.o.DryRun}
	// Requests first, instances second: every instance a listed request
	// names then already exists when the instances are listed, so a request
	// is never judged against a list taken before its instance launched.
	var requests []types.SpotInstanceRequest
	sp := ec2.NewDescribeSpotInstanceRequestsPaginator(r.c.api, &ec2.DescribeSpotInstanceRequestsInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + TagOwner), Values: []string{r.o.Owner}},
		{Name: aws.String("state"), Values: []string{"open", "active", "disabled"}},
		// A cold instance's one-time request carries the owner tag too, and
		// may read active a moment after its instance ends; it never
		// launches a replacement, so it is no orphan.
		{Name: aws.String("type"), Values: []string{string(types.SpotInstanceTypePersistent)}},
	}})
	for sp.HasMorePages() {
		page, err := sp.NextPage(ctx)
		if err != nil {
			return rep, err
		}
		requests = append(requests, page.SpotInstanceRequests...)
	}
	var instances []types.Instance
	ip := ec2.NewDescribeInstancesPaginator(r.c.api, &ec2.DescribeInstancesInput{Filters: []types.Filter{
		{Name: aws.String("tag:" + TagOwner), Values: []string{r.o.Owner}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	for ip.HasMorePages() {
		page, err := ip.NextPage(ctx)
		if err != nil {
			return rep, err
		}
		for _, res := range page.Reservations {
			instances = append(instances, res.Instances...)
		}
	}
	now := r.now()
	// The requests this install signed; an instance one launched is ours.
	signed := map[string]bool{}
	for _, s := range requests {
		if r.c.verifyRequestMAC(s, r.o.Owner) == nil {
			signed[aws.ToString(s.SpotInstanceRequestId)] = true
		}
	}
	alive := map[string]bool{}
	for _, i := range instances {
		alive[aws.ToString(i.InstanceId)] = true
	}
	for _, i := range instances {
		r.judge(ctx, i, now, signed, &rep)
	}

	// Persistent spot requests whose instance is gone would launch an
	// untracked replacement. A request this young may still be filling, and
	// one whose instance this process tracks is in use.
	for _, s := range requests {
		id, inst := aws.ToString(s.SpotInstanceRequestId), aws.ToString(s.InstanceId)
		if inst != "" && (alive[inst] || r.c.isLive(inst)) {
			continue
		}
		if now.Sub(aws.ToTime(s.CreateTime)) < r.o.Grace {
			continue
		}
		if !signed[id] {
			r.log.Warn("spot request left without an instance, with this install's owner tag but not its signature; not cancelling it", "request", id, "instance", inst, "why", r.c.verifyRequestMAC(s, r.o.Owner))
			rep.Unproven = append(rep.Unproven, id)
			continue
		}
		if r.o.DryRun {
			r.log.Info("would cancel a spot request left without an instance", "request", id, "instance", inst)
			rep.SpotRequests = append(rep.SpotRequests, id)
			continue
		}
		if _, err := r.c.api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{id}}); err != nil {
			r.log.Warn("cancel orphan spot request", "request", id, "err", err)
			continue
		}
		r.log.Info("cancelled a spot request left without an instance", "request", id, "instance", inst)
		rep.SpotRequests = append(rep.SpotRequests, id)
	}
	level := slog.LevelDebug
	if len(rep.Instances)+len(rep.SpotRequests) > 0 {
		level = slog.LevelInfo
	}
	r.log.Log(ctx, level, "reaper sweep", "instances", len(rep.Instances), "spotRequests", len(rep.SpotRequests), "busy", len(rep.Busy), "young", len(rep.Young), "unproven", len(rep.Unproven))
	return rep, nil
}

// judge decides one of this install's instances: leave it, or reap it.
// signed holds the spot requests whose request MAC holds.
func (r *Reaper) judge(ctx context.Context, i types.Instance, now time.Time, signed map[string]bool, rep *ReapReport) {
	id := aws.ToString(i.InstanceId)
	if r.c.isLive(id) {
		return
	}
	tier, runner, pool := tagValue(i.Tags, TagTier), tagValue(i.Tags, TagRunner), tagValue(i.Tags, TagPool)
	spotReq := aws.ToString(i.SpotInstanceRequestId)
	log := r.log.With("instance", id, "tier", tier, "runner", runner, "state", i.State.Name)

	inPool := pool == PoolReady || pool == PoolWarming
	if inPool && r.o.PooledTiers[tier] && r.c.verifyPoolMAC(i, r.o.Owner, tier) == nil {
		return // the pool's own
	}
	// Anything else nobody here tracks. Its MACs are tagged a moment after
	// RunInstances, and LaunchTime is the latest start (a pool instance a
	// job just started is young too), so leave it the grace period.
	if now.Sub(aws.ToTime(i.LaunchTime)) < r.o.Grace {
		rep.Young = append(rep.Young, id)
		return
	}
	if err := r.c.launchedHere(i, r.o.Owner, signed); err != nil {
		log.Warn("instance with this install's owner tag that nothing proves it launched; not ending it", "why", err)
		rep.Unproven = append(rep.Unproven, id)
		return
	}
	if inPool {
		why := "pool instance of a tier that keeps no pool"
		if r.o.PooledTiers[tier] {
			// Ours by its runner MAC or its spot request (relaunched by
			// AWS, say), but not a pool instance the pool can use.
			why = "pool instance failing its MAC check: " + r.c.verifyPoolMAC(i, r.o.Owner, tier).Error()
		}
		if r.reap(ctx, log, why, id, spotReq) {
			rep.Instances = append(rep.Instances, id)
		}
		return
	}
	// A job instance (claimed from a pool, or cold) nobody tracks.
	switch i.State.Name {
	case types.InstanceStateNameStopped, types.InstanceStateNameStopping:
		// Its runner ran (or failed) and powered off, so its
		// ephemeral registration is already gone. The runner name is
		// not deregistered: a launch that fell back to a cold
		// instance may be using the same name right now.
		if r.reap(ctx, log, "used instance left stopped", id, spotReq) {
			rep.Instances = append(rep.Instances, id)
		}
	default:
		if r.o.MaxLifetime > 0 && now.Sub(aws.ToTime(i.LaunchTime)) >= r.o.MaxLifetime {
			if r.reap(ctx, log, "orphan past the max lifetime", id, spotReq) {
				rep.Instances = append(rep.Instances, id)
			}
			return
		}
		if r.o.Suspended != nil {
			if why := r.o.Suspended(); why != "" {
				if r.reap(ctx, log, "orphan of a tenant that may run nothing: "+why, id, spotReq) {
					rep.Instances = append(rep.Instances, id)
				}
				return
			}
		}
		if runner == "" {
			return // nothing to ask GitHub; MaxLifetime ends it
		}
		// Anyone who may tag an instance can write a runner tag: on one of
		// their own, it would have an idle runner of ours deregistered.
		// Only a tag this install signed for this instance names a runner.
		if err := r.c.verifyRunnerMAC(i, r.o.Owner, tier, runner); err != nil {
			log.Warn("runner tag not signed for this instance; not deregistering by it, leaving the instance to the max lifetime", "why", err)
			return
		}
		if r.o.DryRun {
			log.Info("would deregister this orphan's runner and, unless it is busy, terminate it")
			rep.Instances = append(rep.Instances, id)
			return
		}
		err := r.o.Remove(ctx, tier, runner)
		switch {
		case errors.Is(err, core.ErrRunnerBusy):
			log.Info("orphan is running a job; leaving it to finish")
			rep.Busy = append(rep.Busy, id)
		case err != nil:
			log.Warn("could not deregister the orphan's runner; trying next sweep", "err", err)
		default:
			if r.reap(ctx, log, "orphan whose runner had no job", id, spotReq) {
				rep.Instances = append(rep.Instances, id)
			}
		}
	}
}

// reap cancels the instance's spot request, if any, then terminates it, and
// reports whether it did (or, in dry run, would). It never terminates with
// the request possibly open.
func (r *Reaper) reap(ctx context.Context, log *slog.Logger, why, id, spotReq string) bool {
	if r.o.DryRun {
		log.Info("would terminate", "why", why)
		return true
	}
	if spotReq != "" {
		if _, err := r.c.api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{spotReq}}); err != nil {
			log.Warn("cancel spot request before terminate; trying next sweep", "request", spotReq, "err", err)
			return false
		}
	}
	if _, err := r.c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		log.Warn("terminate orphan", "err", err)
		return false
	}
	log.Info("terminated", "why", why)
	return true
}
