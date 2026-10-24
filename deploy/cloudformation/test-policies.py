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
if before.count("- Effect: Allow") != 1 or "- Effect" in after:  # the one before is the trust policy
    sys.exit("FAIL byoc-role.yaml has statements outside the runner-compute block")
if byoc.count("PolicyName:") != 1 or "ManagedPolicyArns" in byoc:
    sys.exit("FAIL byoc-role.yaml grants more than its one runner-compute policy")
print("ok   byoc-role.yaml grants only the runner-compute policy")
