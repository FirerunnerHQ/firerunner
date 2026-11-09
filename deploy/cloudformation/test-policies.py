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

# A tenant's instances carry <owner>/<id>, so every grant on the owner tag must
# reach them, or its launches are denied while its scale sets look healthy.
for name in ("control.yaml", "byoc-role.yaml", "reaper.yaml"):
    for line in open(os.path.join(HERE, name)):
        if "Tag/firerunner:owner" in line and '[!Ref Owner, !Sub "${Owner}/*"]' not in line:
            sys.exit(f"FAIL {name}: an owner condition leaves out tenants: {line.strip()}")
print("ok   every owner condition covers the install's tenants")
