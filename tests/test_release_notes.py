"""Tests for scripts/release_notes.py and the CHANGELOG.md it reads."""

import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

import release_notes as rn  # noqa: E402

REPO = "thoras-ai/savings-preview"
BLOB = f"https://github.com/{REPO}/blob"

CHANGELOG = """\
# Changelog

Intro text.

## v0.2.0

### New

- Two.

## v0.1.0

- One.
"""


class NotesTest(unittest.TestCase):
    def test_takes_the_section_up_to_the_next_heading(self):
        self.assertEqual(rn.notes(CHANGELOG, "v0.2.0", REPO), "### New\n\n- Two.\n")
        self.assertEqual(rn.notes(CHANGELOG, "v0.1.0", REPO), "- One.\n")

    def test_heading_may_carry_a_date(self):
        text = CHANGELOG.replace("## v0.2.0", "## v0.2.0 - 2026-10-02")
        self.assertEqual(rn.notes(text, "v0.2.0", REPO), "### New\n\n- Two.\n")

    def test_missing_section_fails(self):
        with self.assertRaisesRegex(rn.NotesError, "no section for v0.3.0"):
            rn.notes(CHANGELOG, "v0.3.0", REPO)

    def test_version_is_matched_whole(self):
        with self.assertRaisesRegex(rn.NotesError, "no section for v0.2"):
            rn.notes(CHANGELOG, "v0.2", REPO)

    def test_empty_section_fails(self):
        with self.assertRaisesRegex(rn.NotesError, "empty"):
            rn.notes("## v0.2.0\n\n\n## v0.1.0\n- One.\n", "v0.2.0", REPO)

    def test_duplicate_section_fails(self):
        with self.assertRaisesRegex(rn.NotesError, "two sections"):
            rn.notes(CHANGELOG + "\n## v0.2.0\n- Again.\n", "v0.2.0", REPO)

    def test_prerelease_falls_back_to_its_release(self):
        self.assertEqual(rn.notes(CHANGELOG, "v0.2.0-rc.1", REPO), "### New\n\n- Two.\n")
        text = CHANGELOG.replace("## v0.2.0\n", "## v0.2.0-rc.1\n\n- Candidate.\n\n## v0.2.0\n")
        self.assertEqual(rn.notes(text, "v0.2.0-rc.1", REPO), "- Candidate.\n")

    def test_relative_links_point_at_the_tag(self):
        text = (
            "## v0.2.0\n\n"
            "See [upgrading](docs/upgrading.md#the-zip), [readme](/README.md), "
            "[above](#v010), [site](https://example.com/a) and [mail](mailto:a@example.com).\n"
        )
        self.assertEqual(
            rn.notes(text, "v0.2.0", REPO),
            f"See [upgrading]({BLOB}/v0.2.0/docs/upgrading.md#the-zip), "
            f"[readme]({BLOB}/v0.2.0/README.md), "
            f"[above]({BLOB}/v0.2.0/CHANGELOG.md#v010), "
            "[site](https://example.com/a) and [mail](mailto:a@example.com).\n",
        )

    def test_link_outside_the_repository_fails(self):
        with self.assertRaisesRegex(rn.NotesError, "outside"):
            rn.notes("## v0.2.0\n\n[x](../other.md)\n", "v0.2.0", REPO)


class ScriptTest(unittest.TestCase):
    def run_script(self, *args):
        return subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "release_notes.py"), *args],
            capture_output=True, text=True,
        )

    def test_prints_notes(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "CHANGELOG.md"
            path.write_text(CHANGELOG)
            out = self.run_script("v0.1.0", "--changelog", str(path))
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertEqual(out.stdout, "- One.\n")

    def test_fails_without_a_section(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "CHANGELOG.md"
            path.write_text(CHANGELOG)
            out = self.run_script("v9.9.9", "--changelog", str(path))
        self.assertEqual(out.returncode, 1)
        self.assertEqual(out.stdout, "")
        self.assertIn("no section for v9.9.9", out.stderr)


class RepositoryChangelogTest(unittest.TestCase):
    def test_every_tag_has_a_section_with_working_links(self):
        text = rn.CHANGELOG.read_text()
        tags = subprocess.run(
            ["git", "tag", "--list", "v*"], cwd=ROOT, capture_output=True, text=True,
        ).stdout.split()
        for tag in sorted(set(tags) | set(rn.sections(text))):
            with self.subTest(tag=tag):
                rn.notes(text, tag, REPO)

    def test_relative_links_name_files_that_exist(self):
        text = rn.CHANGELOG.read_text()
        for m in rn.LINK.finditer(text):
            target = m.group(2)
            if rn.SCHEME.match(target) or target.startswith("#"):
                continue
            path, _, anchor = target.partition("#")
            with self.subTest(link=target):
                self.assertTrue((ROOT / path).exists())
                if anchor:
                    self.assertIn(anchor, anchors((ROOT / path).read_text()))


def anchors(markdown):
    # GitHub's heading anchors: lower case, punctuation dropped, spaces as hyphens.
    found = set()
    for line in markdown.splitlines():
        m = re.match(r"^#+ (.*)$", line)
        if m:
            found.add(re.sub(r"[^\w\- ]", "", m.group(1).strip().lower()).replace(" ", "-"))
    return found


if __name__ == "__main__":
    unittest.main()
