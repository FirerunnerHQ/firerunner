package ec2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/jitserver"
)

// Pool keeps a tier's pre-warmed instances stopped and ready to start.
//
// A pool instance is launched with no user-data. Its boot script finds no
// endpoint, reads the runner's files once (so EBS has fetched those blocks
// from the snapshot), and powers off. The instance stops and waits. A spot
// instance stays on a persistent request, still billed at the spot price
// while running. A job claims a ready instance:
//   - check that it is still as its launch made it (inspect), else discard it;
//   - tag it claimed;
//   - write the JIT endpoint into its user-data;
//   - start it;
//   - then cancel its spot request, or AWS would replace the instance after
//     terminate. The order matters: AWS refuses to start a stopped spot
//     instance whose request is already cancelled.
//
// From then on it is an ordinary job instance, terminated when it stops.
//
// The pool's state lives in tags (TagPool), so a restarted firerunner finds
// its ready instances again. Ones a crash left claimed are the reaper's to
// end, once GitHub confirms no job runs on them.
//
// Tags alone prove nothing: anyone who may tag an instance in the account can
// write them. So each pool instance is signed (TagPoolMAC, poolmac.go) right
// after its launch, and one whose MAC is missing or wrong never joins the
// pool: it is never registered for a JIT config nor started, and it is
// terminated (see reject).
//
// The MAC proves who launched an instance, not that nobody changed it since.
// So while it first warms, the pool also signs the image and volumes it was
// launched with (TagPoolDiskMAC), and a claim starts it only while those, its
// security groups, subnet and instance profile are still what the launch set
// (see inspect).
type Pool struct {
	p      *Provisioner
	target int
	log    *slog.Logger
	tick   time.Duration
	now    func() time.Time

	mu      sync.Mutex
	ready   []pooled
	warming map[string]time.Time // instance -> when it was launched
	// unsigned are instances this process launched whose disks it has not
	// signed yet, with the image each was launched from. Only these are
	// signed, and only while they run their first boot: a stopped instance's
	// volumes can be swapped, and the image of one an earlier process
	// launched is not known.
	unsigned map[string]string
	wake     chan struct{}
	failures int
	retryAt  time.Time
}

// pooled is a ready, stopped pool instance.
type pooled struct {
	id, ip, spotReq, itype, subnet, az string
}

// readyCheckEvery is how many passes (one per tick, 15 s) go between checks
// that ready instances are still stopped.
const readyCheckEvery = 20

// poolRetryMax caps the backoff between failed warm launches.
const poolRetryMax = 5 * time.Minute

// warmMax bounds a warm-up boot. An instance still not stopped after it never
// will be (a wrong image, a boot script that cannot finish): it is discarded,
// so it stops billing and its place in the pool is refilled.
var warmMax = 10 * time.Minute

func newPool(p *Provisioner, target int) *Pool {
	return &Pool{
		p: p, target: target, log: p.log.With("component", "pool"), tick: 15 * time.Second, now: time.Now,
		warming: map[string]time.Time{}, unsigned: map[string]string{}, wake: make(chan struct{}, 1),
	}
}

// Run rediscovers the pool from tags, then keeps it at its target until ctx
// ends. Stopped instances are left in place on exit, ready for the next run.
func (pl *Pool) Run(ctx context.Context) {
	if err := pl.discover(ctx); err != nil {
		pl.log.Warn("rediscover pool", "err", err)
	}
	t := time.NewTicker(pl.tick)
	defer t.Stop()
	for pass := 0; ; pass++ {
		if pass%readyCheckEvery == 0 {
			pl.checkReady(ctx)
		}
		pl.refresh(ctx)
		pl.trim(ctx)
		pl.fill(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-pl.wake:
		}
	}
}

// SetTarget resizes the pool. A larger pool fills on the next pass. A
// smaller one terminates ready instances beyond the target before it
// returns, so a caller that shrinks the pool and then its hot runners ends
// stopped instances first; warming instances beyond it are ended once they
// are ready.
func (pl *Pool) SetTarget(ctx context.Context, n int) {
	pl.mu.Lock()
	old := pl.target
	pl.target = n
	pl.mu.Unlock()
	if n == old {
		return
	}
	pl.log.Info("pool resized", "from", old, "to", n)
	pl.trim(ctx)
	pl.poke()
}

