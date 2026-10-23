package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

// The AWS rules the fake enforces.
//
// An order the fake accepts and AWS rejects reaches a real run: cancelling a
// pooled spot request before StartInstances passed every unit test and failed
// on AWS (R4). So every rule of real EC2 that the fake models is listed here
// with its source. A behaviour nobody has verified is not a rule: it goes
// under "Not modelled" until someone checks it.
//
//	R1  RunInstances: a persistent spot request needs interruption behaviour
//	    stop or hibernate.
//	    Fake: InvalidParameterCombination.
//	    Source: EC2 API Reference, SpotMarketOptions ("persistent Spot
//	    Instance requests are only supported when the instance interruption
//	    behavior is either hibernate or stop"); the code observed on
//	    2026-10-23.
//	R2  RunInstances: a persistent spot request rejects instance-initiated
//	    shutdown behaviour terminate. One-time spot and on-demand accept it.
//	    Fake: InvalidParameterCombination.
//	    Source: observed on real instances on 2026-10-22 (commit 512f5da);
//	    the code observed on 2026-10-23.
//	R3  Only a spot instance from a persistent request can be stopped.
//	    Fake: setState records a violation; the test scripted a state AWS
//	    never produces.
//	    Source: EC2 User Guide, stopping Spot Instances; observed on
//	    2026-10-23 (StopInstances on a one-time spot instance fails with
//	    UnsupportedOperation).
//	R4  StartInstances fails for a stopped spot instance whose persistent
//	    request is cancelled; that instance can never start again.
//	    Fake: IncorrectSpotRequestState.
//	    Source: observed in e2e on 2026-10-23 (commit aae7d7e), and again
//	    on its own the same day.
//	R5  StartInstances on a pending or running instance is a no-op that
//	    succeeds; it starts a stopped one and fails in any other state.
//	    Fake: success without a state change; IncorrectInstanceState for
//	    stopping, shutting-down and terminated.
//	    Source: the no-op observed on 2026-10-23 (spot and on-demand); the
//	    failure from the EC2 User Guide, instance lifecycle (not observed).
//	R6  ModifyInstanceAttribute can change user-data only while the instance
//	    is stopped.
//	    Fake: IncorrectInstanceState.
//	    Source: EC2 User Guide, instance user data.
//	R7  An instance ID EC2 does not know fails DescribeInstances,
//	    StartInstances, TerminateInstances, ModifyInstanceAttribute and
//	    CreateTags.
//	    Fake: InvalidInstanceID.NotFound.
//	    Source: EC2 API Reference, error codes; observed on 2026-10-23 for
//	    DescribeInstances, StartInstances, TerminateInstances and CreateTags
//	    with a well-formed ID. (An ID that is not well formed fails with
//	    InvalidInstanceID.Malformed, or InvalidID from CreateTags; firerunner
//	    only uses IDs EC2 handed it.)
//	R8  A just-launched instance can be missing from DescribeInstances for a
//	    while, which fails the whole call.
//	    Fake: InvalidInstanceID.NotFound for the IDs in invisible.
//	    Source: EC2 API Reference, eventual consistency.
//	R9  TerminateInstances moves an instance to shutting-down. Terminating a
//	    terminated instance again succeeds.
//	    Fake: state shutting-down; no error on a repeat.
//	    Source: EC2 User Guide, terminate instances.
//	R10 Terminating an instance whose persistent spot request is still open
//	    makes AWS launch a replacement, untagged by us and billed.
//	    Fake: records a violation (it launches no replacement).
//	    Source: EC2 User Guide, persistent Spot requests and cancelling them.
//	R11 Cancelling a spot request does not stop or terminate its instance.
//	    Fake: the instance's state is unchanged.
//	    Source: EC2 User Guide, cancel a Spot Instance request; observed on
//	    2026-10-23 (a stopped instance stayed stopped).
//	R12 Cancelling an already cancelled spot request succeeds.
//	    Fake: no error on a repeat.
//	    Source: observed on 2026-10-23.
//
// Scripted per test, not enforced: capacity and quota errors (runErrs),
// throttling and other failures (cancelErrs, describeErr, terminateErr,
// startErr, tagErr), a stale read right after a start (startStaysStopped), and
// a RunInstances response without the private IP (noIPInRun).
//
// Not modelled, so no test may rely on it: a spot interruption; the
// replacement a persistent request launches (R10 fails the test instead); a
// cap on IDs per DescribeInstances call (the watcher batches 100 on its own;
// on 2026-10-23 a call accepted 5000 copies of one ID, so a cap on distinct
// IDs is not ruled out).
//
// newFakeEC2 also checks, when the test ends, the invariants real AWS and our
// cost guarantees imply; see invariantErrors.

