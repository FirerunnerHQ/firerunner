package ec2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/firerunnerhq/firerunner/internal/config"
)

// PreflightAPI is what the preflight checks read from EC2 beyond API.
type PreflightAPI interface {
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeInstanceTypes(context.Context, *ec2.DescribeInstanceTypesInput, ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
}

// QuotaAPI reads a vCPU quota from Service Quotas.
type QuotaAPI interface {
	GetServiceQuota(context.Context, *servicequotas.GetServiceQuotaInput, ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
}

// IdentityAPI tells who the credentials are.
type IdentityAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// Finding is one preflight check's outcome; Level is "PASS", "WARN" or
// "FAIL", as doctor prints it.
type Finding struct {
	Level, Name, Detail string
}

func passf(name, f string, a ...any) Finding { return Finding{"PASS", name, fmt.Sprintf(f, a...)} }
func warnf(name, f string, a ...any) Finding { return Finding{"WARN", name, fmt.Sprintf(f, a...)} }
func failf(name, f string, a ...any) Finding { return Finding{"FAIL", name, fmt.Sprintf(f, a...)} }

// Preflight checks what EC2 tiers need from the account before a job comes:
// credentials, each tier's AMI, subnets and security groups, the launch
// path's metadata options, and the vCPU quotas the tiers' max would use.
// region is the one the clients use. A check the credentials may not make
// warns, naming the permission, rather than failing.
func (c *Client) Preflight(ctx context.Context, region string, pre PreflightAPI, quotas QuotaAPI, id IdentityAPI, tiers []config.Tier) []Finding {
	who, err := id.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return []Finding{failf("aws-identity", "no usable AWS credentials in %s: %v; on EC2 the instance profile provides them, elsewhere set AWS_PROFILE (and --aws-region)", or(region, "no region"), err)}
	}
	out := []Finding{passf("aws-identity", "%s in %s", aws.ToString(who.Arn), region)}
	for _, t := range tiers {
		if !t.IsEC2() {
			continue
		}
		out = append(out, c.amiCheck(ctx, t), subnetCheck(ctx, pre, t), securityGroupCheck(ctx, pre, t))
	}
	out = append(out, metadataCheck())
	return append(out, quotaChecks(ctx, pre, quotas, tiers)...)
}

func (c *Client) amiCheck(ctx context.Context, t config.Tier) Finding {
	name := "ami[" + t.Name + "]"
	img, err := c.amis.resolve(ctx, t.EC2.AMI)
	if err != nil {
		return failf(name, "%s does not resolve: %v; build the runner AMI (images/README.md) and point ec2.ami at its ID or SSM parameter", t.EC2.AMI, err)
	}
	if img.id == t.EC2.AMI {
		return passf(name, "%s", img.id)
	}
	return passf(name, "%s -> %s", t.EC2.AMI, img.id)
}

// subnetCheck: each subnet exists and can reach github.com over IPv4, which
// has no IPv6: a default route through a NAT, or through an internet gateway
// with a public IPv4 address assigned on launch (a launch asks for none).
func subnetCheck(ctx context.Context, pre PreflightAPI, t config.Tier) Finding {
	name := "subnets[" + t.Name + "]"
	subs, err := pre.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: t.EC2.Subnets})
	if err != nil {
		return denied(name, "ec2:DescribeSubnets", err, "ec2.subnets %v: %v", t.EC2.Subnets, err)
	}
	found := map[string]types.Subnet{}
	for _, s := range subs.Subnets {
		found[aws.ToString(s.SubnetId)] = s
	}
	var notes []string
	for _, id := range t.EC2.Subnets {
		s, ok := found[id]
		if !ok {
			return failf(name, "%s not found in this region", id)
		}
		route, err := defaultRoute(ctx, pre, s)
		if err != nil {
			return denied(name, "ec2:DescribeRouteTables", err, "route table of %s: %v", id, err)
		}
		switch {
		case route == "":
			return failf(name, "%s has no IPv4 default route; instances cannot reach github.com (no IPv6 there): route 0.0.0.0/0 through an internet gateway or a NAT", id)
		case strings.HasPrefix(route, "igw-") && !aws.ToBool(s.MapPublicIpOnLaunch):
			return failf(name, "%s routes through %s but assigns no public IPv4 on launch, so instances cannot reach github.com: aws ec2 modify-subnet-attribute --subnet-id %s --map-public-ip-on-launch, or route through a NAT", id, route, id)
		}
		notes = append(notes, fmt.Sprintf("%s (%s via %s)", id, aws.ToString(s.AvailabilityZone), route))
	}
	return passf(name, "%s", strings.Join(notes, ", "))
}

