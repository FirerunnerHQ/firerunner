package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	KeepOnClose bool
	Token       string
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
	// applyMu makes each applyState decide and apply as one step: the
	// watchers and the tenant loops call it at once, and a decision taken
	// on older inputs must not land after one taken on newer.
	applyMu sync.Mutex
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
		MaxJob:      t.Limits.JobDuration,
		OnHold:      tracker.SetHeld,
		OnStop:      s.stopRecorder(tier.Name, tracker),
	}
	if jit != nil {
		o.JIT, o.Remover = jit, jit
	}
	return o
}

// heldBy asks the tier's scheduler, once there is one, how many launches
// its tenant holds back, and why.
func heldBy(sched func() *scheduler.Scheduler) func() (int, string) {
	return func() (int, string) {
		if s := sched(); s != nil {
			return s.Held()
		}
		return 0, ""
	}
}

// listenerConfig is what a tier's scale set listener gets from its tenant.
// held says what the tier's tenant holds back, for the jobs a message
// assigns.
func (s *shard) listenerConfig(t tenant, tier config.Tier, hooks tenantHooks, tracker *health.Tracker, jobs *assigned.Jobs, log *slog.Logger, held func() (int, string)) listener.Config {
	return listener.Config{
		Held:         held,
		URL:          t.URL,
		OnJobStarted: hooks.onJobStarted,
		OnJobOffered: hooks.onJobOffered,
		OnRunnerID:   hooks.onRunnerID,
		Events:       s.events,
		Health:       tracker,
		Pending:      jobs,
		Name:         tier.Name,
		RunnerGroup:  t.RunnerGroup,
		Labels:       tier.Labels,
		MaxRunners:   tier.Max,
		// A named install outlives its process: keep its scale set across
		// restarts and control VM replacements.
		KeepOnClose: t.KeepOnClose,
		Token:       t.Token,
		App:         t.Auth,
		Version:     version,
		Logger:      log,
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
		Events:      s.events,
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
func (s *shard) stopRecorder(tier string, tracker *health.Tracker) func(scheduler.Stop) {
	return func(st scheduler.Stop) {
		tracker.Stopped(st.Kind)
		jobevent.Record(s.events, jobevent.Event{Kind: jobevent.Stopped, Tier: tier, Runner: st.Runner, Reason: st.Reason})
	}
}

// extraTenant is a tenant from the --tenants file, on the install's App.
func extraTenant(cfg *config.Config, c config.Tenant, owner string, jwt *ghauth.JWT) tenant {
	group := c.RunnerGroup
	if group == "" {
		group = cfg.RunnerGroup
	}
	tiers, refused := cfg.TenantTiers(c)
	return tenant{
		ID: c.ID, Owner: owner + "/" + c.ID, URL: c.URL, RunnerGroup: group, KeepOnClose: cfg.Owner != "",
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
		serve: func(ctx context.Context, cancel context.CancelFunc, st *servedTenant) error {
			return serveTiers(ctx, cancel, st.names(), st.runs(s.cfg), st.log)
		},
		stop: func(ctx context.Context, st *servedTenant) {
			st.drain()
			if removedTenant(ctx) {
				// Bounded: a GitHub that hangs must not hold up shutdown.
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tenantRemoveTimeout)
				defer cancel()
				st.remove(rctx)
				return
			}
			st.close(context.WithoutCancel(ctx))
		},
		live: live, log: s.log, min: tenantRetryMin, max: tenantRetryMax,
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
func (s *shard) start(ctx context.Context, t tenant, hooks tenantHooks) (*servedTenant, error) {
	cfg, log := s.cfg, s.log
	if t.ID != "" {
		log = log.With("tenant", t.ID)
	}
	st := &servedTenant{t: t, tiers: t.Tiers, log: log, bound: s.newBound(t, log)}

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
	var newEC2 func(config.Tier) provisioner.Provisioner
	if s.ec2Client != nil {
		newEC2 = func(tier config.Tier) provisioner.Provisioner {
			o := s.ec2Options(t, tier, hooks, tierHealth[tier.Name], tierJobs[tier.Name])
			sched := tierScheds[tier.Name]
			o.OnRunnerJob = alsoMarkBusy(o.OnRunnerJob, sched.Load)
			p := s.ec2Client.Tier(o)
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
// The install is never held: it is its operator's.
//
// A hold refuses new launches only: a launch already granted runs its job.
func (s *shard) applyState(id string, bound *scheduler.Tenant, log *slog.Logger) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if s.states != nil {
		states, known := s.states.States()
		if r, suspended := states[stateKey(id)]; suspended {
			if bound.Suspend(r.Reason) {
				log.Warn("tenant suspended: holding its launches and ending its runners", "tenant", stateKey(id), "reason", r.Reason, "since", r.Since)
			}
			return
		}
		if !known && id != "" {
			if bound.Hold("its state has not been read yet") {
				log.Warn("tenant held until its state is read", "tenant", id, "store", s.states.Status().Store)
			}
			return
		}
	}
	if s.entitlements != nil && id != "" {
		if _, _, d := s.entitlements.Decide(id); d.Hold {
			if bound.Hold(d.Why) {
				log.Warn("tenant held by its entitlement: its jobs wait", "tenant", id, "why", d.Why)
			}
			return
		}
	}
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
			f.Tenants[i].Entitlement = healthEntitlement(s.entitlements.Decide(f.Tenants[i].ID))
		}
	}
}

// healthEntitlement is a tenant's entitlement record and the decision on it,
// for the health file.
func healthEntitlement(r entitlement.Record, ok bool, d entitlement.Decision) *health.Entitlement {
	return &health.Entitlement{None: !ok, Invalid: r.Invalid, Plan: r.Plan, Account: string(r.Account), Budget: string(r.Budget), ValidUntil: r.ValidUntil,
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
	l.skipped = slices.DeleteFunc(l.skipped, func(h health.Tenant) bool { return h.ID == id })
}

// wait records that a tenant is down until its next try.
func (l *liveTenants) wait(id string, err error, backoff time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	serve func(ctx context.Context, cancel context.CancelFunc, st *servedTenant) error
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
// longer than the longest one.
func (l *tenantLoop) try(ctx context.Context, wait *time.Duration) error {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := l.start(tctx)
	if err != nil {
		return err
	}
	began := time.Now()
	l.live.add(st)
	defer l.live.remove(st)
	if l.served != nil {
		l.served(st)
	}
	err = l.serve(tctx, cancel, st)
	cancel()
	l.stop(ctx, st)
	if time.Since(began) > l.max {
		*wait = l.min
	}
	if err == nil {
		err = errors.New("its tiers stopped")
	}
	return err
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

// drain waits for the tenant's runners to finish.
func (st *servedTenant) drain() {
	for _, sched := range st.scheds {
		sched.Drain()
	}
}
