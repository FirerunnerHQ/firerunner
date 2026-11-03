package ec2

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// fakeAccount answers the preflight's reads from maps: subnets with their
// VPC and public-IP setting, route tables by subnet or VPC main, security
// groups, instance types' vCPUs and quota values. denied names calls that
// answer UnauthorizedOperation.
type fakeAccount struct {
	subnets map[string]types.Subnet
	routes  map[string]string // subnet ID, or "main:"+VPC ID -> 0.0.0.0/0 target ("" none)
	groups  map[string]types.SecurityGroup
	vcpus   map[string]int32
	quotas  map[string]float64
	denied  map[string]bool
	noID    bool
}

func (a *fakeAccount) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if a.denied["DescribeSubnets"] {
		return nil, apiErr("UnauthorizedOperation")
	}
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range in.SubnetIds {
		s, ok := a.subnets[id]
		if !ok {
			return nil, apiErr("InvalidSubnetID.NotFound")
		}
		out.Subnets = append(out.Subnets, s)
	}
	return out, nil
}

func (a *fakeAccount) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	key := ""
	for _, f := range in.Filters {
		switch aws.ToString(f.Name) {
		case "association.subnet-id":
			key = f.Values[0]
		case "vpc-id":
			key = "main:" + f.Values[0]
		}
	}
	target, ok := a.routes[key]
	if !ok {
		return &ec2.DescribeRouteTablesOutput{}, nil
	}
	rt := types.RouteTable{Routes: []types.Route{{DestinationCidrBlock: aws.String("10.0.0.0/16"), GatewayId: aws.String("local")}}}
	switch {
	case strings.HasPrefix(target, "igw-"):
		rt.Routes = append(rt.Routes, types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(target)})
	case strings.HasPrefix(target, "nat-"):
		rt.Routes = append(rt.Routes, types.Route{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String(target)})
	}
	return &ec2.DescribeRouteTablesOutput{RouteTables: []types.RouteTable{rt}}, nil
}

func (a *fakeAccount) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, id := range in.GroupIds {
		out.SecurityGroups = append(out.SecurityGroups, a.groups[id])
	}
	return out, nil
}

func (a *fakeAccount) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, it := range in.InstanceTypes {
		out.InstanceTypes = append(out.InstanceTypes, types.InstanceTypeInfo{InstanceType: it, VCpuInfo: &types.VCpuInfo{DefaultVCpus: aws.Int32(a.vcpus[string(it)])}})
	}
	return out, nil
}

func (a *fakeAccount) GetServiceQuota(_ context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	if a.denied["GetServiceQuota"] {
		return nil, apiErr("AccessDeniedException")
	}
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(a.quotas[aws.ToString(in.QuotaCode)])}}, nil
}

func (a *fakeAccount) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if a.noID {
		return nil, apiErr("InvalidClientTokenId")
	}
	return &sts.GetCallerIdentityOutput{Arn: aws.String("arn:aws:sts::000000000000:assumed-role/control/i-1")}, nil
}

// goodAccount is a deployment preflight passes: a public subnet, a private
// one behind a NAT, an outbound-only group, quotas with room.
func goodAccount() *fakeAccount {
	sub := func(id string, public bool) types.Subnet {
		return types.Subnet{SubnetId: aws.String(id), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("az-" + id), MapPublicIpOnLaunch: aws.Bool(public)}
	}
	return &fakeAccount{
		subnets: map[string]types.Subnet{"subnet-a": sub("subnet-a", true), "subnet-b": sub("subnet-b", false)},
		routes:  map[string]string{"subnet-b": "nat-1", "main:vpc-1": "igw-1"},
		groups: map[string]types.SecurityGroup{"sg-1": {GroupId: aws.String("sg-1"), VpcId: aws.String("vpc-1"),
			IpPermissionsEgress: []types.IpPermission{{IpProtocol: aws.String("-1")}}}},
		vcpus:  map[string]int32{"c8a.xlarge": 4, "c7a.xlarge": 4},
		quotas: map[string]float64{"L-34B43A08": 640, "L-1216C47A": 640},
		denied: map[string]bool{},
	}
}

