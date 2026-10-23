package ec2

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
	"github.com/firerunnerhq/firerunner/internal/core"
	"github.com/firerunnerhq/firerunner/internal/jitserver"
)

const testJIT = "eyJqaXRjb25maWciOiJzZWNyZXQifQ=="

type harness struct {
	f   *fakeEC2
	jit *jitserver.Server
	p   *Provisioner
}

// newHarness returns an on-demand provisioner over a fake EC2 with fast
// polling; mut adjusts the options before the provisioner is built.
func newHarness(t *testing.T, mut func(*Options)) *harness {
	t.Helper()
	f := newFakeEC2(t)
	srv := jitserver.New(time.Minute, nil)
	o := Options{
		Tier:  "cloud-4c",
		Owner: "test-owner",
		EC2: config.EC2Tier{
			InstanceTypes:  []string{"c8a.xlarge", "c7a.xlarge"},
			AMI:            "ami-0abc",
			Subnets:        []string{"subnet-a", "subnet-b"},
			SecurityGroups: []string{"sg-1"},
			Market:         config.MarketOnDemand,
		},
		JIT:    srv,
		JITURL: "http://10.0.0.5:8098/jit",
		Logger: quietLog(),
	}
	if mut != nil {
		mut(&o)
	}
	c := NewClient(f, nil, 5*time.Millisecond, quietLog())
	p := c.Tier(o)
	p.tick = 5 * time.Millisecond
	p.termWait, p.termRetry = 300*time.Millisecond, 20*time.Millisecond
	return &harness{f: f, jit: o.JIT, p: p}
}

// launch runs Launch in the background and returns its result channel.
func (h *harness) launch(ctx context.Context) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- h.p.Launch(ctx, "runner-1", testJIT, core.RunnerSpec{}, nil) }()
	return errc
}

func waitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Launch did not return")
		return nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// instance waits for the first launched instance whose config is registered.
func (h *harness) instance(t *testing.T) *fakeInstance {
	t.Helper()
	var inst *fakeInstance
	eventually(t, "a launched instance with its config registered", func() bool {
		l := h.f.launched()
		if len(l) == 0 {
			return false
		}
		inst = l[0]
		return h.jit.Pending(netip.MustParseAddr(inst.ip))
	})
	return inst
}

// fetch asks the JIT endpoint as the instance at ip would.
func (h *harness) fetch(ip string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, jitserver.Path, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.jit.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestLaunch_JobRunsThenInstanceGoes(t *testing.T) {
	h := newHarness(t, nil)
	errc := h.launch(context.Background())
	inst := h.instance(t)

	if code, body := h.fetch(inst.ip); code != http.StatusOK || body != testJIT {
		t.Fatalf("guest fetch = %d %q, want its config", code, body)
	}
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	time.Sleep(20 * time.Millisecond) // a few polls while the job runs
	select {
	case err := <-errc:
		t.Fatalf("Launch returned while the instance ran: %v", err)
	default:
	}
	// The runner finishes and powers off; on-demand terminates on shutdown.
	h.f.setState(inst.id, types.InstanceStateNameShuttingDown)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v, want nil after a normal job", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v; a self-terminating instance needs no call", ids)
	}
	// The registration is gone with the instance.
	if code, _ := h.fetch(inst.ip); code != http.StatusForbidden {
		t.Fatalf("fetch after Launch returned = %d, want 403", code)
	}
}

