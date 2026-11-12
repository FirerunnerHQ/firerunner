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
    assert m, "reaper.yaml: no inline ZipFile code found"
    return "".join(line[10:] if line.strip() else "\n" for line in m.group(1).splitlines(True))


def load(env):
    sys.modules["boto3"] = types.SimpleNamespace(client=lambda name: None)
    os.environ.update(env)
    mod = types.ModuleType("index")
    exec(compile(lambda_source(), "reaper.yaml:index.py", "exec"), mod.__dict__)
    return mod


class FakeEC2:
    """Pages of instances; records calls. Spot requests are one-time when
    listed in one_time; cancels fail for cancel_fails, terminates for
    terminate_fails."""

    def __init__(self, instances, cancel_fails=(), terminate_fails=(), one_time=(), request_tags=None):
        self.instances, self.calls, self.filters = instances, [], None
        self.cancel_fails, self.terminate_fails, self.one_time = set(cancel_fails), set(terminate_fails), set(one_time)
        self.request_tags = request_tags or {}

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

    def describe_spot_instance_requests(self, SpotInstanceRequestIds):
        req = SpotInstanceRequestIds[0]
        tags = [{"Key": k, "Value": v} for k, v in self.request_tags.get(req, {}).items()]
        return {"SpotInstanceRequests": [{"SpotInstanceRequestId": req, "Tags": tags,
                                          "Type": "one-time" if req in self.one_time else "persistent"}]}

    def cancel_spot_instance_requests(self, SpotInstanceRequestIds):
        self.calls.append(("cancel", SpotInstanceRequestIds[0]))
        if SpotInstanceRequestIds[0] in self.cancel_fails:
            raise RuntimeError("RequestLimitExceeded")

    def terminate_instances(self, InstanceIds):
        self.calls.append(("terminate", InstanceIds[0]))
        if InstanceIds[0] in self.terminate_fails:
            raise RuntimeError("OperationNotPermitted")


def inst(iid, minutes_ago, spot=None, state="running", pool=None, tags=None):
    i = {"InstanceId": iid, "LaunchTime": NOW - datetime.timedelta(minutes=minutes_ago), "State": {"Name": state}}
    if spot:
        i["SpotInstanceRequestId"] = spot
    i["Tags"] = [{"Key": k, "Value": v} for k, v in (tags or {}).items()]
    if pool:
        i["Tags"].append({"Key": "firerunner:pool", "Value": pool})
    return i


KEY = "k" * 32
# The vectors internal/ec2/macvector_test.go asserts of the Go signer.
VECTORS = {
    ("firerunner/ec2-pool-mac/v1", "acme", "small", "i-0aaa"): "afac3042f858ac4991846661b231d8737442fe01cfa237ffddcb448f5062a514",
    ("firerunner/ec2-runner-mac/v1", "acme/t1", "small", "runner-1", "i-0bbb"): "0998b25ee7b05b1e1ec6f07bdb111d569bebfa0bb3697a20b101c5307ceb4542",
    ("firerunner/ec2-request-mac/v1", "acme", "small", "sir-0ccc"): "868a83491aee3e25f37ecc2615cc691260a4af6167018b4f3bf0928ad955ccc4",
}


class FakeSecrets:
    def __init__(self, value):
        self.value, self.asked = value, []

    def get_secret_value(self, SecretId):
        self.asked.append(SecretId)
        return {"SecretString": self.value}


class ReaperTest(unittest.TestCase):
    def run_reaper(self, instances, dry_run="false", **fake):
        mod = load({"OWNER": "acme", "MAX_AGE_MINUTES": "720", "DRY_RUN": dry_run})
        ec2 = FakeEC2(instances, **fake)
        out = mod.handler({}, None, ec2=ec2, now=NOW)
        return out, ec2

    def test_ends_only_instances_past_the_limit(self):
        out, ec2 = self.run_reaper([inst("i-young", 719), inst("i-edge", 720), inst("i-older", 3000)])
        self.assertEqual(out["ended"], ["i-edge", "i-older"])  # the limit itself ends
        self.assertEqual(ec2.calls, [("terminate", "i-edge"), ("terminate", "i-older")])

    def test_scoped_to_owner_and_running_states(self):
        _, ec2 = self.run_reaper([])
        self.assertIn({"Name": "tag:firerunner:owner", "Values": ["acme", "acme/*"]}, ec2.filters)
        self.assertIn({"Name": "instance-state-name", "Values": ["pending", "running", "stopped"]}, ec2.filters)

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


    def test_one_time_request_needs_no_cancel(self):
        # Ending a one-time spot instance launches nothing in its place; its
        # request may lack the owner tag the cancel permission needs.
        out, ec2 = self.run_reaper([inst("i-cold", 800, spot="sir-once")], one_time={"sir-once"})
        self.assertEqual(ec2.calls, [("terminate", "i-cold")])
        self.assertEqual(out["ended"], ["i-cold"])

    def test_stopped_pool_kept_used_stopped_ended(self):
        out, ec2 = self.run_reaper([
            inst("i-ready", 5000, state="stopped", pool="ready"),
            inst("i-warming", 5000, state="stopped", pool="warming"),
            inst("i-claimed", 5000, state="stopped", pool="claimed", spot="sir-p"),
            inst("i-claimed-young", 10, state="stopped", pool="claimed"),
        ])
        self.assertEqual(out["ended"], ["i-claimed"])
        self.assertEqual(ec2.calls, [("cancel", "sir-p"), ("terminate", "i-claimed")])

    def test_a_failed_terminate_does_not_stop_the_sweep(self):
        mod = load({"OWNER": "acme", "MAX_AGE_MINUTES": "720", "DRY_RUN": "false"})
        ec2 = FakeEC2([inst("i-locked", 800), inst("i-next", 800)], terminate_fails={"i-locked"})
        with self.assertRaises(RuntimeError) as err:
            mod.handler({}, None, ec2=ec2, now=NOW)
        self.assertIn("i-locked", str(err.exception))
        self.assertIn(("terminate", "i-next"), ec2.calls)  # swept on regardless


