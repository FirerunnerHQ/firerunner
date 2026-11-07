package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/ec2"
	"github.com/firerunnerhq/firerunner/internal/ghrunners"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/listener"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/reconcile"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
)

// tenant is one GitHub organization's or repository's install on a shard: who
// it is to GitHub, and the tiers it serves. Owner is its firerunner:owner tag,
// which scopes the reaper and the stopped pools to its own instances.
type tenant struct {
	Owner       string
	URL         string
	RunnerGroup string
	KeepOnClose bool
	Token       string
	App         scaleset.GitHubAppAuth
	Tiers       []config.Tier
}

// shard holds what a process's tenants share. Its EC2 client carries the
// account's API limits and launch backoff, and the JIT server is one endpoint
// that tells instances apart by their private address.
type shard struct {
	cfg       *config.Config
	log       *slog.Logger
	events    jobevent.Sink
	fc        *provisioner.Firecracker
	ec2Client *ec2.Client
	jit       *jitserver.Server
	ec2Cache  *ec2.CacheOptions
	cacheJobs *jobidentity.Registry
	pending   scheduler.PendingLaunches
}

// tenantHooks are the callbacks that give a tenant's jobs a cache identity.
type tenantHooks struct {
	onJobStarted func(context.Context, jobidentity.Job)
	onJobOffered func(jobidentity.Job)
	onRunnerJob  func(runner string)
}

// identityHooks resolves a tenant's job identities with its own credentials.
func (s *shard) identityHooks(ctx context.Context, t tenant) (tenantHooks, error) {
	resolver, err := jobidentity.NewGitHub(t.URL, t.Token, t.App)
	if err != nil {
		return tenantHooks{}, err
	}
	jobs := s.cacheJobs
	hooks := tenantHooks{
		onJobStarted: func(ctx context.Context, j jobidentity.Job) { jobs.Start(ctx, j, resolver) },
	}
	if s.cfg.UsesEC2Cache() {
		// GitHub may deliver JobStarted only once the job is over: an EC2
		// instance's own report that its job started, matched against
		// the runs GitHub offered, finds it within seconds (#28).
		cands := &jobidentity.Candidates{}
		hooks.onJobOffered = cands.Offer
		hooks.onRunnerJob = func(runner string) { jobs.Discover(ctx, runner, cands, resolver, resolver) }
	}
	return hooks, nil
}

// servedTenant is a tenant running on a shard. Its slices share an order: a
// tier's index means the same in each.
type servedTenant struct {
	tiers       []config.Tier
	trackers    []*health.Tracker
	provs       []provisioner.Provisioner
	listeners   []*listener.ScaleSet
	scheds      []*scheduler.Scheduler
	runnerList  *ghrunners.Client
	reaperState func() *health.Reaper
}

// start registers a tenant's scale sets and builds what serves them. A failure
// leaves none of its scale sets open.
func (s *shard) start(ctx context.Context, t tenant, hooks tenantHooks) (*servedTenant, error) {
	cfg, log := s.cfg, s.log
	st := &servedTenant{tiers: t.Tiers}

	// The REST runner list behind --runner-sweep, shared by the tenant's tiers.
	if cfg.RunnerSweep != "off" {
		list, err := ghrunners.New(t.URL, t.Token, t.App)
		if err != nil {
			log.Warn("runner sweep disabled", "err", err)
		} else {
			st.runnerList = list
		}
	}

	// A tier's provisioner and listener share its health and the jobs
	// assigned to it: the listener records both, and an EC2 provisioner
	// reports its launch paths and asks whether a launch may take the pool.
	st.trackers = make([]*health.Tracker, len(t.Tiers))
	tierHealth := map[string]*health.Tracker{}
	tierJobs := map[string]*assigned.Jobs{}
	for i, tier := range t.Tiers {
		st.trackers[i] = health.NewTracker(tier.Name)
		tierHealth[tier.Name] = st.trackers[i]
		tierJobs[tier.Name] = assigned.New(poolPolicy(tier))
	}
	var newEC2 func(config.Tier) provisioner.Provisioner
	if s.ec2Client != nil {
		newEC2 = func(tier config.Tier) provisioner.Provisioner {
			// A scheduled pool starts at the current window's size; the
			// schedule follower resizes it from there.
			e := *tier.EC2
			e.Pool = tier.SizesAt(time.Now()).Pool
			p := s.ec2Client.Tier(ec2.Options{
				Tier:        tier.Name,
				Owner:       t.Owner,
				EC2:         e,
				Pooled:      tier.MaxPool() > 0,
				JIT:         s.jit,
				JITURL:      jitserver.URL(cfg.JITEndpoint()),
				MaxLifetime: cfg.Firecracker.MaxVMLifetime,
				UsePool:     tierJobs[tier.Name].ClaimPool,
				Cache:       s.ec2Cache,
				Events:      s.events,
				Launched:    tierHealth[tier.Name].Launched,
				FellBack:    tierHealth[tier.Name].FellBack,
				Interrupted: tierHealth[tier.Name].Interrupted,
				Logger:      log.With("tier", tier.Name),
			})
			// A tier's stopped pool fills and refills in the background for
			// the life of the process; its instances stay stopped on exit and
			// are found again on the next start.
			if pool := p.Pool(); pool != nil {
				go pool.Run(ctx)
			}
			return p
		}
	}
	st.provs = make([]provisioner.Provisioner, len(t.Tiers))
	for i, tier := range t.Tiers {
		var shared provisioner.Provisioner
		if s.fc != nil {
			shared = s.fc
		}
		p, err := tierProvisioner(tier, shared, newEC2)
		if err != nil {
			return nil, fmt.Errorf("tier %q: %w", tier.Name, err)
		}
		st.provs[i] = p
	}

	// Register every tier's scale set up front so a failure aborts cleanly
	// before we start booting microVMs.
	for i, tier := range t.Tiers {
		tlog := log.With("tier", tier.Name)
		lis, err := listener.New(ctx, listener.Config{
			URL:          t.URL,
			OnJobStarted: hooks.onJobStarted,
			OnJobOffered: hooks.onJobOffered,
			Events:       s.events,
			Health:       st.trackers[i],
			Pending:      tierJobs[tier.Name],
			Name:         tier.Name,
			RunnerGroup:  t.RunnerGroup,
			Labels:       tier.Labels,
			MaxRunners:   tier.Max,
			// A named install outlives its process: keep its scale set
			// across restarts and control VM replacements.
			KeepOnClose: t.KeepOnClose,
			Token:       t.Token,
			App:         t.App,
			Version:     version,
			Logger:      tlog,
		}, t.Owner)
		if err != nil {
			st.close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("tier %q: %w", tier.Name, err)
		}
		st.listeners = append(st.listeners, lis)
		jit := lis.JIT()
		st.scheds = append(st.scheds, scheduler.New(scheduler.Options{
			Max:         tier.Max,
			Min:         tier.SizesAt(time.Now()).Min,
			Spec:        tier.Spec(),
			Provisioner: st.provs[i],
			JIT:         jit,
			Remover:     jit,
			Logger:      tlog,
			Pending:     tierPending(tier, &s.pending),
		}))
	}

	if s.ec2Client != nil && cfg.EC2Reaper != "off" {
		reaper := s.ec2Client.NewReaper(reaperOptions(cfg, t.Owner, t.Tiers, st.listeners, log))
		go reaper.Run(ctx)
		st.reaperState = func() *health.Reaper {
			rep, ok := reaper.Last()
			if !ok {
				return nil
			}
			return &health.Reaper{
				At: rep.At, DryRun: rep.DryRun, Instances: rep.Instances, SpotRequests: rep.SpotRequests,
				Busy: rep.Busy, Young: rep.Young, Error: rep.Err,
			}
		}
	}
	return st, nil
}