// Target is the size the pool keeps.
func (pl *Pool) Target() int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return pl.target
}

// trim terminates ready instances beyond the target. Warming ones count
// only once ready: ending ready instances to make room for ones still
// booting would leave the pool empty while they boot. One it cannot end
// (its spot request would not cancel) goes back to the pool, and the next
// pass tries again.
func (pl *Pool) trim(ctx context.Context) {
	for ctx.Err() == nil {
		pl.mu.Lock()
		if len(pl.ready) <= pl.target {
			pl.mu.Unlock()
			return
		}
		in := pl.ready[len(pl.ready)-1]
		pl.ready = pl.ready[:len(pl.ready)-1]
		pl.mu.Unlock()
		if !pl.discard(ctx, in.id, in.spotReq) {
			pl.mu.Lock()
			pl.ready = append(pl.ready, in)
			pl.mu.Unlock()
			return
		}
		pl.log.Info("pool instance terminated: above the target", "instance", in.id)
	}
}

// Ready reports how many instances are stopped and ready, and how many are
// still warming.
func (pl *Pool) Ready() (ready, warming int) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	return len(pl.ready), len(pl.warming)
}

func (pl *Pool) poke() {
	select {
	case pl.wake <- struct{}{}:
	default:
	}
}

func (pl *Pool) filters() []types.Filter {
	return []types.Filter{
		{Name: aws.String("tag:" + TagOwner), Values: []string{pl.p.o.Owner}},
		{Name: aws.String("tag:" + TagTier), Values: []string{pl.p.o.Tier}},
		{Name: aws.String("tag-key"), Values: []string{TagPool}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}
}

// discover sorts this tier's pool instances by their tags: ready ones join the
// pool and warming ones are followed until they stop, if their MAC holds;
// those whose MAC fails are rejected. Claimed ones, left by a firerunner that
// died mid-claim or mid-job, are the reaper's: it checks with GitHub whether
// their runner is still running a job before ending them.
func (pl *Pool) discover(ctx context.Context) error {
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: pl.filters()})
	if err != nil {
		return err
	}
	rejected := 0
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			id, state := aws.ToString(i.InstanceId), i.State.Name
			switch tagValue(i.Tags, TagPool) {
			case PoolReady, PoolWarming:
				if err := pl.verify(i); err != nil {
					pl.reject(ctx, i, err)
					rejected++
					continue
				}
				pl.signRequest(ctx, id, aws.ToString(i.SpotInstanceRequestId))
				if state == types.InstanceStateNameStopped {
					pl.markReady(ctx, i)
				} else {
					pl.mu.Lock()
					pl.warming[id] = aws.ToTime(i.LaunchTime)
					pl.mu.Unlock()
				}
			}
		}
	}
	r, w := pl.Ready()
	pl.log.Info("pool rediscovered", "ready", r, "warming", w, "rejected", rejected)
	return nil
}

// verify checks a pool instance's MAC for this tier's owner and tier.
func (pl *Pool) verify(i types.Instance) error {
	return pl.p.c.verifyPoolMAC(i, pl.p.o.Owner, pl.p.o.Tier)
}

// reject keeps an instance whose MAC failed out of the pool for good: it is
// never registered for a JIT config nor started. It is terminated only when
// it carries this install's owner tag and a pool state, as the control role
// may end only those, and anything else is not the pool's to end; and only
// when its pool MAC holds (its disks' failed), since the owner tag alone
// proves nothing: anyone who may tag an instance can write it. One whose
// spot request will not cancel is left, still unused, for the reaper.
func (pl *Pool) reject(ctx context.Context, i types.Instance, why error) {
	id := aws.ToString(i.InstanceId)
	pl.forget(id)
	owner, state := tagValue(i.Tags, TagOwner), tagValue(i.Tags, TagPool)
	if owner != pl.p.o.Owner || (state != PoolReady && state != PoolWarming) {
		pl.log.Warn("pool instance failed its MAC check; not using it", "instance", id, "why", why, "owner", owner, "pool", state)
		return
	}
	if !pl.p.c.poolKeyEphemeral && pl.verify(i) != nil {
		pl.log.Warn("pool instance failed its MAC check; not using it, and not ending it: nothing proves this install launched it", "instance", id, "why", why)
		return
	}
	pl.log.Warn("pool instance failed its MAC check; not using it, terminating it", "instance", id, "why", why)
	if !pl.discard(ctx, id, aws.ToString(i.SpotInstanceRequestId)) {
		pl.log.Warn("could not terminate the rejected pool instance; the reaper retries", "instance", id)
	}
}

