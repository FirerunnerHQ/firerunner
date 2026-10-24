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
