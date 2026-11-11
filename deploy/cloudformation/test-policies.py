#!/usr/bin/env python3
"""Invariants of the templates' IAM policies.

The runner-compute policy is identical in control.yaml and byoc-role.yaml:
CloudFormation has no includes, so the block is copied; this keeps the copies
equal. The cache bucket is reachable only through the cache role. Needs only
python3.
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))


def block(name):
    text = open(os.path.join(HERE, name)).read()
    begin, end = text.find("# runner-compute:begin"), text.find("# runner-compute:end")
    if begin < 0 or end < begin:
        sys.exit(f"FAIL {name}: no runner-compute:begin/end block")
    return text[text.index("\n", begin):end]


a, b = block("control.yaml"), block("byoc-role.yaml")
if a != b:
    sys.exit("FAIL the runner-compute policy differs between control.yaml and byoc-role.yaml")
print("ok   runner-compute policy identical in control.yaml and byoc-role.yaml")

# The cross-account role grants nothing beyond that block.
byoc = open(os.path.join(HERE, "byoc-role.yaml")).read()
before = byoc[:byoc.find("# runner-compute:begin")]
after = byoc[byoc.find("# runner-compute:end"):]
if before.count("- Effect: Allow") != 1 or "- Effect: Allow" in after:  # the one before is the trust policy
    sys.exit("FAIL byoc-role.yaml has statements outside the runner-compute block")
print("ok   byoc-role.yaml grants only the runner-compute policy")

# The block's SSM grant covers the whole prefix, where a control VM in the
# same account keeps its GitHub credentials; the cross-account role must be
# denied them.
deny = '''              - Effect: Deny
                Action: "ssm:*"
                Resource: !Sub "arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter${ParameterPrefix}/control/*"'''
if deny not in after or after.count("- Effect") != 1:
    sys.exit("FAIL byoc-role.yaml must deny exactly <prefix>/control/*, after the runner-compute block")
print("ok   byoc-role.yaml is denied the control VM's parameters")
if byoc.count("PolicyName:") != 1 or "ManagedPolicyArns" in byoc:
    sys.exit("FAIL byoc-role.yaml grants more than its one runner-compute policy")
print("ok   byoc-role.yaml has its one runner-compute policy")

# cache-server parses requests from job code, so the bucket belongs to a role
# of its own, never the control plane's: the control role grants no S3 and may
# assume only the cache role, which trusts only the control role and holds
# only the bucket's prefix.

control = open(os.path.join(HERE, "control.yaml")).read()


def resource(name):
    m = re.search(rf"^  {name}:\n(.*?)(?=^  \S|^\S)", control, re.S | re.M)
    if not m:
        sys.exit(f"FAIL control.yaml has no {name} resource")
    return m.group(1)


control_role = resource("ControlRole")
if re.search(r"\bs3:", control_role):
    sys.exit("FAIL control.yaml: the control role grants S3; only CacheRole may")
if "sts:AssumeRole" in control_role.split("Policies:", 1)[1]:
    sys.exit("FAIL control.yaml: the control role's own policy grants sts:AssumeRole; only CacheAssume may")
print("ok   control.yaml: the control role grants no S3")

cache_role = resource("CacheRole")
trust = cache_role.split("Policies:", 1)[0]
want_trust = "Statement: [{ Effect: Allow, Principal: { AWS: !GetAtt ControlRole.Arn }, Action: sts:AssumeRole }]"
if "Condition: CacheS3" not in cache_role or want_trust not in trust or trust.count("Effect:") != 1:
    sys.exit("FAIL control.yaml: CacheRole must exist only with Cache=s3 and trust only the control role")
print("ok   control.yaml: CacheRole trusts only the control role")
actions = sorted(set(re.findall(r"\bs3:[A-Z][A-Za-z]+", cache_role)))
if actions != ["s3:AbortMultipartUpload", "s3:DeleteObject", "s3:GetObject", "s3:ListBucket", "s3:PutObject"]:
    sys.exit(f"FAIL control.yaml: CacheRole's S3 actions are {actions}")
resources = re.findall(r"Resource: (.*)", cache_role)
if resources != ['!Sub "${CacheBucket.Arn}/firerunner-cache/*"', "!GetAtt CacheBucket.Arn"] or \
        'Condition: { StringLike: { "s3:prefix": ["firerunner-cache/*"] } }' not in cache_role or \
        cache_role.count("Effect: Allow") != 3 or "Deny" in cache_role:
    sys.exit("FAIL control.yaml: CacheRole must grant only objects under firerunner-cache/ and list only that prefix")
print("ok   control.yaml: CacheRole holds only the bucket's prefix")

assume = resource("CacheAssume")
if "Condition: CacheS3" not in assume or "RoleName: !Ref ControlRole" not in assume or \
        assume.count("Effect:") != 1 or "Action: sts:AssumeRole" not in assume or \
        "Resource: !GetAtt CacheRole.Arn" not in assume:
    sys.exit("FAIL control.yaml: CacheAssume must let the control role assume CacheRole and nothing else")
print("ok   control.yaml: the control role may assume only CacheRole")

# Nothing else widens the control role: no wildcard grants in it, and no
# other policy resource attached to it.
if re.search(r"""\bsts:\*|Action: *\[?["']?\*["']?\]?\s*$|Action:.*["']\*["']""", control_role, re.M):
    sys.exit("FAIL control.yaml: the control role has a wildcard action")
refs = sorted(name for name in re.findall(r"^  (\w+):\n", control, re.M)
              if name != "ControlRole" and re.search(r"!Ref ControlRole\b|ControlRole\.Arn|!GetAtt ControlRole\b", resource(name)))
if refs != ["CacheAssume", "CacheRole", "ControlProfile"]:
    sys.exit(f"FAIL control.yaml: only CacheAssume, CacheRole's trust and ControlProfile may name the control role; so do {refs}")
policies = re.findall(r"^  (\w+):\n    Type: AWS::IAM::(?:Policy|RolePolicy|ManagedPolicy)\b", control, re.M)
if policies != ["CacheAssume"]:
    sys.exit(f"FAIL control.yaml: the IAM policy resources are {policies}, want only CacheAssume")
print("ok   control.yaml: nothing else grants the control role more")

# The first VM boots only once the control role may assume the cache role.
if "CacheAssume: !If [CacheS3, !Ref CacheAssume, \"\"]" not in resource("ControlGroup").split("Properties:", 1)[0]:
    sys.exit("FAIL control.yaml: ControlGroup must wait for CacheAssume")
print("ok   control.yaml: the control VM waits for CacheAssume")

# Instance metadata only where the cache unit's IPv4 deny covers it.
options = re.search(r"MetadataOptions: \{(.*)\}", resource("LaunchTemplate"))
if not options or "HttpProtocolIpv6: disabled" not in options.group(1):
    sys.exit("FAIL control.yaml: the launch template must disable IMDS over IPv6")
print("ok   control.yaml: IMDS is IPv4 only")

# A tenant's instances carry <owner>/<id>, so every grant on the owner tag must
# reach them, or its launches are denied while its scale sets look healthy.
for name in ("control.yaml", "byoc-role.yaml", "reaper.yaml"):
    for line in open(os.path.join(HERE, name)):
        if "Tag/firerunner:owner" in line and '[!Ref Owner, !Sub "${Owner}/*"]' not in line:
            sys.exit(f"FAIL {name}: an owner condition leaves out tenants: {line.strip()}")
print("ok   every owner condition covers the install's tenants")

# The JIT name cleanup may delete one A record, the JIT name, in the one zone,
# and nothing else: on a zone it was given, the zone's other records are
# someone else's. It runs after the group is gone and before the zone goes.
cleanup_role = resource("JitNameCleanupRole")
trust, policy = cleanup_role.split("Policies:", 1)
if trust.count("Effect:") != 1 or "Principal: { Service: lambda.amazonaws.com }, Action: sts:AssumeRole" not in trust:
    sys.exit("FAIL control.yaml: JitNameCleanupRole must trust only Lambda")
actions = re.findall(r"Action: (.*)", policy)
if actions != ["route53:ListResourceRecordSets", "route53:ChangeResourceRecordSets", "cloudformation:DescribeStacks",
               "[logs:CreateLogStream, logs:PutLogEvents]"] or policy.count("Effect: Allow") != 4 or "Deny" in policy:
    sys.exit(f"FAIL control.yaml: JitNameCleanupRole's actions are {actions}")
zone_arn = '''Resource: !Sub
                  - "arn:${AWS::Partition}:route53:::hostedzone/${Z}"
                  - { Z: !If [CreateZone, !Ref Zone, !Ref HostedZoneId] }'''
change = policy.split("route53:ChangeResourceRecordSets", 1)[1].split("- Effect", 1)[0]
if policy.count(zone_arn) != 2 or zone_arn not in change or \
        "route53:ChangeResourceRecordSetsActions: [DELETE]" not in change or \
        "route53:ChangeResourceRecordSetsRecordTypes: [A]" not in change or \
        'route53:ChangeResourceRecordSetsNormalizedRecordNames: [!Sub "${JitName}.${ZoneName}"]' not in change or \
        change.count("ForAllValues:") != 2:
    sys.exit("FAIL control.yaml: JitNameCleanupRole may change only the JIT name's A record, by DELETE, in the stack's zone")
if "Resource: !GetAtt JitNameCleanupLogGroup.Arn" not in policy:
    sys.exit("FAIL control.yaml: JitNameCleanupRole may write only its own log group")
describe = policy.split("cloudformation:DescribeStacks", 1)[1].split("- Effect", 1)[0]
if "Resource: !Ref AWS::StackId" not in describe:
    sys.exit("FAIL control.yaml: JitNameCleanupRole may describe only its own stack")
print("ok   control.yaml: the JIT name cleanup may delete only the JIT name's A record")
cleanup = resource("JitNameCleanup")
if "ZoneId: !If [CreateZone, !Ref Zone, !Ref HostedZoneId]" not in cleanup or \
        'Name: !Sub "${JitName}.${ZoneName}"' not in cleanup or \
        'OwnZone: !If [CreateZone, "true", "false"]' not in cleanup or \
        "ServiceTimeout: 300" not in cleanup or \
        "DependsOn: JitNameCleanup" not in resource("ControlGroup").split("Properties:", 1)[0]:
    sys.exit("FAIL control.yaml: JitNameCleanup must name the zone, and ControlGroup depend on it")
print("ok   control.yaml: the JIT name is removed after the group and before the zone")

# The control VM may write its own JIT name's A record, by UPSERT, and nothing
# else in a zone other stacks may share.
control_policy = resource("ControlRole").split("Policies:", 1)[1]
vm_change = control_policy.split("Action: route53:ChangeResourceRecordSets", 1)[1].split("- Effect", 1)[0]
if "route53:ChangeResourceRecordSetsActions: [UPSERT]" not in vm_change or \
        "route53:ChangeResourceRecordSetsRecordTypes: [A]" not in vm_change or \
        'route53:ChangeResourceRecordSetsNormalizedRecordNames: [!Sub "${JitName}.${ZoneName}"]' not in vm_change or \
        vm_change.count("ForAllValues:") != 2:
    sys.exit("FAIL control.yaml: the control VM may change only its JIT name's A record, by UPSERT")
print("ok   control.yaml: the control VM may write only its own JIT name")

# The pool key signs EC2 pool instances: generated by the stack, readable by
# the control role and nothing else this template makes, and only that one
# secret. The runner-compute block, which byoc-role.yaml copies, has none.
pool_key = resource("PoolKey")
if "Type: AWS::SecretsManager::Secret" not in pool_key or "PasswordLength: 64" not in pool_key or \
        "GenerateSecretString:" not in pool_key or "SecretString:" in pool_key.replace("GenerateSecretString:", ""):
    sys.exit("FAIL control.yaml: PoolKey must be a generated 64-character secret")
grants = re.findall(r"secretsmanager:[A-Za-z*]+", control)
if grants != ["secretsmanager:GetSecretValue"] or "secretsmanager" in a:
    sys.exit(f"FAIL control.yaml: the secrets grants are {grants}; want GetSecretValue alone, outside runner-compute")
get = control_policy.split("Action: secretsmanager:GetSecretValue", 1)[1].split("- Effect", 1)[0]
if get.strip() != "Resource: !Ref PoolKey":
    sys.exit("FAIL control.yaml: the control role may read only the pool key")
if "FR_POOL_KEY_SECRET=${PoolKey}" not in resource("LaunchTemplate"):
    sys.exit("FAIL control.yaml: the control VM is not told the pool key's secret")
print("ok   control.yaml: only the control role reads the pool key, and only it")

# The runner subnets deny outbound SMTP through their network ACL, numbered
# ahead of the allow-all rule (ACL rules apply lowest number first), and every
# subnet is on that ACL.
network = open(os.path.join(HERE, "network.yaml")).read()
entries = re.findall(r"^  (\w+):\n    Type: AWS::EC2::NetworkAclEntry\n    Properties: \{(.*)\}$", network, re.M)
deny = {int(re.search(r"From: (\d+)", p).group(1)): int(re.search(r"RuleNumber: (\d+)", p).group(1))
        for _, p in entries if "RuleAction: deny" in p and "Egress: true" in p and "Protocol: 6" in p}
allow_out = [int(re.search(r"RuleNumber: (\d+)", p).group(1)) for _, p in entries
             if "RuleAction: allow" in p and "Egress: true" in p]
if sorted(deny) != [25, 465, 587] or len(allow_out) != 1 or max(deny.values()) >= allow_out[0]:
    sys.exit(f"FAIL network.yaml: outbound SMTP must be denied ahead of the allow-all rule: deny {deny}, allow {allow_out}")
subnets = re.findall(r"^  (Subnet[A-Z]):\n    Type: AWS::EC2::Subnet$", network, re.M)
on_acl = re.findall(r"SubnetNetworkAclAssociation\n(?:    Condition: \w+\n)?    Properties: \{ SubnetId: !Ref (Subnet[A-Z]), NetworkAclId: !Ref RunnerAcl \}", network)
if sorted(subnets) != sorted(on_acl) or not subnets:
    sys.exit(f"FAIL network.yaml: subnets {subnets} on the runner ACL {on_acl}")
print("ok   network.yaml: the runner subnets deny outbound SMTP")
