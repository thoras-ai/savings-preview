#!/usr/bin/env python3
"""Import the Thoras product's node price table into internal/savings/node_prices.json.

Usage: import_node_prices.py PLATFORM_DIR [OUT]

PLATFORM_DIR is a checkout of the platform repository. The product loads its
instance_pricing table from SQL migrations in go/db/migrations, and this reads
the newest of each:

  *_aws_pricing.sql              AWS on-demand and spot, us-east-1
  *_gcp_pricing.sql              Google Cloud on-demand and spot, us-central1
  *_azure_pricing.sql            Azure on-demand and spot, eastus
  *_gcp_custom_node_pricing.sql  Google Cloud custom machine types, per vCPU and per GiB

The product generates the first three with scripts/generate-cloud-pricing and
stores every row under region 'default'. Only Linux rows are kept, as the
product prices every node as Linux. Spot prices are computed the way the
migrations' UPDATE statements compute them, on-demand × (100 − discount) / 100,
rounded to the table's 10 decimal places.
"""
import decimal
import glob
import json
import os
import re
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
OUT = os.path.join(ROOT, "internal", "savings", "node_prices.json")
MIGRATIONS = os.path.join("go", "db", "migrations")
# The region each cloud's table was generated for (generate-cloud-pricing's defaults).
CLOUDS = [("aws", "us-east-1"), ("gcp", "us-central1"), ("azure", "eastus")]
TUPLE = re.compile(r"^\s*\((.*)\)[,;]?\s*$")
PLACES = decimal.Decimal("1e-10")


def newest(platform, suffix):
    found = sorted(glob.glob(os.path.join(platform, MIGRATIONS, "[0-9]*_" + suffix)))
    if not found:
        raise SystemExit("no %s migration in %s" % (suffix, os.path.join(platform, MIGRATIONS)))
    return found[-1]


def fields(text):
    out = []
    for f in text.split(","):
        f = f.strip()
        out.append(f[1:-1] if f.startswith("'") else decimal.Decimal(f))
    return out


def tuples(path):
    """The value tuples of the file's INSERT, then those of its UPDATE."""
    insert, update, cur, values = [], [], None, False
    with open(path, encoding="utf-8") as f:
        for line in f:
            if line.startswith("INSERT"):
                cur, values = insert, False
            elif line.startswith("UPDATE"):
                cur, values = update, False
            elif line.startswith("-- migrate:down"):
                break
            elif line.strip().startswith("VALUES"):
                values = True
            elif values and not line.strip():
                values = False
            m = TUPLE.match(line)
            if m and values and cur is not None:
                cur.append(fields(m.group(1)))
    return insert, update


def flat(path, cloud):
    insert, update = tuples(path)
    types = {}
    for provider, typ, os_, region, vcpu, mib, hourly in insert:
        if provider != cloud:
            raise SystemExit("%s: row for %s in the %s table" % (path, provider, cloud))
        if os_ == "linux" and region == "default":
            types[typ] = [vcpu, mib, hourly, None]
    for os_, typ, region, discount in update:
        if os_ == "linux" and region == "default" and typ in types:
            spot = types[typ][2] * (100 - discount) / 100
            types[typ][3] = spot.quantize(PLACES, rounding=decimal.ROUND_HALF_UP)
    return types


def custom(path):
    insert, update = tuples(path)
    prefixes = {}
    for row in insert:
        provider, prefix, os_, region = row[:4]
        if provider == "gcp" and os_ == "linux" and region == "default" and row[7] == "per_unit":
            prefixes[prefix] = [row[8], row[9], None, None]
    for prefix, vcpu, gib in update:
        if prefix in prefixes:
            prefixes[prefix][2:] = [vcpu, gib]
    return [[p] + v for p, v in prefixes.items()]


def number(x):
    if x is None:
        return "null"
    if isinstance(x, str):
        return json.dumps(x)
    return format(x.normalize(), "f")


def row(values):
    return "[" + ", ".join(number(v) for v in values) + "]"


def render(sources, tables, customs):
    lines = ["{", '  "source": %s,' % json.dumps(sources)]
    for i, (cloud, region) in enumerate(CLOUDS):
        lines.append('  "%s": {' % cloud)
        lines.append('    "region": "%s",' % region)
        types = tables[cloud]
        lines.append('    "types": {')
        names = sorted(types)
        for j, t in enumerate(names):
            lines.append("      %s: %s%s" % (json.dumps(t), row(types[t]), "," if j + 1 < len(names) else ""))
        lines.append("    },")
        rows = customs.get(cloud, [])
        lines.append('    "custom": [' + ("" if rows else "]"))
        for j, r in enumerate(rows):
            lines.append("      %s%s" % (row(r), "," if j + 1 < len(rows) else ""))
        if rows:
            lines.append("    ]")
        lines.append("  }" + ("," if i + 1 < len(CLOUDS) else ""))
    lines.append("}")
    return "\n".join(lines) + "\n"


def main(argv):
    if len(argv) not in (2, 3):
        raise SystemExit(__doc__)
    platform = argv[1]
    out = argv[2] if len(argv) == 3 else OUT
    paths = {cloud: newest(platform, cloud + "_pricing.sql") for cloud, _ in CLOUDS}
    paths["gcp custom"] = newest(platform, "gcp_custom_node_pricing.sql")
    tables = {cloud: flat(paths[cloud], cloud) for cloud, _ in CLOUDS}
    customs = {"gcp": custom(paths["gcp custom"])}
    sources = [os.path.join(MIGRATIONS, os.path.basename(paths[k])) for k in ("aws", "gcp", "azure", "gcp custom")]
    text = render(sources, tables, customs)
    json.loads(text)
    with open(out, "w", encoding="utf-8") as f:
        f.write(text)
    print("wrote %s: %s" % (os.path.relpath(out), ", ".join("%d %s" % (len(tables[c]), c) for c, _ in CLOUDS)))


if __name__ == "__main__":
    main(sys.argv)
