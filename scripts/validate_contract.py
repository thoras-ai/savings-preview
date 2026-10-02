#!/usr/bin/env python3
"""Check an OTLP-JSON metrics file against docs/contract.md.

Usage: validate_contract.py FILE [FILE ...]
Exits 0 if every file is valid, 1 otherwise. Standard library only.
"""
import json
import math
import sys

SCHEMA_URL = "https://opentelemetry.io/schemas/1.44.0"
SCOPE_NAME = "thoras.savings_preview"
CONTRACT_MAJOR = "1"
TIERS = (1, 2, 3)
DAY = 86400

GAUGE, UPDOWN, DELTA = "gauge", "updowncounter", "delta_counter"

# name -> (instrument, unit, value type, required)
CONTAINER_METRICS = {
    "container.cpu.usage": (GAUGE, "{cpu}", "double", True),
    "container.memory.working_set": (UPDOWN, "By", "int", True),
    "k8s.container.cpu.request.desired": (UPDOWN, "{cpu}", "double", True),
    "k8s.container.memory.request.desired": (UPDOWN, "By", "int", True),
    "k8s.container.cpu.limit.desired": (UPDOWN, "{cpu}", "double", False),
    "k8s.container.memory.limit.desired": (UPDOWN, "By", "int", False),
    "thoras.container.oom_kills": (DELTA, "{oom_kill}", "int", False),
}
DEPLOYMENT_METRICS = {
    "k8s.deployment.pod.desired": (UPDOWN, "{pod}", "int", True),
}
STATEFULSET_METRICS = {
    "k8s.statefulset.pod.desired": (UPDOWN, "{pod}", "int", True),
}
NODE_METRICS = {
    "k8s.node.cpu.allocatable": (UPDOWN, "{cpu}", "double", True),
    "k8s.node.memory.allocatable": (UPDOWN, "By", "int", True),
}

COMMON_ATTRS = ("k8s.cluster.name", "thoras.source", "thoras.step_seconds", "thoras.fidelity_tier")
CAPACITY_TYPES = ("on_demand", "spot", "unknown")

CUMULATIVE = (2, "AGGREGATION_TEMPORALITY_CUMULATIVE")
DELTA_TEMPORALITY = (1, "AGGREGATION_TEMPORALITY_DELTA")


def _attr_value(v):
    if "stringValue" in v:
        return v["stringValue"]
    if "intValue" in v:
        return int(v["intValue"])
    if "doubleValue" in v:
        return v["doubleValue"]
    if "boolValue" in v:
        return v["boolValue"]
    if "arrayValue" in v:
        return [_attr_value(x) for x in v["arrayValue"].get("values", [])]
    raise ValueError("unsupported attribute value %r" % (v,))


def _attrs(items):
    out = {}
    for item in items or []:
        out[item["key"]] = _attr_value(item["value"])
    return out


def _is_int_string(x):
    return isinstance(x, str) and x.isdigit()


def _tier_floor(step, span):
    """Best tier a series of this step and span may claim."""
    if step <= 60 and span >= 14 * DAY:
        return 1
    if step <= 300 and span >= 7 * DAY:
        return 2
    return 3


