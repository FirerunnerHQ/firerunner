package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/firerunnerhq/firerunner/internal/assigned"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/ec2"
	"github.com/firerunnerhq/firerunner/internal/ghauth"
	"github.com/firerunnerhq/firerunner/internal/ghrunners"
	"github.com/firerunnerhq/firerunner/internal/guardduty"
	"github.com/firerunnerhq/firerunner/internal/health"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
	"github.com/firerunnerhq/firerunner/internal/jobidentity"
	"github.com/firerunnerhq/firerunner/internal/listener"
	"github.com/firerunnerhq/firerunner/internal/provisioner"
	"github.com/firerunnerhq/firerunner/internal/reconcile"
	"github.com/firerunnerhq/firerunner/internal/schedule"
	"github.com/firerunnerhq/firerunner/internal/scheduler"
	"github.com/firerunnerhq/firerunner/internal/tenantstate"
	"github.com/firerunnerhq/firerunner/proprietary/entitlement"
)

// tenant is one GitHub organization's or repository's install on a shard: who
// it is to GitHub, and the tiers it serves. Owner is its firerunner:owner tag,
// which scopes the reaper and the stopped pools to its own instances.
type tenant struct {
	// ID is empty for the tenant the flags name.
	ID          string
	Owner       string
	URL         string
	RunnerGroup string
	// RunnerGroupID is the group by id, 0 to find it by RunnerGroup's
	// name; GroupPending is a group the tenants list says is not ready
	// (runner_group_id 0): the tenant is not served until it is.
	RunnerGroupID int
	GroupPending  bool
	KeepOnClose   bool
	// WaitForSession stands its tiers by while another daemon holds their
	// scale sets' sessions, rather than failing (listener.Config): the
	// install of a shard under leases, which every shard runs. Its tiers'
	// sessions open all together or not at all, and Standby, when set, is
	// told whether it stands by.
	WaitForSession bool
	Standby        func(bool)
	Token          string
	// Auth is App's installation, with JWTs signed once for the whole shard.
	Auth  ghauth.App
	Tiers []config.Tier
	// Limits bound what it runs; Refused says why tiers it lists are not
	// served to it (above its vCPU limit).
	Limits  config.Limits
	Refused []string
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
	// states reads which tenants are suspended (--tenant-state); nil when
	// none are kept.
	states *tenantstate.Watcher
	// findings flags tenants for review from GuardDuty (--guardduty); nil
	// when off. It never suspends one.
	findings *guardduty.Poller
	// entitlements reads what each tenant may run (--entitlements); nil when
	// off. It holds a tenant's launches, and never stops a runner.
	entitlements *entitlement.Watcher
	// tenants are the tenants of the list, once the list is read.
	tenants atomic.Pointer[tenantSet]
	// removeByName deletes a scale set by name; nil is
	// listener.RemoveByName. Tests replace it.
	removeByName func(context.Context, listener.Config) (bool, error)
	// endAll ends what an owner left on EC2; nil is ec2Client.EndAll.
	// Tests replace it.
	endAll func(ctx context.Context, o ec2.ReaperOptions, why string) (ec2.ReapReport, error)
	// tenantListed, with the tenants in the job store, reports whether a
	// tenant id is listed there now: a removal ends no machine by its owner
	// tag once it is, as they are a run's of the id listed again.
	tenantListed func(ctx context.Context, id string) (bool, error)
	// installationGone, with hosted onboarding, reports whether the App's
	// installation was uninstalled or suspended: GitHub then gives it no
	// token, so its scale sets cannot be deleted, and trying again would
	// never end (#170).
	installationGone func(ctx context.Context, installation int64) (bool, error)
	// applyMu makes each applyState decide and apply as one step: the
	// watchers and the tenant loops call it at once, and a decision taken
	// on older inputs must not land after one taken on newer.
	applyMu sync.Mutex
	// exiting ends when the process is told to exit; nil in tests, where
	// it never does.
	exiting context.Context
}

// tenantHooks are the callbacks that give a tenant's jobs a cache identity.
type tenantHooks struct {
	onJobStarted func(context.Context, jobidentity.Job)
	onJobOffered func(jobidentity.Job)
	onRunnerJob  func(runner string)
	onRunnerID   func(runner string, id int64)
}

// identityHooks resolves a tenant's job identities with its own credentials.
func (s *shard) identityHooks(ctx context.Context, t tenant) (tenantHooks, error) {
	resolver, err := jobidentity.NewGitHub(t.URL, t.Token, t.Auth)
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
		hooks.onRunnerID = jobs.NoteRunnerID // Discover matches by it
	}
	return hooks, nil
}

// servedTenant is a tenant running on a shard. Its slices share an order: a
// tier's index means the same in each.
type servedTenant struct {
	t           tenant
	log         *slog.Logger
	tiers       []config.Tier
	trackers    []*health.Tracker
	provs       []provisioner.Provisioner
	listeners   []*listener.ScaleSet
	scheds      []*scheduler.Scheduler
	runnerList  *ghrunners.Client
	reaperState func() *health.Reaper
	// bound is the tenant's limit on runners across its tiers.
	bound *scheduler.Tenant
}

// schedulerOptions is what a tier's scheduler gets from its tenant: the
// tenant's bound across its tiers and its job limit, reported in the tier's
// health.
func (s *shard) schedulerOptions(t tenant, tier config.Tier, bound *scheduler.Tenant, tracker *health.Tracker, prov provisioner.Provisioner, jit *listener.JITSource, log *slog.Logger) scheduler.Options {
	o := scheduler.Options{
		Max:         tier.Max,
		Min:         tier.SizesAt(time.Now()).Min,
		Spec:        tier.Spec(),
		Provisioner: prov,
		Logger:      log,
		Pending:     tierPending(tier, &s.pending),
		Tenant:      bound,
		Tier:        tier.Name,
		MaxJob:      t.Limits.JobDuration,
		OnHold:      tracker.SetHeld,
		OnStop:      s.stopRecorder(t, tier.Name, tracker),
	}
	if jit != nil {
		o.JIT, o.Remover = jit, jit
	}
	return o
}