// defaultRoute is the target of the subnet's 0.0.0.0/0 route: its own route
// table's, else its VPC's main one. "" when there is none.
func defaultRoute(ctx context.Context, pre PreflightAPI, s types.Subnet) (string, error) {
	for _, filters := range [][]types.Filter{
		{{Name: aws.String("association.subnet-id"), Values: []string{aws.ToString(s.SubnetId)}}},
		{{Name: aws.String("vpc-id"), Values: []string{aws.ToString(s.VpcId)}}, {Name: aws.String("association.main"), Values: []string{"true"}}},
	} {
		out, err := pre.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: filters})
		if err != nil {
			return "", err
		}
		if len(out.RouteTables) == 0 {
			continue
		}
		for _, r := range out.RouteTables[0].Routes {
			if aws.ToString(r.DestinationCidrBlock) != "0.0.0.0/0" || r.State == types.RouteStateBlackhole {
				continue
			}
			for _, target := range []*string{r.GatewayId, r.NatGatewayId, r.TransitGatewayId, r.NetworkInterfaceId, r.VpcPeeringConnectionId, r.InstanceId} {
				if aws.ToString(target) != "" {
					return aws.ToString(target), nil
				}
			}
		}
		return "", nil
	}
	return "", nil
}

// securityGroupCheck: runner instances need no inbound rule (they fetch their
// config and talk to GitHub outbound), and must have an outbound one.
func securityGroupCheck(ctx context.Context, pre PreflightAPI, t config.Tier) Finding {
	name := "security-groups[" + t.Name + "]"
	out, err := pre.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: t.EC2.SecurityGroups})
	if err != nil {
		return denied(name, "ec2:DescribeSecurityGroups", err, "ec2.security_groups %v: %v", t.EC2.SecurityGroups, err)
	}
	vpcs := map[string]bool{}
	egress := false
	var inbound []string
	for _, g := range out.SecurityGroups {
		vpcs[aws.ToString(g.VpcId)] = true
		egress = egress || len(g.IpPermissionsEgress) > 0
		if n := len(g.IpPermissions); n > 0 {
			inbound = append(inbound, fmt.Sprintf("%s (%d)", aws.ToString(g.GroupId), n))
		}
	}
	if subs, err := pre.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: t.EC2.Subnets}); err == nil {
		for _, s := range subs.Subnets {
			if !vpcs[aws.ToString(s.VpcId)] {
				return failf(name, "%v are not in %s, the VPC of subnet %s: RunInstances would refuse them", t.EC2.SecurityGroups, aws.ToString(s.VpcId), aws.ToString(s.SubnetId))
			}
		}
	}
	switch {
	case !egress:
		return failf(name, "%v allow no outbound traffic: instances cannot fetch their config or reach GitHub", t.EC2.SecurityGroups)
	case len(inbound) > 0:
		return warnf(name, "inbound rules on %s: runner instances need none (they only connect out), and a job runs untrusted code; remove them", strings.Join(inbound, ", "))
	}
	return passf(name, "%v: outbound only", t.EC2.SecurityGroups)
}

// metadataCheck states the instance metadata options every launch sets, so a
// change that loosens them shows here.
func metadataCheck() Finding {
	m := launchMetadataOptions()
	if m.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(m.HttpPutResponseHopLimit) != 1 {
		return failf("instance-metadata", "launches set HttpTokens=%s, hop limit %d: want IMDSv2 only and one hop, so a job's containers cannot reach IMDS", m.HttpTokens, aws.ToInt32(m.HttpPutResponseHopLimit))
	}
	return passf("instance-metadata", "launches require IMDSv2 with hop limit 1; no instance profile is attached")
}

// quotaGroup is a vCPU quota covering a set of instance families, with its
// Service Quotas codes per market; "" where the group has no such quota.
type quotaGroup struct {
	name, spot, onDemand string
}

// quotaGroups by family prefix, longest first where prefixes overlap. Codes
// read from Service Quotas in us-east-2 on 2026-11-01.
var quotaGroups = []struct {
	prefix string
	group  quotaGroup
}{
	{"u-", quotaGroup{"High Memory", "", "L-43DA4232"}},
	{"inf", quotaGroup{"Inf", "L-B5D1601B", "L-1945791B"}},
	{"trn", quotaGroup{"Trn", "L-6B0D517C", "L-2C3B7624"}},
	{"hpc", quotaGroup{"HPC", "", "L-F7808C92"}},
	{"dl", quotaGroup{"DL", "L-85EED4F7", "L-6E869C2A"}},
	{"vt", quotaGroup{"G and VT", "L-3819A6DF", "L-DB2E81BA"}},
	{"g", quotaGroup{"G and VT", "L-3819A6DF", "L-DB2E81BA"}},
	{"p", quotaGroup{"P", "L-7212CCBC", "L-417A185B"}},
	{"f", quotaGroup{"F", "L-88CF9481", "L-74FC7D96"}},
	{"x", quotaGroup{"X", "L-E3A00192", "L-7295265B"}},
}