// forget stops following an instance as warming or waiting for its disks to
// be signed.
func (pl *Pool) forget(id string) {
	pl.mu.Lock()
	delete(pl.warming, id)
	delete(pl.unsigned, id)
	pl.mu.Unlock()
}

// sign tags a pool instance this process just launched with its MAC.
func (pl *Pool) sign(ctx context.Context, id string) error {
	return pl.p.c.tagNew(ctx, id, TagPoolMAC, pl.p.c.poolMAC(pl.p.o.Owner, pl.p.o.Tier, id))
}

// signRequest tags a pool instance's persistent spot request, if it has one,
// with its MAC. Unsigned, the request is still cancelled with its instance,
// but a reaper cancels it alone, or ends an instance it relaunched, only by
// the MAC; so a failure is logged and the pool goes on.
func (pl *Pool) signRequest(ctx context.Context, id, req string) {
	if req == "" {
		return
	}
	if err := pl.p.c.tagNew(ctx, req, TagRequestMAC, pl.p.c.requestMAC(pl.p.o.Owner, pl.p.o.Tier, req)); err != nil {
		pl.log.Warn("could not sign the pool instance's spot request; a reaper leaves the request alone once the instance is gone", "instance", id, "request", req, "err", err)
	}
}

// signDisks tags an instance this process launched, and has not signed the
// disks of yet, with the MAC of its image and volumes (TagPoolDiskMAC), once
// EC2 lists its root volume. Only while it runs its first boot: its root
// volume cannot be detached then, while a stopped one's could have been
// swapped. Until it is signed markReady refuses it, so one never signed is
// rejected when it stops, and the pool launches another.
func (pl *Pool) signDisks(ctx context.Context, i types.Instance) {
	id := aws.ToString(i.InstanceId)
	pl.mu.Lock()
	launchedFrom, waiting := pl.unsigned[id]
	pl.mu.Unlock()
	if !waiting || (i.State.Name != types.InstanceStateNamePending && i.State.Name != types.InstanceStateNameRunning) {
		return
	}
	image, root, volumes, listed := diskOf(i)
	if !listed {
		return // not yet; the next pass looks again
	}
	if image != launchedFrom {
		// Not the image the launch asked for: never sign it, so it never
		// joins the pool.
		pl.log.Warn("pool instance does not run the image it was launched from; not signing its disks", "instance", id, "image", image, "launchedFrom", launchedFrom)
		pl.mu.Lock()
		delete(pl.unsigned, id)
		pl.mu.Unlock()
		return
	}
	mac := pl.p.c.diskMAC(pl.p.o.Owner, pl.p.o.Tier, id, image, root, volumes)
	if err := pl.tag(ctx, id, TagPoolDiskMAC, mac); err != nil {
		pl.log.Warn("sign pool instance disks; retrying", "instance", id, "err", err)
		return
	}
	pl.mu.Lock()
	delete(pl.unsigned, id)
	pl.mu.Unlock()
}

// signNewDisks describes a just-launched instance once and signs its disks
// if EC2 lists them already; refresh signs them later otherwise.
func (pl *Pool) signNewDisks(ctx context.Context, id string) {
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return // EC2 may not list it yet; refresh tries again
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				pl.signDisks(ctx, i)
			}
		}
	}
}

