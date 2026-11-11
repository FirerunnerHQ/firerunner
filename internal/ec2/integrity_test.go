package ec2

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/firerunnerhq/firerunner/internal/config"
)

func TestDiskMAC_BindsTheRootDevice(t *testing.T) {
	c := NewClient(newFakeEC2(t), nil, 0, quietLog())
	mapping := func(dev, vol string) types.InstanceBlockDeviceMapping {
		return types.InstanceBlockDeviceMapping{DeviceName: aws.String(dev), Ebs: &types.EbsInstanceBlockDevice{VolumeId: aws.String(vol)}}
	}
	inst := types.Instance{
		InstanceId: aws.String("i-1"), ImageId: aws.String("ami-0abc"), RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []types.InstanceBlockDeviceMapping{mapping("/dev/sda1", "vol-root"), mapping("/dev/sdf", "vol-data")},
	}
	image, root, volumes, ok := diskOf(inst)
	if !ok {
		t.Fatal("the root volume is listed, yet not reported")
	}
	inst.Tags = []types.Tag{{Key: aws.String(TagPoolDiskMAC), Value: aws.String(c.diskMAC("o", "t", "i-1", image, root, volumes))}}
	if err := c.verifyDiskMAC(inst, "o", "t"); err != nil {
		t.Fatalf("genuine: %v", err)
	}
	// The same volumes, but the instance now boots from the data volume.
	inst.RootDeviceName = aws.String("/dev/sdf")
	if err := c.verifyDiskMAC(inst, "o", "t"); !errors.Is(err, errBadDiskMAC) {
		t.Fatalf("root device moved to another volume: %v", err)
	}
	// Only a volume on the root device counts as listed.
	if _, _, _, ok := diskOf(types.Instance{RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []types.InstanceBlockDeviceMapping{mapping("/dev/sdf", "vol-data")}}); ok {
		t.Fatal("reported the root volume listed when only a data volume is")
	}
}

func TestMAC_DomainsAreDistinct(t *testing.T) {
	// Each domain is written raw before the fields: one that prefixes
	// another could let a MAC of one kind pass as the other.
	domains := []string{poolMACDomain, diskMACDomain, runnerMACDomain}
	for _, a := range domains {
		for _, b := range domains {
			if a != b && strings.HasPrefix(a, b) {
				t.Errorf("domain %q starts with domain %q", a, b)
			}
		}
	}
	c := NewClient(newFakeEC2(t), nil, 0, quietLog())
	if c.mac(poolMACDomain, "o", "t", "i-1") == c.mac(runnerMACDomain, "o", "t", "i-1") {
		t.Fatal("a pool MAC equals a runner MAC over the same fields")
	}
}

