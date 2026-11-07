package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/cachetls"
	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
	"github.com/firerunnerhq/firerunner/internal/jobevent"
)

// ErrNoCapacity reports that every instance type in every subnet, in every
// market the tier allows, was out of capacity.
var ErrNoCapacity = errors.New("no EC2 capacity for any configured instance type and subnet")

// ErrRunnerFailed reports that the runner on an instance exited with a
// non-zero status, or could not be started, by the instance's own report.
var ErrRunnerFailed = errors.New("runner failed on the instance")

// capacityCodes are RunInstances errors that mean "not here, try the next
// type or subnet" rather than "this launch is broken".
var capacityCodes = []string{
	"InsufficientInstanceCapacity",      // no capacity for the type in this AZ
	"InsufficientFreeAddressesInSubnet", // subnet out of IPs
	"Unsupported",                       // type (or spot for it) not offered in this AZ
}

// Defaults for Client tuning knobs.
const (
	DefaultPollInterval = 3 * time.Second
	// terminateWait bounds how long Launch keeps asking for a termination it
	// cannot see take effect before it gives up and reports the instance
	// for manual cleanup.
	terminateWait = 5 * time.Minute
	// terminateRetry paces repeated termination requests.
	terminateRetry = 10 * time.Second
)

// Client holds what every EC2 tier in a process shares: the AWS clients, the
// AMI cache and the instance-state watcher.
type Client struct {
	api      API
	amis     *amiResolver
	watch    *watcher
	breakers *breakers
	log      *slog.Logger

	// live holds the instances some Launch in this process is responsible
	// for; the reaper leaves them alone.
	liveMu sync.Mutex
	live   map[string]bool
}

func (c *Client) track(id string) {
	c.liveMu.Lock()
	c.live[id] = true
	c.liveMu.Unlock()
}

func (c *Client) untrack(id string) {
	c.liveMu.Lock()
	delete(c.live, id)
	c.liveMu.Unlock()
}

func (c *Client) isLive(id string) bool {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	return c.live[id]
}

// NewClient returns a Client. params may be nil when no tier names its AMI by
// SSM parameter. pollInterval <= 0 means DefaultPollInterval.
func NewClient(api API, params ParamAPI, pollInterval time.Duration, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if pollInterval <= 0 {
		pollInterval = DefaultPollInterval
	}
	return &Client{api: api, amis: newAMIResolver(api, params), watch: newWatcher(api, pollInterval, log), breakers: newBreakers(), log: log, live: map[string]bool{}}
}

// Options configures one EC2 tier's provisioner.
type Options struct {
	// Tier is the tier name, tagged on every instance.
	Tier string
	// Owner identifies this firerunner install, so cleanup of leftover
	// instances only ever touches what it launched.
	Owner string
	EC2   config.EC2Tier
	// JIT serves instances their configs; JITURL is its endpoint as instances
	// reach it, written into user-data.
	JIT    *jitserver.Server
	JITURL string
	// MaxLifetime terminates an instance still alive after this long; 0
	// disables the backstop.
	MaxLifetime time.Duration
	// Pooled keeps a pool even while EC2.Pool is 0, for a tier whose
	// schedule sizes one later (see Pool.SetTarget).
	Pooled bool
	// UsePool, when set, decides per launch whether it may take a pool
	// instance; false cold-launches and leaves the pool for the jobs
	// ec2.pool_for names. release is called when the launch did not start
	// a pool instance after all. Nil lets every launch take the pool.
	UsePool func() (ok bool, release func())
	// Events, when set, is told of each instance and how its runner went.
	Events jobevent.Sink
	// OnRunnerJob, when set, is told when an instance's runner reports its
	// job, within seconds of it starting.
	OnRunnerJob func(runner string)
	// Launched, when set, is told each launch's path (PathPool, PathCold or
	// PathKept).
	Launched func(path string)
	// Cache, when set and the tier sets ec2.cache, gives each instance a
	// cache bundle and binds its address to its runner's job for
	// cache-server's scope checks.
	Cache *CacheOptions
	// Interrupted, when set, is told each spot instance AWS took back while
	// it ran a runner; duringJob says whether its job had started.
	Interrupted func(duringJob bool)
	// FellBack, when set, is told each new instance that EC2 placed on
	// other than the tier's first choice: FallbackMarket, FallbackType or
	// FallbackSubnet, the first that differs.
	FellBack func(kind string)
	Logger   *slog.Logger
}

