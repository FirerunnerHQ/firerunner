#!/usr/bin/env python3
"""Unit tests for the JIT name cleanup Lambda inline in control.yaml.

The code is read from the template itself, so what is tested is what
deploys. A fake Route 53 client stands in for AWS. Needs only python3.
"""
import os
import re
import sys
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def lambda_source():
    with open(os.path.join(HERE, "control.yaml")) as f:
        text = f.read()
    fn = re.search(r"^  JitNameCleanupFunction:\n(.*?)(?=^  \S|^\S)", text, re.S | re.M)
    assert fn, "control.yaml: no JitNameCleanupFunction resource"
    m = re.search(r"\n        ZipFile: \|\n((?:          .*\n|\n)+)", fn.group(0))
    assert m, "control.yaml: no inline ZipFile code in JitNameCleanupFunction"
    return "".join(line[10:] if line.strip() else "\n" for line in m.group(1).splitlines(True))


def load():
    sys.modules["boto3"] = types.SimpleNamespace(client=lambda name, **kw: None)
    sys.modules["botocore"] = types.ModuleType("botocore")
    sys.modules["botocore.config"] = types.SimpleNamespace(Config=lambda **kw: kw)
    mod = types.ModuleType("index")
    exec(compile(lambda_source(), "control.yaml:index.py", "exec"), mod.__dict__)
    return mod


class NoSuchHostedZone(Exception):
    pass


class InvalidChangeBatch(Exception):
    pass


def rrs(name, rtype="A", value="10.0.0.5", **extra):
    s = {"Name": name, "Type": rtype, "TTL": 10, "ResourceRecords": [{"Value": value}]}
    s.update(extra)
    return s


class FakeRoute53:
    """A zone's record sets, listed from StartRecordName on as Route 53 does.
    change_fails: how many changes fail with InvalidChangeBatch first."""

    exceptions = types.SimpleNamespace(NoSuchHostedZone=NoSuchHostedZone, InvalidChangeBatch=InvalidChangeBatch)

    def __init__(self, sets, zone="Z1", change_fails=0):
        self.sets, self.zone, self.change_fails, self.changes = list(sets), zone, change_fails, []

    def list_resource_record_sets(self, HostedZoneId, StartRecordName, StartRecordType, MaxItems):
        if HostedZoneId != self.zone:
            raise NoSuchHostedZone(HostedZoneId)
        return {"ResourceRecordSets": [s for s in sorted(self.sets, key=lambda s: (s["Name"], s["Type"]))
                                       if (s["Name"], s["Type"]) >= (StartRecordName, StartRecordType)]}

    def change_resource_record_sets(self, HostedZoneId, ChangeBatch):
        self.changes.append(ChangeBatch["Changes"])
        if self.change_fails:
            self.change_fails -= 1
            raise InvalidChangeBatch("not found")
        for c in ChangeBatch["Changes"]:
            assert c["Action"] == "DELETE"
            self.sets.remove(c["ResourceRecordSet"])


ZONE = [
    rrs("firerunner.internal.", "NS", "ns"),
    rrs("firerunner.internal.", "SOA", "soa"),
    rrs("alice.firerunner.internal."),
    rrs("bob.firerunner.internal."),
    rrs("bob.firerunner.internal.", "TXT", '"keep"'),
    rrs("bobby.firerunner.internal."),
]


class FakeCloudFormation:
    """The stack's status as DescribeStacks reports it; fail raises instead."""

    def __init__(self, status="DELETE_IN_PROGRESS", fail=False):
        self.status, self.fail, self.asked = status, fail, []

    def describe_stacks(self, StackName):
        self.asked.append(StackName)
        if self.fail:
            raise RuntimeError("throttled")
        return {"Stacks": [{"StackStatus": self.status}]}


def event(kind, name="bob.firerunner.internal", zone="Z1", physical=None, own_zone="false"):
    e = {"RequestType": kind, "ResponseURL": "https://example.invalid/", "StackId": "stack", "RequestId": "req",
         "LogicalResourceId": "JitNameCleanup",
         "ResourceProperties": {"ZoneId": zone, "Name": name, "OwnZone": own_zone}}
    if physical:
        e["PhysicalResourceId"] = physical
    return e