func TestLaunch_RunInstancesInput(t *testing.T) {
	for _, mkt := range []string{config.MarketOnDemand, config.MarketSpot} {
		t.Run(mkt, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.EC2.Market = mkt
				o.EC2.RootVolumeGiB = 40
			})
			errc := h.launch(context.Background())
			inst := h.instance(t)
			h.f.setState(inst.id, types.InstanceStateNameTerminated)
			waitErr(t, errc)
			in := h.f.runs[0]

			if in.IamInstanceProfile != nil {
				t.Errorf("IamInstanceProfile = %+v, want none", in.IamInstanceProfile)
			}
			m := in.MetadataOptions
			if m == nil || m.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(m.HttpPutResponseHopLimit) != 1 {
				t.Errorf("MetadataOptions = %+v, want IMDSv2 required with hop limit 1", m)
			}
			ud, err := base64.StdEncoding.DecodeString(aws.ToString(in.UserData))
			if err != nil {
				t.Fatal(err)
			}
			if string(ud) != "FIRERUNNER_JIT_URL=http://10.0.0.5:8098/jit\n" {
				t.Errorf("user-data = %q, want only the endpoint", ud)
			}
			if strings.Contains(string(ud), testJIT) {
				t.Error("user-data contains the JIT config")
			}
			for _, ts := range in.TagSpecifications {
				got := map[string]string{}
				for _, tag := range ts.Tags {
					got[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
				}
				if got[TagTier] != "cloud-4c" || got[TagRunner] != "runner-1" || got[TagOwner] != "test-owner" {
					t.Errorf("%s tags = %v", ts.ResourceType, got)
				}
			}
			if len(in.TagSpecifications) != 2 {
				t.Errorf("tag specs = %d, want instance and volume", len(in.TagSpecifications))
			}
			if tok := aws.ToString(in.ClientToken); tok == "" || len(tok) > 64 {
				t.Errorf("ClientToken = %q, want 1-64 chars", tok)
			}
			if len(in.BlockDeviceMappings) != 1 || aws.ToString(in.BlockDeviceMappings[0].DeviceName) != "/dev/sda1" ||
				aws.ToInt32(in.BlockDeviceMappings[0].Ebs.VolumeSize) != 40 ||
				in.BlockDeviceMappings[0].Ebs.VolumeType != types.VolumeTypeGp3 ||
				!aws.ToBool(in.BlockDeviceMappings[0].Ebs.DeleteOnTermination) {
				t.Errorf("root volume = %+v", in.BlockDeviceMappings)
			}
			// Both markets terminate on the guest's poweroff: one-time spot
			// accepts it.
			if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
				t.Errorf("shutdown behaviour = %q, want terminate", in.InstanceInitiatedShutdownBehavior)
			}
			switch mkt {
			case config.MarketOnDemand:
				if in.InstanceMarketOptions != nil {
					t.Errorf("on-demand market options = %+v, want none", in.InstanceMarketOptions)
				}
			case config.MarketSpot:
				mo := in.InstanceMarketOptions
				if mo == nil || mo.MarketType != types.MarketTypeSpot || mo.SpotOptions.SpotInstanceType != types.SpotInstanceTypeOneTime {
					t.Errorf("spot market options = %+v", mo)
				}
			}
		})
	}
}

func TestLaunch_NoRootVolumeOverrideByDefault(t *testing.T) {
	h := newHarness(t, nil)
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	if m := h.f.runs[0].BlockDeviceMappings; m != nil {
		t.Fatalf("BlockDeviceMappings = %+v, want the AMI's own volume", m)
	}
}

func TestLaunch_CapacityWalkOrder(t *testing.T) {
	h := newHarness(t, nil)
	ice := apiErr("InsufficientInstanceCapacity")
	h.f.runErrs["c8a.xlarge/*/*"] = ice
	h.f.runErrs["c7a.xlarge/subnet-a/*"] = ice
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	want := "c8a.xlarge/subnet-a/on-demand c8a.xlarge/subnet-b/on-demand c7a.xlarge/subnet-a/on-demand c7a.xlarge/subnet-b/on-demand"
	if got := h.f.attemptLog(); got != want {
		t.Fatalf("attempts:\n got %s\nwant %s", got, want)
	}
	if inst.itype != "c7a.xlarge" || inst.subnet != "subnet-b" {
		t.Fatalf("launched %s in %s, want c7a.xlarge in subnet-b", inst.itype, inst.subnet)
	}
}

func TestLaunch_OtherCapacityCodesWalkToo(t *testing.T) {
	for _, code := range []string{"Unsupported", "InsufficientFreeAddressesInSubnet"} {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t, nil)
			h.f.runErrs["c8a.xlarge/subnet-a/*"] = apiErr(code)
			errc := h.launch(context.Background())
			inst := h.instance(t)
			h.f.setState(inst.id, types.InstanceStateNameTerminated)
			waitErr(t, errc)
			if inst.itype != "c8a.xlarge" || inst.subnet != "subnet-b" {
				t.Fatalf("launched %s in %s, want the next subnet", inst.itype, inst.subnet)
			}
		})
	}
}