class ProvenanceTest(unittest.TestCase):
    """Given the pool key, the reaper ends only what a MAC proves the install
    launched: anyone who may tag an instance can write its owner tag."""

    def setUp(self):
        self.mod = load({"OWNER": "acme", "MAX_AGE_MINUTES": "720", "DRY_RUN": "false",
                         "POOL_KEY_SECRET": "arn:aws:secretsmanager:us-east-1:000000000000:secret:pool-key"})
        self.addCleanup(os.environ.pop, "POOL_KEY_SECRET", None)

    def mac(self, *a):
        return self.mod.mac(KEY.encode(), *a)

    def sweep(self, instances, key=KEY, **fake):
        ec2, sm = FakeEC2(instances, **fake), FakeSecrets(key + "\n")  # as the secret holds it, trimmed
        return self.mod.handler({}, None, ec2=ec2, now=NOW, sm=sm), ec2, sm

    def test_mac_matches_the_go_signer(self):
        for args, want in VECTORS.items():
            self.assertEqual(self.mac(*args), want, args)

    def test_ends_only_what_a_mac_proves(self):
        own = {"firerunner:owner": "acme/t1", "firerunner:tier": "small"}
        pool = dict(own, **{"firerunner:pool-mac": self.mac("firerunner/ec2-pool-mac/v1", "acme/t1", "small", "i-pool")})
        runner = dict(own, **{"firerunner:runner": "r-1",
                              "firerunner:runner-mac": self.mac("firerunner/ec2-runner-mac/v1", "acme/t1", "small", "r-1", "i-runner")})
        copied = dict(own, **{"firerunner:pool-mac": pool["firerunner:pool-mac"]})
        retiered = dict(pool, **{"firerunner:tier": "large"})
        out, ec2, sm = self.sweep([
            inst("i-pool", 800, state="stopped", pool="claimed", tags=pool),
            inst("i-runner", 800, tags=runner),
            inst("i-relaunched", 800, spot="sir-p", tags=own),
            inst("i-forged", 800, tags=own),
            inst("i-copied", 800, tags=copied),
            inst("i-retiered", 800, tags=dict(retiered, **{"firerunner:pool-mac": self.mac("firerunner/ec2-pool-mac/v1", "acme/t1", "small", "i-retiered")})),
            inst("i-forged-request", 800, spot="sir-f", tags=own),
        ], request_tags={
            "sir-p": {"firerunner:tier": "small", "firerunner:request-mac": self.mac("firerunner/ec2-request-mac/v1", "acme/t1", "small", "sir-p")},
            "sir-f": {"firerunner:tier": "small", "firerunner:request-mac": "ab" * 32},
        })
        self.assertEqual(out["ended"], ["i-pool", "i-runner", "i-relaunched"])
        self.assertEqual(out["unproven"], ["i-forged", "i-copied", "i-retiered", "i-forged-request"])
        for iid in out["unproven"]:
            self.assertNotIn(("terminate", iid), ec2.calls)
        self.assertNotIn(("cancel", "sir-f"), ec2.calls)
        self.assertEqual(sm.asked, ["arn:aws:secretsmanager:us-east-1:000000000000:secret:pool-key"])

    def test_another_key_proves_nothing(self):
        tags = {"firerunner:owner": "acme", "firerunner:tier": "small",
                "firerunner:pool-mac": self.mac("firerunner/ec2-pool-mac/v1", "acme", "small", "i-1")}
        out, ec2, _ = self.sweep([inst("i-1", 800, tags=tags)], key="o" * 32)
        self.assertEqual((out["ended"], out["unproven"], ec2.calls), ([], ["i-1"], []))

    def test_an_unreadable_key_ends_nothing(self):
        class Denied:
            def get_secret_value(self, SecretId):
                raise RuntimeError("AccessDeniedException")
        ec2 = FakeEC2([inst("i-1", 800)])
        with self.assertRaises(RuntimeError):
            self.mod.handler({}, None, ec2=ec2, now=NOW, sm=Denied())
        self.assertEqual(ec2.calls, [])


if __name__ == "__main__":
    unittest.main(verbosity=1)