// heldBy asks the tier's scheduler, once there is one, how many launches
// its tenant holds back, of what kind, and why.
func heldBy(sched func() *scheduler.Scheduler) func() (int, string, string) {
	return func() (int, string, string) {
		if s := sched(); s != nil {
			n, h := s.Holding()
			return n, string(h.Kind), h.Why
		}
		return 0, "", ""
	}
}

// listenerConfig is what a tier's scale set listener gets from its tenant.
// held says what the tier's tenant holds back, for the jobs a message
// assigns.
func (s *shard) listenerConfig(t tenant, tier config.Tier, hooks tenantHooks, tracker *health.Tracker, jobs *assigned.Jobs, log *slog.Logger, held func() (int, string, string)) listener.Config {
	return listener.Config{
		Held:          held,
		URL:           t.URL,
		OnJobStarted:  hooks.onJobStarted,
		OnJobOffered:  hooks.onJobOffered,
		OnRunnerID:    hooks.onRunnerID,
		Events:        s.eventsFor(t),
		Health:        tracker,
		Pending:       jobs,
		Name:          tier.Name,
		RunnerGroup:   t.RunnerGroup,
		RunnerGroupID: t.RunnerGroupID,
		Labels:        tier.Labels,
		MaxRunners:    tier.Max,
		// A named install outlives its process: keep its scale set across
		// restarts and control VM replacements.
		KeepOnClose:    t.KeepOnClose,
		WaitForSession: t.WaitForSession,
		Token:          t.Token,
		App:            t.Auth,
		Version:        version,
		Logger:         log,
	}
}

// ec2Options is what a tier's EC2 provisioner gets from its tenant and shard.
func (s *shard) ec2Options(t tenant, tier config.Tier, hooks tenantHooks, tracker *health.Tracker, jobs *assigned.Jobs) ec2.Options {
	// A scheduled pool starts at the current window's size; the schedule
	// follower resizes it from there.
	e := *tier.EC2
	e.Pool = tier.SizesAt(time.Now()).Pool
	return ec2.Options{
		Tier:        tier.Name,
		Owner:       t.Owner,
		EC2:         e,
		Pooled:      tier.MaxPool() > 0,
		JIT:         s.jit,
		JITURL:      jitserver.URL(s.cfg.JITEndpoint()),
		MaxLifetime: tierLifetime(s.cfg.Firecracker.MaxVMLifetime, t.Limits.JobDuration, tier),
		UsePool:     jobs.ClaimPool,
		Cache:       s.ec2Cache,
		Events:      s.eventsFor(t),
		OnRunnerJob: hooks.onRunnerJob,
		Launched:    tracker.Launched,
		FellBack:    tracker.FellBack,
		Interrupted: tracker.Interrupted,
		Logger:      s.log.With("tier", t.tierKey(tier.Name)),
	}
}

// jobLifetimeSlack is how much longer than a tenant's job limit its machine
// may live, from launch: boot, the wait for the job, and the end. It ends a
// machine whose job's start was never reported, which the job limit cannot.
const jobLifetimeSlack = 15 * time.Minute

// maxLifetime is the lifetime backstop for a tenant's machines: the
// install's, or its job limit plus slack when that is shorter. 0 is none.
func maxLifetime(install, job time.Duration) time.Duration {
	if job <= 0 {
		return install
	}
	if limit := job + jobLifetimeSlack; install <= 0 || limit < install {
		return limit
	}
	return install
}

// tierLifetime is maxLifetime for a tier that keeps no warm runners, whose
// machines launch for a job: a warm runner may wait longer than the slack
// for one, and keeps the install's backstop. A tenant's tiers keep none.
func tierLifetime(install, job time.Duration, tier config.Tier) time.Duration {
	if tier.MaxMin() > 0 {
		return install
	}
	return maxLifetime(install, job)
}

// stopRecorder reports a runner a tier's scheduler ended for its tenant's
// limits: in the tier's health and the job events.
func (s *shard) stopRecorder(t tenant, tier string, tracker *health.Tracker) func(scheduler.Stop) {
	events := s.eventsFor(t)
	return func(st scheduler.Stop) {
		tracker.Stopped(st.Kind)
		jobevent.Record(events, jobevent.Event{Kind: jobevent.Stopped, Tier: tier, Runner: st.Runner, Reason: st.Reason})
	}
}

// eventsFor is where t's job events go: the shard's sink, naming t on each
// event, as tiers and runners are named only within a tenant.
func (s *shard) eventsFor(t tenant) jobevent.Sink {
	return jobevent.ForTenant(s.events, t.ID)
}

// extraTenant is a tenant from the --tenants file, on the install's App.
// tenantOwnerOf is the owner tag the tenants' instances are tagged under,
// <it>/<id>: --tenant-owner, or else the install's owner.
func tenantOwnerOf(cfg *config.Config, owner string) string {
	if cfg.TenantOwner != "" {
		return cfg.TenantOwner
	}
	return owner
}

func extraTenant(cfg *config.Config, c config.Tenant, owner string, jwt *ghauth.JWT) tenant {
	group := c.RunnerGroup
	if group == "" {
		group = cfg.RunnerGroup
	}
	tiers, refused := cfg.TenantTiers(c)
	var groupID int
	pending := false
	if c.RunnerGroupID != nil {
		groupID, pending = int(*c.RunnerGroupID), *c.RunnerGroupID == 0
	}
	return tenant{
		ID: c.ID, Owner: owner + "/" + c.ID, URL: c.URL, RunnerGroup: group, KeepOnClose: cfg.Owner != "",
		RunnerGroupID: groupID, GroupPending: pending,
		Auth:  ghauth.App{InstallationID: c.InstallationID, JWT: jwt},
		Tiers: tiers, Limits: c.EffectiveLimits(), Refused: refused,
	}
}