func preflight(t *testing.T, a *fakeAccount, tiers ...config.Tier) map[string]Finding {
	t.Helper()
	c := NewClient(newFakeEC2(t), nil, 0, quietLog())
	got := map[string]Finding{}
	for _, f := range c.Preflight(context.Background(), "us-east-2", a, a, a, tiers) {
		got[f.Name] = f
	}
	return got
}

func ec2Tier(mut func(*config.EC2Tier)) config.Tier {
	e := config.EC2Tier{InstanceTypes: []string{"c8a.xlarge", "c7a.xlarge"}, AMI: "ami-0abc",
		Subnets: []string{"subnet-a", "subnet-b"}, SecurityGroups: []string{"sg-1"}, Market: config.MarketSpotThenOnDemand}
	if mut != nil {
		mut(&e)
	}
	return config.Tier{Name: "cloud", Provisioner: config.ProvisionerEC2, Max: 10, EC2: &e}
}

func TestPreflight_AGoodDeploymentPasses(t *testing.T) {
	got := preflight(t, goodAccount(), ec2Tier(nil))
	for _, name := range []string{"aws-identity", "ami[cloud]", "subnets[cloud]", "security-groups[cloud]", "instance-metadata", "vcpu-quota[Standard/on-demand]", "vcpu-quota[Standard/spot]"} {
		if f, ok := got[name]; !ok || f.Level != "PASS" {
			t.Errorf("%s = %+v, want PASS", name, f)
		}
	}
	if d := got["subnets[cloud]"].Detail; !strings.Contains(d, "subnet-a (az-subnet-a via igw-1)") || !strings.Contains(d, "via nat-1") {
		t.Errorf("subnets detail = %q, want each subnet's route", d)
	}
	if d := got["vcpu-quota[Standard/spot]"].Detail; !strings.Contains(d, "needs 40 vCPUs of 640") {
		t.Errorf("quota detail = %q, want 10 x 4 vCPUs against 640", d)
	}
}

func TestPreflight_NoCredentialsStopsThere(t *testing.T) {
	a := goodAccount()
	a.noID = true
	got := preflight(t, a, ec2Tier(nil))
	if len(got) != 1 || got["aws-identity"].Level != "FAIL" {
		t.Fatalf("findings = %+v, want only a failed aws-identity", got)
	}
}