// changes are what someone with rights to modify the account's instances
// could do to a stopped pool instance; a claim must refuse each.
var changes = map[string]func(i *fakeInstance){
	"root volume swapped":  func(i *fakeInstance) { i.volumes[i.rootDevice] = "vol-swapped" },
	"root volume detached": func(i *fakeInstance) { delete(i.volumes, i.rootDevice) },
	"a volume attached":    func(i *fakeInstance) { i.volumes["/dev/sdf"] = "vol-extra" },
	"another image":        func(i *fakeInstance) { i.image = "ami-other" },
	"root device renamed": func(i *fakeInstance) {
		v, at := i.volumes[i.rootDevice], i.attachedAt[i.rootDevice]
		delete(i.volumes, i.rootDevice)
		i.rootDevice = "/dev/xvda"
		i.volumes[i.rootDevice], i.attachedAt[i.rootDevice] = v, at
	},
	// Detached, altered while attached elsewhere, and attached again: the
	// same volume on the same device, but a later attach time.
	"root volume reattached":      func(i *fakeInstance) { i.attachedAt[i.rootDevice] = i.attachedAt[i.rootDevice].Add(time.Minute) },
	"root volume kept after end":  func(i *fakeInstance) { i.kept[i.rootDevice] = true },
	"instance type changed":       func(i *fakeInstance) { i.itype = "c7a.48xlarge" },
	"another of the tier.s types": func(i *fakeInstance) { i.itype = "c7a.xlarge" },
	"public address associated":   func(i *fakeInstance) { i.publicIP = "203.0.113.7" },
	"IMDSv1 allowed":              func(i *fakeInstance) { i.tokens = types.HttpTokensStateOptional },
	"metadata hop limit raised":   func(i *fakeInstance) { i.hops = 2 },
	"a security group added":      func(i *fakeInstance) { i.groups = append(i.groups, "sg-open") },
	"security group replaced":     func(i *fakeInstance) { i.groups = []string{"sg-open"} },
	"security groups removed":     func(i *fakeInstance) { i.groups = nil },
	"subnet not the tier's":       func(i *fakeInstance) { i.subnet = "subnet-z" },
	"instance profile attached":   func(i *fakeInstance) { i.profile = "arn:aws:iam::000000000000:instance-profile/x" },
	"a network interface added":   func(i *fakeInstance) { i.enis = 2 },
	"disk MAC removed":            func(i *fakeInstance) { delete(i.tags, TagPoolDiskMAC) },
	"pool MAC removed":            func(i *fakeInstance) { delete(i.tags, TagPoolMAC) },
}

func TestPoolClaim_RefusesAChangedInstance(t *testing.T) {
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			h := readyPool(t, 1)
			id := h.f.launched()[0].id
			h.f.mu.Lock()
			change(h.f.instances[id])
			h.f.mu.Unlock()
			_, err := h.p.pool.claim(context.Background(), "runner-x")
			if !errors.Is(err, errPoolEmpty) {
				t.Fatalf("claim = %v, want the changed instance refused and the pool empty", err)
			}
			in := h.f.instance(id)
			if in.tags[TagPool] == PoolClaimed || in.tags[TagRunner] != "" || in.userData != "" {
				t.Fatalf("the changed instance was prepared: tags %v, user-data %q", in.tags, in.userData)
			}
			// Discarded as any unusable pool instance: its persistent
			// request cancelled first (the fake's R10 check), then ended.
			if in.state != types.InstanceStateNameShuttingDown {
				t.Fatalf("state %s, want the changed instance terminated", in.state)
			}
			h.f.mu.Lock()
			started := len(h.f.started)
			h.f.mu.Unlock()
			if started != 0 || h.p.c.isLive(id) {
				t.Fatalf("started %d, tracked %v", started, h.p.c.isLive(id))
			}
		})
	}
}

func TestPoolClaim_RefusesATypeTheTierNoLongerLists(t *testing.T) {
	// The catalog changed since the pool warmed: an instance of a type the
	// tier dropped is not started, as a new launch would not use it.
	h := readyPool(t, 1)
	id := h.f.launched()[0].id
	h.p.o.EC2.InstanceTypes = []string{"c7a.xlarge"}
	if _, err := h.p.pool.claim(context.Background(), "runner-x"); !errors.Is(err, errPoolEmpty) {
		t.Fatalf("claim = %v, want the instance of a dropped type refused", err)
	}
	if s := h.f.state(id); s != types.InstanceStateNameShuttingDown {
		t.Fatalf("state %s, want it terminated", s)
	}
}

