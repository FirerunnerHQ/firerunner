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


runner_compute, byoc_block = block("control.yaml"), block("byoc-role.yaml")
if runner_compute != byoc_block:
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
        if "Tag/firerunner:owner" in line and '[!Ref Owner, !Sub "${Owner}/*", !If [HasTenantOwner, !Sub "${TenantOwner}/*", !Sub "${Owner}/*"]]' not in line:
            sys.exit(f"FAIL {name}: an owner condition leaves out tenants, under Owner or TenantOwner: {line.strip()}")
    if "HasTenantOwner: !Not [!Equals [!Ref TenantOwner, \"\"]]" not in open(os.path.join(HERE, name)).read():
        sys.exit(f"FAIL {name}: HasTenantOwner must be set exactly when TenantOwner is")
    # A glob in TenantOwner would make <TenantOwner>/* match every owner.
    if 'TenantOwner:\n    Type: String\n    Default: ""\n    AllowedPattern: "([A-Za-z0-9][A-Za-z0-9._-]{0,62})?"' not in open(os.path.join(HERE, name)).read():
        sys.exit(f"FAIL {name}: TenantOwner must be empty or a plain name: no glob, no /")
print("ok   every owner condition covers the install's tenants, under Owner or TenantOwner")

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
# Or, for shards, one given secret (PoolKeySecretArn) in its place: the
# stack then makes none, and reads that one alone.
if "Condition: OwnPoolKey" not in pool_key or 'OwnPoolKey: !Equals [!Ref PoolKeySecretArn, ""]' not in control:
    sys.exit("FAIL control.yaml: PoolKey must be made exactly when no PoolKeySecretArn is given")
OWN_OR_GIVEN = "!If [OwnPoolKey, !Ref PoolKey, !Ref PoolKeySecretArn]"
grants = re.findall(r"secretsmanager:[A-Za-z*]+", control)
# Two: the pool key, and onboarding's webhook secret, which the onboarding
# check below pins to its parameter and condition.
if grants != ["secretsmanager:GetSecretValue"] * 2 or "secretsmanager" in runner_compute:
    sys.exit(f"FAIL control.yaml: the secrets grants are {grants}; want GetSecretValue alone, twice, outside runner-compute")
get = control_policy.split("Action: secretsmanager:GetSecretValue", 1)[1].split("- Effect", 1)[0]
if get.strip().splitlines()[0] != "Resource: " + OWN_OR_GIVEN:
    sys.exit("FAIL control.yaml: the control role may read only the pool key")
template = resource("LaunchTemplate")
if "FR_POOL_KEY_SECRET=${PoolKeySecretRef}" not in template or "PoolKeySecretRef: " + OWN_OR_GIVEN not in template:
    sys.exit("FAIL control.yaml: the control VM is not told the pool key's secret")
if not re.search(r'PoolKeySecretArn:\n    Type: String\n    Default: ""\n    AllowedPattern: "\(arn:\[a-z-\]\+:secretsmanager:', control):
    sys.exit("FAIL control.yaml: PoolKeySecretArn must be empty or a Secrets Manager ARN")
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

# The control VM reads the tenant states and never writes them: only an
# operator suspends. It writes the flags beside them (GuardDuty, --guardduty)
# and deletes none, so it can neither suspend a tenant nor clear a flag. It
# reads the entitlements billing writes, and cannot write them, so it can
# never lift a cap. It writes no other parameter under the prefix, where its
# GitHub credentials live. GuardDuty it only reads, and only this account's
# detectors here.
prefix = 'arn:${AWS::Partition}:ssm:${AWS::Region}:${AWS::AccountId}:parameter${ParameterPrefix}/control/'
by_path = re.findall(r"Action: ssm:GetParametersByPath\n\s+Resource: !Sub \"(.*)\"", control_policy)
if by_path != [prefix + "tenant-state", prefix + "tenant-state-flags", prefix + "entitlements"]:
    sys.exit(f"FAIL control.yaml: the control role reads by path {by_path}; want the tenant states, flags and entitlements")
ssm_writes = re.findall(r"ssm:(?:Put|Delete|Label|Add|Remove|\*)[A-Za-z*]*", control_role)
put = control_policy.split("Action: ssm:PutParameter", 1)[1].split("- Effect", 1)[0] if ssm_writes else ""
if ssm_writes != ["ssm:PutParameter"] or put.strip().splitlines()[0] != f'Resource: !Sub "{prefix}tenant-state-flags/*"':
    sys.exit(f"FAIL control.yaml: the control role's SSM writes are {ssm_writes}; want PutParameter on the flags alone")
gd = re.findall(r"guardduty:[A-Za-z*]+", control)
if gd != ["guardduty:ListDetectors", "guardduty:ListFindings", "guardduty:GetFindings"]:
    sys.exit(f"FAIL control.yaml: the GuardDuty grants are {gd}; want ListDetectors, ListFindings and GetFindings")
read = control_policy.split("Action: [guardduty:ListFindings, guardduty:GetFindings]", 1)[1].split("- Effect", 1)[0]
if read.strip().splitlines()[0] != 'Resource: !Sub "arn:${AWS::Partition}:guardduty:${AWS::Region}:${AWS::AccountId}:detector/*"' or \
        "guardduty" in runner_compute:
    sys.exit("FAIL control.yaml: GuardDuty findings must be read only from this account's detectors, outside runner-compute")
print("ok   control.yaml: the control VM writes only tenant flags, and only reads GuardDuty")