def _check_points(where, name, spec, metric, step, errors):
    """Validate one metric's data points. Returns (first, last) seconds or None."""
    instrument, unit, vtype, _ = spec
    if metric.get("unit") != unit:
        errors.append("%s: unit is %r, expected %r" % (where, metric.get("unit"), unit))
    if instrument == GAUGE:
        body = metric.get("gauge")
        if body is None:
            errors.append("%s: must be a gauge" % where)
            return None
    else:
        body = metric.get("sum")
        if body is None:
            errors.append("%s: must be a sum" % where)
            return None
        mono = instrument == DELTA
        if bool(body.get("isMonotonic", False)) != mono:
            errors.append("%s: isMonotonic must be %s" % (where, str(mono).lower()))
        want = DELTA_TEMPORALITY if instrument == DELTA else CUMULATIVE
        if body.get("aggregationTemporality") not in want:
            errors.append("%s: aggregationTemporality must be %s" % (where, want[0]))
    points = body.get("dataPoints") or []
    if not points:
        errors.append("%s: no data points (omit the metric and list it in thoras.missing_metrics)" % where)
        return None
    prev = None
    first = None
    for i, p in enumerate(points):
        t = p.get("timeUnixNano")
        if not _is_int_string(t):
            errors.append("%s: point %d timeUnixNano must be a digit string" % (where, i))
            return None
        t = int(t)
        if t % 1_000_000_000:
            errors.append("%s: point %d timestamp is not a whole second" % (where, i))
            return None
        t //= 1_000_000_000
        if first is None:
            first = t
        if prev is not None:
            d = t - prev
            if d <= 0 or d % step:
                errors.append("%s: point %d is %ds after the previous, not a multiple of step %ds" % (where, i, d, step))
                return None
        prev = t
        if vtype == "int":
            if "asInt" not in p or not _is_int_string(p["asInt"]):
                errors.append("%s: point %d must carry asInt as a non-negative digit string" % (where, i))
                return None
        else:
            v = p.get("asDouble")
            if isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) or v < 0:
                errors.append("%s: point %d asDouble must be a finite number >= 0" % (where, i))
                return None
        if p.get("attributes"):
            errors.append("%s: data point attributes are not allowed" % where)
            return None
    return first, prev