// Launch paths, as Options.Launched reports them.
const (
	// PathPool started a stopped pool instance.
	PathPool = "pool"
	// PathCold launched a new instance: the tier keeps no pool, the pool was
	// empty, or its instance would not start.
	PathCold = "cold"
	// PathKept launched a new instance although the pool had one ready,
	// leaving it for the jobs ec2.pool_for names.
	PathKept = "kept"
)

// CacheOptions are what an EC2 tier's cache proxy needs from the control VM.
type CacheOptions struct {
	// CA issues each instance's leaf for the results host.
	CA *cachetls.CA
	// GitCA issues each instance's leaf for github.com, for a tier with a git
	// cache.
	GitCA *cachetls.CA
	// Upstream is cache-server's host:port as instances reach it.
	Upstream string
	// Jobs binds an instance's address to its runner, as cache-server reads
	// it; nil leaves cache-server to deny every request.
	Jobs VMJobs
	// LeafTTL is how long a leaf lives: the instance's whole life, from its
	// registration through boot and its job.
	LeafTTL time.Duration
}

// VMJobs binds instance addresses to the runners on them (jobidentity's
// Registry), so cache-server knows whose job a request comes from.
type VMJobs interface {
	Register(runner, ip string) error
	Release(runner string) error
}

// register hands the instance at ip its JIT config and, for a cached tier,
// its cache bundle, and binds the address to the runner. No bundle leaves the
// job uncached. A binding that fails fails the launch: cache-server knows a
// job by its address alone, and the failure may be another live job holding
// this address, whose scope the instance would then act in. release undoes
// all of it.
func (p *Provisioner) register(ip netip.Addr, name, jit string) (release func(), err error) {
	c := p.o.Cache
	if c == nil || !p.o.EC2.Cache {
		if err := p.o.JIT.Register(ip, name, jit, p.o.OnRunnerJob); err != nil {
			return nil, err
		}
		return func() { p.o.JIT.Revoke(ip) }, nil
	}
	var git *cachetls.CA
	if p.o.EC2.GitCache {
		git = c.GitCA
	}
	bundle, err := c.CA.Bundle(c.LeafTTL, c.Upstream, git)
	if err != nil {
		p.log.Warn("no cache bundle; the job runs without the cache", "runner", name, "err", err)
		if err := p.o.JIT.Register(ip, name, jit, p.o.OnRunnerJob); err != nil {
			return nil, err
		}
		return func() { p.o.JIT.Revoke(ip) }, nil
	}
	if err := p.o.JIT.RegisterWithCache(ip, name, jit, bundle, p.o.OnRunnerJob); err != nil {
		return nil, err
	}
	if c.Jobs == nil {
		return func() { p.o.JIT.Revoke(ip) }, nil
	}
	if err := c.Jobs.Register(name, ip.String()); err != nil {
		p.o.JIT.Revoke(ip)
		return nil, fmt.Errorf("bind cache identity for %s: %w", ip, err)
	}
	return func() {
		p.o.JIT.Revoke(ip)
		if err := c.Jobs.Release(name); err != nil {
			p.log.Warn("release cache identity", "runner", name, "err", err)
		}
	}, nil
}

// Fallbacks, as Options.FellBack reports them.
const (
	// FallbackMarket: the tier's first market (spot, for
	// spot-then-on-demand) had nothing to give.
	FallbackMarket = "market"
	// FallbackType: a later instance type in the tier's list.
	FallbackType = "type"
	// FallbackSubnet: the first type, in a later subnet.
	FallbackSubnet = "subnet"
)

// Provisioner launches one EC2 instance per runner for one tier.
type Provisioner struct {
	c *Client
	// pool, when the tier keeps one, supplies pre-warmed instances; Launch
	// cold-launches when it is empty.
	pool *Pool
	o    Options
	log  *slog.Logger
	now  func() time.Time
	tick time.Duration // how often Launch re-checks the JIT handoff and lifetime
	// termWait and termRetry are terminateWait and terminateRetry, shortened
	// by tests.
	termWait, termRetry time.Duration
}

// Tier returns the provisioner for one EC2 tier.
func (c *Client) Tier(o Options) *Provisioner {
	log := o.Logger
	if log == nil {
		log = c.log
	}
	p := &Provisioner{
		c: c, o: o, log: log.With("provisioner", "ec2"), now: time.Now, tick: time.Second,
		termWait: terminateWait, termRetry: terminateRetry,
	}
	if o.EC2.Pool > 0 || o.Pooled {
		p.pool = newPool(p, o.EC2.Pool)
	}
	return p
}

// Pool returns the tier's stopped pool, or nil when it keeps none. Its Run
// must be started for the pool to fill.
func (p *Provisioner) Pool() *Pool { return p.pool }