// runTenant serves a tenant of the list until ctx ends, as cur returns its
// entry each time it starts.
func (s *shard) runTenant(ctx context.Context, cur func() config.Tenant, served func(*servedTenant), live *liveTenants, owner string, jwt *ghauth.JWT) {
	get := func() tenant { return extraTenant(s.cfg, cur(), owner, jwt) }
	et := get()
	defer live.forget(et.ID)
	et.warnRefused(s.log)
	if et.GroupPending {
		// Registered by the name alone, its scale sets could land in a
		// group of that name the organization keeps for itself, which
		// onboarding never opened to the installation's repositories only.
		s.log.Warn("tenant not served yet: its runner group is not ready", "tenant", et.ID, "group", et.RunnerGroup)
		live.skip(et, "its runner group is not ready: onboarding sets it up, and says why it cannot")
		<-ctx.Done()
		return
	}
	if len(et.Tiers) == 0 {
		s.log.Error("tenant not served: none of its tiers is within its limits", "tenant", et.ID, "limits", et.Limits.String())
		live.skip(et, "none of its tiers is within its limits")
		<-ctx.Done()
		return
	}
	l := s.loop(get, live)
	l.served = served
	l.run(ctx)
}

// loop builds the loop that keeps a tenant running on this shard. Each try
// starts the tenant as get returns it then: the tenants list may have
// changed it since the last.
func (s *shard) loop(get func() tenant, live *liveTenants) *tenantLoop {
	return &tenantLoop{
		id: get().ID,
		start: func(ctx context.Context) (*servedTenant, error) {
			st, err := s.start(ctx, get(), tenantHooks{})
			if err == nil {
				st.logTiers()
			}
			return st, err
		},
		serve: func(ctx context.Context, cancel context.CancelFunc, st *servedTenant, onPanic func()) error {
			return serveTiers(ctx, cancel, st.names(), st.runs(s.cfg), onPanic, st.log)
		},
		stopLimit: panicStopLimit(get().Limits.JobDuration),
		stop: func(ctx context.Context, st *servedTenant) {
			p := s.stopPlan(ctx)
			if p.sessionsFirst {
				st.closeSessions(context.WithoutCancel(ctx))
			}
			st.drain(p.handOff)
			switch {
			case p.remove && !p.removesNow(ctx):
				// Its lease lapsed while it drained: another shard may
				// serve the account under these names now. A sweep deletes
				// them under a lease.
				s.log.Warn("removed tenant's lease lapsed while its runners finished: leaving its scale sets to a sweep", "tenant", st.t.ID)
			case p.remove:
				// Bounded: a GitHub that hangs must not hold up shutdown,
				// and a lease that lapses meanwhile stops it at once.
				rctx, release := boundRemoval(ctx)
				defer release()
				st.remove(rctx)
			case !p.sessionsFirst:
				st.close(context.WithoutCancel(ctx))
			}
		},
		live: live, log: s.log, min: tenantRetryMin, max: tenantRetryMax,
	}
}

// removeLeftovers deletes, by name, the scale sets a removed tenant's tiers
// registered in its runner group, each tier its last entry lists, served or
// not. A run that served it when it was removed has deleted its own, and
// finds none; this covers the rest: a tenant in retry backoff, skipped, or
// waiting on a run before it when removed, and one whose scale sets an
// earlier run kept (KeepOnClose); with the tenants in the job store under
// leases, also a tenant removed while no shard served it (a sweep). Not
// covered: a tier or runner group an earlier entry named.
//
// It goes on as the daemon stops, each tier bounded, but stops at once,
// mid-call too, once its lease lapses (leaseLapse): another shard may
// serve the account then, under the same scale set names. It returns an
// error unless every tier's scale set is gone, or the tenant's App
// installation is (installationGone): nothing can delete them then, and a
// reinstall or unsuspend serves them again by name.
func (s *shard) removeLeftovers(ctx context.Context, t tenant, names []string) error {
	remove := s.removeByName
	if remove == nil {
		remove = listener.RemoveByName
	}
	if t.GroupPending {
		// Not served, it registered nothing; and by name, the group could
		// be one the organization keeps for itself, whose scale sets of
		// the same names are its own.
		s.log.Info("a removed tenant's runner group was not ready: it registered no scale set, and none is deleted", "tenant", t.ID)
		return nil
	}
	if s.installationGone != nil {
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tenantRemoveTimeout)
		gone, err := s.installationGone(gctx, t.Auth.InstallationID)
		cancel()
		switch {
		case err != nil:
			s.log.Warn("could not read whether a removed tenant's App installation is gone; deleting its scale sets", "tenant", t.ID, "err", err)
		case gone:
			s.log.Info("a removed tenant's App installation is gone; its scale sets cannot be deleted, and are left to it", "tenant", t.ID, "installation", t.Auth.InstallationID)
			return nil
		}
	}
	lapse := leaseLapse(ctx)
	var errs []error
	for _, name := range names {
		if lapse != nil && lapse.Err() != nil {
			return errors.Join(append(errs, errLeaseLapsed)...)
		}
		cfg := s.listenerConfig(t, config.Tier{Name: name}, tenantHooks{}, nil, nil, s.log, nil)
		// Bounded per tier: a GitHub that hangs must not hold up shutdown,
		// and a slow first tier must not use up the rest's time.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tenantRemoveTimeout)
		stop := func() bool { return false }
		if lapse != nil {
			stop = context.AfterFunc(lapse, cancel)
		}
		removed, err := remove(rctx, cfg)
		stop()
		cancel()
		switch {
		case err != nil:
			s.log.Warn("scale set of a removed tenant left registered", "tenant", t.ID, "tier", name, "err", err)
			errs = append(errs, fmt.Errorf("tier %s: %w", name, err))
		case removed:
			s.log.Info("deregistered runner scale set left by a removed tenant", "tenant", t.ID, "tier", name)
		}
	}
	return errors.Join(errs...)
}