func TestLaunch_SpotThenOnDemand(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.EC2.Market = config.MarketSpotThenOnDemand })
	h.f.runErrs["*/*/spot"] = apiErr("InsufficientInstanceCapacity")
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	waitErr(t, errc)
	if inst.spot || inst.itype != "c8a.xlarge" || inst.subnet != "subnet-a" {
		t.Fatalf("launched spot=%v %s in %s, want on-demand at the top of the list", inst.spot, inst.itype, inst.subnet)
	}
	if n := strings.Count(h.f.attemptLog(), "/spot"); n != 4 {
		t.Fatalf("%d spot attempts before falling back, want every type x subnet (4)", n)
	}
}

func TestLaunch_SpotOnlyRunsOutOfCapacity(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.EC2.Market = config.MarketSpot })
	h.f.runErrs["*/*/*"] = apiErr("InsufficientInstanceCapacity")
	err := waitErr(t, h.launch(context.Background()))
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if strings.Contains(h.f.attemptLog(), "on-demand") {
		t.Fatalf("spot-only tier tried on-demand: %s", h.f.attemptLog())
	}
	if len(h.f.launched()) != 0 {
		t.Fatal("an instance was created")
	}
}

func TestLaunch_NonCapacityErrorEndsWalk(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.EC2.Market = config.MarketSpotThenOnDemand })
	h.f.runErrs["*/*/spot"] = apiErr("MaxSpotInstanceCountExceeded")
	err := waitErr(t, h.launch(context.Background()))
	if err == nil || errors.Is(err, ErrNoCapacity) || !isCode(err, "MaxSpotInstanceCountExceeded") {
		t.Fatalf("err = %v, want the quota error itself", err)
	}
	if got := h.f.attemptLog(); got != "c8a.xlarge/subnet-a/spot" {
		t.Fatalf("attempts = %s, want one (quota fallback is a separate policy)", got)
	}
}

func TestLaunch_CancelWhileIdleTerminates(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errc := h.launch(ctx)
	inst := h.instance(t)
	h.fetch(inst.ip) // the runner is up and listening, but idle
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	cancel()
	eventually(t, "a terminate call", func() bool { return len(h.f.terminatedIDs()) > 0 })
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch after cancel = %v, want nil", err)
	}
	if ids := h.f.terminatedIDs(); ids[0] != inst.id {
		t.Fatalf("terminated %v, want %s", ids, inst.id)
	}
	if s := h.f.state(inst.id); s != types.InstanceStateNameShuttingDown {
		t.Fatalf("instance state %q, want shutting-down", s)
	}
}

func TestLaunch_CancelDuringCapacityWalk(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitErr(t, h.launch(ctx))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(h.f.launched()) != 0 {
		t.Fatal("launched an instance after cancel")
	}
}

func TestLaunch_StoppedInstanceIsTerminated(t *testing.T) {
	// On-demand: AWS cannot stop a one-time spot instance (R3 in
	// fake_test.go), but an on-demand one can be stopped, by a StopInstances
	// from outside if not by its own poweroff.
	h := newHarness(t, nil)
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.fetch(inst.ip)
	// Belt and braces: should the instance ever stop rather than terminate,
	// Launch terminates it.
	h.f.setState(inst.id, types.InstanceStateNameStopped)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v, want nil after the job", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) == 0 || ids[0] != inst.id {
		t.Fatalf("terminated %v, want the stopped instance", ids)
	}
}

func TestLaunch_ConfigNeverFetched(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.JIT = jitserver.New(30*time.Millisecond, nil) })
	errc := h.launch(context.Background())
	inst := h.instance(t)
	err := waitErr(t, errc)
	if err == nil || !strings.Contains(err.Error(), "never fetched") {
		t.Fatalf("err = %v, want a never-fetched error so the scheduler backs off", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) == 0 || ids[0] != inst.id {
		t.Fatalf("terminated %v, want the instance that never asked", ids)
	}
}