// Name implements provisioner.Provisioner.
func (p *Provisioner) Name() string { return "ec2" }

// Launch implements provisioner.Provisioner: it starts one instance for the
// runner, hands it jit over the VPC, and blocks until the instance is shutting
// down or terminated.
//
// Cancelling ctx means the scheduler is removing an idle runner, so the
// instance is terminated. Busy detection does not come from here: the listener
// marks the runner busy from GitHub's JobStarted message, so onBusy is unused.
func (p *Provisioner) Launch(ctx context.Context, name, jit string, _ core.RunnerSpec, _ func()) error {
	path := PathCold
	if p.pool != nil {
		use, release := true, func() {}
		if p.o.UsePool != nil {
			// With a policy, a pool with nothing ready is not waited on:
			// the launch goes cold without spending a claim, and one that
			// refills meanwhile stays kept.
			if ready, _ := p.pool.Ready(); ready == 0 {
				use = false
			} else if use, release = p.o.UsePool(); !use {
				path = PathKept
				p.log.Info("no pending job needs the pool; cold-launching", "runner", name)
			}
		}
		if use {
			started := false
			err := p.launchPooled(ctx, name, jit, func() { started = true; p.launched(PathPool) })
			if !started {
				release() // the claim was not spent
			}
			if !errors.Is(err, errPoolEmpty) && !errors.Is(err, errPoolStart) {
				return err
			}
			if errors.Is(err, errPoolStart) {
				p.log.Warn("pool instance unavailable; cold-launching instead", "runner", name, "err", err)
			}
		}
	}
	entered := time.Now()
	img, err := p.c.amis.resolve(ctx, p.o.EC2.AMI)
	if err != nil {
		return err
	}
	resolved := time.Now()
	inst, err := p.run(ctx, name, img, forJob)
	if err != nil {
		return err
	}
	p.launched(path)
	id := aws.ToString(inst.InstanceId)
	p.c.track(id)
	defer p.c.untrack(id)
	log := p.log.With("runner", name, "instance", id, "type", inst.InstanceType, "subnet", aws.ToString(inst.SubnetId))
	log.Info("instance launched",
		"ami", resolved.Sub(entered).Round(time.Millisecond),
		"runInstances", time.Since(resolved).Round(time.Millisecond))

	states, stopWatch := p.c.watch.watch(id)
	defer stopWatch()
	w := &waiter{p: p, id: id, runner: name, log: log, states: states,
		itype: string(inst.InstanceType), subnet: aws.ToString(inst.SubnetId), az: placementAZ(inst),
		spot: inst.InstanceLifecycle == types.InstanceLifecycleTypeSpot}
	w.record(jobevent.Event{Kind: jobevent.Launched, Path: path, At: resolved})

	ip, err := p.privateIP(ctx, inst)
	if err != nil {
		return w.abort(ctx, "no private IP", err)
	}
	release, err := p.register(ip, name, jit)
	if err != nil {
		return w.abort(ctx, "jit registration failed", fmt.Errorf("register jit config for %s at %s: %w", id, ip, err))
	}
	defer release()
	return w.run(ctx, ip)
}

func (p *Provisioner) launched(path string) {
	if p.o.Launched != nil {
		p.o.Launched(path)
	}
}

// errPoolStart reports that a claimed pool instance failed to start; the
// launch falls back to a cold instance.
var errPoolStart = errors.New("pool instance did not start")

