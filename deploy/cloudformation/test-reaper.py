#!/usr/bin/env python3
"""Unit tests for the reaper Lambda inline in reaper.yaml.

The code is read from the template itself, so what is tested is what
deploys. A fake boto3 module stands in for AWS. Needs only python3.
"""
import datetime
import os
import re
import sys
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
UTC = datetime.timezone.utc
NOW = datetime.datetime(2026, 10, 24, 12, 0, tzinfo=UTC)


def lambda_source():
    text = open(os.path.join(HERE, "reaper.yaml")).read()
    m = re.search(r"\n        ZipFile: \|\n((?:          .*\n|\n)+)", text)
    return "".join(line[10:] if line.strip() else "\n" for line in m.group(1).splitlines(True))


def load(env):
    sys.modules["boto3"] = types.SimpleNamespace(client=lambda name: None)
    os.environ.update(env)
    mod = types.ModuleType("index")
    exec(compile(lambda_source(), "reaper.yaml:index.py", "exec"), mod.__dict__)
    return mod


class FakeEC2:
    """Pages of instances; records calls; fails cancels for listed requests."""

    def __init__(self, instances, cancel_fails=()):
        self.instances, self.cancel_fails, self.calls, self.filters = instances, set(cancel_fails), [], None

    def get_paginator(self, name):
        assert name == "describe_instances"
        fake = self

        class P:
            def paginate(self, Filters):
                fake.filters = Filters
                # Two pages, to prove every page is read.
                half = len(fake.instances) // 2
                return [{"Reservations": [{"Instances": fake.instances[:half]}]},
                        {"Reservations": [{"Instances": fake.instances[half:]}]}]
        return P()

    def cancel_spot_instance_requests(self, SpotInstanceRequestIds):
        self.calls.append(("cancel", SpotInstanceRequestIds[0]))
        if SpotInstanceRequestIds[0] in self.cancel_fails:
            raise RuntimeError("RequestLimitExceeded")

    def terminate_instances(self, InstanceIds):
        self.calls.append(("terminate", InstanceIds[0]))


def inst(iid, minutes_ago, spot=None):
    i = {"InstanceId": iid, "LaunchTime": NOW - datetime.timedelta(minutes=minutes_ago)}
    if spot:
        i["SpotInstanceRequestId"] = spot
    return i


class ReaperTest(unittest.TestCase):
    def run_reaper(self, instances, dry_run="false", cancel_fails=()):
        mod = load({"OWNER": "acme", "MAX_AGE_MINUTES": "720", "DRY_RUN": dry_run})
        ec2 = FakeEC2(instances, cancel_fails)
        out = mod.handler({}, None, ec2=ec2, now=NOW)
        return out, ec2

    def test_ends_only_instances_past_the_limit(self):
        out, ec2 = self.run_reaper([inst("i-young", 719), inst("i-old", 721), inst("i-older", 3000)])
        self.assertEqual(out["ended"], ["i-old", "i-older"])
        self.assertEqual(ec2.calls, [("terminate", "i-old"), ("terminate", "i-older")])

    def test_scoped_to_owner_and_running_states(self):
        _, ec2 = self.run_reaper([])
        self.assertIn({"Name": "tag:firerunner:owner", "Values": ["acme"]}, ec2.filters)
        self.assertIn({"Name": "instance-state-name", "Values": ["pending", "running"]}, ec2.filters)

    def test_cancels_the_spot_request_before_terminating(self):
        _, ec2 = self.run_reaper([inst("i-spot", 800, spot="sir-1")])
        self.assertEqual(ec2.calls, [("cancel", "sir-1"), ("terminate", "i-spot")])

    def test_never_terminates_with_the_request_open(self):
        out, ec2 = self.run_reaper([inst("i-stuck", 800, spot="sir-bad"), inst("i-ok", 800)], cancel_fails={"sir-bad"})
        self.assertNotIn(("terminate", "i-stuck"), ec2.calls)
        self.assertEqual(out["kept_open_request"], ["i-stuck"])
        self.assertEqual(out["ended"], ["i-ok"])

    def test_dry_run_changes_nothing(self):
        out, ec2 = self.run_reaper([inst("i-old", 800, spot="sir-1")], dry_run="true")
        self.assertEqual(ec2.calls, [])
        self.assertEqual(out["would_end"], ["i-old"])


if __name__ == "__main__":
    unittest.main(verbosity=1)
