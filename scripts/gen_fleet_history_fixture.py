#!/usr/bin/env python3
"""Write a synthetic fleet whose clusters keep different amounts of history.

Four clusters at 15-minute steps, every one ending at the same time:
- arn:aws:eks:us-east-1:111122223333:cluster/prod, 2 days. Its batch/report
  workload runs for its first and last six hours only, so on its own the
  cluster's chart has a gap between them.
- arn:aws:eks:eu-west-1:111122223333:cluster/prod, 1 day: the same short name
  in another region.
- gke_acme-data_us-central1_analytics, 12 hours.
- staging, 6 hours.

Usage: gen_fleet_history_fixture.py [OUTPUT]   (default: fixtures/fleet-history.otlp.json)
Output is deterministic.
"""
import json
import math
import os
import sys

from gen_fixture import GIB, NS, SCHEMA_URL, UNITS, INT_METRICS, sattr, iattr

STEP = 900
END = 1789516800 + 2 * 86400  # 2026-09-18T00:00:00Z
FLEET_VERSION = "1.4.0"

CLUSTERS = (
    ("arn:aws:eks:us-east-1:111122223333:cluster/prod", 2 * 86400, True),
    ("arn:aws:eks:eu-west-1:111122223333:cluster/prod", 86400, False),
    ("gke_acme-data_us-central1_analytics", 12 * 3600, False),
    ("staging", 6 * 3600, False),
)


def metric(name, start, values):
    """values: list of numbers, or None for a missing step."""
    pts = []
    for i, v in enumerate(values):
        if v is None:
            continue
        p = {"timeUnixNano": str((start + i * STEP) * NS)}
        if name in INT_METRICS:
            p["asInt"] = str(int(v))
        else:
            p["asDouble"] = round(v, 4)
        pts.append(p)
    body = {"dataPoints": pts}
    if name == "container.cpu.usage":
        key = "gauge"
    else:
        key = "sum"
        body["aggregationTemporality"] = 2
        body["isMonotonic"] = False
    return {"name": name, "unit": UNITS[name], key: body}


def resource(cluster, attrs, metrics):
    items = [
        sattr("k8s.cluster.name", cluster),
        sattr("thoras.source", "prometheus"),
        iattr("thoras.step_seconds", STEP),
        iattr("thoras.fidelity_tier", 3),
    ] + attrs
    return {
        "resource": {"attributes": items},
        "scopeMetrics": [{
            "scope": {"name": "thoras.savings_preview", "version": FLEET_VERSION},
            "schemaUrl": SCHEMA_URL,
            "metrics": metrics,
        }],
        "schemaUrl": SCHEMA_URL,
    }


def build():
    rms = []
    for k, (cluster, span, gap) in enumerate(CLUSTERS):
        points = span // STEP
        start = END - span

        def wave(lo, hi, phase):
            return [lo + (hi - lo) * (0.5 + 0.5 * math.sin(i / 10 + phase)) for i in range(points)]

        edge = 6 * 3600 // STEP
        workloads = [("web", "frontend", 3, 1.0, 2, [True] * points),
                     ("web", "api", 2, 0.5, 1, [True] * points)]
        if gap:
            workloads.append(("batch", "report", 4, 2.0, 8, [i < edge or i >= points - edge for i in range(points)]))
        for j, (ns, name, replicas, cpu_req, mem_req, up) in enumerate(workloads):
            wl = [sattr("k8s.namespace.name", ns), sattr("k8s.deployment.name", name)]
            rms.append(resource(cluster, wl, [metric("k8s.deployment.pod.desired", start, [replicas] * points)]))
            cpu = [v if u else None for v, u in zip(wave(0.1, 0.4 * cpu_req, k + j), up)]
            mem = [v if u else None for v, u in zip(wave(0.2, 0.5 * mem_req, k + j + 1), up)]
            rms.append(resource(cluster, wl + [sattr("k8s.container.name", "app")], [
                metric("container.cpu.usage", start, cpu),
                metric("container.memory.working_set", start, [None if v is None else v * GIB for v in mem]),
                metric("k8s.container.cpu.request.desired", start, [None if v is None else cpu_req for v in cpu]),
                metric("k8s.container.memory.request.desired", start, [None if v is None else mem_req * GIB for v in mem]),
            ]))
    return {"resourceMetrics": rms}


def main(argv):
    out = argv[1] if len(argv) > 1 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..", "fixtures", "fleet-history.otlp.json")
    with open(out, "w") as f:
        json.dump(build(), f, separators=(",", ":"))
        f.write("\n")
    print("wrote", out)


if __name__ == "__main__":
    main(sys.argv)