// launchPooled runs the runner on a ready pool instance: claim it, register
// its config, start it, then wait as for any job instance. It returns
// errPoolEmpty when no instance is ready.
func (p *Provisioner) launchPooled(ctx context.Context, name, jit string, onStart func()) error {
	entered := time.Now()
	in, err := p.pool.claim(ctx, name)
	switch {
	case errors.Is(err, errPoolEmpty), err != nil && ctx.Err() != nil:
		return err
	case err != nil:
		// A pool-wide failure (permissions, throttling): this job cold-
		// launches; the instance went back to the pool.
		return fmt.Errorf("%w: claim: %w", errPoolStart, err)
	}
	defer p.c.untrack(in.id) // tracked by claim
	log := p.log.With("runner", name, "instance", in.id, "type", in.itype, "pooled", true)
	ip, err := netip.ParseAddr(in.ip)
	if err != nil {
		p.pool.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
		return fmt.Errorf("%w: pool instance %s private IP %q: %w", errPoolStart, in.id, in.ip, err)
	}
	// Register before the start, so the boot script's first fetch succeeds.
	release, err := p.register(ip, name, jit)
	if err != nil {
		p.pool.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
		return fmt.Errorf("register jit config for %s at %s: %w", in.id, ip, err)
	}
	defer release()
	claimed := time.Now()
	if _, err := p.c.api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{in.id}}); err != nil {
		// A stopped spot instance can lack capacity to start again.
		p.pool.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
		return fmt.Errorf("%w: start %s: %w", errPoolStart, in.id, err)
	}
	onStart()
	started := time.Now()
	// Now that it runs, release the persistent request: a terminate later
	// must not bring a replacement. (Cancelling it before the start would
	// make the start fail.) If this fails, terminate retries it first.
	spotReq := in.spotReq
	if spotReq != "" {
		if err := p.pool.cancelSpot(context.WithoutCancel(ctx), spotReq); err != nil {
			log.Warn("cancel spot request; retrying before terminate", "request", spotReq, "err", err)
		} else {
			spotReq = ""
		}
	}
	log.Info("pool instance started",
		"claim", claimed.Sub(entered).Round(time.Millisecond),
		"startInstances", started.Sub(claimed).Round(time.Millisecond))

	states, stopWatch := p.c.watch.watch(in.id)
	defer stopWatch()
	// Until it is seen running, a "stopped" state is the instance before
	// the start took effect, not its job ending.
	w := &waiter{p: p, id: in.id, runner: name, log: log, states: states, awaitStart: true, spotReq: spotReq,
		itype: in.itype, subnet: in.subnet, az: in.az, spot: in.spotReq != ""}
	// As the launch began: the instance may fetch its config before this line.
	w.record(jobevent.Event{Kind: jobevent.Launched, Path: PathPool, At: claimed})
	return w.run(ctx, ip)
}

// waiter follows one launched instance until it is gone.
type waiter struct {
	p      *Provisioner
	id     string
	runner string
	log    *slog.Logger
	// interruptedWhy is why AWS took the instance, for its record.
	interruptedWhy string
	states         <-chan instanceState

	terminating bool
	termSince   time.Time // first termination request
	termLast    time.Time // latest termination request
	// awaitStart is set for a pool instance started from stopped: stop
	// states are ignored until the instance has been seen running.
	awaitStart bool
	// spotReq is a persistent spot request still to cancel before the
	// instance may be terminated.
	spotReq string
	// Where the instance runs, for reporting a spot interruption; spot says
	// it is a spot instance, whose request's status may tell one.
	itype, subnet, az string
	spot              bool
	interrupted       bool
	checkedRequest    bool
}

// spotInterruptionCodes are the state reasons of a spot instance AWS took
// back: terminated, or stopped (a persistent request's pool instance).
var spotInterruptionCodes = []string{"Server.SpotInstanceTermination", "Server.SpotInstanceShutdown"}

// interruptionStatus reports whether a spot request status code says AWS
// took the instance back. A state reason alone is not enough: an
// interruption FIS sends shows Client.UserInitiatedShutdown on the instance
// and instance-terminated-by-experiment on its request (seen 2026-11-02).
func interruptionStatus(code string) bool {
	if strings.HasPrefix(code, "marked-for-") {
		return true
	}
	for _, p := range []string{"instance-terminated-", "instance-stopped-", "instance-hibernated-"} {
		if rest, ok := strings.CutPrefix(code, p); ok {
			switch rest {
			case "by-price", "no-capacity", "capacity-oversubscribed", "launch-group-constraint", "by-experiment":
				return true
			}
		}
	}
	return false
}

// interruption returns why a spot instance that is ending was taken back by
// AWS, or "" when it was not: the state reason, else (asked once) its spot
// request's status code. That fallback is proven for one-time requests only:
// a pool instance's persistent request is cancelled once it starts, and
// whether AWS still updates a cancelled request's status is unchecked. An
// end after the runner reported its exit is its own power-off, so it is not
// asked about.
func (w *waiter) interruption(ctx context.Context, ip netip.Addr, s instanceState) string {
	if slices.Contains(spotInterruptionCodes, s.reason) {
		return s.reason
	}
	if !w.spot || w.checkedRequest || (ip.IsValid() && !w.p.o.JIT.Report(ip).Exited.IsZero()) {
		return ""
	}
	w.checkedRequest = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	out, err := w.p.c.api.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []types.Filter{{Name: aws.String("instance-id"), Values: []string{w.id}}},
	})
	if err != nil {
		w.log.Warn("read the spot request of an ending instance", "err", err)
		return ""
	}
	for _, r := range out.SpotInstanceRequests {
		if r.Status != nil && interruptionStatus(aws.ToString(r.Status.Code)) {
			return aws.ToString(r.Status.Code)
		}
	}
	return ""
}