// endRemoved ends what a removed tenant left on EC2, by its owner tag: its
// stopped pool, the pool's spot requests, and job instances no process
// tracks, busy or not, such as runners a stop left to this start (see
// ec2.Client.EndAll). Its runners went with its scale sets. It runs before
// a later run on the tenant's id or account may start, so it never ends
// what a run of a tenant listed again launched. A daemon stopping meanwhile
// still sweeps once, skips the second, and warns.
//
// The owner tag is the tenant id's, on whatever account: before each sweep
// it checks that its lease still holds and, with the tenants in the job
// store, that the id is not listed again (on another account, say), whose
// run's reaper ends what is left of the old one as its own. A lapse also
// ends the wait for the second; what the first can end is older than its
// grace (2 minutes), so launched before another shard could serve the
// account. It returns an error unless both sweeps ran, or the id is
// listed again.
func (s *shard) endRemoved(ctx context.Context, t tenant) error {
	endAll := s.endAll
	if endAll == nil && s.ec2Client != nil {
		endAll = s.ec2Client.EndAll
	}
	if endAll == nil || s.cfg.EC2Reaper == "off" {
		return nil
	}
	lapse := leaseLapse(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if lapse != nil {
		defer context.AfterFunc(lapse, cancel)()
	}
	log := s.log.With("tenant", t.ID)
	o := reaperOptions(s.cfg, t.Owner, nil, nil, log)
	o.Proceed = s.mayEndRemoved(lapse, t.ID)
	rep, err := endAll(ctx, o, "removed from the tenants list")
	if errors.Is(err, errListedAgain) {
		log.Info("removed tenant listed again: leaving what it left on EC2 to the reaper of its new run", "instances", len(rep.Instances))
		return nil
	}
	if err != nil {
		log.Warn("a removed tenant's instances or spot requests may be left; end those with its owner tag by hand, or let a shard sweep it", "owner", t.Owner, "instances", len(rep.Instances), "spotRequests", len(rep.SpotRequests), "err", err)
		return err
	}
	// Logged even when there was nothing: moving the tenant waits for it.
	log.Info("ended what a removed tenant left on EC2", "instances", len(rep.Instances), "spotRequests", len(rep.SpotRequests), "unproven", len(rep.Unproven), "dryRun", rep.DryRun)
	return nil
}

// mayEndRemoved is asked before each EC2 sweep of removed tenant id's
// owner tag: errLeaseLapsed once lapse has ended, errListedAgain once the
// job store lists the id again, or the error reading it.
func (s *shard) mayEndRemoved(lapse context.Context, id string) func(context.Context) error {
	return func(ctx context.Context) error {
		if lapse != nil && lapse.Err() != nil {
			return errLeaseLapsed
		}
		if s.tenantListed == nil {
			return nil
		}
		listed, err := s.tenantListed(ctx, id)
		if err == nil && listed {
			err = errListedAgain
		}
		return err
	}
}

// warnRefused logs each tier the tenant lists but is not served, and why.
func (t tenant) warnRefused(log *slog.Logger) {
	for _, why := range t.Refused {
		if t.ID != "" {
			log.Warn("tier not served to the tenant", "tenant", t.ID, "why", why)
			continue
		}
		log.Warn("tier not served (--limit-vcpu)", "why", why)
	}
}

// tierKey names a tier uniquely on a shard: tenants share the default labels.
func (t tenant) tierKey(tier string) string {
	if t.ID == "" {
		return tier
	}
	return t.ID + "/" + tier
}

// start registers a tenant's scale sets and builds what serves them. A failure
// leaves none of its scale sets open.
func (s *shard) start(ctx context.Context, t tenant, hooks tenantHooks) (_ *servedTenant, err error) {
	cfg, log := s.cfg, s.log
	if t.ID != "" {
		log = log.With("tenant", t.ID)
	}
	st := &servedTenant{t: t, tiers: t.Tiers, log: log, bound: s.newBound(t, log)}
	// A panic part way leaves no session open either: it is an error here.
	defer func() {
		if p := recover(); p != nil {
			st.close(context.WithoutCancel(ctx))
			err = recovered(p, fmt.Sprintf("tenant %q start", t.ID), log)
		}
	}()

	// The REST runner list behind --runner-sweep, shared by the tenant's tiers.
	if cfg.RunnerSweep != "off" {
		list, err := ghrunners.New(t.URL, t.Token, t.Auth)
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
	// An EC2 instance's report that its job started marks its runner busy,
	// as the guest console does for a microVM: GitHub's JobStarted can come
	// as late as the job's end, and a job's limit counts from its start.
	// The scheduler is built after the provisioner, so it is found here.
	tierScheds := map[string]*atomic.Pointer[scheduler.Scheduler]{}
	for i, tier := range t.Tiers {
		st.trackers[i] = health.NewTracker(t.tierKey(tier.Name))
		tierHealth[tier.Name] = st.trackers[i]
		tierJobs[tier.Name] = assigned.New(poolPolicy(tier))
		tierScheds[tier.Name] = &atomic.Pointer[scheduler.Scheduler]{}
	}
	// The tiers' stopped pools fill and refill in the background for the
	// life of the process, once every tier's session is open: one standing
	// by for another daemon's sessions launches nothing.
	var pools []interface{ Run(context.Context) }
	var newEC2 func(config.Tier) provisioner.Provisioner
	if s.ec2Client != nil {
		newEC2 = func(tier config.Tier) provisioner.Provisioner {
			o := s.ec2Options(t, tier, hooks, tierHealth[tier.Name], tierJobs[tier.Name])
			sched := tierScheds[tier.Name]
			o.OnRunnerJob = alsoMarkBusy(o.OnRunnerJob, sched.Load)
			p := s.ec2Client.Tier(o)
			// Its instances stay stopped on exit and are found again on the
			// next start.
			if pool := p.Pool(); pool != nil {
				pools = append(pools, pool)
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
		lis, err := listener.New(ctx, s.listenerConfig(t, tier, hooks, st.trackers[i], tierJobs[tier.Name], tlog, heldBy(tierScheds[tier.Name].Load)), t.Owner)
		if err != nil {
			st.close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("tier %q: %w", tier.Name, err)
		}
		st.listeners = append(st.listeners, lis)
		jit := lis.JIT()
		sched := scheduler.New(s.schedulerOptions(t, tier, st.bound, st.trackers[i], st.provs[i], jit, tlog))
		tierScheds[tier.Name].Store(sched)
		st.scheds = append(st.scheds, sched)
	}
	if t.WaitForSession {
		sets := make([]sessionSet, len(st.listeners))
		for i, l := range st.listeners {
			sets[i] = l
		}
		if err := openTogether(ctx, sets, sessionWait, t.Standby, log); err != nil {
			st.close(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	for _, pool := range pools {
		go pool.Run(ctx)
	}

	if s.ec2Client != nil && cfg.EC2Reaper != "off" {
		reaper := s.ec2Client.NewReaper(s.tenantReaperOptions(t, st.listeners, log))
		go reaper.Run(ctx)
		st.reaperState = func() *health.Reaper {
			rep, ok := reaper.Last()
			if !ok {
				return nil
			}
			return &health.Reaper{
				At: rep.At, DryRun: rep.DryRun, Instances: rep.Instances, SpotRequests: rep.SpotRequests,
				Busy: rep.Busy, Young: rep.Young, Unproven: rep.Unproven, Error: rep.Err,
			}
		}
	}
	return st, nil
}

// sessionWait is how long a tenant standing by for another daemon's
// sessions waits between attempts at them all.
const sessionWait = 10 * time.Second

// sessionSet is a scale set whose session opens once; a *listener.ScaleSet.
type sessionSet interface {
	TryOpenSession(ctx context.Context) (bool, error)
	ReleaseSession(ctx context.Context)
}

// openTogether opens the sessions of sets all together, or holds none: a
// tenant served by daemons that share its scale sets (shards of one
// configuration) is served by whichever opens them all. Holding some while
// it waits for the rest, two daemons could each hold part and wait on the
// other for good. It tries them in order, about every wait (randomly from
// half to one and a half of it, so that two starting together fall out of
// step rather than each releasing what the other needs, round after
// round), and on the first one held releases those it opened, until ctx
// ends. standby, when set, is told whether it stands by.
func openTogether(ctx context.Context, sets []sessionSet, wait time.Duration, standby func(bool), log *slog.Logger) error {
	if standby == nil {
		standby = func(bool) {}
	}
	defer standby(false)
	for round := 0; ; round++ {
		opened := 0
		for _, s := range sets {
			ok, err := s.TryOpenSession(ctx)
			if err != nil {
				releaseAll(ctx, sets[:opened])
				return err
			}
			if !ok {
				break
			}
			opened++
		}
		if opened == len(sets) {
			if round > 0 {
				log.Info("took over the scale sets' sessions another daemon held", "after", round)
			}
			return nil
		}
		releaseAll(ctx, sets[:opened])
		if round == 0 {
			log.Info("another daemon holds the scale sets' sessions: standing by to take them over")
		}
		standby(true)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait/2 + rand.N(wait)):
		}
	}
}

func releaseAll(ctx context.Context, sets []sessionSet) {
	for _, s := range sets {
		s.ReleaseSession(context.WithoutCancel(ctx))
	}
}

// alsoMarkBusy adds marking the runner busy, on the scheduler sched returns
// once there is one, to an instance's job report.
func alsoMarkBusy(onJob func(string), sched func() *scheduler.Scheduler) func(string) {
	return func(runner string) {
		if onJob != nil {
			onJob(runner)
		}
		if s := sched(); s != nil {
			s.MarkBusy(runner)
		}
	}
}

// newBound returns the bound on a tenant's runners, in the state last read:
// before any of its tiers serves, so a suspension outlives a restart.
func (s *shard) newBound(t tenant, log *slog.Logger) *scheduler.Tenant {
	b := scheduler.NewTenant(t.ID, t.Limits.Runners)
	s.applyState(t.ID, b, log)
	return b
}

// tenantReaperOptions are the reaper's for a tenant: an orphan of a limited
// tenant, after a restart, ends by its limit, and a suspended tenant's at
// once.
func (s *shard) tenantReaperOptions(t tenant, listeners []*listener.ScaleSet, log *slog.Logger) ec2.ReaperOptions {
	ro := reaperOptions(s.cfg, t.Owner, t.Tiers, listeners, log)
	ro.MaxLifetime = maxLifetime(ro.MaxLifetime, t.Limits.JobDuration)
	ro.Suspended = s.suspension(t.ID)
	return ro
}

// stateKey is a tenant's id in the tenant states: the install's own is
// tenantstate.Install.
func stateKey(id string) string {
	if id == "" {
		return tenantstate.Install
	}
	return id
}

// applyState suspends, holds or resumes a tenant's bound by the states and
// entitlements read, first match wins:
//
//   - suspended while it has a suspension record (--tenant-state);
//   - held, for a tenant of the catalog, until a read of the states
//     succeeds, so a restart never serves a tenant whose suspension it
//     could not read;
//   - held while its entitlement says so (--entitlements; see
//     entitlement.Decide), a tenant of the catalog only;
//   - active otherwise.
//
// The install is never held: it is its operator's. A tenant's entitlement
// also sets its plan's limits, whatever else holds it, so they are in force
// the moment it resumes; after any hold, since a plan wakes the tenant's
// schedulers, and one woken before the hold could launch.
//
// A hold refuses new launches only: a launch already granted runs its job.
func (s *shard) applyState(id string, bound *scheduler.Tenant, log *slog.Logger) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	var ent *entitlement.Decision
	setPlan := func() {}
	if s.entitlements != nil && id != "" {
		r, _, d := s.entitlements.Decide(id)
		ent = &d
		l := r.PlanLimits()
		setPlan = func() {
			if bound.SetPlan(scheduler.Plan{Runners: l.MaxRunners, Tiers: l.Tiers}) {
				log.Info("tenant's plan limits changed", "tenant", id, "max_runners", l.MaxRunners, "tiers", l.Tiers, "in_force", bound.Max())
			}
		}
	}
	if s.states != nil {
		states, known := s.states.States()
		if r, suspended := states[stateKey(id)]; suspended {
			if bound.Suspend(r.Reason) {
				log.Warn("tenant suspended: holding its launches and ending its runners", "tenant", stateKey(id), "reason", r.Reason, "since", r.Since)
			}
			setPlan()
			return
		}
		if !known && id != "" {
			if bound.Hold("its state has not been read yet") {
				log.Warn("tenant held until its state is read", "tenant", id, "store", s.states.Status().Store)
			}
			setPlan()
			return
		}
	}
	if ent != nil && ent.Hold {
		if bound.Hold(ent.Why) {
			log.Warn("tenant held by its entitlement: its jobs wait", "tenant", id, "why", ent.Why)
		}
		setPlan()
		return
	}
	// Not held from here on, unless it already was: Resume wakes the
	// schedulers then, and the plan does now.
	setPlan()
	if bound.Resume() {
		log.Info("tenant resumed", "tenant", stateKey(id))
	}
}

// suspension returns why tenant id is suspended, or "", by the latest
// states read.
func (s *shard) suspension(id string) func() string {
	return func() string {
		if s.states == nil {
			return ""
		}
		states, _ := s.states.States()
		return states[stateKey(id)].Reason
	}
}

// applyOnAdd has live judge each tenant as it is served, by the states and
// entitlements read so far: a change read while a tenant starts is no
// change by the time it is served. Both watchers set the same hook.
func (s *shard) applyOnAdd(live *liveTenants) {
	live.mu.Lock()
	live.onAdd = func(st *servedTenant) { s.applyState(st.t.ID, st.bound, s.log.With("component", "tenant-state")) }
	live.mu.Unlock()
}

// entitlementHealth sets each tenant's entitlement in the health file's
// tenants, and how reading them goes; nothing when they are off.
func (s *shard) entitlementHealth(f *health.File) {
	if s.entitlements == nil {
		return
	}
	st := s.entitlements.Status()
	f.Entitlements = &health.TenantStates{Store: st.Store, LastRead: st.LastRead, LastError: st.LastError, LastErrorAt: st.LastErrorAt}
	for i := range f.Tenants {
		if f.Tenants[i].ID != "" {
			e := healthEntitlement(s.entitlements.Decide(f.Tenants[i].ID))
			fo := s.entitlements.FailOpen(f.Tenants[i].ID)
			e.FailOpenSince, e.FailOpens = fo.Since, fo.Count
			f.Tenants[i].Entitlement = e
		}
	}
}

// healthEntitlement is a tenant's entitlement record and the decision on it,
// for the health file.
func healthEntitlement(r entitlement.Record, ok bool, d entitlement.Decision) *health.Entitlement {
	l := r.PlanLimits()
	return &health.Entitlement{None: !ok, Invalid: r.Invalid, Plan: r.Plan, Account: string(r.Account), Budget: string(r.Budget), ValidUntil: r.ValidUntil,
		WrittenAt: r.Written, MaxRunners: l.MaxRunners, Tiers: l.Tiers,
		Held: d.Hold, FailOpen: d.FailOpen, Warning: d.Warning, Why: d.Why}
}

// health is what the health file says of the tenant, with its state.
func (st *servedTenant) health(states tenantstate.States) health.Tenant {
	h := tenantHealth(st.t)
	h.Running = st.bound.Running()
	// The tenants list may have changed it since the tenant started.
	h.Limits.Runners = st.bound.Max()
	suspended, why := st.bound.State()
	switch {
	case suspended:
		r := states[stateKey(st.t.ID)]
		h.State, h.Reason, h.Since = "suspended", r.Reason, r.Since
	case why != "":
		h.State, h.Reason = "held", why
	}
	return h
}

// tenantHealth is a tenant's limits for the health file.
func tenantHealth(t tenant) health.Tenant {
	return health.Tenant{
		ID: t.ID, Refused: t.Refused,
		Limits: health.Limits{Runners: t.Limits.Runners, VCPU: t.Limits.VCPU, JobDuration: t.Limits.JobDuration.String()},
	}
}

// names returns each tier's shard-wide name, in runs' order.
func (st *servedTenant) names() []string {
	var out []string
	for _, tier := range st.tiers {
		out = append(out, st.t.tierKey(tier.Name))
	}
	return out
}

// liveTenants is what the health file reports: the tenants being served, and
// for each that is not, why.
type liveTenants struct {
	mu      sync.Mutex
	served  []*servedTenant
	waiting map[string]*health.Tracker
	// restarts is how each tenant's loop has stopped it and tried again.
	restarts map[string]*health.Restarts
	// skipped are tenants never started: no tier is within their limits.
	skipped []health.Tenant
	// onAdd, when set, is called with each tenant once it is served: the
	// state read while it was starting reaches it here, as a change applies
	// only to the tenants served.
	onAdd func(*servedTenant)
}

// skip records a tenant that is not served, and why.
func (l *liveTenants) skip(t tenant, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := tenantHealth(t)
	h.NotServed = why
	l.skipped = append(l.skipped, h)
}

// tenants reports the tenants, their limits, states and flags, the install's
// own first; nil when the install serves only itself, with no limits, and is
// neither suspended nor flagged.
func (l *liveTenants) tenants(states tenantstate.States, flags tenantstate.Flags) []health.Tenant {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []health.Tenant
	for _, st := range l.served {
		out = append(out, st.health(states))
	}
	out = append(out, l.skipped...)
	served := map[string]bool{}
	for _, h := range out {
		served[h.ID] = true
	}
	// A tenant between tries is listed too, so its restarts show.
	for _, id := range slices.Sorted(maps.Keys(l.waiting)) {
		if !served[id] {
			out = append(out, health.Tenant{ID: id, NotServed: "starting again after it stopped"})
		}
	}
	for i := range out {
		if r, ok := l.restarts[out[i].ID]; ok {
			c := *r
			out[i].Restarts = &c
		}
	}
	healthFlag := func(fl tenantstate.Flag) *health.Flag {
		return &health.Flag{Type: fl.Type, Severity: fl.Severity, Tier: fl.Tier, Instance: fl.Instance,
			Finding: fl.Finding, Count: fl.Count, First: fl.First, Last: fl.Last}
	}
	listed := map[string]bool{}
	for i := range out {
		listed[stateKey(out[i].ID)] = true
		if fl, ok := flags[stateKey(out[i].ID)]; ok {
			out[i].Flag = healthFlag(fl)
		}
	}
	// A flag on a tenant not served now (starting, or out of the catalog)
	// still shows, so doctor never passes while one is open.
	for _, id := range flags.IDs() {
		if !listed[id] {
			hid := id
			if id == tenantstate.Install {
				hid = ""
			}
			out = append(out, health.Tenant{ID: hid, NotServed: "not served by this daemon now", Flag: healthFlag(flags[id])})
		}
	}
	slices.SortStableFunc(out, func(a, b health.Tenant) int { return strings.Compare(a.ID, b.ID) })
	if len(out) == 1 && out[0].ID == "" && out[0].Limits == (health.Limits{JobDuration: "0s"}) && len(out[0].Refused) == 0 && out[0].State == "" && out[0].Flag == nil {
		return nil
	}
	return out
}

// bounds returns each served tenant's id and bound.
func (l *liveTenants) bounds() map[string]*scheduler.Tenant {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]*scheduler.Tenant{}
	for _, st := range l.served {
		out[st.t.ID] = st.bound
	}
	return out
}

func (l *liveTenants) add(st *servedTenant) {
	l.mu.Lock()
	l.served = append(l.served, st)
	delete(l.waiting, st.t.ID)
	onAdd := l.onAdd
	l.mu.Unlock()
	if onAdd != nil {
		onAdd(st)
	}
}

func (l *liveTenants) remove(st *servedTenant) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.served = slices.DeleteFunc(l.served, func(x *servedTenant) bool { return x == st })
}

// forget drops what it says of a tenant not served, once its loop has
// ended: waiting for a retry, or skipped.
func (l *liveTenants) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.waiting, id)
	delete(l.restarts, id)
	l.skipped = slices.DeleteFunc(l.skipped, func(h health.Tenant) bool { return h.ID == id })
}

