"""Tests for scripts/update_tap.sh.

The tap is a local bare git repository, so nothing touches the network.
"""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "update_tap.sh"
PATH_IN_TAP = "Formula/savings-preview.rb"


def git(*args, cwd=None):
    return subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True).stdout


class UpdateTapTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: subprocess.run(["rm", "-rf", str(self.tmp)]))
        self.tap = self.tmp / "homebrew-tap.git"
        git("init", "-q", "--bare", "-b", "main", str(self.tap))
        self.env = {
            **os.environ,
            "TAP_URL": str(self.tap),
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_AUTHOR_NAME": "Release", "GIT_AUTHOR_EMAIL": "release@example.com",
            "GIT_COMMITTER_NAME": "Release", "GIT_COMMITTER_EMAIL": "release@example.com",
        }
        self.formula = self.tmp / "savings-preview.rb"
        self.formula.write_text('class SavingsPreview < Formula\n  version "1.2.3"\nend\n')

    def seed(self, files):
        work = self.tmp / "seed"
        git("clone", "-q", str(self.tap), str(work))
        for name, text in files.items():
            (work / name).parent.mkdir(parents=True, exist_ok=True)
            (work / name).write_text(text)
        git("add", "-A", cwd=work)
        git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "seed", cwd=work)
        git("push", "-q", "origin", "HEAD:main", cwd=work)

    def run_script(self, formula=None):
        return subprocess.run([str(SCRIPT), "v1.2.3", str(formula or self.formula)], env=self.env, capture_output=True, text=True)

    def tap_file(self):
        return git("--git-dir", str(self.tap), "show", f"main:{PATH_IN_TAP}")

    def tap_log(self):
        return git("--git-dir", str(self.tap), "log", "--format=%s", "main").splitlines()

    def test_adds_the_formula_to_a_tap_without_one(self):
        self.seed({"README.md": "tap\n"})
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.tap_file(), self.formula.read_text())
        self.assertEqual(self.tap_log()[0], "savings-preview 1.2.3")
        self.assertIn("pushed", r.stdout)

    def test_replaces_an_older_formula(self):
        self.seed({PATH_IN_TAP: "old\n", "Formula/other.rb": "other\n"})
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.tap_file(), self.formula.read_text())
        self.assertEqual(git("--git-dir", str(self.tap), "show", "main:Formula/other.rb"), "other\n")
        self.assertEqual(len(self.tap_log()), 2)

    def test_leaves_the_same_formula_alone(self):
        self.seed({PATH_IN_TAP: self.formula.read_text()})
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("already has the formula", r.stdout)
        self.assertEqual(self.tap_log(), ["seed"])

    def test_fails_when_the_tap_cannot_be_cloned(self):
        self.env["TAP_URL"] = str(self.tmp / "missing.git")
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("cannot clone", r.stderr)

    def test_refuses_a_formula_that_downloads_through_the_api(self):
        self.seed({PATH_IN_TAP: "good\n"})
        old = self.tmp / "old.rb"
        old.write_text('url "https://api.github.com/repos/thoras-ai/savings-preview/releases/assets/1", headers: HEADERS\n')
        r = self.run_script(old)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("api.github.com", r.stderr)
        self.assertEqual(self.tap_file(), "good\n")

    def test_commits_as_the_callers_git_identity(self):
        self.seed({"README.md": "tap\n"})
        r = self.run_script()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(git("--git-dir", str(self.tap), "log", "-1", "--format=%ae", "main").strip(), "release@example.com")

    def test_fails_without_the_formula(self):
        r = self.run_script(self.tmp / "missing.rb")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("does not exist", r.stderr)


if __name__ == "__main__":
    unittest.main()