// refresh moves warming instances that have stopped into the pool, and
// discards ones that have warmed longer than warmMax.
func (pl *Pool) refresh(ctx context.Context) {
	pl.mu.Lock()
	ids := make([]string, 0, len(pl.warming))
	for id := range pl.warming {
		ids = append(ids, id)
	}
	pl.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		if !isCode(err, errNotFound) {
			pl.log.Warn("describe warming instances", "err", err)
		}
		return
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			switch i.State.Name {
			case types.InstanceStateNameStopped:
				pl.markReady(ctx, i)
			case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
				pl.forget(aws.ToString(i.InstanceId))
			default:
				pl.signDisks(ctx, i)
				id := aws.ToString(i.InstanceId)
				pl.mu.Lock()
				since, ok := pl.warming[id]
				pl.mu.Unlock()
				if ok && !since.IsZero() && pl.now().Sub(since) > warmMax {
					pl.log.Warn("pool instance did not finish warming; discarding it", "instance", id, "warming", pl.now().Sub(since).Round(time.Second))
					if pl.discard(ctx, id, aws.ToString(i.SpotInstanceRequestId)) {
						pl.forget(id)
					}
				}
			}
		}
	}
}

// markReady tags a stopped pool instance ready and adds it to the pool, if
// its MACs hold: its launch's, and its disks'.
func (pl *Pool) markReady(ctx context.Context, i types.Instance) {
	id := aws.ToString(i.InstanceId)
	err := pl.verify(i)
	if err == nil {
		err = pl.p.c.verifyDiskMAC(i, pl.p.o.Owner, pl.p.o.Tier)
	}
	if err != nil {
		pl.reject(ctx, i, err)
		return
	}
	if err := pl.tag(ctx, id, TagPool, PoolReady); err != nil {
		pl.log.Warn("tag pool instance ready", "instance", id, "err", err)
		return
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.warming, id)
	delete(pl.unsigned, id)
	pl.ready = append(pl.ready, pooled{
		id: id, ip: aws.ToString(i.PrivateIpAddress),
		spotReq: aws.ToString(i.SpotInstanceRequestId), itype: string(i.InstanceType),
		subnet: aws.ToString(i.SubnetId), az: placementAZ(i),
	})
}

// fill launches warm instances until ready plus warming reaches the target,
// backing off after failures.
func (pl *Pool) fill(ctx context.Context) {
	pl.mu.Lock()
	deficit := pl.target - len(pl.ready) - len(pl.warming)
	waiting := pl.now().Before(pl.retryAt)
	pl.mu.Unlock()
	if deficit <= 0 || waiting {
		return
	}
	img, err := pl.p.c.amis.resolve(ctx, pl.p.o.EC2.AMI)
	if err != nil {
		pl.failed(err)
		return
	}
	for range deficit {
		// The target may have shrunk since the deficit was taken.
		pl.mu.Lock()
		short := pl.target - len(pl.ready) - len(pl.warming)
		pl.mu.Unlock()
		if short <= 0 {
			return
		}
		inst, err := pl.p.run(ctx, poolName(pl.p.o.Tier), img, forPool)
		if err != nil {
			pl.failed(err)
			return
		}
		id := aws.ToString(inst.InstanceId)
		// Signed at once, and even as ctx ends: unsigned, no later process
		// would trust it.
		if err := pl.sign(context.WithoutCancel(ctx), id); err != nil {
			pl.log.Warn("could not sign the new pool instance; discarding it", "instance", id, "err", err)
			pl.discard(context.WithoutCancel(ctx), id, aws.ToString(inst.SpotInstanceRequestId))
			pl.failed(err)
			return
		}
		pl.signRequest(context.WithoutCancel(ctx), id, aws.ToString(inst.SpotInstanceRequestId))
		pl.log.Info("warming pool instance", "instance", id, "type", inst.InstanceType)
		pl.mu.Lock()
		pl.warming[id] = pl.now()
		pl.unsigned[id] = img.id
		pl.failures = 0
		pl.mu.Unlock()
		pl.signNewDisks(context.WithoutCancel(ctx), id)
	}
}

func (pl *Pool) failed(err error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.failures++
	d := min(time.Duration(1<<min(pl.failures, 10))*time.Second, poolRetryMax)
	pl.retryAt = pl.now().Add(d)
	pl.log.Warn("warm pool instance", "err", err, "retryIn", d)
}

// errPoolEmpty reports that no ready instance could be claimed.
var errPoolEmpty = errors.New("no ready pool instance")