// apiErr builds the error shape the AWS SDK returns for a service error code.
func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " (fake)"}
}

type fakeInstance struct {
	id, ip, itype, subnet string
	spot                  bool
	spotReq               string // persistent spot request, if any
	shutdown              types.ShutdownBehavior
	volumeTags            map[string]string // tags requested for the root volume
	userData              string            // decoded
	state                 types.InstanceStateName
	tags                  map[string]string
	spotTags              map[string]string // tags on its spot request
	launched              time.Time
}

// fakeEC2 is an in-memory EC2. RunInstances fails per type/subnet/market as
// scripted in runErrs, keyed "type/subnet/market" with "*" wildcards; every
// other call reflects the instances it launched.
type fakeEC2 struct {
	mu        sync.Mutex
	runErrs   map[string]error
	runs      []*ec2.RunInstancesInput
	attempts  []string // "type/subnet/market" of every RunInstances call
	instances map[string]*fakeInstance
	order     []string
	next      int
	// noIPInRun leaves PrivateIpAddress out of the RunInstances response, as
	// AWS may, so the launcher has to describe the instance for it.
	noIPInRun   bool
	describeErr error
	// invisible IDs make DescribeInstances fail with InvalidInstanceID.NotFound
	// whenever they are asked for, as for a just-launched instance.
	invisible      map[string]bool
	describeCalls  int
	describeIDs    [][]string
	terminated     []string
	terminateErr   error
	terminateCalls int
	images         map[string]string // image ID -> root device
	imageCalls     int
	// afterRun runs (without the lock held) once an instance is created.
	afterRun func(id string)
	// startErr fails StartInstances; started records started IDs.
	// startStaysStopped leaves a started instance reporting stopped, as a
	// stale read right after StartInstances can.
	startErr          error
	startStaysStopped bool
	// tagErr fails CreateTags.
	tagErr error
	// clock, when set, is the fake's notion of now (launch times).
	clock func() time.Time
	// modifyErr fails ModifyInstanceAttribute.
	modifyErr error
	// pageSize, when > 0, splits filtered listings into pages.
	pageSize  int
	started   []string
	cancelled map[string]bool // spot request ID -> cancelled
	// cancelErrs fails this many CancelSpotInstanceRequests calls first.
	cancelErrs int
	// termWithOpenRequest records terminates of an instance whose persistent
	// request was still open: AWS would launch a replacement (R10).
	termWithOpenRequest []string
	// violations records calls AWS would never allow, or states it would
	// never produce; the end-of-test check reports them.
	violations []string
}

// newFakeEC2 returns a fake whose invariants are checked when t ends.
func newFakeEC2(t testing.TB) *fakeEC2 {
	t.Helper()
	f := newUncheckedFakeEC2()
	t.Cleanup(func() {
		for _, e := range f.invariantErrors() {
			t.Errorf("fake EC2 invariant: %s", e)
		}
	})
	return f
}

// newUncheckedFakeEC2 returns a fake nothing checks, for testing the check.
func newUncheckedFakeEC2() *fakeEC2 {
	return &fakeEC2{
		runErrs:   map[string]error{},
		cancelled: map[string]bool{},
		invisible: map[string]bool{},
		instances: map[string]*fakeInstance{},
		images:    map[string]string{"ami-0abc": "/dev/sda1"},
	}
}

func market(in *ec2.RunInstancesInput) string {
	if in.InstanceMarketOptions != nil && in.InstanceMarketOptions.MarketType == types.MarketTypeSpot {
		return "spot"
	}
	return "on-demand"
}