// wait records that a tenant is down until its next try, and counts it.
func (l *liveTenants) wait(id string, err error, backoff time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restarts == nil {
		l.restarts = map[string]*health.Restarts{}
	}
	r := l.restarts[id]
	if r == nil {
		r = &health.Restarts{}
		l.restarts[id] = r
	}
	r.Count++
	if errors.Is(err, errPanicked) {
		r.Panics++
	}
	r.LastError, r.LastAt = fmt.Sprint(err), time.Now()
	if l.waiting == nil {
		l.waiting = map[string]*health.Tracker{}
	}
	tr, ok := l.waiting[id]
	if !ok {
		tr = health.NewTracker(id)
		l.waiting[id] = tr
	}
	tr.Reconnecting(err, backoff)
}

func (l *liveTenants) trackers() []*health.Tracker {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*health.Tracker
	for _, st := range l.served {
		out = append(out, st.trackers...)
	}
	for _, id := range slices.Sorted(maps.Keys(l.waiting)) {
		out = append(out, l.waiting[id])
	}
	return out
}

// reaper adds up the served tenants' latest sweeps.
func (l *liveTenants) reaper() *health.Reaper {
	l.mu.Lock()
	defer l.mu.Unlock()
	var sum *health.Reaper
	for _, st := range l.served {
		if st.reaperState == nil {
			continue
		}
		r := st.reaperState()
		if r == nil {
			continue
		}
		if sum == nil {
			c := *r
			sum = &c // appended to below, never to the reaper's own slices
			sum.Instances = slices.Clone(r.Instances)
			sum.SpotRequests = slices.Clone(r.SpotRequests)
			sum.Busy = slices.Clone(r.Busy)
			sum.Young = slices.Clone(r.Young)
			sum.Unproven = slices.Clone(r.Unproven)
			continue
		}
		sum.Instances = append(sum.Instances, r.Instances...)
		sum.SpotRequests = append(sum.SpotRequests, r.SpotRequests...)
		sum.Busy = append(sum.Busy, r.Busy...)
		sum.Young = append(sum.Young, r.Young...)
		sum.Unproven = append(sum.Unproven, r.Unproven...)
		if r.At.After(sum.At) {
			sum.At = r.At
		}
		if sum.Error == "" {
			sum.Error = r.Error
		}
	}
	return sum
}