// claim takes a ready instance for the runner and prepares it to start: it is
// tagged claimed and the JIT endpoint is written into its user-data. Its spot
// request stays open until it has started (see cancelSpot).
//
// Only an error about that one instance (it is no longer stopped, or gone)
// discards it and tries the next. Anything else (ctx cancelled, missing
// permissions, throttling) would fail for every instance alike, so the
// instance goes back to the pool and the error is returned: emptying the pool
// on such an error would re-warm it for every job.
func (pl *Pool) claim(ctx context.Context, runner string) (pooled, error) {
	defer pl.poke() // refill behind the claim
	for {
		if err := ctx.Err(); err != nil {
			return pooled{}, err
		}
		pl.mu.Lock()
		if len(pl.ready) == 0 {
			pl.mu.Unlock()
			return pooled{}, errPoolEmpty
		}
		in := pl.ready[0]
		pl.ready = pl.ready[1:]
		pl.mu.Unlock()
		// From here this process owns the instance: the reaper must leave
		// it alone even once it is tagged claimed.
		pl.p.c.track(in.id)
		if err := pl.inspect(ctx, in.id, in.itype); err != nil {
			switch {
			case errors.Is(err, errChanged):
				pl.log.Warn("pool instance changed since its launch; not using it, terminating it", "instance", in.id, "why", err)
				if !pl.discard(context.WithoutCancel(ctx), in.id, in.spotReq) {
					pl.log.Warn("could not terminate the changed pool instance; it stays out of the pool", "instance", in.id)
				}
				pl.p.c.untrack(in.id)
				continue
			case instanceGone(err):
				pl.log.Warn("pool instance could not be described; discarding it", "instance", in.id, "err", err)
				pl.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
				pl.p.c.untrack(in.id)
				continue
			default:
				pl.requeue(context.WithoutCancel(ctx), in, false)
				pl.p.c.untrack(in.id)
				return pooled{}, fmt.Errorf("describe: %w", err)
			}
		}
		tagged, err := pl.prepare(ctx, in, runner)
		switch {
		case err == nil:
			return in, nil
		case isCode(err, "IncorrectInstanceState"):
			// Not stopped: something started it again (a persistent spot
			// request restarts its instance once capacity returns after an
			// interruption). It warms and powers off like a new one, so
			// follow it as warming instead of discarding it.
			pl.log.Info("ready pool instance is not stopped; following it as warming", "instance", in.id)
			pl.rewarm(context.WithoutCancel(ctx), in.id)
			pl.p.c.untrack(in.id)
			continue
		case instanceGone(err):
			pl.log.Warn("pool instance could not be prepared; discarding it", "instance", in.id, "err", err)
			pl.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
			pl.p.c.untrack(in.id)
			continue
		default:
			pl.requeue(context.WithoutCancel(ctx), in, tagged)
			pl.p.c.untrack(in.id)
			return pooled{}, err
		}
	}
}

// errChanged reports a pool instance no longer as its launch made it.
var errChanged = errors.New("changed since its launch")

