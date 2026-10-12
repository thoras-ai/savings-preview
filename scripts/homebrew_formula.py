#!/usr/bin/env python3
"""Fill in the Homebrew formula template for a release.

    gh release view v0.2.0 --json tagName,assets > release.json
    scripts/homebrew_formula.py v0.2.0 dist/release/checksums.txt release.json > savings-preview.rb

The release JSON comes from `gh release view --json tagName,assets` or from the
GitHub REST API, and is used to check that every archive is attached to the
release. The formula downloads each archive from the release's public download
URL and pins it to the SHA-256 in checksums.txt.
"""

import argparse
import json
import re
import sys
from pathlib import Path

TEMPLATE = Path(__file__).resolve().parent.parent / "packaging" / "homebrew" / "savings-preview.rb"
PLATFORMS = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"]
DOWNLOAD = "https://github.com/thoras-ai/savings-preview/releases/download"
SHA256 = re.compile(r"^[0-9a-f]{64}$")


class FormulaError(Exception):
    pass


def read_checksums(text):
    sums = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        parts = line.split()
        if len(parts) != 2 or not SHA256.match(parts[0]):
            raise FormulaError(f"checksums.txt: cannot parse line {line!r}")
        name = parts[1].lstrip("*")
        if name in sums:
            raise FormulaError(f"checksums.txt: {name} is listed twice")
        sums[name] = parts[0]
    return sums


def render(template, tag, checksums, release):
    # gh names the field tagName, the REST API tag_name.
    found = release.get("tagName", release.get("tag_name"))
    if found != tag:
        raise FormulaError(f"release JSON is for {found!r}, not {tag!r}")
    assets = {a["name"] for a in release.get("assets", [])}
    values = {"VERSION": tag.removeprefix("v")}
    for platform in PLATFORMS:
        name = f"savings-preview_{tag}_{platform}.tar.gz"
        if name not in checksums:
            raise FormulaError(f"checksums.txt has no entry for {name}")
        if name not in assets:
            raise FormulaError(f"release {tag} has no asset named {name}")
        values[f"URL_{platform.upper()}"] = f"{DOWNLOAD}/{tag}/{name}"
        values[f"SHA256_{platform.upper()}"] = checksums[name]

    def fill(m):
        if m.group(1) not in values:
            raise FormulaError(f"template has an unknown placeholder {m.group(0)}")
        return values[m.group(1)]

    return re.sub(r"@([A-Z0-9_]+)@", fill, template)


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("tag", help="release tag, such as v0.2.0")
    p.add_argument("checksums", type=Path, help="the release's checksums.txt")
    p.add_argument("release", type=Path, help="the release as returned by the GitHub API")
    p.add_argument("--template", type=Path, default=TEMPLATE)
    args = p.parse_args(argv)
    try:
        sys.stdout.write(render(
            args.template.read_text(),
            args.tag,
            read_checksums(args.checksums.read_text()),
            json.loads(args.release.read_text()),
        ))
    except FormulaError as e:
        print(f"homebrew_formula.py: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