// A tenant that stops is tried again after these, doubling.
const (
	tenantRetryMin = 30 * time.Second
	tenantRetryMax = 5 * time.Minute
)

// tenantLoop keeps one tenant running for as long as its context lives. What
// goes wrong with it stays with it: a session it must register again, an App
// since removed, a start that fails. Its goroutines end with each try.
type tenantLoop struct {
	id    string
	start func(ctx context.Context) (*servedTenant, error)
	serve func(ctx context.Context, cancel context.CancelFunc, st *servedTenant, onPanic func()) error
	// stopLimit bounds its stop after a recovered panic (guardAfterPanic).
	stopLimit time.Duration
	// stop is given the loop's context, whose cause says why it ended.
	stop func(ctx context.Context, st *servedTenant)
	// served, when set, is called with each start once it is served.
	served   func(st *servedTenant)
	live     *liveTenants
	log      *slog.Logger
	min, max time.Duration
}

func (l *tenantLoop) run(ctx context.Context) {
	wait := l.min
	for ctx.Err() == nil {
		err := l.try(ctx, &wait)
		if ctx.Err() != nil {
			return
		}
		l.log.Error("tenant stopped; trying again", "tenant", l.id, "err", err, "in", wait)
		l.live.wait(l.id, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, l.max)
	}
}

