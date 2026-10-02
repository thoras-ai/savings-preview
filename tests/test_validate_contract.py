import copy
import json
import os
import sys
import unittest

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, os.path.join(ROOT, "scripts"))

import validate_contract as vc  # noqa: E402

FIXTURE = os.path.join(ROOT, "fixtures", "savings-preview.otlp.json")


def load():
    with open(FIXTURE) as f:
        return json.load(f)


def attr(res, key):
    for a in res["resource"]["attributes"]:
        if a["key"] == key:
            return a
    return None


def find(doc, **want):
    """First resource whose string attributes match."""
    for rm in doc["resourceMetrics"]:
        if all((attr(rm, k) or {}).get("value", {}).get("stringValue") == v for k, v in want.items()):
            return rm
    raise KeyError(want)


def metrics(rm):
    return rm["scopeMetrics"][0]["metrics"]


class FixtureTest(unittest.TestCase):
    def test_fixture_is_valid(self):
        self.assertEqual(vc.validate(load()), [])

    def test_fixture_covers_the_cases(self):
        doc = load()
        names = {a["value"]["stringValue"] for rm in doc["resourceMetrics"]
                 for a in rm["resource"]["attributes"] if a["key"] == "k8s.deployment.name"
                 or a["key"] == "k8s.statefulset.name"}
        self.assertEqual(names, {"checkout-api", "ledger-db", "report-worker", "search-indexer"})
        worker = find(doc, **{"k8s.container.name": "worker"})
        self.assertNotIn("container.memory.working_set", [m["name"] for m in metrics(worker)])
        self.assertEqual(len(metrics(find(doc, **{"k8s.container.name": "app"}))[0]["gauge"]["dataPoints"]), 14 * 288)

    def test_cli_exit_codes(self):
        self.assertEqual(vc.main(["validate", FIXTURE]), 0)
        self.assertEqual(vc.main(["validate", os.path.join(ROOT, "README.md")]), 1)


class RejectTest(unittest.TestCase):
    def setUp(self):
        self.doc = load()
        self.app = find(self.doc, **{"k8s.container.name": "app"})
        self.worker = find(self.doc, **{"k8s.container.name": "worker"})

    def errors(self):
        return vc.validate(self.doc)

    def assertHas(self, text):
        errs = self.errors()
        self.assertTrue(any(text in e for e in errs), "%r not in %r" % (text, errs))

    def test_not_otlp(self):
        self.assertTrue(vc.validate([]))
        self.assertTrue(vc.validate({"resourceMetrics": []}))

    def test_wrong_schema_url(self):
        self.app["schemaUrl"] = "https://opentelemetry.io/schemas/1.30.0"
        self.assertHas("schemaUrl")

    def test_unknown_metric(self):
        metrics(self.app)[0]["name"] = "container.cpu.time"
        self.assertHas("not part of the contract")

    def test_wrong_unit(self):
        metrics(self.app)[0]["unit"] = "s"
        self.assertHas("unit is 's'")

    def test_wrong_instrument(self):
        m = metrics(self.app)[0]
        m["sum"] = m.pop("gauge")
        self.assertHas("must be a gauge")

    def test_cumulative_required_for_updown(self):
        m = next(m for m in metrics(self.app) if m["name"] == "container.memory.working_set")
        m["sum"]["aggregationTemporality"] = 1
        self.assertHas("aggregationTemporality")

    def test_required_metric_absent_without_declaration(self):
        metrics(self.app).pop(0)
        self.assertHas("required metric container.cpu.usage is absent")

    def test_declared_missing_but_present(self):
        self.app["resource"]["attributes"].append({"key": "thoras.missing_metrics", "value": {
            "arrayValue": {"values": [{"stringValue": "container.cpu.usage"}]}}})
        self.assertHas("listed in thoras.missing_metrics but present")

    def test_missing_list_rejects_optional_or_unknown(self):
        attr(self.worker, "thoras.missing_metrics")["value"]["arrayValue"]["values"].append(
            {"stringValue": "thoras.container.oom_kills"})
        self.assertHas("not a required metric")

    def test_fidelity_notes(self):
        self.app["resource"]["attributes"].append({"key": "thoras.fidelity_notes", "value": {
            "arrayValue": {"values": [{"stringValue": "usage_split_from_pod_by_requests"}]}}})
        self.assertEqual(vc.validate(self.doc), [])
        attr(self.app, "thoras.fidelity_notes")["value"] = {"stringValue": "x"}
        self.assertHas("thoras.fidelity_notes must be an array")

    def test_missing_attribute(self):
        self.app["resource"]["attributes"] = [
            a for a in self.app["resource"]["attributes"] if a["key"] != "k8s.namespace.name"]
        self.assertHas("k8s.namespace.name")

    def test_both_workload_kinds(self):
        self.app["resource"]["attributes"].append(
            {"key": "k8s.statefulset.name", "value": {"stringValue": "x"}})
        self.assertHas("exactly one of")

    def test_mixed_clusters(self):
        attr(self.app, "k8s.cluster.name")["value"]["stringValue"] = "other"
        self.assertHas("mixes clusters")

    def test_missing_workload_resource(self):
        self.doc["resourceMetrics"] = [
            r for r in self.doc["resourceMetrics"]
            if not (attr(r, "k8s.deployment.name") and not attr(r, "k8s.container.name")
                    and attr(r, "k8s.deployment.name")["value"]["stringValue"] == "checkout-api")]
        self.assertHas("no workload resource")

    def test_step_mismatch(self):
        attr(self.app, "thoras.step_seconds")["value"]["intValue"] = "60"
        pts = metrics(self.app)[0]["gauge"]["dataPoints"]
        del pts[5]
        pts[5]["timeUnixNano"] = str(int(pts[5]["timeUnixNano"]) + 1_000_000_000 * 7)
        self.assertHas("not a multiple of step")

    def test_gap_in_whole_steps_is_allowed(self):
        del metrics(self.app)[0]["gauge"]["dataPoints"][100:110]
        self.assertEqual(self.errors(), [])

    def test_out_of_order(self):
        pts = metrics(self.app)[0]["gauge"]["dataPoints"]
        pts[3], pts[4] = pts[4], pts[3]
        self.assertHas("not a multiple of step")

    def test_negative_value(self):
        metrics(self.app)[0]["gauge"]["dataPoints"][0]["asDouble"] = -0.1
        self.assertHas("finite number >= 0")

    def test_int_metric_needs_as_int(self):
        m = next(m for m in metrics(self.app) if m["name"] == "k8s.container.memory.request.desired")
        p = m["sum"]["dataPoints"][0]
        p["asDouble"] = float(p.pop("asInt"))
        self.assertHas("asInt")

    def test_empty_series(self):
        metrics(self.app)[0]["gauge"]["dataPoints"] = []
        self.assertHas("no data points")

    def test_tier_cannot_exceed_resolution(self):
        attr(self.app, "thoras.fidelity_tier")["value"]["intValue"] = "1"
        self.assertHas("claims more than")

    def test_optional_series_does_not_widen_tier_span(self):
        for m in metrics(self.app):
            if m["name"] != "thoras.container.oom_kills":
                for body in (m.get("gauge"), m.get("sum")):
                    if body:
                        body["dataPoints"] = body["dataPoints"][-288 * 3:]
        metrics(self.app).append({"name": "thoras.container.oom_kills", "unit": "{oom_kill}",
                                  "sum": {"isMonotonic": True, "aggregationTemporality": 1, "dataPoints": [
                                      {"timeUnixNano": str(1789516800 * 10**9), "asInt": "1"}]}})
        attr(self.app, "thoras.fidelity_tier")["value"]["intValue"] = "2"
        self.assertHas("claims more than")

    def test_tier_value(self):
        attr(self.app, "thoras.fidelity_tier")["value"]["intValue"] = "4"
        self.assertHas("fidelity_tier")

    def test_node_needs_capacity_type(self):
        node = find(self.doc, **{"host.type": "m5.xlarge"})
        attr(node, "thoras.node.capacity_type")["value"]["stringValue"] = "reserved"
        self.assertHas("capacity_type")

    def test_scope_version(self):
        self.app["scopeMetrics"][0]["scope"]["version"] = "2.0.0"
        self.assertHas("scope version")

    def test_datapoint_attributes_rejected(self):
        metrics(self.app)[0]["gauge"]["dataPoints"][0]["attributes"] = [
            {"key": "cpu.mode", "value": {"stringValue": "user"}}]
        self.assertHas("data point attributes")

    def test_nodes_are_optional(self):
        doc = copy.deepcopy(self.doc)
        doc["resourceMetrics"] = [r for r in doc["resourceMetrics"] if not attr(r, "k8s.node.name")]
        self.assertEqual(vc.validate(doc), [])


