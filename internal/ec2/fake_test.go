package ec2

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

// apiErr builds the error shape the AWS SDK returns for a service error code.
func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " (fake)"}
}

type fakeInstance struct {
	id, ip, itype, subnet string
	spot                  bool
	spotReq               string // persistent spot request, if any
	shutdown              types.ShutdownBehavior
	userData              string // decoded
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
	// termBeforeCancel records terminates of an instance whose persistent
	// request was still open: AWS would launch a replacement.
	termBeforeCancel []string
}

func newFakeEC2() *fakeEC2 {
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
	f.next++
	inst := &fakeInstance{
		id:       fmt.Sprintf("i-%04d", f.next),
		ip:       fmt.Sprintf("10.0.1.%d", f.next),
		itype:    itype,
		subnet:   subnet,
		spot:     mkt == "spot",
		shutdown: in.InstanceInitiatedShutdownBehavior,
		state:    types.InstanceStateNamePending,
		tags:     map[string]string{},
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
		switch ts.ResourceType {
		case types.ResourceTypeInstance:
			for _, t := range ts.Tags {
				inst.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		case types.ResourceTypeSpotInstancesRequest:
			for _, t := range ts.Tags {
				inst.spotTags[aws.ToString(t.Key)] = aws.ToString(t.Value)
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
		if !ok || i.state != types.InstanceStateNameStopped {
			return nil, apiErr("IncorrectInstanceState")
		}
		// As on AWS: a stopped spot instance whose request was cancelled
		// can never be started again.
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
		return nil, apiErr("InvalidInstanceID.NotFound")
	}
	if in.UserData != nil {
		if i.state != types.InstanceStateNameStopped {
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
		if i, ok := f.instances[id]; ok {
			for _, t := range in.Tags {
				i.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
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
		if f.invisible[id] {
			return nil, apiErr("InvalidInstanceID.NotFound")
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
		if i, ok := f.instances[id]; ok && i.spotReq != "" && !f.cancelled[i.spotReq] && i.tags[TagPool] == PoolClaimed {
			f.termBeforeCancel = append(f.termBeforeCancel, id)
		}
		f.terminated = append(f.terminated, id)
		if i, ok := f.instances[id]; ok && i.state != types.InstanceStateNameTerminated {
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
	f.instances[id].state = state
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