// spotInterrupted reports a spot instance AWS took back while it ran a
// runner. It is not the runner failing, so the launch does not count as a
// failure; but GitHub marks a job the runner had started failed, and the
// instance's option goes to the end of its market for a while (see
// breakers), since AWS wants that capacity back.
func (w *waiter) spotInterrupted(ip netip.Addr, s instanceState, why string) {
	r := w.p.o.JIT.Report(ip)
	duringJob := ip.IsValid() && !r.Job.IsZero() && r.Exited.IsZero()
	// The waiter's logger already names the runner, instance, type and subnet.
	w.log.Warn("spot instance interrupted", "reason", why, "state", s.name, "az", w.az, "duringJob", duringJob)
	if duringJob {
		w.log.Warn("the runner's job fails at GitHub; re-run it from the run's page (Re-run failed jobs)")
	}
	if w.itype != "" && w.subnet != "" {
		w.p.c.breakers.fail(option{config.MarketSpot, w.itype, w.subnet}, why)
	}
	if w.p.o.Interrupted != nil {
		w.p.o.Interrupted(duringJob)
	}
}

// endingState reports a stop or termination: the only states whose reason
// says why the instance ended. A pending or running read can carry a reason
// left from an earlier stop.
func endingState(s types.InstanceStateName) bool {
	switch s {
	case types.InstanceStateNameStopping, types.InstanceStateNameStopped,
		types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
		return true
	}
	return false
}

// launchMetadataOptions: IMDSv2 only, one hop, so a job's containers cannot
// reach IMDS. No instance profile is attached, so IMDS holds no credentials
// anyway. doctor reports them (see metadataCheck).
func launchMetadataOptions() *types.InstanceMetadataOptionsRequest {
	return &types.InstanceMetadataOptionsRequest{
		HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		HttpTokens:              types.HttpTokensStateRequired,
		HttpPutResponseHopLimit: aws.Int32(1),
		InstanceMetadataTags:    types.InstanceMetadataTagsStateDisabled,
	}
}

func placementAZ(i types.Instance) string {
	if i.Placement == nil {
		return ""
	}
	return aws.ToString(i.Placement.AvailabilityZone)
}

// run waits for the instance to finish its job, terminating it when ctx is
// cancelled, when it never claims its config, when it outlives MaxLifetime,
// or when it stops instead of terminating (a spot instance powering off). It
// returns once the instance is shutting down or terminated; the error says why
// the runner ended early, if it did.
func (w *waiter) run(ctx context.Context, ip netip.Addr) error {
	p := w.p
	started := p.now()
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()
	var result error
	done := ctx.Done()
	for {
		select {
		case <-done:
			done = nil // terminate once; keep waiting for it to take effect
			w.terminate(context.WithoutCancel(ctx), "runner removed while idle")
		case s := <-w.states:
			if !w.terminating && !w.awaitStart && !w.interrupted && endingState(s.name) {
				if why := w.interruption(ctx, ip, s); why != "" {
					w.interrupted = true
					w.interruptedWhy = why
					w.spotInterrupted(ip, s, why)
				}
			}
			switch s.name {
			case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
				w.log.Info("instance gone", "state", s.name, "reason", s.reason)
				reason := s.reason
				if w.interruptedWhy != "" {
					reason = w.interruptedWhy // the instance's own reason can read as a normal end
				}
				w.record(jobevent.Event{Kind: jobevent.InstanceGone, Reason: reason})
				if err := w.runnerResult(ip); result == nil && ctx.Err() == nil {
					result = err
				}
				return result
			case types.InstanceStateNamePending, types.InstanceStateNameRunning:
				w.awaitStart = false
			case types.InstanceStateNameStopping, types.InstanceStateNameStopped:
				if w.awaitStart {
					continue // the state from before the start
				}
				// The runner powered off and the instance stopped: a pool
				// instance always does, and a job launch should not.
				// Finish the job here.
				if !w.terminating {
					w.terminate(context.WithoutCancel(ctx), "instance stopped")
				}
			}
		case <-ticker.C:
			now := p.now()
			switch {
			case w.terminating && now.Sub(w.termSince) >= p.termWait:
				return errors.Join(result, fmt.Errorf("instance %s did not terminate within %s; terminate it by hand", w.id, p.termWait))
			case w.terminating && now.Sub(w.termLast) >= p.termRetry:
				// The request may have been throttled or lost; repeat it.
				w.terminate(context.WithoutCancel(ctx), "retry")
			case w.terminating:
			case !p.o.JIT.Pending(ip) && !p.o.JIT.Served(ip):
				result = fmt.Errorf("instance %s never fetched its JIT config", w.id)
				w.terminate(context.WithoutCancel(ctx), "jit config expired unclaimed")
			case p.o.MaxLifetime > 0 && now.Sub(started) >= p.o.MaxLifetime:
				result = fmt.Errorf("instance %s exceeded the max lifetime %s", w.id, p.o.MaxLifetime)
				w.terminate(context.WithoutCancel(ctx), "max lifetime exceeded")
			}
		}
	}
}

