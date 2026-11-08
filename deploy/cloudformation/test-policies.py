#!/usr/bin/env python3
"""The runner-compute policy is identical in control.yaml and byoc-role.yaml.

CloudFormation has no includes, so the block is copied; this keeps the copies
equal. Needs only python3.
"""
import os
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

# A tenant's instances carry <owner>/<id>, so every grant on the owner tag must
# reach them, or its launches are denied while its scale sets look healthy.
for name in ("control.yaml", "byoc-role.yaml", "reaper.yaml"):
    for line in open(os.path.join(HERE, name)):
        if "Tag/firerunner:owner" in line and '[!Ref Owner, !Sub "${Owner}/*"]' not in line:
            sys.exit(f"FAIL {name}: an owner condition leaves out tenants: {line.strip()}")
print("ok   every owner condition covers the install's tenants")