def validate(doc):
    """Return a list of error strings; empty means the document is valid."""
    errors = []
    if not isinstance(doc, dict) or not isinstance(doc.get("resourceMetrics"), list):
        return ["top level must be an object with a resourceMetrics array"]
    if not doc["resourceMetrics"]:
        return ["resourceMetrics is empty"]

    clusters = set()
    seen = set()
    workloads = {}       # (ns, kind, name) -> has workload resource
    container_wl = set()  # workloads that have container resources

    for ri, rm in enumerate(doc["resourceMetrics"]):
        try:
            attrs = _attrs(rm.get("resource", {}).get("attributes"))
        except (KeyError, ValueError) as e:
            errors.append("resource %d: bad attributes: %s" % (ri, e))
            continue
        where = "resource %d" % ri
        if rm.get("schemaUrl") != SCHEMA_URL:
            errors.append("%s: schemaUrl must be %s" % (where, SCHEMA_URL))

        for k in COMMON_ATTRS:
            if k not in attrs:
                errors.append("%s: missing attribute %s" % (where, k))
        step = attrs.get("thoras.step_seconds")
        tier = attrs.get("thoras.fidelity_tier")
        if not isinstance(step, int) or isinstance(step, bool) or step <= 0:
            errors.append("%s: thoras.step_seconds must be a positive integer" % where)
            step = None
        if tier not in TIERS:
            errors.append("%s: thoras.fidelity_tier must be one of %s" % (where, TIERS))
            tier = None
        if not isinstance(attrs.get("thoras.source", ""), str) or not attrs.get("thoras.source"):
            errors.append("%s: thoras.source must be a non-empty string" % where)
        if "k8s.cluster.name" in attrs:
            clusters.add(attrs["k8s.cluster.name"])

        dep, sts = attrs.get("k8s.deployment.name"), attrs.get("k8s.statefulset.name")
        container = attrs.get("k8s.container.name")
        if "k8s.node.name" in attrs and not (dep or sts or container):
            kind, specs = "node", NODE_METRICS
            for k in ("host.type", "cloud.provider", "cloud.region", "thoras.node.capacity_type"):
                if not attrs.get(k):
                    errors.append("%s: node resource missing attribute %s" % (where, k))
            if attrs.get("thoras.node.capacity_type") not in CAPACITY_TYPES:
                errors.append("%s: thoras.node.capacity_type must be one of %s" % (where, CAPACITY_TYPES))
            ident = ("node", attrs["k8s.node.name"])
        else:
            if bool(dep) == bool(sts):
                errors.append("%s: set exactly one of k8s.deployment.name or k8s.statefulset.name" % where)
                continue
            if not attrs.get("k8s.namespace.name"):
                errors.append("%s: missing attribute k8s.namespace.name" % where)
            wkind = "deployment" if dep else "statefulset"
            wl = (attrs.get("k8s.namespace.name"), wkind, dep or sts)
            if container:
                kind, specs = "container", CONTAINER_METRICS
                ident = ("container",) + wl + (container,)
                container_wl.add(wl)
            else:
                kind = "workload"
                specs = DEPLOYMENT_METRICS if dep else STATEFULSET_METRICS
                ident = ("workload",) + wl
                workloads[wl] = True
        if ident in seen:
            errors.append("%s: duplicate resource %s" % (where, "/".join(map(str, ident[1:]))))
        seen.add(ident)
        where = "%s (%s)" % (where, "/".join(str(x) for x in ident[1:]))

        missing = attrs.get("thoras.missing_metrics", [])
        if not isinstance(missing, list) or not all(isinstance(m, str) for m in missing):
            errors.append("%s: thoras.missing_metrics must be an array of strings" % where)
            missing = []
        for m in missing:
            if m not in specs or not specs[m][3]:
                errors.append("%s: thoras.missing_metrics lists %s, which is not a required metric here" % (where, m))
        notes = attrs.get("thoras.fidelity_notes", [])
        if not isinstance(notes, list) or not all(isinstance(n, str) and n for n in notes):
            errors.append("%s: thoras.fidelity_notes must be an array of non-empty strings" % where)

        scopes = rm.get("scopeMetrics") or []
        if not scopes:
            errors.append("%s: no scopeMetrics" % where)
        present = {}
        first_last = []
        for sm in scopes:
            sc = sm.get("scope") or {}
            if sc.get("name") != SCOPE_NAME:
                errors.append("%s: scope name must be %s" % (where, SCOPE_NAME))
            if not str(sc.get("version", "")).startswith(CONTRACT_MAJOR + "."):
                errors.append("%s: scope version must start with %s." % (where, CONTRACT_MAJOR))
            if sm.get("schemaUrl") != SCHEMA_URL:
                errors.append("%s: scope schemaUrl must be %s" % (where, SCHEMA_URL))
            for metric in sm.get("metrics") or []:
                name = metric.get("name")
                if name not in specs:
                    errors.append("%s: metric %r is not part of the contract for a %s resource" % (where, name, kind))
                    continue
                if name in present:
                    errors.append("%s: metric %s appears twice" % (where, name))
                    continue
                present[name] = True
                if step is None:
                    continue
                r = _check_points("%s %s" % (where, name), name, specs[name], metric, step, errors)
                if r and specs[name][3]:
                    first_last.append(r)
        for name, spec in specs.items():
            if spec[3] and name not in present and name not in missing:
                errors.append("%s: required metric %s is absent and not listed in thoras.missing_metrics" % (where, name))
            if name in missing and name in present:
                errors.append("%s: %s is listed in thoras.missing_metrics but present" % (where, name))

        if tier is not None and step is not None and first_last:
            span = max(l for _, l in first_last) - min(f for f, _ in first_last) + step
            floor = _tier_floor(step, span)
            if tier < floor:
                errors.append("%s: fidelity tier %d claims more than step %ds over %.1f days supports (best is %d)"
                              % (where, tier, step, span / DAY, floor))

    if len(clusters) > 1:
        errors.append("file mixes clusters: %s" % sorted(clusters))
    for wl in sorted(container_wl - set(workloads), key=str):
        errors.append("workload %s/%s/%s has containers but no workload resource carrying its replica count" % wl)
    return errors


def main(argv):
    if len(argv) < 2:
        print(__doc__.strip(), file=sys.stderr)
        return 2
    bad = False
    for path in argv[1:]:
        try:
            with open(path) as f:
                doc = json.load(f, parse_constant=lambda c: (_ for _ in ()).throw(ValueError("non-finite number " + c)))
        except (OSError, ValueError) as e:
            print("%s: cannot read: %s" % (path, e))
            bad = True
            continue
        errs = validate(doc)
        if errs:
            bad = True
            print("%s: %d error(s)" % (path, len(errs)))
            for e in errs[:50]:
                print("  " + e)
            if len(errs) > 50:
                print("  ... %d more" % (len(errs) - 50))
        else:
            print("%s: ok" % path)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
