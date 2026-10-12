"""Tests for scripts/update_tap.sh.

A fake gh serves the tap repository from a JSON file and records each call, so
nothing touches the network.
"""

import base64
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "update_tap.sh"
TAP = "thoras-ai/homebrew-tap"
PATH_IN_TAP = "Formula/savings-preview.rb"

FAKE_GH = textwrap.dedent(f"""\
    #!{sys.executable}
    import base64, json, os, sys
    state_path = os.environ["FAKE_STATE"]
    state = json.load(open(state_path))
    args = sys.argv[1:]
    state["calls"].append(args)
    json.dump(state, open(state_path, "w"))
    assert args[0] == "api", args
    fields = {{}}
    for i, a in enumerate(args):
        if a == "-f":
            k, v = args[i + 1].split("=", 1)
            fields[k] = v
    method = args[args.index("-X") + 1] if "-X" in args else "GET"
    path = next(a for a in args if a.startswith("repos/"))
    if not state["exists"]:
        sys.exit(1)
    if path == "repos/{TAP}":
        print("{TAP}")
    elif path == "repos/{TAP}/contents/{PATH_IN_TAP}" and method == "GET":
        if state["file"] is None:
            print("gh: Not Found (HTTP 404)", file=sys.stderr)
            sys.exit(1)
        enc = base64.b64encode(state["file"].encode()).decode()
        lines = "\\n".join(enc[i:i + 60] for i in range(0, len(enc), 60)) + "\\n"
        print("blobsha " + lines)
    elif path == "repos/{TAP}/contents/{PATH_IN_TAP}" and method == "PUT":
        if state["file"] is not None and fields.get("sha") != "blobsha":
            sys.exit(1)
        state["file"] = base64.b64decode(fields["content"]).decode()
        state["message"] = fields["message"]
        json.dump(state, open(state_path, "w"))
        print("https://github.com/{TAP}/commit/abc")
    else:
        sys.exit(1)
""")


class UpdateTapTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: subprocess.run(["rm", "-rf", str(self.tmp)]))
        bindir = self.tmp / "bin"
        bindir.mkdir()
        (bindir / "gh").write_text(FAKE_GH)
        (bindir / "gh").chmod(0o755)
        self.env = {**os.environ, "PATH": f"{bindir}:{os.environ['PATH']}", "FAKE_STATE": str(self.tmp / "state.json")}
        self.formula = self.tmp / "savings-preview.rb"
        self.formula.write_text('class SavingsPreview < Formula\n  version "1.2.3"\n' + "  # padding\n" * 20 + "end\n")

    def run_script(self, exists=True, file=None):
        (self.tmp / "state.json").write_text(json.dumps({"exists": exists, "file": file, "calls": []}))
        r = subprocess.run([str(SCRIPT), "v1.2.3", str(self.formula)], env=self.env, capture_output=True, text=True)
        return r, json.loads((self.tmp / "state.json").read_text())

    def test_adds_the_formula_to_an_empty_tap(self):
        r, state = self.run_script(file=None)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(state["file"], self.formula.read_text())
        self.assertEqual(state["message"], "savings-preview 1.2.3")
        self.assertIn("commit/abc", r.stdout)

    def test_replaces_an_older_formula(self):
        r, state = self.run_script(file="old\n")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(state["file"], self.formula.read_text())
        put = [c for c in state["calls"] if "PUT" in c]
        self.assertEqual(len(put), 1)
        self.assertIn("sha=blobsha", put[0])

    def test_leaves_the_same_formula_alone(self):
        r, state = self.run_script(file=self.formula.read_text())
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("already has the formula", r.stdout)
        self.assertFalse([c for c in state["calls"] if "PUT" in c])

    def test_fails_when_the_tap_cannot_be_read(self):
        r, state = self.run_script(exists=False)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn(f"cannot read {TAP}", r.stderr)
        self.assertFalse([c for c in state["calls"] if "PUT" in c])

    def test_fails_without_the_formula(self):
        r = subprocess.run([str(SCRIPT), "v1.2.3", str(self.tmp / "missing.rb")], env=self.env, capture_output=True, text=True)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("does not exist", r.stderr)


if __name__ == "__main__":
    unittest.main()
