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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/solcreek/firerunner/internal/config"
	"github.com/solcreek/firerunner/internal/core"
	"github.com/solcreek/firerunner/internal/jitserver"
)

// ErrNoCapacity reports that every instance type in every subnet, in every
// market the tier allows, was out of capacity.
var ErrNoCapacity = errors.New("no EC2 capacity for any configured instance type and subnet")

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
	// cannot see take effect before it gives up and leaves the instance to
	// the reaper.
	terminateWait = 5 * time.Minute
	// terminateRetry paces repeated termination requests.
	terminateRetry = 10 * time.Second
)

// Client holds what every EC2 tier in a process shares: the AWS clients, the
// AMI cache and the instance-state watcher.
type Client struct {
	api   API
	amis  *amiResolver
	watch *watcher
	log   *slog.Logger
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
	return &Client{api: api, amis: newAMIResolver(api, params), watch: newWatcher(api, pollInterval, log), log: log}
}

// Options configures one EC2 tier's provisioner.
type Options struct {
	// Tier is the tier name, tagged on every instance.
	Tier string
	// Owner identifies this firerunner install, so the reaper only ever
	// touches what it launched.
	Owner string
	EC2   config.EC2Tier
	// JIT serves instances their configs; JITURL is its endpoint as instances
	// reach it, written into user-data.
	JIT    *jitserver.Server
	JITURL string
	// MaxLifetime terminates an instance still alive after this long; 0
	// disables the backstop.
	MaxLifetime time.Duration
	Logger      *slog.Logger
}

// Provisioner launches one EC2 instance per runner for one tier.
type Provisioner struct {
	c    *Client
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
	return &Provisioner{
		c: c, o: o, log: log.With("provisioner", "ec2"), now: time.Now, tick: time.Second,
		termWait: terminateWait, termRetry: terminateRetry,
	}
}

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
	img, err := p.c.amis.resolve(ctx, p.o.EC2.AMI)
	if err != nil {
		return err
	}
	inst, err := p.run(ctx, name, img)
	if err != nil {
		return err
	}
	id := aws.ToString(inst.InstanceId)
	log := p.log.With("runner", name, "instance", id, "type", inst.InstanceType, "subnet", aws.ToString(inst.SubnetId))
	log.Info("instance launched")

	states, stopWatch := p.c.watch.watch(id)
	defer stopWatch()
	w := &waiter{p: p, id: id, log: log, states: states}

	ip, err := p.privateIP(ctx, inst)
	if err != nil {
		return w.abort(ctx, "no private IP", err)
	}
	if err := p.o.JIT.Register(ip, name, jit); err != nil {
		return w.abort(ctx, "jit registration failed", fmt.Errorf("register jit config for %s at %s: %w", id, ip, err))
	}
	defer p.o.JIT.Revoke(ip)
	return w.run(ctx, ip)
}

// waiter follows one launched instance until it is gone.
type waiter struct {
	p      *Provisioner
	id     string
	log    *slog.Logger
	states <-chan types.InstanceStateName

	terminating bool
	termSince   time.Time // first termination request
	termLast    time.Time // latest termination request
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
			switch s {
			case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
				w.log.Info("instance gone", "state", s)
				return result
			case types.InstanceStateNameStopping, types.InstanceStateNameStopped:
				// The runner powered off but the instance only stopped, which
				// a terminate-on-shutdown launch should never do (a pooled
				// persistent-spot instance would). Finish the job here.
				if !w.terminating {
					w.terminate(context.WithoutCancel(ctx), "instance stopped")
				}
			}
		case <-ticker.C:
			now := p.now()
			switch {
			case w.terminating && now.Sub(w.termSince) >= p.termWait:
				return errors.Join(result, fmt.Errorf("instance %s did not terminate within %s; leaving it to the reaper", w.id, p.termWait))
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

// run walks markets × instance types × subnets in the configured order and
// returns the first instance EC2 accepts. Capacity errors move on to the next
// combination; any other error ends the walk.
func (p *Provisioner) run(ctx context.Context, name string, img image) (types.Instance, error) {
	var markets []string
	switch p.o.EC2.Market {
	case config.MarketSpot:
		markets = []string{config.MarketSpot}
	case config.MarketOnDemand:
		markets = []string{config.MarketOnDemand}
	default: // spot-then-on-demand
		markets = []string{config.MarketSpot, config.MarketOnDemand}
	}
	var last error
	for _, market := range markets {
		for _, itype := range p.o.EC2.InstanceTypes {
			for _, subnet := range p.o.EC2.Subnets {
				if err := ctx.Err(); err != nil {
					return types.Instance{}, err
				}
				out, err := p.c.api.RunInstances(ctx, p.input(name, img, market, itype, subnet))
				if err == nil {
					if len(out.Instances) != 1 {
						return types.Instance{}, fmt.Errorf("RunInstances returned %d instances, want 1", len(out.Instances))
					}
					return out.Instances[0], nil
				}
				if !isCapacityErr(err) {
					return types.Instance{}, fmt.Errorf("run %s %s in %s: %w", market, itype, subnet, err)
				}
				p.log.Info("no capacity; trying the next option", "runner", name, "market", market, "type", itype, "subnet", subnet, "err", err)
				last = err
			}
		}
	}
	return types.Instance{}, fmt.Errorf("%w: %w", ErrNoCapacity, last)
}

func isCapacityErr(err error) bool {
	for _, c := range capacityCodes {
		if isCode(err, c) {
			return true
		}
	}
	return false
}

// input builds the RunInstancesInput for one attempt.
func (p *Provisioner) input(name string, img image, market, itype, subnet string) *ec2.RunInstancesInput {
	tags := []types.Tag{
		{Key: aws.String(TagTier), Value: aws.String(p.o.Tier)},
		{Key: aws.String(TagRunner), Value: aws.String(name)},
		{Key: aws.String(TagOwner), Value: aws.String(p.o.Owner)},
		{Key: aws.String("Name"), Value: aws.String(name)},
	}
	in := &ec2.RunInstancesInput{
		ImageId:          aws.String(img.id),
		InstanceType:     types.InstanceType(itype),
		MinCount:         aws.Int32(1),
		MaxCount:         aws.Int32(1),
		SubnetId:         aws.String(subnet),
		SecurityGroupIds: p.o.EC2.SecurityGroups,
		// The only user-data is the JIT endpoint: nothing secret.
		UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(jitserver.UserData(p.o.JITURL)))),
		// IMDSv2 only, one hop: a job's containers cannot reach IMDS. No
		// instance profile is attached, so IMDS holds no credentials anyway.
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			InstanceMetadataTags:    types.InstanceMetadataTagsStateDisabled,
		},
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
	}
	if p.o.EC2.RootVolumeGiB > 0 || p.o.EC2.RootVolumeType != "" {
		ebs := &types.EbsBlockDevice{
			VolumeType:          types.VolumeType(p.o.EC2.VolumeType()),
			DeleteOnTermination: aws.Bool(true),
		}
		if p.o.EC2.RootVolumeGiB > 0 {
			ebs.VolumeSize = aws.Int32(int32(p.o.EC2.RootVolumeGiB))
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
