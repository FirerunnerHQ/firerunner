package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/cachetls"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
)

// fakeJobs records the runner each address is bound to.
type fakeJobs struct {
	mu       sync.Mutex
	bound    map[string]string // runner -> ip
	released []string
	fail     error
}

func (j *fakeJobs) Register(runner, ip string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail != nil {
		return j.fail
	}
	j.bound[runner] = ip
	return nil
}

func (j *fakeJobs) Release(runner string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.bound, runner)
	j.released = append(j.released, runner)
	return nil
}

func (j *fakeJobs) ipOf(runner string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.bound[runner]
}

func cacheOpts(t *testing.T, jobs VMJobs) *CacheOptions {
	t.Helper()
	ca, err := cachetls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &CacheOptions{CA: ca, Upstream: "10.0.0.5:8099", Jobs: jobs, LeafTTL: 7 * time.Hour}
}

// cacheBundleCode asks for the instance's bundle without fetching its config
// first, and returns only the status.
func (h *harness) cacheBundleCode(ip string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, jitserver.CachePath, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.jit.ServeHTTP(rec, req)
	return rec.Code, ""
}

// cacheBundle fetches the instance's bundle as its boot script would, after
// its config.
func (h *harness) cacheBundle(t *testing.T, ip string) (int, cachetls.Bundle) {
	t.Helper()
	if code, _ := h.fetch(ip); code != http.StatusOK {
		t.Fatalf("config fetch = %d", code)
	}
	req := httptest.NewRequest(http.MethodGet, jitserver.CachePath, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.jit.ServeHTTP(rec, req)
	var b cachetls.Bundle
	if rec.Code == http.StatusOK {
		raw, _ := io.ReadAll(rec.Body)
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("bundle: %v", err)
		}
	}
	return rec.Code, b
}

func TestLaunch_CachedTierGetsABundleAndAnIdentity(t *testing.T) {
	jobs := &fakeJobs{bound: map[string]string{}}
	h := newHarness(t, func(o *Options) {
		o.EC2.Cache = true
		o.Cache = cacheOpts(t, jobs)
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if got := jobs.ipOf("runner-1"); got != inst.ip {
		t.Fatalf("runner-1 bound to %q, want its instance %s", got, inst.ip)
	}
	code, b := h.cacheBundle(t, inst.ip)
	if code != http.StatusOK || b.Upstream != "10.0.0.5:8099" || b.Key == "" || b.Cert == "" || b.CA == "" {
		t.Fatalf("bundle = %d %+v, want one with the upstream, a leaf and the CA", code, b.Upstream)
	}
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if jobs.ipOf("runner-1") != "" || len(jobs.released) != 1 {
		t.Fatalf("identity not released with the instance: %+v", jobs)
	}
}

func TestLaunch_GitCacheTierGetsAGitIdentity(t *testing.T) {
	git, err := cachetls.LoadOrCreateFor(t.TempDir(), cachetls.GitHost)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.EC2.Cache, o.EC2.GitCache = true, true
		o.Cache = cacheOpts(t, nil)
		o.Cache.GitCA = git
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	code, b := h.cacheBundle(t, inst.ip)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	if code != http.StatusOK || b.Git == nil || b.Git.CA != string(git.CertPEM()) {
		t.Fatalf("bundle = %d, git identity %+v", code, b.Git)
	}
}

// A deployment may have the git CA while this tier does not ask for it: the
// identity for github.com goes only where a tier chose it.
func TestLaunch_CachedTierWithoutGitCacheGetsNoGitIdentity(t *testing.T) {
	git, err := cachetls.LoadOrCreateFor(t.TempDir(), cachetls.GitHost)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) {
		o.EC2.Cache = true
		o.Cache = cacheOpts(t, nil)
		o.Cache.GitCA = git
	})
	errc := h.launch(context.Background())
	inst := h.instance(t)
	code, b := h.cacheBundle(t, inst.ip)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	if code != http.StatusOK || b.Git != nil {
		t.Fatalf("bundle = %d, git identity %+v", code, b.Git)
	}
}

func TestLaunch_UncachedTierGetsNoBundle(t *testing.T) {
	jobs := &fakeJobs{bound: map[string]string{}}
	h := newHarness(t, func(o *Options) { o.Cache = cacheOpts(t, jobs) }) // ec2.cache off
	errc := h.launch(context.Background())
	inst := h.instance(t)
	if code, _ := h.cacheBundle(t, inst.ip); code != http.StatusNotFound {
		t.Fatalf("bundle for an uncached tier = %d, want 404", code)
	}
	if jobs.ipOf("runner-1") != "" {
		t.Fatal("an uncached tier bound a cache identity")
	}
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
}

// TestLaunch_IdentityFailureFailsClosed: cache-server knows a job by its
// address alone, and a binding can fail because another live job holds the
// address; the instance must then get neither its config nor a bundle, or
// its cache calls would run in that job's scope.
func TestLaunch_IdentityFailureFailsClosed(t *testing.T) {
	jobs := &fakeJobs{bound: map[string]string{}, fail: errors.New("address already registered")}
	h := newHarness(t, func(o *Options) {
		o.EC2.Cache = true
		o.Cache = cacheOpts(t, jobs)
	})
	err := waitErr(t, h.launch(context.Background()))
	if err == nil || !strings.Contains(err.Error(), "bind cache identity") {
		t.Fatalf("Launch = %v, want it to fail on the binding", err)
	}
	inst := h.f.launched()[0]
	if h.jit.Pending(netip.MustParseAddr(inst.ip)) {
		t.Fatal("the instance's config is still registered after the binding failed")
	}
	if len(jobs.released) != 0 {
		t.Fatal("released an identity that was never bound")
	}
	if len(h.f.terminatedIDs()) == 0 {
		t.Fatal("the instance was left running")
	}
}

func TestLaunch_PooledCachedTierGetsABundle(t *testing.T) {
	jobs := &fakeJobs{bound: map[string]string{}}
	h := readyPool(t, 1)
	h.p.o.EC2.Cache = true
	h.p.o.Cache = cacheOpts(t, jobs)
	pooledID := h.f.launched()[0].id
	errc := h.launch(context.Background())
	eventually(t, "the pooled instance to start", func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return len(h.f.started) == 1
	})
	in := h.f.instance(pooledID)
	if code, b := h.cacheBundle(t, in.ip); code != http.StatusOK || b.Upstream == "" {
		t.Fatalf("pooled bundle = %d", code)
	}
	if jobs.ipOf("runner-1") != in.ip {
		t.Fatalf("pooled runner bound to %q, want %s", jobs.ipOf("runner-1"), in.ip)
	}
	h.f.setState(pooledID, types.InstanceStateNameShuttingDown)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
	if jobs.ipOf("runner-1") != "" || len(jobs.released) != 1 {
		t.Fatalf("pooled identity not released with the instance: %+v", jobs)
	}
}