func TestLaunch_ServedConfigDoesNotExpire(t *testing.T) {
	// Once fetched, the config's TTL no longer matters: a long job runs on.
	h := newHarness(t, func(o *Options) { o.JIT = jitserver.New(30*time.Millisecond, nil) })
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.fetch(inst.ip)
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	time.Sleep(100 * time.Millisecond)
	if ids := h.f.terminatedIDs(); len(ids) != 0 {
		t.Fatalf("terminated %v while its job ran", ids)
	}
	h.f.setState(inst.id, types.InstanceStateNameShuttingDown)
	if err := waitErr(t, errc); err != nil {
		t.Fatalf("Launch = %v", err)
	}
}

func TestLaunch_MaxLifetime(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxLifetime = 50 * time.Millisecond })
	errc := h.launch(context.Background())
	inst := h.instance(t)
	h.fetch(inst.ip)
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	err := waitErr(t, errc)
	if err == nil || !strings.Contains(err.Error(), "max lifetime") {
		t.Fatalf("err = %v, want a max-lifetime error", err)
	}
	if len(h.f.terminatedIDs()) == 0 {
		t.Fatal("the stuck instance was not terminated")
	}
}

func TestLaunch_TerminateRetriedThenBounded(t *testing.T) {
	h := newHarness(t, nil)
	h.f.terminateErr = apiErr("RequestLimitExceeded")
	ctx, cancel := context.WithCancel(context.Background())
	errc := h.launch(ctx)
	inst := h.instance(t)
	h.f.setState(inst.id, types.InstanceStateNameRunning)
	cancel()
	err := waitErr(t, errc)
	if err == nil || !strings.Contains(err.Error(), "did not terminate") {
		t.Fatalf("err = %v, want a bounded give-up", err)
	}
	h.f.mu.Lock()
	calls := h.f.terminateCalls
	h.f.mu.Unlock()
	if calls < 3 {
		t.Fatalf("terminate attempted %d times, want repeated retries", calls)
	}
}

func TestLaunch_PrivateIPDescribedWhenMissing(t *testing.T) {
	h := newHarness(t, nil)
	h.f.noIPInRun = true
	errc := h.launch(context.Background())
	inst := h.instance(t) // waits for the config under the described IP
	if code, body := h.fetch(inst.ip); code != http.StatusOK || body != testJIT {
		t.Fatalf("fetch = %d %q", code, body)
	}
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestLaunch_RegistrationConflictAborts(t *testing.T) {
	// The address already holds another launch's registration (it should be
	// impossible, but a stale one must never be overwritten or revoked).
	h := newHarness(t, nil)
	other := netip.MustParseAddr("10.0.1.1") // the fake's first instance IP
	if err := h.jit.Register(other, "someone-else", "theirs"); err != nil {
		t.Fatal(err)
	}
	err := waitErr(t, h.launch(context.Background()))
	if !errors.Is(err, jitserver.ErrRegistered) {
		t.Fatalf("err = %v, want ErrRegistered", err)
	}
	if ids := h.f.terminatedIDs(); len(ids) != 1 {
		t.Fatalf("terminated %v, want the instance that could not be registered", ids)
	}
	if !h.jit.Pending(other) {
		t.Fatal("the other launch's registration was revoked")
	}
}

func TestLaunch_AMIErrorLaunchesNothing(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.EC2.AMI = "ami-0missing" })
	if err := waitErr(t, h.launch(context.Background())); err == nil {
		t.Fatal("expected an error for an unknown image")
	}
	if len(h.f.runs) != 0 {
		t.Fatal("RunInstances called without an image")
	}
}

func TestClientToken(t *testing.T) {
	a := clientToken("r1", "spot", "c8a.xlarge", "subnet-a")
	if a != clientToken("r1", "spot", "c8a.xlarge", "subnet-a") {
		t.Fatal("token not deterministic")
	}
	for _, other := range []string{
		clientToken("r2", "spot", "c8a.xlarge", "subnet-a"),
		clientToken("r1", "on-demand", "c8a.xlarge", "subnet-a"),
		clientToken("r1", "spot", "c7a.xlarge", "subnet-a"),
		clientToken("r1", "spot", "c8a.xlarge", "subnet-b"),
		clientToken("r1spot", "", "c8a.xlarge", "subnet-a"),
	} {
		if other == a {
			t.Fatal("distinct attempts share a token")
		}
	}
	if len(a) > 64 {
		t.Fatalf("token length %d > 64", len(a))
	}
}