// runnerResult logs the phases the instance reported for its runner and
// returns ErrRunnerFailed when the runner reported a non-zero exit. No report
// (a hard crash, an instance that never fetched its config) is no error.
func (w *waiter) runnerResult(ip netip.Addr) error {
	if !ip.IsValid() {
		return nil
	}
	r := w.p.o.JIT.Report(ip)
	if r.Served.IsZero() {
		return nil
	}
	var attrs []any
	span := func(key string, from, to time.Time) {
		if !from.IsZero() && !to.IsZero() {
			attrs = append(attrs, key, to.Sub(from).Round(time.Millisecond))
		}
	}
	span("servedToListening", r.Served, r.Listening)
	span("listeningToJob", r.Listening, r.Job)
	span("jobToExit", r.Job, r.Exited)
	w.recordPhases(r)
	if r.Exited.IsZero() {
		w.log.Info("runner phases; no exit reported", attrs...)
		return nil
	}
	attrs = append(attrs, "rc", r.ExitCode)
	w.log.Info("runner phases", attrs...)
	if r.ExitCode != 0 {
		return fmt.Errorf("%w: instance %s, exit status %d", ErrRunnerFailed, w.id, r.ExitCode)
	}
	return nil
}

// record stamps an event with the instance and sends it on.
func (w *waiter) record(e jobevent.Event) {
	e.Tier, e.Runner, e.Instance, e.InstanceType, e.Subnet, e.Zone = w.p.o.Tier, w.runner, w.id, w.itype, w.subnet, w.az
	e.Market = config.MarketOnDemand
	if w.spot {
		e.Market = config.MarketSpot
	}
	jobevent.Record(w.p.o.Events, e)
}

// recordPhases sends the stages the instance reported, at their own times.
func (w *waiter) recordPhases(r jitserver.Report) {
	for _, p := range []struct {
		kind jobevent.Kind
		at   time.Time
	}{
		{jobevent.Served, r.Served},
		{jobevent.Listening, r.Listening},
		{jobevent.Running, r.Job},
	} {
		if !p.at.IsZero() {
			w.record(jobevent.Event{Kind: p.kind, At: p.at})
		}
	}
	if !r.Exited.IsZero() {
		w.record(jobevent.Event{Kind: jobevent.Exited, At: r.Exited, ExitCode: r.ExitCode})
	}
}

// terminate asks EC2 to terminate the instance. The first request is logged
// with its reason; failures are retried from run's ticker.
func (w *waiter) terminate(ctx context.Context, reason string) {
	now := w.p.now()
	if !w.terminating {
		w.terminating = true
		w.termSince = now
		w.log.Info("terminating instance", "reason", reason)
	}
	w.termLast = now
	if w.spotReq != "" {
		// An open persistent request would replace the instance after the
		// terminate; it must be cancelled first.
		if _, err := w.p.c.api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{w.spotReq}}); err != nil {
			w.log.Warn("cancel spot request before terminate", "request", w.spotReq, "err", err)
			return
		}
		w.spotReq = ""
	}
	if _, err := w.p.c.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{w.id}}); err != nil {
		w.log.Warn("terminate instance", "err", err)
	}
}

// abort terminates an instance whose launch failed before its runner could
// start, and waits (bounded) for it to go.
func (w *waiter) abort(ctx context.Context, reason string, cause error) error {
	w.terminate(context.WithoutCancel(ctx), reason)
	return errors.Join(cause, w.run(context.WithoutCancel(ctx), netip.Addr{}))
}

// launchKind says what an instance is launched for.
type launchKind int

const (
	// forJob launches an instance that runs the named runner's job at once.
	forJob launchKind = iota
	// forPool launches a pool instance: it boots once with no job, then
	// stops and waits to be claimed.
	forPool
)

