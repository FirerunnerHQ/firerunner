package ec2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/solcreek/firerunner/internal/jitserver"
)

// Pool keeps a tier's pre-warmed instances stopped and ready to start.
//
// A pool instance is launched with no user-data. Its boot script finds no
// endpoint, reads the runner's files once (so EBS has fetched those blocks
// from the snapshot), and powers off. The instance stops and waits. A spot
// instance stays on a persistent request, still billed at the spot price
// while running. A job claims a ready instance:
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
// its ready instances again, and terminates the ones a crash left claimed.
type Pool struct {
	p      *Provisioner
	target int
	log    *slog.Logger
	tick   time.Duration
	now    func() time.Time

	mu       sync.Mutex
	ready    []pooled
	warming  map[string]bool
	wake     chan struct{}
	failures int
	retryAt  time.Time
}

// pooled is a ready, stopped pool instance.
type pooled struct {
	id, ip, spotReq, itype string
}

// poolRetryMax caps the backoff between failed warm launches.
const poolRetryMax = 5 * time.Minute

func newPool(p *Provisioner, target int) *Pool {
	return &Pool{
		p: p, target: target, log: p.log.With("pool", target), tick: 15 * time.Second, now: time.Now,
		warming: map[string]bool{}, wake: make(chan struct{}, 1),
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
	for {
		pl.refresh(ctx)
		pl.fill(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-pl.wake:
		}
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
// pool, warming ones are followed until they stop, and claimed ones, left by a
// firerunner that died mid-job, are terminated.
func (pl *Pool) discover(ctx context.Context) error {
	out, err := pl.p.c.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: pl.filters()})
	if err != nil {
		return err
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			id, state := aws.ToString(i.InstanceId), i.State.Name
			switch tagValue(i.Tags, TagPool) {
			case PoolClaimed:
				pl.log.Info("terminating a pool instance a previous run claimed", "instance", id)
				pl.discard(ctx, id, aws.ToString(i.SpotInstanceRequestId))
			case PoolReady, PoolWarming:
				if state == types.InstanceStateNameStopped {
					pl.markReady(ctx, i)
				} else {
					pl.mu.Lock()
					pl.warming[id] = true
					pl.mu.Unlock()
				}
			}
		}
	}
	r, w := pl.Ready()
	pl.log.Info("pool rediscovered", "ready", r, "warming", w)
	return nil
}

// refresh moves warming instances that have stopped into the pool.
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
				pl.mu.Lock()
				delete(pl.warming, aws.ToString(i.InstanceId))
				pl.mu.Unlock()
			}
		}
	}
}

// markReady tags a stopped pool instance ready and adds it to the pool.
func (pl *Pool) markReady(ctx context.Context, i types.Instance) {
	id := aws.ToString(i.InstanceId)
	if err := pl.tag(ctx, id, TagPool, PoolReady); err != nil {
		pl.log.Warn("tag pool instance ready", "instance", id, "err", err)
		return
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	delete(pl.warming, id)
	pl.ready = append(pl.ready, pooled{
		id: id, ip: aws.ToString(i.PrivateIpAddress),
		spotReq: aws.ToString(i.SpotInstanceRequestId), itype: string(i.InstanceType),
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
		inst, err := pl.p.run(ctx, poolName(pl.p.o.Tier), img, forPool)
		if err != nil {
			pl.failed(err)
			return
		}
		id := aws.ToString(inst.InstanceId)
		pl.log.Info("warming pool instance", "instance", id, "type", inst.InstanceType)
		pl.mu.Lock()
		pl.warming[id] = true
		pl.failures = 0
		pl.mu.Unlock()
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
// request stays open until it has started (see cancelSpot). An instance that
// cannot be prepared is terminated and the next one is tried.
func (pl *Pool) claim(ctx context.Context, runner string) (pooled, error) {
	defer pl.poke() // refill behind the claim
	for {
		pl.mu.Lock()
		if len(pl.ready) == 0 {
			pl.mu.Unlock()
			return pooled{}, errPoolEmpty
		}
		in := pl.ready[0]
		pl.ready = pl.ready[1:]
		pl.mu.Unlock()
		if err := pl.prepare(ctx, in, runner); err != nil {
			pl.log.Warn("pool instance could not be prepared; discarding it", "instance", in.id, "err", err)
			pl.discard(context.WithoutCancel(ctx), in.id, in.spotReq)
			continue
		}
		return in, nil
	}
}

func (pl *Pool) prepare(ctx context.Context, in pooled, runner string) error {
	api := pl.p.c.api
	if _, err := api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{in.id},
		Tags: []types.Tag{
			{Key: aws.String(TagPool), Value: aws.String(PoolClaimed)},
			{Key: aws.String(TagRunner), Value: aws.String(runner)},
			{Key: aws.String("Name"), Value: aws.String(runner)},
		},
	}); err != nil {
		return fmt.Errorf("tag claimed: %w", err)
	}
	if _, err := api.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: aws.String(in.id),
		UserData:   &types.SecureBlobAttributeValue{Value: []byte(jitserver.UserData(pl.p.o.JITURL))},
	}); err != nil {
		return fmt.Errorf("set user-data: %w", err)
	}
	return nil
}

// cancelSpot cancels a persistent spot request, so terminating its instance
// does not make AWS launch a replacement. Cancelling an already cancelled
// request succeeds.
func (pl *Pool) cancelSpot(ctx context.Context, req string) error {
	_, err := pl.p.c.api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{req}})
	return err
}

// discard cancels an instance's spot request, if any, then terminates it.
func (pl *Pool) discard(ctx context.Context, id, spotReq string) {
	api := pl.p.c.api
	if spotReq != "" {
		if _, err := api.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{spotReq}}); err != nil {
			pl.log.Warn("cancel spot request", "request", spotReq, "err", err)
		}
	}
	if _, err := api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		pl.log.Warn("terminate pool instance", "instance", id, "err", err)
	}
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