func TestPreflight_Failures(t *testing.T) {
	for _, c := range []struct {
		name, check, level, detail string
		account                    func(*fakeAccount)
		tier                       func(*config.EC2Tier)
	}{
		{"ami missing", "ami[cloud]", "FAIL", "does not resolve", nil, func(e *config.EC2Tier) { e.AMI = "ami-gone" }},
		{"subnet missing", "subnets[cloud]", "FAIL", "subnet-x", nil, func(e *config.EC2Tier) { e.Subnets = []string{"subnet-x"} }},
		{"public route, no public IP", "subnets[cloud]", "FAIL", "--map-public-ip-on-launch", func(a *fakeAccount) {
			s := a.subnets["subnet-a"]
			s.MapPublicIpOnLaunch = aws.Bool(false)
			a.subnets["subnet-a"] = s
		}, nil},
		{"no default route", "subnets[cloud]", "FAIL", "no IPv4 default route", func(a *fakeAccount) { a.routes["main:vpc-1"] = "" }, nil},
		{"subnets not allowed", "subnets[cloud]", "WARN", "ec2:DescribeSubnets", func(a *fakeAccount) { a.denied["DescribeSubnets"] = true }, nil},
		{"inbound rules", "security-groups[cloud]", "WARN", "sg-1 (1)", func(a *fakeAccount) {
			g := a.groups["sg-1"]
			g.IpPermissions = []types.IpPermission{{IpProtocol: aws.String("tcp")}}
			a.groups["sg-1"] = g
		}, nil},
		{"no egress", "security-groups[cloud]", "FAIL", "no outbound", func(a *fakeAccount) {
			g := a.groups["sg-1"]
			g.IpPermissionsEgress = nil
			a.groups["sg-1"] = g
		}, nil},
		{"group in another VPC", "security-groups[cloud]", "FAIL", "not in vpc-1", func(a *fakeAccount) {
			g := a.groups["sg-1"]
			g.VpcId = aws.String("vpc-2")
			a.groups["sg-1"] = g
		}, nil},
		{"quota too small", "vcpu-quota[Standard/spot]", "WARN", "needs 40 vCPUs, the quota (L-34B43A08) is 32", func(a *fakeAccount) { a.quotas["L-34B43A08"] = 32 }, nil},
		{"quota not readable", "vcpu-quota[Standard/spot]", "WARN", "servicequotas:GetServiceQuota", func(a *fakeAccount) { a.denied["GetServiceQuota"] = true }, nil},
		{"unknown family", "vcpu-quota", "WARN", "mac2.metal", nil, func(e *config.EC2Tier) { e.InstanceTypes = []string{"mac2.metal"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := goodAccount()
			if c.account != nil {
				c.account(a)
			}
			got := preflight(t, a, ec2Tier(c.tier))
			f, ok := got[c.check]
			if !ok || f.Level != c.level || !strings.Contains(f.Detail, c.detail) {
				t.Fatalf("%s = %+v, want %s containing %q", c.check, f, c.level, c.detail)
			}
		})
	}
}

func TestPreflight_QuotaSumsTiersPerGroupAndMarket(t *testing.T) {
	a := goodAccount()
	a.vcpus["g5.xlarge"] = 4
	a.quotas["L-DB2E81BA"] = 64
	spot := ec2Tier(func(e *config.EC2Tier) { e.Market = config.MarketSpot })
	spot.Name = "spot"
	gpu := ec2Tier(func(e *config.EC2Tier) { e.Market = config.MarketOnDemand; e.InstanceTypes = []string{"g5.xlarge"} })
	gpu.Name, gpu.Max = "gpu", 20
	got := preflight(t, a, ec2Tier(nil), spot, gpu)
	if d := got["vcpu-quota[Standard/spot]"].Detail; !strings.Contains(d, "needs 80 vCPUs") {
		t.Errorf("Standard/spot = %q, want both spot-capable tiers (2 x 40)", d)
	}
	if d := got["vcpu-quota[Standard/on-demand]"].Detail; !strings.Contains(d, "needs 40 vCPUs") {
		t.Errorf("Standard/on-demand = %q, want only the spot-then-on-demand tier", d)
	}
	if f := got["vcpu-quota[G and VT/on-demand]"]; f.Level != "WARN" || !strings.Contains(f.Detail, "needs 80 vCPUs") {
		t.Errorf("G and VT/on-demand = %+v, want a warning: 20 x 4 over 64", f)
	}
	if _, ok := got["vcpu-quota[G and VT/spot]"]; ok {
		t.Error("an on-demand-only tier counted against a spot quota")
	}
}

func TestGroupOf(t *testing.T) {
	for it, want := range map[string]string{
		"c7a.large": "Standard", "m7g.medium": "Standard", "t4g.small": "Standard", "z1d.large": "Standard", "i4i.large": "Standard",
		"g5.xlarge": "G and VT", "vt1.3xlarge": "G and VT", "p4d.24xlarge": "P", "inf2.xlarge": "Inf", "trn1.2xlarge": "Trn",
		"dl1.24xlarge": "DL", "hpc7g.4xlarge": "HPC", "x2gd.medium": "X", "f1.2xlarge": "F", "u-6tb1.metal": "High Memory",
	} {
		if g, ok := groupOf(it); !ok || g.name != want {
			t.Errorf("groupOf(%s) = %q, %v; want %q", it, g.name, ok, want)
		}
	}
	if _, ok := groupOf("mac2.metal"); ok {
		t.Error("mac2.metal has a vCPU quota group")
	}
}
