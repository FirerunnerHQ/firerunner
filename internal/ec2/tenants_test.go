package ec2

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/core"
)

// Two tenants' tiers share one JIT server, as on a shard. When AWS gives tenant
// B's instance the private address tenant A's just released, B gets its own
// config and nothing of A's: not A's config, not A's cache bundle, and A's
// cache identity is gone with A's instance (#168).
func TestTenantReusingAnotherTenantsAddressGetsOnlyItsOwn(t *testing.T) {
	jobs := &fakeJobs{bound: map[string]string{}}
	a := newHarness(t, func(o *Options) {
		o.Owner = "base"
		o.EC2.Cache = true
		o.Cache = cacheOpts(t, jobs)
	})
	errA := a.launch(context.Background())
	instA := a.instance(t)
	if code, body := a.fetch(instA.ip); code != http.StatusOK || !strings.Contains(body, testJIT) {
		t.Fatalf("tenant A's own fetch = %d", code)
	}
	if jobs.ipOf("runner-1") != instA.ip {
		t.Fatal("tenant A's cache identity was not bound to its instance")
	}
	a.f.setStateFor(instA.id, types.InstanceStateNameTerminated, "Client.UserInitiatedShutdown")
	if err := waitErr(t, errA); err != nil {
		t.Fatal(err)
	}
	if jobs.ipOf("runner-1") != "" {
		t.Fatal("tenant A's cache identity outlived its instance")
	}

	// Tenant B: another owner on the same server and account. As on a shard,
	// it is handed the shard's cache options, but its tier's cache is off
	// (config.TenantTiers).
	ob := a.p.o
	ob.Owner, ob.EC2.Cache = "base/t1", false
	b := NewClient(a.f, nil, 5*time.Millisecond, quietLog()).Tier(ob)
	b.tick = 5 * time.Millisecond
	b.termWait, b.termRetry = 300*time.Millisecond, 20*time.Millisecond

	// Before B registers, the address holds nothing at all: 403, as for an
	// address never registered, not 404, a tombstone of A's.
	if code, _ := a.fetch(instA.ip); code != http.StatusForbidden {
		t.Fatalf("a released address answers %d, want 403", code)
	}

	a.f.mu.Lock()
	a.f.nextIP = instA.ip
	a.f.mu.Unlock()
	const configB = "dGVuYW50LWI="
	errB := make(chan error, 1)
	go func() { errB <- b.Launch(context.Background(), "runner-b", configB, core.RunnerSpec{}, nil) }()
	var instB *fakeInstance
	eventually(t, "tenant B's instance on the reused address", func() bool {
		l := a.f.launched()
		if len(l) < 2 {
			return false
		}
		instB = l[1]
		return a.jit.Pending(netip.MustParseAddr(instB.ip))
	})
	if instB.ip != instA.ip {
		t.Fatalf("instance B at %s, want the reused %s", instB.ip, instA.ip)
	}
	a.f.mu.Lock()
	ownerB := instB.tags[TagOwner]
	a.f.mu.Unlock()
	if ownerB != "base/t1" {
		t.Fatalf("instance B owner %q", ownerB)
	}
	code, body := a.fetch(instB.ip)
	if code != http.StatusOK || !strings.Contains(body, configB) || strings.Contains(body, testJIT) {
		t.Fatalf("tenant B's fetch = %d, its own config %v, A's config %v", code, strings.Contains(body, configB), strings.Contains(body, testJIT))
	}
	if code, _ := a.cacheBundleCode(instB.ip); code == http.StatusOK {
		t.Fatal("tenant B was handed a cache bundle")
	}
	a.f.setStateFor(instB.id, types.InstanceStateNameTerminated, "Client.UserInitiatedShutdown")
	if err := waitErr(t, errB); err != nil {
		t.Fatal(err)
	}
}
