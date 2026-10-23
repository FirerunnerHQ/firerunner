package ec2

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/solcreek/firerunner/internal/core"
)

// put adds an instance to the fake as a previous firerunner left it.
func (f *fakeEC2) put(tags map[string]string, state types.InstanceStateName, launched time.Time, spot bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	i := &fakeInstance{
		id: fmt.Sprintf("i-%04d", f.next), ip: fmt.Sprintf("10.0.2.%d", f.next),
		itype: "c7a.large", subnet: "subnet-a", state: state, tags: map[string]string{},
		spotTags: map[string]string{}, launched: launched,
	}
	for k, v := range tags {
		i.tags[k] = v
	}
	if spot {
		i.spot = true
		i.spotReq = "sir-" + i.id
		for k, v := range tags {
			i.spotTags[k] = v
		}
	}
	f.instances[i.id] = i
	f.order = append(f.order, i.id)
	return i.id
}

// githubRunners plays GitHub for the reaper: busy runners refuse removal.
type githubRunners struct {
	busy    map[string]bool
	removed []string
	failing bool
}

func (g *githubRunners) remove(_ context.Context, _, runner string) error {
	if g.failing {
		return fmt.Errorf("503")
	}
	if g.busy[runner] {
		return core.ErrRunnerBusy
	}
	g.removed = append(g.removed, runner)
	return nil
}

func reaperFor(f *fakeEC2, g *githubRunners, mut func(*ReaperOptions)) (*Reaper, *Client) {
	c := NewClient(f, nil, time.Hour, quietLog())
	o := ReaperOptions{Owner: "test-owner", Remove: g.remove, MaxLifetime: 6 * time.Hour, PooledTiers: map[string]bool{"pooled": true}}
	if mut != nil {
		mut(&o)
	}
	return c.NewReaper(o), c
}

var old = time.Now().Add(-10 * time.Minute)

func job(runner string, extra ...string) map[string]string {
	t := map[string]string{TagOwner: "test-owner", TagTier: "cloud", TagRunner: runner}
	for i := 0; i+1 < len(extra); i += 2 {
		t[extra[i]] = extra[i+1]
	}
	return t
}

func TestReaper_StoppedOrphanIsTerminatedWithItsRequestCancelled(t *testing.T) {
	// A pooled instance a crashed firerunner had claimed: its job ended and
	// it stopped. Nobody will terminate it but the reaper.
	f := newFakeEC2()
	id := f.put(job("r1", TagPool, PoolClaimed), types.InstanceStateNameStopped, old, true)
	g := &githubRunners{}
	r, _ := reaperFor(f, g, nil)
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Instances, []string{id}) || f.state(id) != types.InstanceStateNameShuttingDown {
		t.Fatalf("report %+v, state %q", rep, f.state(id))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.cancelled["sir-"+id] || len(f.termBeforeCancel) != 0 {
		t.Fatalf("spot request cancelled=%v, terminated-before-cancel %v", f.cancelled["sir-"+id], f.termBeforeCancel)
	}
}

func TestReaper_RunningOrphanIdleIsDeregisteredAndTerminated(t *testing.T) {
	f := newFakeEC2()
	id := f.put(job("idle-runner"), types.InstanceStateNameRunning, old, false)
	g := &githubRunners{}
	r, _ := reaperFor(f, g, nil)
	r.Sweep(context.Background())
	if !slices.Equal(g.removed, []string{"idle-runner"}) || f.state(id) != types.InstanceStateNameShuttingDown {
		t.Fatalf("removed %v, state %q", g.removed, f.state(id))
	}
}

func TestReaper_RunningOrphanBusyIsLeftUntilItStops(t *testing.T) {
	// The chaos case: firerunner killed mid-job and restarted. The job must
	// finish; the instance is ended by the first sweep after it stops.
	f := newFakeEC2()
	id := f.put(job("busy-runner", TagPool, PoolClaimed), types.InstanceStateNameRunning, old, true)
	g := &githubRunners{busy: map[string]bool{"busy-runner": true}}
	r, _ := reaperFor(f, g, nil)
	rep, _ := r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameRunning || !slices.Equal(rep.Busy, []string{id}) {
		t.Fatalf("busy orphan: state %q report %+v", f.state(id), rep)
	}
	f.setState(id, types.InstanceStateNameStopped) // the job ended; it powered off
	r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameShuttingDown || len(f.openSpotRequests()) != 0 {
		t.Fatalf("after the job: state %q open requests %v", f.state(id), f.openSpotRequests())
	}
}

