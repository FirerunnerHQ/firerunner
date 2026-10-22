package ec2

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
	state                 types.InstanceStateName
	tags                  map[string]string
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
}

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{
		runErrs:   map[string]error{},
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
		id:     fmt.Sprintf("i-%04d", f.next),
		ip:     fmt.Sprintf("10.0.1.%d", f.next),
		itype:  itype,
		subnet: subnet,
		spot:   mkt == "spot",
		state:  types.InstanceStateNamePending,
		tags:   map[string]string{},
	}
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == types.ResourceTypeInstance {
			for _, t := range ts.Tags {
				inst.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
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
	return types.Instance{
		InstanceId:       aws.String(i.id),
		PrivateIpAddress: aws.String(i.ip),
		InstanceType:     types.InstanceType(i.itype),
		SubnetId:         aws.String(i.subnet),
		State:            &types.InstanceState{Name: i.state},
	}
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
	for _, id := range in.InstanceIds {
		if i, ok := f.instances[id]; ok {
			res.Instances = append(res.Instances, f.view(i))
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{res}}, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls++
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	for _, id := range in.InstanceIds {
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
		c := *f.instances[id]
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