// try serves the tenant until it stops, and resets the wait if it served for
// longer than the longest one. A panic in its start, serve or stop ends the
// try as an error, as any other failure does, and stop still runs: one
// tenant's bug starts that tenant again, not the daemon. From the panic on,
// the tenant is out of the health file, which must not wait on a lock the
// panic may have left held, and its stop has stopLimit to end before the
// daemon exits for a restart (guardAfterPanic).
func (l *tenantLoop) try(ctx context.Context, wait *time.Duration) (err error) {
	settled := make(chan struct{})
	defer close(settled)
	var st *servedTenant
	onPanic := sync.OnceFunc(func() {
		if st != nil {
			l.live.remove(st)
		}
		_ = guardAfterPanic(settled, l.stopLimit, fmt.Sprintf("tenant %q", l.id), l.log)
	})
	protect := func(what string, f func() error) (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = recovered(p, fmt.Sprintf("tenant %q %s", l.id, what), l.log)
				onPanic()
			}
		}()
		return f()
	}
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := protect("start", func() (err error) { st, err = l.start(tctx); return err }); err != nil {
		return err
	}
	began := time.Now()
	// Removed whatever happens from here: adding it may panic (onAdd).
	defer l.live.remove(st)
	err = protect("serve", func() error {
		l.live.add(st)
		if l.served != nil {
			l.served(st)
		}
		return l.serve(tctx, cancel, st, onPanic)
	})
	cancel()
	if perr := protect("stop", func() error { l.stop(ctx, st); return nil }); err == nil {
		err = perr
	}
	if time.Since(began) > l.max {
		*wait = l.min
	}
	if err == nil {
		err = errors.New("its tiers stopped")
	}
	return err
}