# Hosted onboarding. The ingress runs no code and verifies nothing, so its
# role may only send to its own queue; the control VM only reads and
# deletes from it, never sends (a forged delivery would still need the
# webhook secret, but the VM has no reason to send), and only with
# onboarding on. Its secret grants are the pool key's and, with
# onboarding, the webhook secret's: no other.
onboarding = open(os.path.join(HERE, "onboarding.yaml")).read()
sqs_in = re.findall(r"sqs:[A-Z][A-Za-z*]*", onboarding)
if sqs_in != ["sqs:SendMessage"] or "Resource: !GetAtt Queue.Arn" not in onboarding:
    sys.exit(f"FAIL onboarding.yaml: the ingress role's SQS grants are {sqs_in}; want SendMessage on its queue alone")
if onboarding.count("PolicyName:") != 1 or "ManagedPolicyArns" in onboarding or "AWS::Lambda" in onboarding:
    sys.exit("FAIL onboarding.yaml: the ingress holds one policy and runs no code")
if "PassthroughBehavior: NEVER" not in onboarding or "application/json: |" not in onboarding:
    sys.exit("FAIL onboarding.yaml: only application/json is enqueued; anything else must be refused")
print("ok   onboarding.yaml: the ingress only sends to its own queue, and runs no code")
sqs_ctl = re.findall(r"sqs:[A-Z][A-Za-z*]*", control)
if sorted(sqs_ctl) != sorted(["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes"]):
    sys.exit(f"FAIL control.yaml: the control role's SQS grants are {sqs_ctl}; want to read and delete, never to send")
onb = control_policy.split("- Onboard\n", 1)
if len(onb) != 2 or "Resource: !Ref OnboardQueueArn" not in onb[1].split("- !Ref AWS::NoValue", 1)[0]:
    sys.exit("FAIL control.yaml: the queue grant must be on OnboardQueueArn alone, under the Onboard condition")
secrets = re.findall(r"Action: secretsmanager:GetSecretValue\n\s+Resource: (.*)", control_policy)
if secrets != ["!If [OwnPoolKey, !Ref PoolKey, !Ref PoolKeySecretArn]", "!Ref OnboardWebhookSecretArn"]:
    sys.exit(f"FAIL control.yaml: the control role reads secrets {secrets}; want the pool key's and onboarding's webhook secret")
if "- !If\n                - Onboard\n                - Effect: Allow\n                  Action: secretsmanager:GetSecretValue" not in control_policy:
    sys.exit("FAIL control.yaml: the webhook secret grant must be under the Onboard condition")
print("ok   control.yaml: the control VM reads onboarding's queue and secret only with onboarding on, and never sends")

# The ingress's envelope is the worker's: the keys the mapping template
# writes are onboard.Envelope's JSON tags, and the body goes base64. As text,
# not a binary media type: binary made API Gateway answer 500 for a queued
# delivery; and a UTF-8 body with an em dash, CJK and a check mark came off
# the queue byte-identical, its signature holding (measured 2026-11-18).
worker = open(os.path.join(HERE, "..", "..", "proprietary", "onboard", "worker.go")).read()
env_struct = re.search(r"type Envelope struct \{(.*?)\n\}", worker, re.S).group(1)
tags = re.findall(r'json:"([a-z_]+)"', env_struct)
vtl = re.search(r"application/json: \|-\n(.*?)\n        IntegrationResponses", onboarding, re.S).group(1)
keys = re.findall(r'"([a-z_]+)":"', vtl)
if keys != tags:
    sys.exit(f"FAIL onboarding.yaml: the template writes {keys}; onboard.Envelope reads {tags}")
if "BinaryMediaTypes" in onboarding or "ContentHandling" in onboarding or "#set($b = $util.base64Encode($input.body))" not in vtl:
    sys.exit("FAIL onboarding.yaml: the body must go base64 by the template, the API taking no binary media type")
print("ok   onboarding.yaml: the envelope is onboard.Envelope's, the body base64")
# SQS refusing a message is a failure GitHub sees; GitHub's headers are
# required; and a message as large as SQS takes fits.
if 'SelectionPattern: "4\\\\d{2}|5\\\\d{2}"' not in onboarding:
    sys.exit("FAIL onboarding.yaml: an SQS 4xx or 5xx must answer 500")
for h in ("X-GitHub-Delivery", "X-GitHub-Event", "X-Hub-Signature-256"):
    if f"method.request.header.{h}: true" not in onboarding:
        sys.exit(f"FAIL onboarding.yaml: {h} must be required")
if onboarding.count("MaximumMessageSize: 1048576") != 2:
    sys.exit("FAIL onboarding.yaml: both queues must take SQS's largest message")
print("ok   onboarding.yaml: SQS's refusals answer 500, GitHub's headers required, large deliveries fit")
# A deployment is a snapshot, made anew only under a new logical id: its
# name carries a hash of what it deploys.
import hashlib
a, b = onboarding.index("\n  Api:\n"), onboarding.index("\n  # A deployment is a snapshot")
want = "Deployment" + hashlib.sha256(onboarding[a:b].encode()).hexdigest()[:10]
names = re.findall(r"^  (Deployment[0-9a-f]*):$", onboarding, re.M)
if names != [want] or f"DeploymentId: !Ref {want}" not in onboarding:
    sys.exit(f"FAIL onboarding.yaml: the API changed, so its deployment must too: rename {names} to {want}, and the stage's DeploymentId")
print("ok   onboarding.yaml: the deployment is named by what it deploys")

# Found on the first deployment: API Gateway assumes the integration's role
# with no source account, so a condition on it refuses every delivery.
if "aws:SourceAccount" in onboarding:
    sys.exit("FAIL onboarding.yaml: API Gateway cannot assume a role conditioned on aws:SourceAccount")
print("ok   onboarding.yaml: API Gateway can assume the ingress role")
