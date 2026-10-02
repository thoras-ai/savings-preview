#!/usr/bin/env python3
"""Write a synthetic fixture with containers that run only part of the time.

Two days at 15-minute steps:
- shop/loadgenerator has three steady containers, a check container with CPU
  usage at one step and memory at nine, and an init container with memory only
  for its first steps and no CPU series.
- monitoring/prometheus-server and monitoring/prometheus-alertmanager have CPU
  usage from the start but memory and replica counts only after six hours.
- shop/frontend and shop/cart run the whole time.

Usage: gen_intermittent_fixture.py [OUTPUT]   (default: fixtures/intermittent.otlp.json)
Output is deterministic.
"""
import json
import math
import os
import sys

from gen_fixture import GIB, NS, SCHEMA_URL, UNITS, INT_METRICS, sattr, iattr

STEP = 900
START = 1789516800  # 2026-09-16T00:00:00Z
POINTS = 2 * 86400 // STEP
LATE = 6 * 3600 // STEP  # steps before memory and replicas appear
CHECK_CPU = (150,)
CHECK_MEM = (20, 40, 60, 80, 100, 120, 150, 170, 190)
INIT_MEM = (0, 1, 2)


def metric(name, values):
    """values: list of numbers, or None for a missing step."""
    pts = []
    for i, v in enumerate(values):
        if v is None:
            continue
        p = {"timeUnixNano": str((START + i * STEP) * NS)}
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


def resource(attrs, metrics, missing=()):
    items = [
        sattr("k8s.cluster.name", "intermittent"),
        sattr("thoras.source", "prometheus"),
        iattr("thoras.step_seconds", STEP),
        iattr("thoras.fidelity_tier", 3),
    ] + attrs
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


def at(steps, v):
    return [v if i in steps else None for i in range(POINTS)]


def wave(lo, hi, phase=0):
    return [lo + (hi - lo) * (0.5 + 0.5 * math.sin(i / 12 + phase)) for i in range(POINTS)]


def container(wl, name, cpu, mem_gib, cpu_req, mem_req_gib, missing=()):
    """cpu and mem_gib: per-step values or None. Requests are written where the container has usage."""
    metrics = []
    if "container.cpu.usage" not in missing:
        metrics.append(metric("container.cpu.usage", cpu))
    metrics.append(metric("container.memory.working_set", [None if v is None else v * GIB for v in mem_gib]))
    if "k8s.container.cpu.request.desired" not in missing:
        metrics.append(metric("k8s.container.cpu.request.desired", [None if v is None else cpu_req for v in cpu]))
    metrics.append(metric("k8s.container.memory.request.desired", [None if v is None else mem_req_gib * GIB for v in mem_gib]))
    return resource(wl + [sattr("k8s.container.name", name)], metrics, missing)


def workload(ns, kind, name, replicas, first=0):
    wl = [sattr("k8s.namespace.name", ns), sattr("k8s.%s.name" % kind, name)]
    pods = [replicas if i >= first else None for i in range(POINTS)]
    return wl, resource(wl, [metric("k8s.%s.pod.desired" % kind, pods)])


def build():
    rms = []
    late = range(LATE, POINTS)

    wl, r = workload("shop", "deployment", "frontend", 3)
    rms += [r, container(wl, "app", wave(0.2, 0.5), wave(0.8, 1.1), 1.0, 2)]
    wl, r = workload("shop", "deployment", "cart", 2)
    rms += [r, container(wl, "app", wave(0.1, 0.3, 1), wave(0.3, 0.5, 1), 0.5, 1)]

    wl, r = workload("shop", "deployment", "loadgenerator", 1)
    rms += [
        r,
        container(wl, "main", wave(0.1, 0.25, 2), wave(0.2, 0.3, 2), 0.3, 0.5),
        container(wl, "istio-proxy", wave(0.02, 0.05), wave(0.04, 0.06), 0.1, 0.125),
        container(wl, "test-sidecar", wave(0.001, 0.003), wave(0.002, 0.003), 0.05, 0.0625),
        container(wl, "frontend-check", at(CHECK_CPU, 0.02), at(CHECK_MEM, 0.003), 0.1, 0.125),
        container(wl, "istio-init", at((), None), at(INIT_MEM, 0.004), 0.1, 0.125,
                  missing=("container.cpu.usage", "k8s.container.cpu.request.desired")),
    ]

    for name, containers in (
        ("prometheus-server", (("prometheus-server", 0.05, 0.4, 0.1, 0.5), ("configmap-reload", 0.001, 0.01, 0.0, 0.0))),
        ("prometheus-alertmanager", (("alertmanager", 0.004, 0.02, 0.05, 0.125),)),
    ):
        wl, r = workload("monitoring", "statefulset", name, 1, first=LATE)
        rms.append(r)
        for cname, cpu, mem, cpu_req, mem_req in containers:
            rms.append(container(wl, cname, [cpu] * POINTS, at(late, mem), cpu_req, mem_req))
    return {"resourceMetrics": rms}


def main(argv):
    out = argv[1] if len(argv) > 1 else os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "..", "fixtures", "intermittent.otlp.json")
    with open(out, "w") as f:
        json.dump(build(), f, separators=(",", ":"))
        f.write("\n")
    print("wrote", out)


if __name__ == "__main__":
    main(sys.argv)