// panicStopLimit is how long a tenant's stop may take after a recovered
// panic: a drain waits for jobs no longer than their limit and its slack.
// With no job limit, the slack alone: a drain that waits longer may be
// waiting on a lock the panic left held.
func panicStopLimit(job time.Duration) time.Duration {
	return max(job, 0) + jobLifetimeSlack
}

// closeSessions closes the tenant's scale set sessions and keeps its scale
// sets: it is handed off.
func (st *servedTenant) closeSessions(ctx context.Context) {
	for _, l := range st.listeners {
		l.CloseSession(ctx)
	}
}

// close closes the tenant's scale set sessions.
func (st *servedTenant) close(ctx context.Context) {
	for _, l := range st.listeners {
		_ = l.Close(ctx)
	}
}

// remove closes the tenant's sessions and deletes its scale sets: the
// tenants list no longer names it.
func (st *servedTenant) remove(ctx context.Context) {
	for i, l := range st.listeners {
		if err := l.Remove(ctx); err != nil {
			st.log.Warn("scale set of a removed tenant left registered", "tier", st.tiers[i].Name, "err", err)
		}
	}
}

// logTiers says what each tier runs on.
func (st *servedTenant) logTiers() {
	log := st.log
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
func (st *servedTenant) runs(cfg *config.Config) []func(context.Context) error {
	log := st.log
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

// tenantStop is how a tenant's run ends: whether it closes only its
// sessions, before its runners finish, rather than closing its scale sets
// after; leaves its busy EC2 runners to the next process or shard; and
// deletes its scale sets.
type tenantStop struct{ sessionsFirst, handOff, remove bool }

// stopPlan is how a tenant whose run's context has ended stops, by why it
// ended. A tenant handed off to another shard closes its sessions first, so
// that shard opens them at once rather than after GitHub's hold, and never
// deletes a scale set: the next shard may be serving it already. A removed
// one deletes them once its runners have finished, unless its lease has
// lapsed meanwhile: another shard may be serving its account, under the
// same scale set names.
func (s *shard) stopPlan(tenantCtx context.Context) tenantStop {
	return tenantStop{
		sessionsFirst: handedOffTenant(tenantCtx),
		handOff:       s.handsOff(tenantCtx),
		remove:        removedTenant(tenantCtx) && !leaseLapsed(tenantCtx),
	}
}

// removesNow reports whether a tenant that stops as removed deletes its
// scale sets now, once its runners have finished: not if its lease lapsed
// meanwhile, as another shard may serve the account under those names.
func (p tenantStop) removesNow(tenantCtx context.Context) bool {
	return p.remove && !leaseLapsed(tenantCtx)
}

// handsOff says whether a tenant stopping now leaves its busy EC2 runners
// to the next process or shard instead of waiting for their jobs: only when
// the process is exiting or the tenant is handed off, the tenant is not
// being removed (nothing would serve it next), and the next one's reaper
// ends what it finds. That takes --owner: without it, the close after the
// drain deletes the scale set, and its runners with it, so the next reaper
// would end a handed-off job as an idle orphan; and the owner tag is the
// host name, which the next host's reaper does not look for. A tenant handed
// off without these drains as one changed does, its sessions held until its
// runners finish.
func (s *shard) handsOff(tenantCtx context.Context) bool {
	if removedTenant(tenantCtx) || s.ec2Client == nil || s.cfg.EC2Reaper != "on" || s.cfg.Owner == "" {
		return false
	}
	return handedOffTenant(tenantCtx) || (s.exiting != nil && s.exiting.Err() != nil)
}

// drain waits for the tenant's runners to finish. With handOff, its EC2
// tiers' busy runners are left to run on instead: their instances outlive
// the process, and the next one's reaper ends each once its job is done.
// A Firecracker microVM ends with the process, so its job is waited for.
func (st *servedTenant) drain(handOff bool) {
	if handOff {
		for i, sched := range st.scheds {
			if !st.tiers[i].IsEC2() {
				continue
			}
			if n := sched.HandOff(); n > 0 {
				st.log.Info("leaving busy runners to the next firerunner", "tier", st.tiers[i].Name, "runners", n)
			}
		}
	}
	for _, sched := range st.scheds {
		sched.Drain()
	}
}