func TestPoolClaim_TakesAnUnchangedInstance(t *testing.T) {
	// The tier lists its groups in another order than EC2 does: no change.
	h := poolHarness(t, 1, config.MarketSpot)
	h.p.o.EC2.SecurityGroups = []string{"sg-2", "sg-1"}
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	id := h.f.launched()[0].id
	h.f.mu.Lock()
	h.f.instances[id].groups = []string{"sg-1", "sg-2"}
	h.f.mu.Unlock()
	h.stopWarming()
	eventually(t, "ready", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
	in, err := h.p.pool.claim(context.Background(), "runner-x")
	if err != nil || in.id != id {
		t.Fatalf("claim = %+v, %v; want %s", in, err, id)
	}
	if got := h.f.instance(id).tags[TagPool]; got != PoolClaimed {
		t.Fatalf("tagged %q, want claimed", got)
	}
	h.p.c.untrack(id)
	h.p.pool.discard(context.Background(), id, in.spotReq)
}

func TestPoolClaim_ChangedInstanceFallsBackCold(t *testing.T) {
	// Through Launch: the job does not wait on the refused instance, and its
	// address is never registered for a JIT config.
	h := readyPool(t, 1)
	pooled := h.f.launched()[0]
	h.f.mu.Lock()
	h.f.instances[pooled.id].groups = append(h.f.instances[pooled.id].groups, "sg-open")
	h.f.mu.Unlock()
	errc := h.launch(context.Background())
	var cold *fakeInstance
	eventually(t, "a cold launch", func() bool {
		for _, i := range h.f.launched() {
			if i.tags[TagRunner] == "runner-1" && i.tags[TagPool] == "" {
				cold = i
				return true
			}
		}
		return false
	})
	if code, _ := h.fetch(pooled.ip); code == http.StatusOK {
		t.Fatal("the changed instance's address was served a JIT config")
	}
	eventually(t, "the cold instance registered", func() bool { return h.jit.Pending(netip.MustParseAddr(cold.ip)) })
	h.f.setState(cold.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
	if s := h.f.state(pooled.id); s != types.InstanceStateNameShuttingDown {
		t.Fatalf("the changed pool instance is %s", s)
	}
}

func TestPoolClaim_DescribeFailureKeepsThePool(t *testing.T) {
	// A describe error that is not about the instance (throttling) would
	// fail for every one alike: the instance stays, the job goes cold.
	h := readyPool(t, 1)
	h.f.mu.Lock()
	h.f.describeErr = apiErr("RequestLimitExceeded")
	h.f.mu.Unlock()
	if _, err := h.p.pool.claim(context.Background(), "runner-x"); err == nil || errors.Is(err, errPoolEmpty) {
		t.Fatalf("claim = %v, want the describe error", err)
	}
	if r, _ := h.p.pool.Ready(); r != 1 || len(h.f.terminatedIDs()) != 0 {
		t.Fatalf("ready %d, terminated %v; want the instance kept", r, h.f.terminatedIDs())
	}
}

func TestPool_DisksSignedOnceListed(t *testing.T) {
	// EC2 may list a pending instance without its volumes: the pool signs
	// them on a later pass, while the instance still runs its first boot.
	h := poolHarness(t, 1, config.MarketSpot)
	h.f.volumesLate = true
	h.runPool(t)
	eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
	id := h.f.launched()[0].id
	if mac := h.f.instance(id).tags[TagPoolDiskMAC]; mac != "" {
		t.Fatalf("signed volumes EC2 did not list: %s", mac)
	}
	h.f.setState(id, types.InstanceStateNameRunning)
	eventually(t, "the disks signed", func() bool { return h.f.instance(id).tags[TagPoolDiskMAC] != "" })
	in := h.f.instance(id)
	root := in.volumes["/dev/sda1"] + "@" + in.attachedAt["/dev/sda1"].UTC().Format(time.RFC3339Nano) + "/true"
	if want := h.p.c.diskMAC("test-owner", "cloud-4c", id, "ami-0abc", "/dev/sda1", []string{"/dev/sda1=" + root}); in.tags[TagPoolDiskMAC] != want {
		t.Fatalf("%s = %s, want the MAC of its image and root volume", TagPoolDiskMAC, in.tags[TagPoolDiskMAC])
	}
	h.stopWarming()
	eventually(t, "ready", func() bool { r, _ := h.p.pool.Ready(); return r == 1 })
}

func TestPool_DisksNeverSignedNeverJoin(t *testing.T) {
	for name, change := range map[string]func(i *fakeInstance){
		// It stopped before EC2 listed its volumes: they could have been
		// swapped since, so they are not signed now.
		"stopped before its volumes were listed": func(*fakeInstance) {},
		// EC2 reports an image the launch did not ask for.
		"another image than launched": func(i *fakeInstance) { i.image = "ami-other"; i.state = types.InstanceStateNameRunning },
		// Its volumes were first listed as it stopped: a stopping instance's
		// volumes are not signed either.
		"stopping when its volumes were listed": func(i *fakeInstance) { i.state = types.InstanceStateNameStopping },
	} {
		t.Run(name, func(t *testing.T) {
			h := poolHarness(t, 1, config.MarketSpot)
			h.f.volumesLate = true
			h.runPool(t)
			eventually(t, "warming", func() bool { _, w := h.p.pool.Ready(); return w == 1 })
			id := h.f.launched()[0].id
			h.f.mu.Lock()
			change(h.f.instances[id])
			calls := h.f.describeCalls
			h.f.mu.Unlock()
			// Let refresh look at it twice, so that it could sign it, wrongly.
			eventually(t, "two refresh passes", func() bool {
				h.f.mu.Lock()
				defer h.f.mu.Unlock()
				return h.f.describeCalls >= calls+2
			})
			h.f.setState(id, types.InstanceStateNameStopped)
			eventually(t, "the unsigned instance discarded", func() bool { return h.f.state(id) == types.InstanceStateNameShuttingDown })
			if r, _ := h.p.pool.Ready(); r != 0 {
				t.Fatalf("ready = %d: an instance with unsigned disks joined the pool", r)
			}
			if mac := h.f.instance(id).tags[TagPoolDiskMAC]; mac != "" {
				t.Fatalf("its disks were signed: %s", mac)
			}
		})
	}
}

func TestPool_RestartDoesNotSignAnotherProcesssInstance(t *testing.T) {
	// A warming instance an earlier process launched and never signed the
	// disks of: a later process does not know its launch image, so it never
	// signs them, and the instance never joins the pool.
	h := poolHarness(t, 1, config.MarketSpot)
	if err := h.p.c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	id := h.f.put(poolTags(PoolWarming), types.InstanceStateNameRunning, time.Now(), true)
	h.f.signPool(h.p.c, id)
	h.f.mu.Lock()
	delete(h.f.instances[id].tags, TagPoolDiskMAC)
	h.f.mu.Unlock()
	p := h.restart(t, testKey)
	eventually(t, "followed as warming", func() bool { _, w := p.pool.Ready(); return w >= 1 })
	h.f.setState(id, types.InstanceStateNameStopped)
	eventually(t, "discarded", func() bool { return h.f.state(id) == types.InstanceStateNameShuttingDown })
	if mac := h.f.instance(id).tags[TagPoolDiskMAC]; mac != "" {
		t.Fatalf("a later process signed its disks: %s", mac)
	}
}

func TestLaunch_ColdInstanceRunnerTagIsSigned(t *testing.T) {
	h := newHarness(t, nil)
	errc := h.launch(context.Background())
	inst := h.instance(t)
	eventually(t, "the runner tag signed", func() bool { return h.f.instance(inst.id).tags[TagRunnerMAC] != "" })
	if got, want := h.f.instance(inst.id).tags[TagRunnerMAC], h.p.c.runnerMAC("test-owner", "cloud-4c", "runner-1", inst.id); got != want {
		t.Fatalf("%s = %q, want the MAC of its owner, tier, runner and ID", TagRunnerMAC, got)
	}
	h.f.setState(inst.id, types.InstanceStateNameTerminated)
	if err := waitErr(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestReaper_DeregistersOnlyBySignedRunnerTag(t *testing.T) {
	// Someone able to tag instances puts our owner tag and an idle runner's
	// name on an instance of their own: the reaper must not deregister
	// that runner. Only the genuine instance's runner is removed.
	f := newFakeEC2(t)
	g := &githubRunners{}
	r, c := reaperFor(f, g, nil)
	other := NewClient(f, nil, time.Hour, quietLog()) // another key
	genuine := f.put(job("victim"), types.InstanceStateNameRunning, old, false)
	f.signRunner(c, genuine)
	forged := map[string]func(id string){
		"no MAC":                   func(string) {},
		"MAC copied from genuine":  func(id string) { f.setTag(id, TagRunnerMAC, f.instance(genuine).tags[TagRunnerMAC]) },
		"signed under another key": func(id string) { f.signRunner(other, id) },
		"renamed after signing": func(id string) {
			f.setTag(id, TagRunner, "decoy")
			f.signRunner(c, id)
			f.setTag(id, TagRunner, "victim")
		},
		"retiered after signing": func(id string) { f.setTag(id, TagTier, "other"); f.signRunner(c, id); f.setTag(id, TagTier, "cloud") },
		"signed for another owner": func(id string) {
			f.setTag(id, TagOwner, "someone-else")
			f.signRunner(c, id)
			f.setTag(id, TagOwner, "test-owner")
		},
	}
	ids := map[string]string{}
	for how, forge := range forged {
		id := f.put(job("victim"), types.InstanceStateNameRunning, old, false)
		forge(id)
		ids[id] = how
	}
	rep, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.removed, []string{"victim"}) || !slices.Equal(rep.Instances, []string{genuine}) {
		t.Fatalf("removed %v, reaped %v; want only the genuine instance's runner and instance", g.removed, rep.Instances)
	}
	for id, how := range ids {
		if s := f.state(id); s != types.InstanceStateNameRunning {
			t.Errorf("%s: state %s, want it left to the max lifetime", how, s)
		}
	}
	// Past the max lifetime, they are ended without asking GitHub.
	r.now = func() time.Time { return time.Now().Add(7 * time.Hour) }
	g.removed = nil
	r.Sweep(context.Background())
	for id, how := range ids {
		if s := f.state(id); s != types.InstanceStateNameShuttingDown {
			t.Errorf("%s: state %s past the max lifetime", how, s)
		}
	}
	if len(g.removed) != 0 {
		t.Fatalf("deregistered %v by unsigned tags", g.removed)
	}
}

func TestReaper_TrustsARunnerTagSignedByAnEarlierProcess(t *testing.T) {
	// The point of the key file: a restarted process, with the same key,
	// deregisters the runner a crashed one launched.
	h := newHarness(t, nil)
	if err := h.p.c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	errc := h.launch(context.Background())
	inst := h.instance(t)
	eventually(t, "the runner tag signed", func() bool { return h.f.instance(inst.id).tags[TagRunnerMAC] != "" })
	h.f.mu.Lock()
	h.f.instances[inst.id].state = types.InstanceStateNameRunning
	h.f.instances[inst.id].launched = old
	h.f.mu.Unlock()

	// The process that launched it crashed; a new one, with the same key,
	// sweeps.
	g := &githubRunners{}
	c := NewClient(h.f, nil, time.Hour, quietLog())
	if err := c.SetPoolKey(testKey); err != nil {
		t.Fatal(err)
	}
	rep, err := c.NewReaper(ReaperOptions{Owner: "test-owner", Remove: g.remove, MaxLifetime: 6 * time.Hour}).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.removed, []string{"runner-1"}) || !slices.Equal(rep.Instances, []string{inst.id}) {
		t.Fatalf("removed %v, reaped %v", g.removed, rep.Instances)
	}
	waitErr(t, errc) // the first launch sees its instance go
}

// setTag sets one tag on a fake instance.
func (f *fakeEC2) setTag(id, key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[id].tags[key] = value
}