// inspect describes a ready instance just before a claim prepares it, and
// returns errChanged unless it is still as its launch made it: signed by this
// install, with the image and volumes it was launched with, exactly the
// tier's security groups, in one of the tier's subnets, of the type it was
// launched as, with no instance profile, a single network interface, no
// public address and IMDSv2-only metadata. The MACs alone prove only who
// launched it; whoever may modify an instance could have swapped its root
// volume or opened its security groups while it was stopped.
func (pl *Pool) inspect(ctx context.Context, id, itype string) error {
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return err
	}
	var inst *types.Instance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				inst = &i
			}
		}
	}
	if inst == nil {
		return fmt.Errorf("%w: not described", errChanged)
	}
	var why []string
	if err := pl.verify(*inst); err != nil {
		why = append(why, err.Error())
	}
	if err := pl.p.c.verifyDiskMAC(*inst, pl.p.o.Owner, pl.p.o.Tier); err != nil {
		why = append(why, err.Error())
	}
	e := pl.p.o.EC2
	var groups []string
	for _, g := range inst.SecurityGroups {
		groups = append(groups, aws.ToString(g.GroupId))
	}
	slices.Sort(groups)
	if want := slices.Sorted(slices.Values(e.SecurityGroups)); !slices.Equal(groups, want) {
		why = append(why, fmt.Sprintf("security groups %v, want %v", groups, want))
	}
	if subnet := aws.ToString(inst.SubnetId); !slices.Contains(e.Subnets, subnet) {
		why = append(why, fmt.Sprintf("subnet %q is not one of the tier's", subnet))
	}
	if p := inst.IamInstanceProfile; p != nil {
		why = append(why, fmt.Sprintf("instance profile %s, want none", aws.ToString(p.Arn)))
	}
	if n := len(inst.NetworkInterfaces); n > 1 {
		why = append(why, fmt.Sprintf("%d network interfaces, want 1", n))
	}
	// Stopped, it has a public address only if one was associated with it.
	if ip := aws.ToString(inst.PublicIpAddress); ip != "" {
		why = append(why, fmt.Sprintf("public address %s, want none while stopped", ip))
	}
	// A bigger type costs more; the claim also reports the type it was given.
	if it := string(inst.InstanceType); it != itype || !slices.Contains(e.InstanceTypes, it) {
		why = append(why, fmt.Sprintf("instance type %s, want %s", it, itype))
	}
	want := launchMetadataOptions()
	if mo := inst.MetadataOptions; mo == nil || mo.HttpTokens != want.HttpTokens || aws.ToInt32(mo.HttpPutResponseHopLimit) != aws.ToInt32(want.HttpPutResponseHopLimit) {
		why = append(why, "metadata options are not IMDSv2-only with one hop")
	}
	if len(why) > 0 {
		return fmt.Errorf("%w: %s", errChanged, strings.Join(why, "; "))
	}
	return nil
}

// rewarm moves an instance back to warming: tagged so (and no longer naming
// a runner), and followed by refresh until it stops again, ends, or
// overstays warmMax. It is followed even if the retag fails.
func (pl *Pool) rewarm(ctx context.Context, id string) {
	if _, err := pl.p.c.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{id},
		Tags: []types.Tag{
			{Key: aws.String(TagPool), Value: aws.String(PoolWarming)},
			{Key: aws.String(TagRunner), Value: aws.String("")},
			{Key: aws.String(TagRunnerMAC), Value: aws.String("")},
		},
	}); err != nil {
		pl.log.Warn("retag pool instance warming", "instance", id, "err", err)
	}
	pl.mu.Lock()
	pl.warming[id] = pl.now()
	pl.mu.Unlock()
}

// checkReady makes sure the ready instances are still stopped. One started
// again (see claim) goes back to warming; one gone is dropped.
func (pl *Pool) checkReady(ctx context.Context) {
	pl.mu.Lock()
	ids := make([]string, 0, len(pl.ready))
	for _, in := range pl.ready {
		ids = append(ids, in.id)
	}
	pl.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		if !isCode(err, errNotFound) {
			pl.log.Warn("describe ready pool instances", "err", err)
		}
		return
	}
	state := map[string]types.InstanceStateName{}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			state[aws.ToString(i.InstanceId)] = i.State.Name
		}
	}
	for _, id := range ids {
		st, seen := state[id]
		if !seen || st == types.InstanceStateNameStopped {
			continue
		}
		in, ok := pl.takeReady(id)
		if !ok {
			continue // claimed meanwhile
		}
		switch st {
		case types.InstanceStateNameShuttingDown, types.InstanceStateNameTerminated:
			// Ended from outside. Its persistent request would launch an
			// untracked replacement: cancel it.
			pl.log.Warn("ready pool instance is gone; dropping it", "instance", id, "state", st)
			if in.spotReq != "" {
				if err := pl.cancelSpot(ctx, in.spotReq); err != nil {
					pl.log.Warn("cancel the gone instance's spot request; the reaper retries", "request", in.spotReq, "err", err)
				}
			}
		default:
			pl.log.Info("ready pool instance is not stopped; following it as warming", "instance", id, "state", st)
			pl.rewarm(ctx, id)
		}
	}
}

// takeReady removes an instance from the ready list and returns it, if it
// was there.
func (pl *Pool) takeReady(id string) (pooled, bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	for i, in := range pl.ready {
		if in.id == id {
			pl.ready = append(pl.ready[:i], pl.ready[i+1:]...)
			return in, true
		}
	}
	return pooled{}, false
}