func TestReaper_LeavesWhatItMustNotTouch(t *testing.T) {
	f := newFakeEC2()
	live := f.put(job("live"), types.InstanceStateNameRunning, old, false)
	young := f.put(job("young"), types.InstanceStateNameRunning, time.Now(), false)
	pooled := f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameStopped, old, true)
	foreign := f.put(map[string]string{TagOwner: "someone-else", TagTier: "cloud", TagRunner: "x"}, types.InstanceStateNameStopped, old, false)
	g := &githubRunners{}
	r, c := reaperFor(f, g, nil)
	c.track(live)
	rep, _ := r.Sweep(context.Background())
	if len(rep.Instances) != 0 || len(g.removed) != 0 {
		t.Fatalf("reaped %v, deregistered %v", rep.Instances, g.removed)
	}
	for _, id := range []string{live, young, pooled, foreign} {
		if s := f.state(id); s == types.InstanceStateNameShuttingDown {
			t.Errorf("%s was terminated", id)
		}
	}
}

func TestReaper_PoolInstanceOfATierWithoutAPool(t *testing.T) {
	f := newFakeEC2()
	id := f.put(job("", TagPool, PoolReady, TagTier, "unpooled"), types.InstanceStateNameStopped, old, true)
	r, _ := reaperFor(f, &githubRunners{}, nil)
	r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameShuttingDown {
		t.Fatalf("state %q, want the leftover pool instance terminated", f.state(id))
	}
}

func TestReaper_MaxLifetimeEndsEvenABusyOrphan(t *testing.T) {
	f := newFakeEC2()
	id := f.put(job("stuck"), types.InstanceStateNameRunning, time.Now().Add(-7*time.Hour), false)
	g := &githubRunners{busy: map[string]bool{"stuck": true}}
	r, _ := reaperFor(f, g, nil)
	r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameShuttingDown {
		t.Fatalf("state %q, want a 7h-old orphan terminated", f.state(id))
	}
}

func TestReaper_LookupFailureLeavesTheOrphan(t *testing.T) {
	f := newFakeEC2()
	id := f.put(job("r"), types.InstanceStateNameRunning, old, false)
	r, _ := reaperFor(f, &githubRunners{failing: true}, nil)
	r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameRunning {
		t.Fatalf("state %q: terminated without knowing whether it ran a job", f.state(id))
	}
}

func TestReaper_CancelsSpotRequestsWithoutAnInstance(t *testing.T) {
	f := newFakeEC2()
	gone := f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameTerminated, old, true)
	kept := f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameStopped, old, true)
	r, _ := reaperFor(f, &githubRunners{}, nil)
	rep, _ := r.Sweep(context.Background())
	if !slices.Equal(rep.SpotRequests, []string{"sir-" + gone}) {
		t.Fatalf("cancelled %v, want only the request whose instance is gone", rep.SpotRequests)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled["sir-"+kept] {
		t.Fatal("cancelled the request of a live pool instance")
	}
}

func TestReaper_DryRunChangesNothing(t *testing.T) {
	f := newFakeEC2()
	stopped := f.put(job("a", TagPool, PoolClaimed), types.InstanceStateNameStopped, old, true)
	running := f.put(job("b"), types.InstanceStateNameRunning, old, false)
	gone := f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameTerminated, old, true)
	g := &githubRunners{}
	r, _ := reaperFor(f, g, func(o *ReaperOptions) { o.DryRun = true })
	rep, _ := r.Sweep(context.Background())
	if len(rep.Instances) != 2 || len(rep.SpotRequests) != 1 {
		t.Fatalf("dry-run report %+v, want 2 instances and 1 request", rep)
	}
	if f.state(stopped) != types.InstanceStateNameStopped || f.state(running) != types.InstanceStateNameRunning || len(g.removed) != 0 {
		t.Fatal("dry run changed something")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelled["sir-"+gone] || len(f.terminated) != 0 {
		t.Fatal("dry run cancelled or terminated")
	}
}

func TestReaper_NeverTerminatesWithAnOpenRequest(t *testing.T) {
	f := newFakeEC2()
	id := f.put(job("a", TagPool, PoolClaimed), types.InstanceStateNameStopped, old, true)
	f.cancelErrs = 1
	r, _ := reaperFor(f, &githubRunners{}, nil)
	r.Sweep(context.Background())
	if f.state(id) != types.InstanceStateNameStopped {
		t.Fatal("terminated although the spot request could not be cancelled")
	}
	r.Sweep(context.Background()) // the next sweep succeeds
	if f.state(id) != types.InstanceStateNameShuttingDown {
		t.Fatalf("state %q after the retry", f.state(id))
	}
}

func TestReaper_ClaimedByThisProcessIsLeftAlone(t *testing.T) {
	// A claim in flight is tagged claimed but owned here: never reaped.
	h := readyPool(t, 1)
	in, err := h.p.pool.claim(context.Background(), "runner-x")
	if err != nil {
		t.Fatal(err)
	}
	h.f.mu.Lock()
	h.f.instances[in.id].launched = old
	h.f.mu.Unlock()
	r := h.p.c.NewReaper(ReaperOptions{Owner: "test-owner", Remove: (&githubRunners{}).remove, PooledTiers: map[string]bool{"cloud-4c": true}})
	rep, _ := r.Sweep(context.Background())
	if len(rep.Instances) != 0 {
		t.Fatalf("reaped a claim this process holds: %v", rep.Instances)
	}
}
