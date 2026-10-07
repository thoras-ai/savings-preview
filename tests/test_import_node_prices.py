"""Tests for scripts/import_node_prices.py and the node_prices.json it writes."""

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import import_node_prices as inp  # noqa: E402

FLAT = """\
-- migrate:up

DELETE FROM instance_pricing WHERE provider = '{p}';

INSERT INTO instance_pricing
  (provider, instance_type, operating_system, region, vcpu_cores, memory_mebibytes, on_demand_cost_hourly)
  VALUES
    ('{p}', '{a}', 'linux', 'default', 4, 16384, 0.192),
    ('{p}', '{b}', 'linux', 'default', 2, 4096, 0.0855),
    ('{p}', '{a}', 'windows', 'default', 4, 16384, 0.376);

UPDATE instance_pricing
SET spot_cost_hourly = on_demand_cost_hourly * (100 - spot.spot_discount) / 100
FROM (
  VALUES
    ('linux', '{a}', 'default', 68)
  ) spot(operating_system, instance_type, region, spot_discount)
WHERE instance_pricing.provider = '{p}'
;

-- migrate:down
-- no down migration possible; leave the current values in place
"""

CUSTOM = """\
-- migrate:up

INSERT INTO instance_pricing (provider, instance_type, operating_system, region, vcpu_cores, memory_mebibytes, on_demand_cost_hourly, pricing_model, vcpu_cost_hourly, memory_cost_hourly)
VALUES
    ('gcp', 'n2d-custom-', 'linux', 'default', 0, 0, 0, 'per_unit', 0.0325248, 0.00435855),
    ('gcp', 'e2-custom-',  'linux', 'default', 0, 0, 0, 'per_unit', 0.02579582, 0.00345659);

UPDATE instance_pricing
SET
    spot_vcpu_cost_hourly   = spot.spot_vcpu_cost_hourly,
    spot_memory_cost_hourly = spot.spot_memory_cost_hourly
FROM (
  VALUES
    ('n2d-custom-', 0.01015,  0.001663)
) spot(instance_type, spot_vcpu_cost_hourly, spot_memory_cost_hourly)
WHERE instance_pricing.provider = 'gcp';

-- migrate:down
"""


class ImportTest(unittest.TestCase):
    def test_reads_the_newest_migrations(self):
        with tempfile.TemporaryDirectory() as d:
            mig = Path(d) / "go" / "db" / "migrations"
            mig.mkdir(parents=True)
            types = {"aws": ("m5.xlarge", "c5.large"), "gcp": ("n2-standard-4", "e2-medium"), "azure": ("Standard_D4s_v5", "Standard_B2s")}
            for p, (a, b) in types.items():
                (mig / ("20260101000000_%s_pricing.sql" % p)).write_text(FLAT.format(p=p, a="old." + a, b=b))
                (mig / ("20260201000000_%s_pricing.sql" % p)).write_text(FLAT.format(p=p, a=a, b=b))
            (mig / "20260201000000_gcp_custom_node_pricing.sql").write_text(CUSTOM)
            out = Path(d) / "prices.json"
            inp.main(["import_node_prices.py", d, str(out)])
            got = json.loads(out.read_text())

        self.assertEqual(got["source"][0], "go/db/migrations/20260201000000_aws_pricing.sql")
        self.assertEqual([got[c]["region"] for c in ("aws", "gcp", "azure")], ["us-east-1", "us-central1", "eastus"])
        # Linux rows only, the newest file only, spot from the discount.
        self.assertEqual(got["aws"]["types"], {"m5.xlarge": [4, 16384, 0.192, 0.06144], "c5.large": [2, 4096, 0.0855, None]})
        self.assertEqual(got["gcp"]["custom"], [["n2d-custom-", 0.0325248, 0.00435855, 0.01015, 0.001663],
                                                ["e2-custom-", 0.02579582, 0.00345659, None, None]])
        self.assertEqual(got["azure"]["custom"], [])

    def test_committed_table_is_well_formed(self):
        got = json.loads((ROOT / "internal" / "savings" / "node_prices.json").read_text())
        for cloud in ("aws", "gcp", "azure"):
            self.assertGreater(len(got[cloud]["types"]), 100, cloud)
            for t, row in got[cloud]["types"].items():
                self.assertEqual(len(row), 4, t)
                self.assertTrue(row[0] > 0 and row[1] > 0 and row[2] > 0, t)
                self.assertTrue(row[3] is None or 0 <= row[3] <= row[2], t)
        self.assertTrue(all(len(r) == 5 and r[0].endswith("-custom-") for r in got["gcp"]["custom"]))


if __name__ == "__main__":
    unittest.main()