class JitNameCleanupTest(unittest.TestCase):
    def run_handler(self, ev, route53, cloudformation=None):
        sent = []
        load().handler(ev, types.SimpleNamespace(log_stream_name="ls"), route53=route53,
                       cloudformation=cloudformation or FakeCloudFormation(),
                       send=lambda e, body: sent.append(body))
        self.assertEqual(len(sent), 1, "exactly one response to CloudFormation")
        return sent[0]

    def test_delete_removes_only_the_jit_name(self):
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", physical="JitNameCleanup"), r53)
        self.assertEqual(out["Status"], "SUCCESS")
        self.assertEqual(out["Data"]["Deleted"], "1")
        self.assertEqual(out["PhysicalResourceId"], "JitNameCleanup")
        self.assertEqual([s for s in ZONE if s not in r53.sets], [rrs("bob.firerunner.internal.")])

    def test_delete_matches_case_and_trailing_dot(self):
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", name="Bob.Firerunner.Internal."), r53)
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "1"))

    def test_delete_removes_every_set_of_the_name(self):
        weighted = [rrs("bob.firerunner.internal.", SetIdentifier=i, Weight=1) for i in ("a", "b")]
        r53 = FakeRoute53(ZONE[:2] + weighted)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual(out["Data"]["Deleted"], "2")
        self.assertEqual(r53.sets, ZONE[:2])

    def test_delete_with_no_record_succeeds(self):
        r53 = FakeRoute53([s for s in ZONE if s["Name"] != "bob.firerunner.internal."])
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "0"))
        self.assertEqual(r53.changes, [])

    def test_delete_with_the_zone_gone_succeeds(self):
        out = self.run_handler(event("Delete", zone="Zgone"), FakeRoute53(ZONE))
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "0"))

    def test_delete_lists_again_after_a_race(self):
        r53 = FakeRoute53(ZONE, change_fails=1)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "1"))
        self.assertEqual(len(r53.changes), 2)

    def test_delete_fails_when_the_record_cannot_go(self):
        r53 = FakeRoute53(ZONE, change_fails=3)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual(out["Status"], "FAILED")
        self.assertIn("InvalidChangeBatch", out["Reason"])
        self.assertIn("ls", out["Reason"])  # where the log is
        self.assertEqual(out["PhysicalResourceId"], "JitNameCleanup")

    def test_rolled_back_updates_keep_the_record(self):
        # An update that added this resource and failed deletes it on the way
        # back, with the stack's VM still serving the name.
        for status in ("UPDATE_ROLLBACK_IN_PROGRESS", "UPDATE_ROLLBACK_COMPLETE_CLEANUP_IN_PROGRESS",
                       "UPDATE_COMPLETE_CLEANUP_IN_PROGRESS"):
            for own in ("true", "false"):
                r53 = FakeRoute53(ZONE)
                out = self.run_handler(event("Delete", own_zone=own), r53, FakeCloudFormation(status))
                self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "0"), status)
                self.assertEqual((r53.changes, r53.sets), ([], ZONE), status)

    def test_a_create_rolled_back_keeps_a_shared_zones_record(self):
        # In a shared zone the name may be another stack's, with its VM live.
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", own_zone="false"), r53, FakeCloudFormation("ROLLBACK_IN_PROGRESS"))
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "0"))
        self.assertEqual(r53.sets, ZONE)

    def test_a_create_rolled_back_clears_its_own_zone(self):
        # A zone this stack made holds only its own name, and must be empty to go.
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", own_zone="true"), r53, FakeCloudFormation("ROLLBACK_IN_PROGRESS"))
        self.assertEqual((out["Status"], out["Data"]["Deleted"]), ("SUCCESS", "1"))

    def test_delete_asks_about_its_own_stack(self):
        cf = FakeCloudFormation()
        self.run_handler(event("Delete"), FakeRoute53(ZONE), cf)
        self.assertEqual(cf.asked, ["stack"])

    def test_an_unknown_stack_status_deletes_nothing(self):
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete"), r53, FakeCloudFormation(fail=True))
        self.assertEqual(out["Status"], "FAILED")
        self.assertEqual(r53.changes, [])

    def test_create_and_update_touch_nothing(self):
        for kind in ("Create", "Update"):
            r53 = FakeRoute53(ZONE)
            out = self.run_handler(event(kind, physical="JitNameCleanup" if kind == "Update" else None), r53)
            self.assertEqual((out["Status"], out["PhysicalResourceId"]), ("SUCCESS", "JitNameCleanup"))
            self.assertEqual((r53.changes, r53.sets), ([], ZONE))


if __name__ == "__main__":
    unittest.main(verbosity=1)