// close closes the tenant's scale set sessions.
func (st *servedTenant) close(ctx context.Context) {
	for _, l := range st.listeners {
		_ = l.Close(ctx)
	}
}

// logTiers says what each tier runs on.
func (st *servedTenant) logTiers(log *slog.Logger) {
	for i, t := range st.tiers {
		if t.IsEC2() {
			log.Info("serving tier",
				"name", t.Name, "provisioner", st.provs[i].Name(),
				"instanceTypes", t.EC2.InstanceTypes, "market", t.EC2.Market,
				"min", t.Min, "max", t.Max)
			continue
		}
		log.Info("serving tier",
			"name", t.Name, "provisioner", st.provs[i].Name(),
			"vcpu", t.VCPU, "memMiB", t.MemMiB,
			"golden", t.Golden, "min", t.Min, "max", t.Max)
	}
}

// runs returns one function per tier that serves it until its context ends.
func (st *servedTenant) runs(cfg *config.Config, log *slog.Logger) []func(context.Context) error {
	runs := make([]func(context.Context) error, len(st.listeners))
	for i := range st.listeners {
		lis, sched, tracker, prov, tier := st.listeners[i], st.scheds[i], st.trackers[i], st.provs[i], st.tiers[i]
		// tierCtx is this tier's own lifetime (see serveTiers): launches, the
		// re-plan after a runner exits and the warm-pool top-up bind to it,
		// so a tier that stops cancels its idle runners and stops launching,
		// while busy ones finish.
		runs[i] = func(tierCtx context.Context) error {
			onDesired := func(_ context.Context, desired int) int {
				sched.Reconcile(tierCtx, desired)
				return sched.Running()
			}
			onBusy := func(name string) { sched.MarkBusy(name) }
			onCompleted := func(name string) { sched.MarkCompleted(name) }
			capacity := func() int { return sched.Capacity() }
			// The reconciler compares GitHub's statistics with this tier's
			// runners for as long as the tier serves.
			jit := lis.JIT()
			ropts := reconcile.Options{
				Health: tracker, Logger: log.With("tier", tier.Name, "component", "reconcile"),
			}
			if st.runnerList != nil {
				ropts.Offline = offlineRunners{list: st.runnerList, jit: jit}
				ropts.OfflineDryRun = cfg.RunnerSweep == "dry-run"
			}
			go reconcile.New(statsSource{lis}, sched, jit.Exists, ropts).Run(tierCtx)
			if tier.Schedule != nil {
				go schedule.Follow(tierCtx, tier.SizesAt, scheduleTarget(sched, prov, tracker),
					30*time.Second, nil, log.With("tier", tier.Name, "component", "schedule"))
			}
			return lis.Run(tierCtx, onDesired, onBusy, onCompleted, capacity)
		}
	}
	return runs
}

// drain waits for the tenant's runners to finish.
func (st *servedTenant) drain() {
	for _, sched := range st.scheds {
		sched.Drain()
	}
}