var standardQuota = quotaGroup{"Standard", "L-34B43A08", "L-1216C47A"}

// groupOf returns the vCPU quota group of an instance type, false when it
// has none firerunner knows (e.g. mac, on dedicated hosts).
func groupOf(itype string) (quotaGroup, bool) {
	if strings.HasPrefix(itype, "mac") {
		return quotaGroup{}, false
	}
	for _, g := range quotaGroups {
		if strings.HasPrefix(itype, g.prefix) {
			return g.group, true
		}
	}
	if itype != "" && strings.ContainsRune("acdhimrtz", rune(itype[0])) {
		return standardQuota, true
	}
	return quotaGroup{}, false
}

// quotaChecks compare, per quota group and market, the vCPUs every EC2 tier
// at its max would use (its largest type in the group) against the account's
// quota. Other instances in the account count against the same quota, so
// fitting is necessary, not sufficient.
func quotaChecks(ctx context.Context, pre PreflightAPI, quotas QuotaAPI, tiers []config.Tier) []Finding {
	var all []string
	seen := map[string]bool{}
	for _, t := range tiers {
		if !t.IsEC2() {
			continue
		}
		for _, it := range t.EC2.InstanceTypes {
			if !seen[it] {
				seen[it] = true
				all = append(all, it)
			}
		}
	}
	if len(all) == 0 {
		return nil
	}
	vcpus := map[string]int{}
	for start := 0; start < len(all); start += 100 {
		out, err := pre.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: toTypes(all[start:min(start+100, len(all))])})
		if err != nil {
			return []Finding{denied("vcpu-quota", "ec2:DescribeInstanceTypes", err, "instance types %v: %v", all, err)}
		}
		for _, it := range out.InstanceTypes {
			if it.VCpuInfo != nil {
				vcpus[string(it.InstanceType)] = int(aws.ToInt32(it.VCpuInfo.DefaultVCpus))
			}
		}
	}
	type key struct {
		group  quotaGroup
		market string
	}
	need := map[key]int{}
	var findings []Finding
	for _, t := range tiers {
		if !t.IsEC2() {
			continue
		}
		largest := map[quotaGroup]int{}
		for _, it := range t.EC2.InstanceTypes {
			g, ok := groupOf(it)
			if !ok {
				findings = append(findings, warnf("vcpu-quota", "tier %s: no vCPU quota known for %s; check its quota by hand", t.Name, it))
				continue
			}
			largest[g] = max(largest[g], vcpus[it])
		}
		for g, v := range largest {
			for _, m := range markets(t.EC2.Market) {
				need[key{g, m}] += t.Max * v
			}
		}
	}
	keys := make([]key, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group.name != keys[j].group.name {
			return keys[i].group.name < keys[j].group.name
		}
		return keys[i].market < keys[j].market
	})
	for _, k := range keys {
		name := "vcpu-quota[" + k.group.name + "/" + k.market + "]"
		code := k.group.onDemand
		if k.market == config.MarketSpot {
			code = k.group.spot
		}
		if code == "" {
			findings = append(findings, warnf(name, "the %s group has no %s quota; its types cannot launch on %s", k.group.name, k.market, k.market))
			continue
		}
		q, err := quotas.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: aws.String("ec2"), QuotaCode: aws.String(code)})
		if err != nil || q.Quota == nil {
			findings = append(findings, denied(name, "servicequotas:GetServiceQuota", err, "quota %s: %v", code, err))
			continue
		}
		have := int(aws.ToFloat64(q.Quota.Value))
		if need[k] > have {
			findings = append(findings, warnf(name, "the tiers' max needs %d vCPUs, the quota (%s) is %d: request an increase in Service Quotas, or lower max", need[k], code, have))
			continue
		}
		findings = append(findings, passf(name, "the tiers' max needs %d vCPUs of %d (%s); other instances in the account share it", need[k], have, code))
	}
	return findings
}

func markets(policy string) []string {
	switch policy {
	case config.MarketSpot:
		return []string{config.MarketSpot}
	case config.MarketOnDemand:
		return []string{config.MarketOnDemand}
	}
	return []string{config.MarketSpot, config.MarketOnDemand}
}

func toTypes(names []string) []types.InstanceType {
	out := make([]types.InstanceType, len(names))
	for i, n := range names {
		out[i] = types.InstanceType(n)
	}
	return out
}

// denied turns a call the credentials may not make into a warning naming the
// permission; any other error fails the check with the given detail.
func denied(name, action string, err error, f string, a ...any) Finding {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "UnauthorizedOperation", "AccessDenied", "AccessDeniedException":
			return warnf(name, "cannot check: %s is not allowed; update the control stack (deploy/cloudformation/control.yaml grants it)", action)
		}
	}
	return failf(name, f, a...)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
