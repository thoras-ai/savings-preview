#!/usr/bin/env python3
"""Say whether CI passed on main for a commit, waiting while it runs.

    scripts/main_ci_result.py --repo thoras-ai/savings-preview --sha COMMIT [--wait SECONDS]

Reads the runs of .github/workflows/ci.yml for the commit through `gh api`.
Only runs on main in the repository itself count: a push, the nightly
schedule or a manual run, which all run every check including the race
detector. Pull request runs do not. Prints `passed=true` when one of them
succeeded, and `passed=false` when none did, none exists, one is still
running after --wait seconds, or the runs cannot be listed. The release
workflow appends the line to $GITHUB_OUTPUT and runs the checks itself
unless it says true.
"""

import argparse
import json
import subprocess
import sys
import time

MAIN_EVENTS = {"push", "schedule", "workflow_dispatch"}


def main_runs(runs, repo, sha):
    return [
        r for r in runs
        if r.get("event") in MAIN_EVENTS
        and r.get("head_branch") == "main"
        and r.get("head_sha") == sha
        and (r.get("head_repository") or {}).get("full_name") == repo
    ]


def state(runs):
    """success, running, failed or none, for the runs main_runs kept."""
    if any(r.get("conclusion") == "success" for r in runs):
        return "success"
    if any(r.get("status") != "completed" for r in runs):
        return "running"
    return "failed" if runs else "none"


def fetch(repo, sha):
    out = subprocess.run(
        ["gh", "api", "--paginate", "--jq", ".workflow_runs[]",
         f"repos/{repo}/actions/workflows/ci.yml/runs?head_sha={sha}&branch=main&per_page=100"],
        check=True, capture_output=True, text=True,
    ).stdout
    return [json.loads(line) for line in out.splitlines() if line.strip()]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--repo", required=True)
    ap.add_argument("--sha", required=True)
    ap.add_argument("--wait", type=float, default=0, help="seconds to wait for a run in progress")
    ap.add_argument("--interval", type=float, default=15, help="seconds between checks while waiting")
    args = ap.parse_args(argv)

    deadline = time.monotonic() + args.wait
    while True:
        try:
            runs = main_runs(fetch(args.repo, args.sha), args.repo, args.sha)
            s = state(runs)
        except subprocess.CalledProcessError as e:
            print(f"Could not list the CI runs: {e.stderr.strip()}", file=sys.stderr)
            runs, s = [], "error"
        if s != "running" or time.monotonic() >= deadline:
            break
        print(f"CI on main for {args.sha} is still running; checking again in {args.interval:g} s", file=sys.stderr)
        time.sleep(args.interval)

    messages = {
        "success": "CI passed on main for {sha}: {url}",
        "running": "CI on main for {sha} is still running after {wait:g} s; running the checks here",
        "failed": "CI did not pass on main for {sha}: {url}; running the checks here",
        "none": "No CI run on main for {sha}; running the checks here",
        "error": "Running the checks here",
    }
    shown = [r for r in runs if r.get("conclusion") == "success"] or runs
    url = max(shown, key=lambda r: r.get("created_at", ""), default={}).get("html_url", "")
    print(messages[s].format(sha=args.sha, wait=args.wait, url=url), file=sys.stderr)
    print(f"passed={'true' if s == 'success' else 'false'}")


if __name__ == "__main__":
    main()
