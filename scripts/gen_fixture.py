#!/usr/bin/env python3
"""Write the synthetic fixture: 4 single-container workloads and 2 nodes, 14 days at 5-minute steps.

Usage: gen_fixture.py [OUTPUT]   (default: fixtures/savings-preview.otlp.json)
Output is deterministic.
"""
import json
import math
import os
import random
import sys

SCHEMA_URL = "https://opentelemetry.io/schemas/1.44.0"
STEP = 300
START = 1789516800  # 2026-09-16T00:00:00Z
POINTS = 14 * 86400 // STEP
GIB = 1 << 30
NS = 1_000_000_000

UNITS = {
    "container.cpu.usage": "{cpu}",
    "container.memory.working_set": "By",
    "k8s.container.cpu.request.desired": "{cpu}",
    "k8s.container.memory.request.desired": "By",
    "k8s.container.cpu.limit.desired": "{cpu}",
    "k8s.container.memory.limit.desired": "By",
    "thoras.container.oom_kills": "{oom_kill}",
    "k8s.deployment.pod.desired": "{pod}",
    "k8s.statefulset.pod.desired": "{pod}",
    "k8s.node.cpu.allocatable": "{cpu}",
    "k8s.node.memory.allocatable": "By",
}
INT_METRICS = {n for n, u in UNITS.items() if u in ("By", "{pod}", "{oom_kill}")}


def sattr(k, v):
    return {"key": k, "value": {"stringValue": v}}


def iattr(k, v):
    return {"key": k, "value": {"intValue": str(v)}}


def metric(name, values):
    """values: list of numbers, or None for a missing step."""
    is_int = name in INT_METRICS
    pts = []
    for i, v in enumerate(values):
        if v is None:
            continue
        p = {"timeUnixNano": str((START + i * STEP) * NS)}
        if is_int:
            p["asInt"] = str(int(v))
        else:
            p["asDouble"] = round(v, 3)
        pts.append(p)
    body = {"dataPoints": pts}
    if name == "container.cpu.usage":
        key = "gauge"
    else:
        key = "sum"
        body["aggregationTemporality"] = 1 if name == "thoras.container.oom_kills" else 2
        body["isMonotonic"] = name == "thoras.container.oom_kills"
    return {"name": name, "unit": UNITS[name], key: body}


def resource(attrs, metrics, missing=()):
    base = [
        sattr("k8s.cluster.name", "demo-cluster"),
        sattr("thoras.source", "prometheus"),
        iattr("thoras.step_seconds", STEP),
        iattr("thoras.fidelity_tier", 2),
    ]
    items = base + attrs
    if missing:
        items.append({"key": "thoras.missing_metrics", "value": {"arrayValue": {
            "values": [{"stringValue": m} for m in missing]}}})
    return {
        "resource": {"attributes": items},
        "scopeMetrics": [{
            "scope": {"name": "thoras.savings_preview", "version": "1.0.0"},
            "schemaUrl": SCHEMA_URL,
            "metrics": metrics,
        }],
        "schemaUrl": SCHEMA_URL,
    }


def diurnal(i, lo, hi, rng, noise=0.05):
    hour = (i * STEP / 3600) % 24
    day = (i * STEP // 86400) % 7
    weekend = 0.7 if day >= 5 else 1.0
    shape = 0.5 - 0.5 * math.cos((hour - 4) / 24 * 2 * math.pi)
    base = lo + (hi - lo) * shape * weekend
    return max(0.0, base * (1 + rng.uniform(-noise, noise)))


def const(v):
    return [v] * POINTS


def workload(rng, ns, kind, name, replicas, containers, oom_at=()):
    """containers: name -> dict(cpu=(lo,hi), mem=(lo,hi), cpu_req, mem_req, cpu_lim, mem_lim, no_mem)."""
    out = []
    pod_metric = "k8s.%s.pod.desired" % kind
    wl = [sattr("k8s.namespace.name", ns), sattr("k8s.%s.name" % kind, name)]
    out.append(resource(wl, [metric(pod_metric, const(replicas))]))
    for cname, c in containers.items():
        cpu = [diurnal(i, *c["cpu"], rng, 0.15) for i in range(POINTS)]
        mem = [diurnal(i, *c["mem"], rng, 0.02) * GIB for i in range(POINTS)]
        metrics = [metric("container.cpu.usage", cpu)]
        missing = []
        if c.get("no_mem"):
            missing.append("container.memory.working_set")
        else:
            metrics.append(metric("container.memory.working_set", mem))
        metrics += [
            metric("k8s.container.cpu.request.desired", const(c["cpu_req"])),
            metric("k8s.container.memory.request.desired", const(c["mem_req"] * GIB)),
        ]
        if "cpu_lim" in c:
            metrics.append(metric("k8s.container.cpu.limit.desired", const(c["cpu_lim"])))
            metrics.append(metric("k8s.container.memory.limit.desired", const(c["mem_lim"] * GIB)))
        if oom_at and cname == next(iter(containers)):
            kills = [1 if i in oom_at else None for i in range(POINTS)]
            metrics.append(metric("thoras.container.oom_kills", kills))
        out.append(resource(wl + [sattr("k8s.container.name", cname)], metrics, missing))
    return out


def node(name, instance, capacity, cpu, mem_gib):
    attrs = [
        sattr("k8s.node.name", name),
        sattr("host.type", instance),
        sattr("cloud.provider", "aws"),
        sattr("cloud.region", "us-east-1"),
        sattr("thoras.node.capacity_type", capacity),
    ]
    return resource(attrs, [
        metric("k8s.node.cpu.allocatable", const(cpu)),
        metric("k8s.node.memory.allocatable", const(int(mem_gib * GIB))),
    ])


def build():
    rng = random.Random(1)
    rms = []
    # Over-requested: asks for several times what it uses.
    rms += workload(rng, "shop", "deployment", "checkout-api", 6, {
        "app": dict(cpu=(0.12, 0.35), mem=(0.5, 0.75), cpu_req=1.0, mem_req=2),
    })
    # Right-sized: requests sit just above the peak.
    rms += workload(rng, "data", "statefulset", "ledger-db", 3, {
        "postgres": dict(cpu=(1.2, 1.8), mem=(6.4, 7.1), cpu_req=2.0, mem_req=8),
    })
    # Memory series could not be retrieved.
    rms += workload(rng, "batch", "deployment", "report-worker", 2, {
        "worker": dict(cpu=(0.2, 0.9), mem=(0, 0), cpu_req=1.5, mem_req=3, no_mem=True),
    })
    # Memory close to its limit, with two OOM kills.
    rms += workload(rng, "search", "deployment", "search-indexer", 2, {
        "indexer": dict(cpu=(0.3, 1.1), mem=(2.6, 3.7), cpu_req=1.0, mem_req=4, cpu_lim=2.0, mem_lim=4),
    }, oom_at=(1500, 3100))
    rms.append(node("ip-10-0-1-11", "m5.xlarge", "on_demand", 3.92, 14.8))
    rms.append(node("ip-10-0-2-27", "m5.2xlarge", "spot", 7.91, 30.1))
    return {"resourceMetrics": rms}


def main(argv):
    out = argv[1] if len(argv) > 1 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..", "fixtures", "savings-preview.otlp.json")
    with open(out, "w") as f:
        json.dump(build(), f, separators=(",", ":"))
        f.write("\n")
    print("wrote", out)


if __name__ == "__main__":
    main(sys.argv)