// instanceGone reports an error about one instance's own state, which no
// other pool instance shares.
func instanceGone(err error) bool {
	return isCode(err, "IncorrectInstanceState") || isCode(err, errNotFound)
}

// requeue returns an unused instance to the pool. One the claim had already
// tagged is first tagged ready again, no longer naming the runner it was
// claimed for (that name may now be used by a cold launch). If that retag
// fails, the instance stays tagged claimed and out of the pool, and the
// reaper ends it once it is untracked.
func (pl *Pool) requeue(ctx context.Context, in pooled, tagged bool) {
	if !tagged {
		pl.mu.Lock()
		pl.ready = append([]pooled{in}, pl.ready...)
		pl.mu.Unlock()
		return
	}
	_, err := pl.p.c.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{in.id},
		Tags: []types.Tag{
			{Key: aws.String(TagPool), Value: aws.String(PoolReady)},
			{Key: aws.String(TagRunner), Value: aws.String("")},
			{Key: aws.String(TagRunnerMAC), Value: aws.String("")},
		},
	})
	if err != nil {
		pl.log.Warn("retag pool instance ready; leaving it to the reaper", "instance", in.id, "err", err)
		return
	}
	pl.mu.Lock()
	pl.ready = append([]pooled{in}, pl.ready...)
	pl.mu.Unlock()
}

// prepare readies a claimed instance to start. tagged reports whether it got
// as far as tagging the instance claimed.
func (pl *Pool) prepare(ctx context.Context, in pooled, runner string) (tagged bool, err error) {
	api := pl.p.c.api
	if _, err := api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{in.id},
		Tags: []types.Tag{
			{Key: aws.String(TagPool), Value: aws.String(PoolClaimed)},
			{Key: aws.String(TagRunner), Value: aws.String(runner)},
			{Key: aws.String(TagRunnerMAC), Value: aws.String(pl.p.c.runnerMAC(pl.p.o.Owner, pl.p.o.Tier, runner, in.id))},
			{Key: aws.String("Name"), Value: aws.String(runner)},
		},
	}); err != nil {
		return false, fmt.Errorf("tag claimed: %w", err)
	}
	if _, err := api.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(in.id),
		UserData:   &types.SecureBlobAttributeValue{Value: []byte(jitserver.UserData(pl.p.o.JITURL, pl.p.o.EC2.DockerStorage))},
	}); err != nil {
		return true, fmt.Errorf("set user-data: %w", err)
	}
	return true, nil
}

// cancelSpot cancels a persistent spot request, so terminating its instance
// does not make AWS launch a replacement. Cancelling an already cancelled
// request succeeds.
func (pl *Pool) cancelSpot(ctx context.Context, req string) error {
	_, err := pl.p.c.api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{req}})
	return err
}

// discard cancels an instance's spot request, if any, then terminates it, and
// reports whether it did. It never terminates while the request may still be
// open, since AWS would launch an untracked replacement: on a failed cancel
// the instance is left (tagged as it is) for the next sweep or rediscovery.
func (pl *Pool) discard(ctx context.Context, id, spotReq string) bool {
	api := pl.p.c.api
	if spotReq != "" {
		var err error
		for attempt := range 3 {
			if err = pl.cancelSpot(ctx, spotReq); err == nil {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			}
		}
		if err != nil {
			pl.log.Warn("cancel spot request; leaving the instance for the next sweep", "request", spotReq, "instance", id, "err", err)
			return false
		}
	}
	if _, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		pl.log.Warn("terminate pool instance", "instance", id, "err", err)
		return false
	}
	return true
}

func (pl *Pool) tag(ctx context.Context, id, key, value string) error {
	_, err := pl.p.c.api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{id},
		Tags:      []types.Tag{{Key: aws.String(key), Value: aws.String(value)}},
	})
	return err
}

func tagValue(tags []types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// poolName names a pool instance until a job claims it; it also seeds the
// launch's idempotency token, so it must be unique per launch.
func poolName(tier string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return tier + "-pool-" + hex.EncodeToString(b[:])
}