func (f *fakeEC2) runErr(itype, subnet, mkt string) error {
	for _, k := range []string{
		itype + "/" + subnet + "/" + mkt, itype + "/*/" + mkt, "*/" + subnet + "/" + mkt, "*/*/" + mkt,
		itype + "/" + subnet + "/*", itype + "/*/*", "*/" + subnet + "/*", "*/*/*",
	} {
		if err, ok := f.runErrs[k]; ok {
			return err
		}
	}
	return nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	itype, subnet, mkt := string(in.InstanceType), aws.ToString(in.SubnetId), market(in)
	f.runs = append(f.runs, in)
	f.attempts = append(f.attempts, itype+"/"+subnet+"/"+mkt)
	if err := f.runErr(itype, subnet, mkt); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	if mo := in.InstanceMarketOptions; mo != nil && mo.SpotOptions != nil && mo.SpotOptions.SpotInstanceType == types.SpotInstanceTypePersistent {
		ib := mo.SpotOptions.InstanceInterruptionBehavior
		if (ib != types.InstanceInterruptionBehaviorStop && ib != types.InstanceInterruptionBehaviorHibernate) || // R1
			in.InstanceInitiatedShutdownBehavior == types.ShutdownBehaviorTerminate { // R2
			f.mu.Unlock()
			return nil, apiErr("InvalidParameterCombination")
		}
	}
	f.next++
	inst := &fakeInstance{
		id:         fmt.Sprintf("i-%04d", f.next),
		ip:         fmt.Sprintf("10.0.1.%d", f.next),
		itype:      itype,
		subnet:     subnet,
		spot:       mkt == "spot",
		shutdown:   in.InstanceInitiatedShutdownBehavior,
		state:      types.InstanceStateNamePending,
		tags:       map[string]string{},
		volumeTags: map[string]string{},
	}
	if mo := in.InstanceMarketOptions; mo != nil && mo.SpotOptions != nil && mo.SpotOptions.SpotInstanceType == types.SpotInstanceTypePersistent {
		inst.spotReq = fmt.Sprintf("sir-%04d", f.next)
	}
	if in.UserData != nil {
		b, _ := base64.StdEncoding.DecodeString(aws.ToString(in.UserData))
		inst.userData = string(b)
	}
	inst.launched = f.now()
	inst.spotTags = map[string]string{}
	for _, ts := range in.TagSpecifications {
		into := map[types.ResourceType]map[string]string{
			types.ResourceTypeInstance:             inst.tags,
			types.ResourceTypeVolume:               inst.volumeTags,
			types.ResourceTypeSpotInstancesRequest: inst.spotTags,
		}[ts.ResourceType]
		for _, t := range ts.Tags {
			if into != nil {
				into[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
	}
	f.instances[inst.id] = inst
	f.order = append(f.order, inst.id)
	out := f.view(inst)
	if f.noIPInRun {
		out.PrivateIpAddress = nil
	}
	hook := f.afterRun
	f.mu.Unlock()
	if hook != nil {
		hook(inst.id)
	}
	return &ec2.RunInstancesOutput{Instances: []types.Instance{out}}, nil
}

func (f *fakeEC2) view(i *fakeInstance) types.Instance {
	v := types.Instance{
		LaunchTime:       aws.Time(i.launched),
		InstanceId:       aws.String(i.id),
		PrivateIpAddress: aws.String(i.ip),
		InstanceType:     types.InstanceType(i.itype),
		SubnetId:         aws.String(i.subnet),
		State:            &types.InstanceState{Name: i.state},
	}
	if i.spot {
		v.InstanceLifecycle = types.InstanceLifecycleTypeSpot
	}
	if i.spotReq != "" {
		v.SpotInstanceRequestId = aws.String(i.spotReq)
	}
	for k, val := range i.tags {
		v.Tags = append(v.Tags, types.Tag{Key: aws.String(k), Value: aws.String(val)})
	}
	return v
}

// matches applies DescribeInstances filters: tag:K, tag-key and
// instance-state-name.
func (i *fakeInstance) matches(filters []types.Filter) bool {
	for _, fl := range filters {
		name, ok := aws.ToString(fl.Name), false
		for _, want := range fl.Values {
			switch {
			case name == "instance-state-name":
				ok = ok || string(i.state) == want
			case name == "tag-key":
				_, has := i.tags[want]
				ok = ok || has
			case strings.HasPrefix(name, "tag:"):
				ok = ok || i.tags[strings.TrimPrefix(name, "tag:")] == want
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (f *fakeEC2) now() time.Time {
	if f.clock != nil {
		return f.clock()
	}
	return time.Now()
}

func (f *fakeEC2) DescribeSpotInstanceRequests(_ context.Context, in *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.SpotInstanceRequest
	for _, id := range f.order {
		i := f.instances[id]
		if i.spotReq == "" {
			continue
		}
		state := types.SpotInstanceStateActive
		switch {
		case f.cancelled[i.spotReq]:
			state = types.SpotInstanceStateCancelled
		case i.state == types.InstanceStateNameStopped:
			state = types.SpotInstanceStateDisabled
		}
		r := types.SpotInstanceRequest{
			SpotInstanceRequestId: aws.String(i.spotReq),
			InstanceId:            aws.String(i.id),
			State:                 state,
			CreateTime:            aws.Time(i.launched),
		}
		for k, v := range i.spotTags {
			r.Tags = append(r.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		match := true
		for _, fl := range in.Filters {
			name := aws.ToString(fl.Name)
			ok := false
			for _, want := range fl.Values {
				switch {
				case name == "state":
					ok = ok || string(state) == want
				case strings.HasPrefix(name, "tag:"):
					ok = ok || i.spotTags[strings.TrimPrefix(name, "tag:")] == want
				}
			}
			match = match && ok
		}
		if match {
			out = append(out, r)
		}
	}
	out, next := page(out, in.NextToken, f.pageSize)
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: out, NextToken: next}, nil
}

func (f *fakeEC2) StartInstances(_ context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	for _, id := range in.InstanceIds {
		i, ok := f.instances[id]
		if !ok {
			return nil, apiErr(errNotFound) // R7
		}
		switch i.state {
		case types.InstanceStateNamePending, types.InstanceStateNameRunning:
			// R5: a no-op that succeeds.
			f.started = append(f.started, id)
			continue
		case types.InstanceStateNameStopped:
		default:
			return nil, apiErr("IncorrectInstanceState") // R5
		}
		// R4: a stopped spot instance whose request was cancelled can
		// never be started again.
		if i.spotReq != "" && f.cancelled[i.spotReq] {
			return nil, apiErr("IncorrectSpotRequestState")
		}
		if !f.startStaysStopped {
			i.state = types.InstanceStateNamePending
		}
		i.launched = f.now() // as on AWS, LaunchTime is the latest start
		f.started = append(f.started, id)
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeEC2) ModifyInstanceAttribute(_ context.Context, in *ec2.ModifyInstanceAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.modifyErr != nil {
		return nil, f.modifyErr
	}
	i, ok := f.instances[aws.ToString(in.InstanceId)]
	if !ok {
		return nil, apiErr(errNotFound) // R7
	}
	if in.UserData != nil {
		if i.state != types.InstanceStateNameStopped { // R6
			return nil, apiErr("IncorrectInstanceState")
		}
		// The SDK base64-encodes a blob attribute itself.
		i.userData = string(in.UserData.Value)
	}
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	for _, id := range in.Resources {
		if _, ok := f.instances[id]; !ok {
			return nil, apiErr(errNotFound) // R7
		}
	}
	for _, id := range in.Resources {
		for _, t := range in.Tags {
			f.instances[id].tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErrs > 0 {
		f.cancelErrs--
		return nil, apiErr("RequestLimitExceeded")
	}
	for _, id := range in.SpotInstanceRequestIds {
		f.cancelled[id] = true
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

// openSpotRequests lists the persistent spot requests still open behind
// instances that left the pool (claimed, or discarded): AWS would relaunch
// those after a terminate. Requests behind instances still pooled are
// expected to be open.
func (f *fakeEC2) openSpotRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var open []string
	for _, i := range f.instances {
		pooled := (i.tags[TagPool] == PoolReady || i.tags[TagPool] == PoolWarming) &&
			i.state != types.InstanceStateNameShuttingDown && i.state != types.InstanceStateNameTerminated
		if i.spotReq != "" && !f.cancelled[i.spotReq] && !pooled {
			open = append(open, i.spotReq)
		}
	}
	return open
}

func (f *fakeEC2) instance(id string) fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[id].clone()
}

// clone copies an instance, tags included, for reading outside the lock.
func (i *fakeInstance) clone() fakeInstance {
	c := *i
	c.tags = make(map[string]string, len(i.tags))
	for k, v := range i.tags {
		c.tags[k] = v
	}
	return c
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeCalls++
	f.describeIDs = append(f.describeIDs, append([]string(nil), in.InstanceIds...))
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	for _, id := range in.InstanceIds {
		if _, known := f.instances[id]; f.invisible[id] || !known {
			return nil, apiErr(errNotFound) // R8, R7
		}
	}
	var res types.Reservation
	var next *string
	if len(in.InstanceIds) == 0 {
		var all []types.Instance
		for _, id := range f.order {
			// An invisible instance is too new to show in a listing yet.
			if i := f.instances[id]; i.matches(in.Filters) && !f.invisible[id] {
				all = append(all, f.view(i))
			}
		}
		res.Instances, next = page(all, in.NextToken, f.pageSize)
	}
	for _, id := range in.InstanceIds {
		if i, ok := f.instances[id]; ok {
			res.Instances = append(res.Instances, f.view(i))
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{res}, NextToken: next}, nil
}

// page returns one page of items from token (an index), and the next token.
func page[T any](items []T, token *string, size int) ([]T, *string) {
	if size <= 0 {
		return items, nil
	}
	start := 0
	if token != nil {
		fmt.Sscan(*token, &start)
	}
	end := min(start+size, len(items))
	if end >= len(items) {
		return items[start:end], nil
	}
	return items[start:end], aws.String(fmt.Sprint(end))
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls++
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	for _, id := range in.InstanceIds {
		if _, ok := f.instances[id]; !ok {
			return nil, apiErr(errNotFound) // R7
		}
	}
	for _, id := range in.InstanceIds {
		i := f.instances[id]
		if i.spotReq != "" && !f.cancelled[i.spotReq] && i.state != types.InstanceStateNameTerminated { // R10
			f.termWithOpenRequest = append(f.termWithOpenRequest, id)
			f.violations = append(f.violations, fmt.Sprintf("terminated %s while its persistent spot request %s was open: AWS would launch a replacement (R10)", id, i.spotReq))
		}
		f.terminated = append(f.terminated, id)
		if i.state != types.InstanceStateNameTerminated { // R9
			i.state = types.InstanceStateNameShuttingDown
		}
	}
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageCalls++
	var out []types.Image
	for _, id := range in.ImageIds {
		if dev, ok := f.images[id]; ok {
			out = append(out, types.Image{ImageId: aws.String(id), RootDeviceName: aws.String(dev)})
		}
	}
	return &ec2.DescribeImagesOutput{Images: out}, nil
}

// setState moves an instance to state, as the guest or AWS would.
func (f *fakeEC2) setState(id string, state types.InstanceStateName) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.instances[id]
	if i.spot && i.spotReq == "" && (state == types.InstanceStateNameStopping || state == types.InstanceStateNameStopped) {
		f.violations = append(f.violations, fmt.Sprintf("the test stopped one-time spot instance %s, which AWS never does (R3)", id))
	}
	i.state = state
}

// invariantErrors lists what real AWS, or firerunner's cost guarantees, rule
// out in the state a test leaves behind, after every violation recorded on
// the way:
//   - an instance shutting down or terminated whose persistent spot request is
//     still open: AWS launches a replacement nobody tracks (R10);
//   - a pool instance tagged ready that can never start, because its spot
//     request is cancelled: every claim of it falls back to a cold launch
//     while its volume bills (R4);
//   - an instance, or its volume, without the owner and tier tags that cleanup
//     finds leftovers by.
func (f *fakeEC2) invariantErrors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	errs := append([]string(nil), f.violations...)
	for _, id := range f.order {
		i := f.instances[id]
		open := i.spotReq != "" && !f.cancelled[i.spotReq]
		gone := i.state == types.InstanceStateNameShuttingDown || i.state == types.InstanceStateNameTerminated
		if gone && open {
			errs = append(errs, fmt.Sprintf("instance %s is %s with its persistent spot request %s still open: AWS would launch a replacement (R10)", id, i.state, i.spotReq))
		}
		if i.tags[TagPool] == PoolReady && i.state == types.InstanceStateNameStopped && i.spotReq != "" && f.cancelled[i.spotReq] {
			errs = append(errs, fmt.Sprintf("pool instance %s is tagged ready but its spot request %s is cancelled: it can never start (R4)", id, i.spotReq))
		}
		for _, k := range []string{TagOwner, TagTier} {
			if _, ok := i.tags[k]; !ok {
				errs = append(errs, fmt.Sprintf("instance %s has no %s tag: cleanup cannot find it", id, k))
			}
			if _, ok := i.volumeTags[k]; !ok {
				errs = append(errs, fmt.Sprintf("the volume of instance %s has no %s tag: cleanup cannot find it", id, k))
			}
		}
	}
	return errs
}

func (f *fakeEC2) state(id string) types.InstanceStateName {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[id].state
}

func (f *fakeEC2) launched() []*fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeInstance, 0, len(f.order))
	for _, id := range f.order {
		c := f.instances[id].clone()
		out = append(out, &c)
	}
	return out
}

func (f *fakeEC2) attemptLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.attempts, " ")
}

func (f *fakeEC2) terminatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.terminated...)
}

// fakeSSM maps parameter names to values.
type fakeSSM struct {
	mu     sync.Mutex
	params map[string]string
	calls  int
	err    error
}

func (s *fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.params[aws.ToString(in.Name)]
	if !ok {
		return nil, apiErr("ParameterNotFound")
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

// fakeLaunch runs one instance tagged as firerunner tags its own, with the
// given market options (nil for on-demand) and shutdown behaviour.
func fakeLaunch(t *testing.T, f *fakeEC2, mo *types.InstanceMarketOptionsRequest, shutdown types.ShutdownBehavior) (string, error) {
	t.Helper()
	tags := []types.Tag{
		{Key: aws.String(TagOwner), Value: aws.String("test-owner")},
		{Key: aws.String(TagTier), Value: aws.String("cloud-4c")},
	}
	out, err := f.RunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: types.InstanceTypeC7aXlarge, SubnetId: aws.String("subnet-a"),
		InstanceMarketOptions:             mo,
		InstanceInitiatedShutdownBehavior: shutdown,
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}

func spotOpts(kind types.SpotInstanceType, interruption types.InstanceInterruptionBehavior) *types.InstanceMarketOptionsRequest {
	return &types.InstanceMarketOptionsRequest{
		MarketType:  types.MarketTypeSpot,
		SpotOptions: &types.SpotMarketOptions{SpotInstanceType: kind, InstanceInterruptionBehavior: interruption},
	}
}

func TestFake_RunInstancesSpotRules(t *testing.T) {
	persistent, oneTime := types.SpotInstanceTypePersistent, types.SpotInstanceTypeOneTime
	stop, term := types.InstanceInterruptionBehaviorStop, types.InstanceInterruptionBehaviorTerminate
	for _, c := range []struct {
		name     string
		mo       *types.InstanceMarketOptionsRequest
		shutdown types.ShutdownBehavior
		ok       bool
	}{
		{"on-demand, terminate on shutdown", nil, types.ShutdownBehaviorTerminate, true},
		{"one-time spot, terminate on shutdown", spotOpts(oneTime, term), types.ShutdownBehaviorTerminate, true},
		{"persistent spot, stop on interruption", spotOpts(persistent, stop), "", true},
		{"R1: persistent spot, terminate on interruption", spotOpts(persistent, term), "", false},
		{"R2: persistent spot, terminate on shutdown", spotOpts(persistent, stop), types.ShutdownBehaviorTerminate, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := fakeLaunch(t, newFakeEC2(t), c.mo, c.shutdown)
			if c.ok != (err == nil) {
				t.Fatalf("RunInstances err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestFake_StartInstancesByState(t *testing.T) {
	for _, c := range []struct {
		state types.InstanceStateName
		ok    bool
	}{
		{types.InstanceStateNamePending, true}, // R5: no-op
		{types.InstanceStateNameRunning, true}, // R5: no-op
		{types.InstanceStateNameStopped, true},
		{types.InstanceStateNameStopping, false},
		{types.InstanceStateNameShuttingDown, false},
		{types.InstanceStateNameTerminated, false},
	} {
		t.Run(string(c.state), func(t *testing.T) {
			f := newFakeEC2(t)
			id, err := fakeLaunch(t, f, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			f.setState(id, c.state)
			_, err = f.StartInstances(context.Background(), &ec2.StartInstancesInput{InstanceIds: []string{id}})
			if c.ok != (err == nil) {
				t.Fatalf("StartInstances from %s: err = %v, want ok=%v", c.state, err, c.ok)
			}
			if !c.ok && !isCode(err, "IncorrectInstanceState") {
				t.Fatalf("StartInstances from %s: err = %v, want IncorrectInstanceState", c.state, err)
			}
			if c.ok && c.state != types.InstanceStateNameStopped && f.state(id) != c.state {
				t.Fatalf("a no-op start moved %s to %s", c.state, f.state(id))
			}
		})
	}
}

// TestFake_CancelTwice covers R12.
func TestFake_CancelTwice(t *testing.T) {
	f := newFakeEC2(t)
	id, err := fakeLaunch(t, f, spotOpts(types.SpotInstanceTypePersistent, types.InstanceInterruptionBehaviorStop), "")
	if err != nil {
		t.Fatal(err)
	}
	req := f.instance(id).spotReq
	for i := 0; i < 2; i++ {
		if _, err := f.CancelSpotInstanceRequests(context.Background(), &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{req}}); err != nil {
			t.Fatalf("cancel %d: %v", i+1, err)
		}
	}
	if _, err := f.TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
		t.Fatal(err)
	}
}

// TestFake_InvariantsCatchViolations proves each end-of-test invariant can
// fail, and that a clean run passes them all.
func TestFake_InvariantsCatchViolations(t *testing.T) {
	ctx := context.Background()
	persistent := spotOpts(types.SpotInstanceTypePersistent, types.InstanceInterruptionBehaviorStop)
	cancel := func(t *testing.T, f *fakeEC2, id string) {
		req := f.instance(id).spotReq
		if _, err := f.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: []string{req}}); err != nil {
			t.Fatal(err)
		}
	}
	terminate := func(t *testing.T, f *fakeEC2, id string) {
		if _, err := f.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		do   func(t *testing.T, f *fakeEC2, id string)
		want string // a substring of the expected error; "" for none
	}{
		{"clean: cancel, then terminate", func(t *testing.T, f *fakeEC2, id string) {
			cancel(t, f, id)
			terminate(t, f, id)
		}, ""},
		{"terminated with the request open", func(t *testing.T, f *fakeEC2, id string) {
			terminate(t, f, id)
			cancel(t, f, id) // too late: AWS has replaced it already
		}, "(R10)"},
		{"gone with the request open", func(t *testing.T, f *fakeEC2, id string) {
			f.setState(id, types.InstanceStateNameTerminated)
		}, "(R10)"},
		{"tagged ready with the request cancelled", func(t *testing.T, f *fakeEC2, id string) {
			f.setState(id, types.InstanceStateNameStopped)
			f.mu.Lock()
			f.instances[id].tags[TagPool] = PoolReady
			f.mu.Unlock()
			cancel(t, f, id)
		}, "(R4)"},
		{"volume untagged", func(t *testing.T, f *fakeEC2, id string) {
			f.mu.Lock()
			f.instances[id].volumeTags = map[string]string{}
			f.mu.Unlock()
		}, "volume"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newUncheckedFakeEC2()
			id, err := fakeLaunch(t, f, persistent, "")
			if err != nil {
				t.Fatal(err)
			}
			c.do(t, f, id)
			errs := f.invariantErrors()
			if c.want == "" {
				if len(errs) != 0 {
					t.Fatalf("invariant errors on a clean run: %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), c.want) {
				t.Fatalf("invariant errors = %v, want one containing %q", errs, c.want)
			}
		})
	}
	t.Run("one-time spot stopped", func(t *testing.T) {
		f := newUncheckedFakeEC2()
		id, err := fakeLaunch(t, f, spotOpts(types.SpotInstanceTypeOneTime, types.InstanceInterruptionBehaviorTerminate), types.ShutdownBehaviorTerminate)
		if err != nil {
			t.Fatal(err)
		}
		f.setState(id, types.InstanceStateNameStopped)
		if errs := f.invariantErrors(); !strings.Contains(strings.Join(errs, "\n"), "(R3)") {
			t.Fatalf("invariant errors = %v, want R3", errs)
		}
	})
}