class StepSpanTest(unittest.TestCase):
    """A resource whose older points are 300s apart and newer ones 60s apart."""

    def setUp(self):
        self.doc = load()
        self.app = find(self.doc, **{"k8s.container.name": "app"})
        times = [int(p["timeUnixNano"]) // 10**9 for p in metrics(self.app)[0]["gauge"]["dataPoints"]]
        self.first, self.last = times[0], times[-1]
        self.mid = self.last - 3600
        attr(self.app, "thoras.step_seconds")["value"] = {"intValue": "60"}
        self.set_spans(["%d/%d/300" % (self.first, self.mid), "%d/%d/60" % (self.mid, self.last + 60)])

    def set_spans(self, spans):
        self.app["resource"]["attributes"] = [
            a for a in self.app["resource"]["attributes"] if a["key"] != "thoras.step_spans"] + [
            {"key": "thoras.step_spans", "value": {"arrayValue": {"values": [{"stringValue": x} for x in spans]}}}]

    def assertHas(self, text):
        errs = vc.validate(self.doc)
        self.assertTrue(any(text in e for e in errs), "%r not in %r" % (text, errs))

    def test_valid(self):
        self.assertEqual(vc.validate(self.doc), [])

    def test_max_metrics_are_optional_and_accepted(self):
        usage = metrics(self.app)[0]
        metrics(self.app).append(dict(copy.deepcopy(usage), name="thoras.container.cpu.usage.max"))
        self.assertEqual(vc.validate(self.doc), [])

    def test_point_outside_spans(self):
        self.set_spans(["%d/%d/300" % (self.first + 300, self.mid), "%d/%d/60" % (self.mid, self.last + 60)])
        self.assertHas("outside thoras.step_spans")

    def test_point_off_span_grid(self):
        self.set_spans(["%d/%d/300" % (self.first - 60, self.mid - 60), "%d/%d/60" % (self.mid - 60, self.last + 60)])
        self.assertHas("off the 300s grid")

    def test_bad_entry(self):
        self.set_spans(["%d-%d-300" % (self.first, self.last)])
        self.assertHas("is not START/END/STEP")

    def test_finest_step_must_match(self):
        self.set_spans(["%d/%d/300" % (self.first, self.last + 300)])
        self.assertHas("finest step")

    def test_tier_follows_coarsest_span(self):
        attr(self.app, "thoras.fidelity_tier")["value"] = {"intValue": "1"}
        self.assertHas("claims more than step 300s")


if __name__ == "__main__":
    unittest.main()
