"""Tests for scripts/homebrew_formula.py and the formula template."""

import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import homebrew_formula as hf  # noqa: E402

TAG = "v1.2.3"
API = "https://api.github.com/repos/thoras-ai/savings-preview/releases/assets"


def checksums():
    return "".join(
        f"{str(i) * 64}  savings-preview_{TAG}_{p}.tar.gz\n" for i, p in enumerate(hf.PLATFORMS, 1)
    )


def release(tag=TAG):
    names = [f"savings-preview_{tag}_{p}.tar.gz" for p in hf.PLATFORMS] + ["checksums.txt", "install.sh"]
    return {
        "tag_name": tag,
        "assets": [{"name": n, "url": f"{API}/{i}", "id": i} for i, n in enumerate(names, 10)],
    }


class RenderTest(unittest.TestCase):
    def render(self, sums=None, rel=None, tag=TAG):
        return hf.render(hf.TEMPLATE.read_text(), tag, hf.read_checksums(sums or checksums()), rel or release())

    def test_fills_every_placeholder(self):
        out = self.render()
        self.assertNotIn("@", out.replace("#{", ""))
        self.assertIn('version "1.2.3"', out)
        for i, p in enumerate(hf.PLATFORMS):
            self.assertIn(f'url "{API}/{10 + i}", headers: HEADERS\n      sha256 "{str(i + 1) * 64}"', out)

    def test_gh_release_view_json(self):
        rel = release()
        gh = {
            "tagName": rel["tag_name"],
            "assets": [
                {"name": a["name"], "apiUrl": a["url"],
                 "url": f"https://github.com/thoras-ai/savings-preview/releases/download/{TAG}/{a['name']}"}
                for a in rel["assets"]
            ],
        }
        self.assertEqual(self.render(rel=gh), self.render())

    def test_each_archive_is_in_its_platform_block(self):
        out = self.render()
        macos, linux = out.split("on_linux do")
        sums = {p: str(i) * 64 for i, p in enumerate(hf.PLATFORMS, 1)}
        for os_block, os_name in ((macos, "darwin"), (linux, "linux")):
            arm, intel = os_block.split("on_arm do")[1].split("on_intel do")
            with self.subTest(os=os_name):
                self.assertIn(sums[f"{os_name}_arm64"], arm)
                self.assertIn(sums[f"{os_name}_amd64"], intel)
                self.assertNotIn(sums[f"{os_name}_amd64"], arm)

    def test_missing_checksum(self):
        sums = "".join(l for l in checksums().splitlines(keepends=True) if "linux_arm64" not in l)
        with self.assertRaisesRegex(hf.FormulaError, "no entry for savings-preview_v1.2.3_linux_arm64"):
            self.render(sums=sums)

    def test_missing_asset(self):
        rel = release()
        rel["assets"] = [a for a in rel["assets"] if "darwin_amd64" not in a["name"]]
        with self.assertRaisesRegex(hf.FormulaError, "no asset named savings-preview_v1.2.3_darwin_amd64"):
            self.render(rel=rel)

    def test_release_for_another_tag(self):
        with self.assertRaisesRegex(hf.FormulaError, "not 'v1.2.3'"):
            self.render(rel=release("v1.2.4"))

    def test_bad_checksum_lines(self):
        for line in ["nothex  savings-preview.tar.gz", f"{'a' * 63}  x", f"{'a' * 64}", f"{'A' * 64}  x"]:
            with self.subTest(line=line):
                with self.assertRaises(hf.FormulaError):
                    hf.read_checksums(line + "\n")

    def test_duplicate_checksum(self):
        with self.assertRaisesRegex(hf.FormulaError, "listed twice"):
            hf.read_checksums(checksums() + checksums().splitlines()[0] + "\n")

    def test_binary_mode_checksum_lines(self):
        self.assertIn(f"savings-preview_{TAG}_linux_amd64.tar.gz", hf.read_checksums(checksums().replace("  ", " *")))

    def test_unknown_placeholder(self):
        with self.assertRaisesRegex(hf.FormulaError, "@NOPE@"):
            hf.render('x "@NOPE@"', TAG, hf.read_checksums(checksums()), release())


class CommandTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        (self.tmp / "checksums.txt").write_text(checksums())
        (self.tmp / "release.json").write_text(json.dumps(release()))

    def run_cmd(self, tag=TAG):
        return subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "homebrew_formula.py"), tag,
             str(self.tmp / "checksums.txt"), str(self.tmp / "release.json")],
            capture_output=True, text=True,
        )

    def test_writes_the_formula(self):
        r = self.run_cmd()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue(r.stdout.startswith("# Homebrew formula"))
        self.assertIn("class SavingsPreview < Formula", r.stdout)

    def test_error_exits_nonzero_with_no_output(self):
        r = self.run_cmd(tag="v9.9.9")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(r.stdout, "")
        self.assertIn("homebrew_formula.py:", r.stderr)

    @unittest.skipIf(shutil.which("ruby") is None, "ruby is not installed")
    def test_template_and_output_are_valid_ruby(self):
        out = self.tmp / "savings-preview.rb"
        out.write_text(self.run_cmd().stdout)
        for path in (hf.TEMPLATE, out):
            with self.subTest(path=path.name):
                r = subprocess.run(["ruby", "-c", str(path)], capture_output=True, text=True)
                self.assertEqual(r.returncode, 0, r.stderr)


if __name__ == "__main__":
    unittest.main()