// run walks markets × instance types × subnets in the configured order and
// returns the first instance EC2 accepts. A capacity error moves on to the
// next subnet, a quota error to the next type or market (the quota is
// regional); any other error ends the walk. Options EC2 refused lately (see
// breakers) go to the end of their market rather than out of the walk: spot
// still comes before on-demand, since a refused call costs only time and an
// on-demand job costs more for its whole run.
func (p *Provisioner) run(ctx context.Context, name string, img image, kind launchKind) (types.Instance, error) {
	var markets []string
	switch p.o.EC2.Market {
	case config.MarketSpot:
		markets = []string{config.MarketSpot}
	case config.MarketOnDemand:
		markets = []string{config.MarketOnDemand}
	default: // spot-then-on-demand
		markets = []string{config.MarketSpot, config.MarketOnDemand}
	}
	var walk []option
	backingOff := 0
	for _, market := range markets {
		var ready, later []option
		for _, itype := range p.o.EC2.InstanceTypes {
			for _, subnet := range p.o.EC2.Subnets {
				o := option{market, itype, subnet}
				if p.c.breakers.blocked(o) {
					later = append(later, o)
				} else {
					ready = append(ready, o)
				}
			}
		}
		backingOff += len(later)
		walk = append(append(walk, ready...), later...)
	}
	firstChoice := option{markets[0], p.o.EC2.InstanceTypes[0], p.o.EC2.Subnets[0]}
	if backingOff > 0 {
		p.log.Info("launch options backing off; trying them last in their market", "runner", name, "backingOff", backingOff, "of", len(walk))
	}
	overQuota := map[option]bool{} // market and type, this walk
	var last error
	for _, o := range walk {
		if overQuota[option{o.market, o.itype, ""}] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return types.Instance{}, err
		}
		out, err := p.c.api.RunInstances(ctx, p.input(name, img, o.market, o.itype, o.subnet, kind), capacityFailsFast)
		if err == nil {
			if len(out.Instances) != 1 {
				return types.Instance{}, fmt.Errorf("RunInstances returned %d instances, want 1", len(out.Instances))
			}
			p.c.breakers.succeeded(o)
			p.fellBack(name, firstChoice, o)
			return out.Instances[0], nil
		}
		switch {
		case isQuotaErr(err):
			q := option{o.market, o.itype, ""}
			overQuota[q] = true
			p.c.breakers.fail(q, errorCode(err))
			p.log.Warn("vCPU quota used up; trying the next type or market", "runner", name, "market", o.market, "type", o.itype, "err", err)
		case isCapacityErr(err):
			p.c.breakers.fail(o, errorCode(err))
			p.log.Info("no capacity; trying the next option", "runner", name, "market", o.market, "type", o.itype, "subnet", o.subnet, "err", err)
		default:
			return types.Instance{}, fmt.Errorf("run %s %s in %s: %w", o.market, o.itype, o.subnet, err)
		}
		last = err
	}
	return types.Instance{}, fmt.Errorf("%w: %w", ErrNoCapacity, last)
}

// fellBack reports where an instance landed, when not at the first choice.
func (p *Provisioner) fellBack(name string, first, got option) {
	kind := ""
	switch {
	case got.market != first.market:
		kind = FallbackMarket
	case got.itype != first.itype:
		kind = FallbackType
	case got.subnet != first.subnet:
		kind = FallbackSubnet
	default:
		return
	}
	p.log.Info("launched on a fallback option", "runner", name, "fallback", kind, "market", got.market, "type", got.itype, "subnet", got.subnet)
	if p.o.FellBack != nil {
		p.o.FellBack(kind)
	}
}

// capacityFailsFast keeps the AWS SDK from retrying a launch's capacity error. EC2
// answers no spot capacity with HTTP 500, which the SDK retries for about 5 s, and
// a launch would wait that long for each type and subnet it tries before one that
// has room. The walk over options is the retry. Only RunInstances has one: a start
// from the pool keeps the SDK's retries, as a failure there discards an instance.
func capacityFailsFast(o *ec2.Options) {
	if inner, ok := o.Retryer.(aws.RetryerV2); ok {
		o.Retryer = capacityRetryer{inner}
	}
}

// capacityRetryer wraps the configured retryer, so its mode and attempts stand.
type capacityRetryer struct{ aws.RetryerV2 }

func (r capacityRetryer) IsErrorRetryable(err error) bool {
	return !isCapacityErr(err) && r.RetryerV2.IsErrorRetryable(err)
}

func isCapacityErr(err error) bool {
	for _, c := range capacityCodes {
		if isCode(err, c) {
			return true
		}
	}
	return false
}

