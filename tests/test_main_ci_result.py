"""Tests for scripts/main_ci_result.py, with a fake gh that serves recorded runs."""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import main_ci_result  # noqa: E402

REPO = "thoras-ai/savings-preview"
SHA = "2e6cd52fb5d690d7ef8b518d4ee48b85f149ebbb"

# Answers `gh api ...` with the next file in $FAKE_RUNS (one JSON run per
# line), repeating the last one, and logs each call.
FAKE_GH = """\
#!/bin/sh
echo "$*" >>"$FAKE_LOG"
n=$(wc -l <"$FAKE_LOG" | tr -d ' ')
set -- $FAKE_RUNS
[ "$n" -le $# ] || n=$#
eval f=\\${$n}
[ "$f" = fail ] && { echo "HTTP 502" >&2; exit 1; }
cat "$f"
"""


def run(**overrides):
    r = {
        "event": "push",
        "head_branch": "main",
        "head_sha": SHA,
        "head_repository": {"full_name": REPO},
        "status": "completed",
        "conclusion": "success",
        "created_at": "2026-10-02T18:26:09Z",
        "html_url": "https://github.com/thoras-ai/savings-preview/actions/runs/1",
    }
    r.update(overrides)
    return r


class StateTest(unittest.TestCase):
    def kept(self, *runs):
        return main_ci_result.main_runs(list(runs), REPO, SHA)

    def test_success_on_main(self):
        for event in ("push", "schedule", "workflow_dispatch"):
            with self.subTest(event=event):
                self.assertEqual(main_ci_result.state(self.kept(run(event=event))), "success")

    def test_ignores_runs_that_skip_checks_or_are_not_this_commit_on_main(self):
        others = [
            run(event="pull_request"),
            run(head_branch="feature"),
            run(head_sha="0" * 40),
            run(head_repository={"full_name": "someone/savings-preview"}),
            run(head_repository=None),
        ]
        self.assertEqual(self.kept(*others), [])
        self.assertEqual(main_ci_result.state(self.kept(*others)), "none")

    def test_one_success_is_enough(self):
        runs = self.kept(run(conclusion="failure"), run(status="in_progress", conclusion=None), run())
        self.assertEqual(main_ci_result.state(runs), "success")

    def test_running_before_failed(self):
        runs = self.kept(run(conclusion="failure"), run(status="queued", conclusion=None))
        self.assertEqual(main_ci_result.state(runs), "running")

    def test_failed_or_cancelled(self):
        for conclusion in ("failure", "cancelled", "timed_out"):
            with self.subTest(conclusion=conclusion):
                self.assertEqual(main_ci_result.state(self.kept(run(conclusion=conclusion))), "failed")


class CommandTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        (self.tmp / "gh").write_text(FAKE_GH)
        (self.tmp / "gh").chmod(0o755)
        self.log = self.tmp / "gh.log"

    def answers(self, *pages):
        files = []
        for i, runs in enumerate(pages):
            if runs == "fail":
                files.append("fail")
                continue
            f = self.tmp / f"runs{i}.jsonl"
            f.write_text("".join(json.dumps(r) + "\n" for r in runs))
            files.append(str(f))
        return " ".join(files)

    def check(self, *pages, wait="0"):
        env = dict(os.environ, PATH=f"{self.tmp}:{os.environ['PATH']}",
                   FAKE_LOG=str(self.log), FAKE_RUNS=self.answers(*pages))
        p = subprocess.run(
            [sys.executable, ROOT / "scripts" / "main_ci_result.py", "--repo", REPO, "--sha", SHA,
             "--wait", wait, "--interval", "0"],
            env=env, check=True, capture_output=True, text=True,
        )
        return p.stdout, p.stderr, self.log.read_text().splitlines()

    def test_passed(self):
        out, err, calls = self.check([run()])
        self.assertEqual(out, "passed=true\n")
        self.assertIn("runs/1", err)
        self.assertEqual(len(calls), 1)
        self.assertIn(f"repos/{REPO}/actions/workflows/ci.yml/runs?head_sha={SHA}&branch=main", calls[0])

    def test_no_run(self):
        out, _, _ = self.check([])
        self.assertEqual(out, "passed=false\n")

    def test_failed(self):
        out, _, _ = self.check([run(conclusion="failure")])
        self.assertEqual(out, "passed=false\n")

    def test_waits_for_a_run_in_progress(self):
        running = [run(status="in_progress", conclusion=None)]
        out, _, calls = self.check(running, running, [run()], wait="60")
        self.assertEqual(out, "passed=true\n")
        self.assertEqual(len(calls), 3)

    def test_still_running_after_the_wait(self):
        out, err, _ = self.check([run(status="in_progress", conclusion=None)])
        self.assertEqual(out, "passed=false\n")
        self.assertIn("still running", err)

    def test_api_error_runs_the_checks(self):
        out, err, _ = self.check("fail")
        self.assertEqual(out, "passed=false\n")
        self.assertIn("HTTP 502", err)


if __name__ == "__main__":
    unittest.main()
