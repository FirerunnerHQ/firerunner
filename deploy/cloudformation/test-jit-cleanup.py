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
    sys.modules["boto3"] = types.SimpleNamespace(client=lambda name: None)
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


def event(kind, name="bob.firerunner.internal", zone="Z1", physical=None):
    e = {"RequestType": kind, "ResponseURL": "https://example.invalid/", "StackId": "stack", "RequestId": "req",
         "LogicalResourceId": "JitNameCleanup", "ResourceProperties": {"ZoneId": zone, "Name": name}}
    if physical:
        e["PhysicalResourceId"] = physical
    return e


class JitNameCleanupTest(unittest.TestCase):
    def run_handler(self, ev, route53):
        sent = []
        load().handler(ev, types.SimpleNamespace(log_stream_name="ls"), route53=route53,
                       send=lambda e, body: sent.append(body))
        self.assertEqual(len(sent), 1, "exactly one response to CloudFormation")
        return sent[0]

    def test_delete_removes_only_the_jit_name(self):
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", physical="JitNameCleanup"), r53)
        self.assertEqual(out["Status"], "SUCCESS")
        self.assertEqual(out["Data"], {"Deleted": "1"})
        self.assertEqual(out["PhysicalResourceId"], "JitNameCleanup")
        self.assertEqual([s for s in ZONE if s not in r53.sets], [rrs("bob.firerunner.internal.")])

    def test_delete_matches_case_and_trailing_dot(self):
        r53 = FakeRoute53(ZONE)
        out = self.run_handler(event("Delete", name="Bob.Firerunner.Internal."), r53)
        self.assertEqual((out["Status"], out["Data"]), ("SUCCESS", {"Deleted": "1"}))

    def test_delete_removes_every_set_of_the_name(self):
        weighted = [rrs("bob.firerunner.internal.", SetIdentifier=i, Weight=1) for i in ("a", "b")]
        r53 = FakeRoute53(ZONE[:2] + weighted)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual(out["Data"], {"Deleted": "2"})
        self.assertEqual(r53.sets, ZONE[:2])

    def test_delete_with_no_record_succeeds(self):
        r53 = FakeRoute53([s for s in ZONE if s["Name"] != "bob.firerunner.internal."])
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual((out["Status"], out["Data"]), ("SUCCESS", {"Deleted": "0"}))
        self.assertEqual(r53.changes, [])

    def test_delete_with_the_zone_gone_succeeds(self):
        out = self.run_handler(event("Delete", zone="Zgone"), FakeRoute53(ZONE))
        self.assertEqual((out["Status"], out["Data"]), ("SUCCESS", {"Deleted": "0"}))

    def test_delete_lists_again_after_a_race(self):
        r53 = FakeRoute53(ZONE, change_fails=1)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual((out["Status"], out["Data"]), ("SUCCESS", {"Deleted": "1"}))
        self.assertEqual(len(r53.changes), 2)

    def test_delete_fails_when_the_record_cannot_go(self):
        r53 = FakeRoute53(ZONE, change_fails=3)
        out = self.run_handler(event("Delete"), r53)
        self.assertEqual(out["Status"], "FAILED")
        self.assertIn("InvalidChangeBatch", out["Reason"])
        self.assertIn("ls", out["Reason"])  # where the log is
        self.assertEqual(out["PhysicalResourceId"], "JitNameCleanup")

    def test_create_and_update_touch_nothing(self):
        for kind in ("Create", "Update"):
            r53 = FakeRoute53(ZONE)
            out = self.run_handler(event(kind, physical="JitNameCleanup" if kind == "Update" else None), r53)
            self.assertEqual((out["Status"], out["PhysicalResourceId"]), ("SUCCESS", "JitNameCleanup"))
            self.assertEqual((r53.changes, r53.sets), ([], ZONE))


if __name__ == "__main__":
    unittest.main(verbosity=1)