// input builds the RunInstancesInput for one attempt. name is the runner for
// a job launch, or a unique pool name for a pool launch.
func (p *Provisioner) input(name string, img image, market, itype, subnet string, kind launchKind) *ec2.RunInstancesInput {
	tags := []types.Tag{
		{Key: aws.String(TagTier), Value: aws.String(p.o.Tier)},
		{Key: aws.String(TagOwner), Value: aws.String(p.o.Owner)},
		{Key: aws.String("Name"), Value: aws.String(name)},
	}
	if kind == forPool {
		tags = append(tags, types.Tag{Key: aws.String(TagPool), Value: aws.String(PoolWarming)})
	} else {
		tags = append(tags, types.Tag{Key: aws.String(TagRunner), Value: aws.String(name)})
	}
	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(img.id),
		InstanceType:     types.InstanceType(itype),
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		SubnetId:         aws.String(subnet),
		SecurityGroupIds: p.o.EC2.SecurityGroups,
		// User-data names the JIT endpoint and the tier's Docker storage:
		// nothing secret.
		UserData:        aws.String(base64.StdEncoding.EncodeToString([]byte(jitserver.UserData(p.o.JITURL, p.o.EC2.DockerStorage)))),
		MetadataOptions: launchMetadataOptions(),
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
		// Idempotent across SDK retries of this exact attempt.
		ClientToken: aws.String(clientToken(name, market, itype, subnet)),
		// The guest's poweroff ends the instance. One-time spot accepts this
		// too; only a persistent spot request (the stopped pool) rejects it.
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
	}
	if market == config.MarketSpot {
		in.InstanceMarketOptions = &types.InstanceMarketOptionsRequest{
			MarketType: types.MarketTypeSpot,
			SpotOptions: &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypeOneTime,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate,
			},
		}
		// Every spot request carries the owner tag, so a role scoped to it
		// (the control VM's, the reaper's) may cancel it, and the reaper
		// can find one left without its instance.
		in.TagSpecifications = append(in.TagSpecifications, types.TagSpecification{
			ResourceType: types.ResourceTypeSpotInstancesRequest, Tags: tags,
		})
	}
	if kind == forPool {
		// No user-data: the boot script finds no endpoint, warms the disk
		// and powers off, and the instance stops. It stays stopped until a
		// job claims it and writes the endpoint in.
		in.UserData = nil
		in.InstanceInitiatedShutdownBehavior = types.ShutdownBehaviorStop
		if market == config.MarketSpot {
			// Only a persistent request can be stopped and started again,
			// and it rejects terminate-on-shutdown.
			in.InstanceInitiatedShutdownBehavior = ""
			in.InstanceMarketOptions.SpotOptions = &types.SpotMarketOptions{
				SpotInstanceType:             types.SpotInstanceTypePersistent,
				InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorStop,
			}
		}
	}
	e := p.o.EC2
	throughput := e.VolumeThroughput(kind == forPool)
	if e.RootVolumeGiB > 0 || e.RootVolumeType != "" || throughput > 0 || e.RootVolumeIOPS > 0 || e.RootVolumeInitRate > 0 {
		ebs := &types.EbsBlockDevice{
			VolumeType:          types.VolumeType(e.VolumeType()),
			DeleteOnTermination: aws.Bool(true),
		}
		for _, f := range []struct {
			field **int32
			val   int
		}{
			{&ebs.VolumeSize, e.RootVolumeGiB},
			{&ebs.Throughput, throughput},
			{&ebs.Iops, e.RootVolumeIOPS},
			{&ebs.VolumeInitializationRate, e.RootVolumeInitRate},
		} {
			if f.val > 0 {
				*f.field = aws.Int32(int32(f.val))
			}
		}
		in.BlockDeviceMappings = []types.BlockDeviceMapping{{DeviceName: aws.String(img.rootDevice), Ebs: ebs}}
	}
	return in
}

// clientToken derives RunInstances' idempotency token (at most 64 ASCII
// characters) from one launch attempt.
func clientToken(parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:48]
}

// privateIP returns the instance's private IP, describing it until AWS
// reports one if RunInstances did not.
func (p *Provisioner) privateIP(ctx context.Context, inst types.Instance) (netip.Addr, error) {
	id := aws.ToString(inst.InstanceId)
	addr := aws.ToString(inst.PrivateIpAddress)
	for wait := 200 * time.Millisecond; addr == ""; wait = min(2*wait, 5*time.Second) {
		select {
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		case <-time.After(wait):
		}
		out, err := p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
		if err != nil && !isCode(err, errNotFound) {
			return netip.Addr{}, fmt.Errorf("describe %s for its private IP: %w", id, err)
		}
		if err == nil {
			for _, r := range out.Reservations {
				for _, i := range r.Instances {
					addr = aws.ToString(i.PrivateIpAddress)
				}
			}
		}
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("instance %s private IP %q: %w", id, addr, err)
	}
	return ip, nil
}
