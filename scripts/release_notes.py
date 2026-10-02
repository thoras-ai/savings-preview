#!/usr/bin/env python3
"""Print the CHANGELOG.md section for a release, for use as its release notes.

    scripts/release_notes.py v0.2.0 --repo thoras-ai/savings-preview > notes.md

The section runs from the `## v0.2.0` heading to the next `## ` heading. A
prerelease tag such as v0.2.0-rc.1 uses its own section when there is one and
the v0.2.0 section otherwise. Relative links point into the repository, so they
are rewritten to absolute links at the tag, which work on the release page.
"""

import argparse
import posixpath
import re
import sys
from pathlib import Path

CHANGELOG = Path(__file__).resolve().parent.parent / "CHANGELOG.md"
HEADING = re.compile(r"^## (v\S+)(?:\s.*)?$")
LINK = re.compile(r"(\]\()([^)\s]+)(\))")
SCHEME = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")


class NotesError(Exception):
    pass


def sections(text):
    found = {}
    version, lines = None, []
    for line in text.splitlines():
        if line.startswith("## "):
            if version is not None:
                found[version] = lines
            m = HEADING.match(line)
            version, lines = (m.group(1) if m else None), []
            if version in found:
                raise NotesError(f"CHANGELOG.md has two sections for {version}")
        elif version is not None:
            lines.append(line)
    if version is not None:
        found[version] = lines
    return found


def absolute_links(text, repo, tag, changelog_path="CHANGELOG.md"):
    base = f"https://github.com/{repo}/blob/{tag}/"

    def fix(m):
        target = m.group(2)
        if SCHEME.match(target):
            return m.group(0)
        if target.startswith("#"):
            return m.group(1) + base + changelog_path + target + m.group(3)
        if target.startswith("/"):
            path = target[1:]
        else:
            path = posixpath.normpath(posixpath.join(posixpath.dirname(changelog_path), target))
        if path == ".." or path.startswith("../"):
            raise NotesError(f"link {target!r} points outside the repository")
        return m.group(1) + base + path + m.group(3)

    return LINK.sub(fix, text)


def notes(text, tag, repo):
    found = sections(text)
    version = tag
    if version not in found and "-" in tag:
        version = tag.split("-", 1)[0]
    if version not in found:
        raise NotesError(f"CHANGELOG.md has no section for {tag}")
    body = "\n".join(found[version]).strip()
    if not body:
        raise NotesError(f"the CHANGELOG.md section for {version} is empty")
    return absolute_links(body, repo, tag) + "\n"


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("tag", help="release tag, such as v0.2.0")
    p.add_argument("--repo", default="thoras-ai/savings-preview", help="owner/name for absolute links")
    p.add_argument("--changelog", type=Path, default=CHANGELOG)
    args = p.parse_args(argv)
    try:
        sys.stdout.write(notes(args.changelog.read_text(), args.tag, args.repo))
    except NotesError as e:
        print(f"release_notes.py: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
