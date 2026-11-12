package ec2

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// signRequest tags an instance's persistent spot request with the request MAC
// c's pool would have given it, from its own owner and tier tags.
func (f *fakeEC2) signRequest(c *Client, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.instances[id]
	i.spotTags[TagRequestMAC] = c.requestMAC(i.spotTags[TagOwner], i.spotTags[TagTier], i.spotReq)
}

// keyedReaper is reaperFor with the install's pool key set, so that the
// reaper ends only what it can prove this install launched.
func keyedReaper(t *testing.T, f *fakeEC2, g *githubRunners) (*Reaper, *Client) {
	t.Helper()
	r, c := reaperFor(f, g, nil)
	if err := c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	return r, c
}

// Anyone who may tag an instance in the account can give one this install's
// owner tag, and every other tag but the MACs. Whatever the reaper would end
// it for, it ends none it cannot prove this install launched, nor cancels
// such a spot request; it reports each as unproven.
func TestReaper_EndsNothingByItsOwnerTagAlone(t *testing.T) {
	f := newFakeEC2(t)
	g := &githubRunners{}
	r, c := keyedReaper(t, f, g)
	other := NewClient(f, nil, time.Hour, quietLog()) // another key: another install
	if err := other.SetPoolKey([]byte(strings.Repeat("o", MinPoolKeyBytes))); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-24 * time.Hour) // past MaxLifetime
	forged := map[string]string{
		f.put(job("r1", TagPool, PoolClaimed), types.InstanceStateNameStopped, old, true):                  "used and left stopped",
		f.put(job("r2"), types.InstanceStateNameRunning, long, false):                                      "running past the max lifetime",
		f.put(job("", TagPool, PoolReady, TagTier, "unpooled"), types.InstanceStateNameStopped, old, true): "pool instance of a tier that keeps no pool",
		f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameStopped, old, true):   "pool instance failing its MAC",
	}
	signedElsewhere := f.put(job("r3"), types.InstanceStateNameRunning, long, true)
	f.signRunner(other, signedElsewhere)
	f.signRequest(other, signedElsewhere)
	forged[signedElsewhere] = "signed under another key"
	copied := f.put(job("r4"), types.InstanceStateNameRunning, long, false)
	genuine := f.put(job("r4"), types.InstanceStateNameRunning, long, false)
	f.signRunner(c, genuine)
	f.mu.Lock()
	f.instances[copied].tags[TagRunnerMAC] = f.instances[genuine].tags[TagRunnerMAC]
	f.mu.Unlock()
	forged[copied] = "runner MAC copied from another instance"

	// An orphan persistent request: its instance is gone.
	orphan := f.put(job("r5"), types.InstanceStateNameTerminated, old, true)

	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for id, how := range forged {
		if s := f.state(id); s == types.InstanceStateNameShuttingDown {
			t.Errorf("%s (%s) was terminated", id, how)
		}
		if !slices.Contains(rep.Unproven, id) {
			t.Errorf("%s (%s) not reported unproven: %+v", id, how, rep.Unproven)
		}
	}
	f.mu.Lock()
	for id, how := range forged {
		if req := f.instances[id].spotReq; req != "" && f.cancelled[req] {
			t.Errorf("%s (%s): its spot request was cancelled", id, how)
		}
	}
	if f.cancelled["sir-"+orphan] {
		t.Error("an unsigned orphan spot request was cancelled")
	}
	// Not this install's to cancel: its owner does, before the fake's
	// end-of-test check of open requests.
	f.cancelled["sir-"+orphan] = true
	f.mu.Unlock()
	if !slices.Contains(rep.Unproven, "sir-"+orphan) {
		t.Errorf("the unsigned orphan request is not reported unproven: %+v", rep.Unproven)
	}
	if f.state(genuine) != types.InstanceStateNameShuttingDown || !slices.Equal(rep.Instances, []string{genuine}) {
		t.Fatalf("genuine %s is %s; terminated %v", genuine, f.state(genuine), rep.Instances)
	}
	if len(g.removed) != 0 {
		t.Fatalf("runners deregistered: %v", g.removed)
	}
}

// What this install did launch, it still ends, proven by any of its MACs: a
// used pool instance by its pool MAC, a cold orphan by its runner MAC, an
// instance AWS relaunched for a pool request by the request's MAC (its own
// pool MAC names the instance it replaced), and an orphan request by its own.
func TestReaper_EndsWhatItsMACsProve(t *testing.T) {
	f := newFakeEC2(t)
	g := &githubRunners{}
	r, c := keyedReaper(t, f, g)
	long := time.Now().Add(-24 * time.Hour)

	used := f.put(job("r1", TagPool, PoolClaimed, TagTier, "pooled"), types.InstanceStateNameStopped, old, true)
	f.signPool(c, used)
	cold := f.put(job("r2"), types.InstanceStateNameRunning, long, false)
	f.signRunner(c, cold)
	relaunched := f.put(job("", TagPool, PoolReady, TagTier, "pooled"), types.InstanceStateNameStopped, old, true)
	f.signRequest(c, relaunched)
	f.mu.Lock()
	f.instances[relaunched].tags[TagPoolMAC] = c.poolMAC("test-owner", "pooled", "i-replaced")
	f.mu.Unlock()
	orphan := f.put(job("r3"), types.InstanceStateNameTerminated, old, true)
	f.signRequest(c, orphan)

	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{used, cold, relaunched} {
		if f.state(id) != types.InstanceStateNameShuttingDown || !slices.Contains(rep.Instances, id) {
			t.Errorf("%s is %s; terminated %v", id, f.state(id), rep.Instances)
		}
	}
	if !slices.Equal(rep.SpotRequests, []string{"sir-" + orphan}) {
		t.Errorf("cancelled %v, want the signed orphan request", rep.SpotRequests)
	}
	if len(rep.Unproven) != 0 {
		t.Errorf("unproven %v", rep.Unproven)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.termWithOpenRequest) != 0 {
		t.Fatalf("terminated before cancelling: %v", f.termWithOpenRequest)
	}
}

// The pool signs each spot request it launches, and, when it takes in an
// instance an earlier process launched, that instance's request, so that a
// reaper can prove the requests launched before request MACs too.
func TestPool_SignsItsSpotRequests(t *testing.T) {
	h, kept := keyedPool(t, testKey)
	h.f.mu.Lock()
	delete(h.f.instances[kept].spotTags, TagRequestMAC) // as launched before request MACs
	h.f.mu.Unlock()
	p := h.restart(t, testKey)
	eventually(t, "the instance rediscovered", func() bool { r, _ := p.pool.Ready(); return r == 1 })
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for _, i := range h.f.instances {
		if i.spotReq == "" {
			continue
		}
		if want := p.c.requestMAC("test-owner", "cloud-4c", i.spotReq); i.spotTags[TagRequestMAC] != want {
			t.Errorf("%s's request %s: request MAC %q, want %q", i.id, i.spotReq, i.spotTags[TagRequestMAC], want)
		}
	}
}
