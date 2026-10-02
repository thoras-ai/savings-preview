"""Checks on the GitHub Actions workflows and the Dependabot configuration."""

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WORKFLOWS = sorted((ROOT / ".github" / "workflows").glob("*.yml"))
USES = re.compile(r"^\s*(?:-\s+)?uses:\s*(\S+)(.*)$")
PINNED = re.compile(r"^[\w.-]+/[\w./-]+@[0-9a-f]{40}$")
VERSION_COMMENT = re.compile(r"^\s+# v\d+(\.\d+)*$")


def uses_lines():
    for path in WORKFLOWS:
        for n, line in enumerate(path.read_text().splitlines(), 1):
            m = USES.match(line)
            if m:
                yield f"{path.name}:{n}", m.group(1), m.group(2)


class WorkflowTest(unittest.TestCase):
    def test_actions_are_pinned_to_a_commit_with_its_version(self):
        found = 0
        for where, ref, rest in uses_lines():
            if ref.startswith("./"):
                continue
            found += 1
            with self.subTest(where=where):
                self.assertRegex(ref, PINNED)
                self.assertRegex(rest, VERSION_COMMENT)
        self.assertGreater(found, 0)

    def test_each_action_version_uses_one_commit(self):
        commits = {}
        for where, ref, rest in uses_lines():
            if ref.startswith("./"):
                continue
            path, sha = ref.split("@")
            repo = "/".join(path.split("/")[:2])
            commits.setdefault((repo, rest.strip()), set()).add(sha)
        for key, shas in commits.items():
            with self.subTest(action=key):
                self.assertEqual(len(shas), 1)


def cache_steps(path):
    """The text of each actions/cache step in a workflow, without its indentation."""
    lines = path.read_text().splitlines()
    steps = []
    for i, line in enumerate(lines):
        if re.match(r"^\s*-?\s*uses: actions/cache", line):
            indent = len(line) - len(line.lstrip(" -"))
            j = i + 1
            while j < len(lines) and (not lines[j].strip() or len(lines[j]) - len(lines[j].lstrip()) >= indent
                                      and not lines[j].lstrip().startswith("- ")):
                j += 1
            steps.append("\n".join(l[indent:] for l in lines[i:j]).strip())
    return steps


class ReleaseCacheTest(unittest.TestCase):
    """The release builds restore the Go build cache CI saves on main and never save one."""

    def setUp(self):
        workflows = ROOT / ".github" / "workflows"
        self.ci = cache_steps(workflows / "ci.yml")
        self.release = cache_steps(workflows / "release.yml")

    def test_release_restores_the_cache_ci_saves(self):
        self.assertEqual(len(self.release), 1)
        restore = self.release[0]
        self.assertTrue(restore.startswith("uses: actions/cache/restore@"), restore)
        # The vet job's restore step is the one whose key the save step reuses.
        self.assertIn(restore, self.ci)

    def test_ci_compiles_the_release_binaries_into_the_cache_before_saving_it(self):
        vet = (ROOT / ".github" / "workflows" / "ci.yml").read_text().split("\n  test:")[0]
        build = vet.find("run: scripts/release.sh build ")
        self.assertGreater(build, 0)
        self.assertLess(build, vet.index("-mmin +60"))
        self.assertLess(build, vet.index("uses: actions/cache/save@"))

    def test_release_never_saves_a_cache(self):
        for step in self.release:
            self.assertNotRegex(step, r"^uses: actions/cache(/save)?@")


class DependabotTest(unittest.TestCase):
    def setUp(self):
        self.config = (ROOT / ".github" / "dependabot.yml").read_text()

    def directories(self, ecosystem):
        blocks = re.split(r"\n  - ", self.config)
        return {
            re.search(r"directory: (\S+)", b).group(1)
            for b in blocks
            if re.search(rf"package-ecosystem: {ecosystem}\b", b)
        }

    def test_covers_every_go_module(self):
        modules = set()
        for p in ROOT.glob("**/go.mod"):
            rel = p.parent.relative_to(ROOT)
            if any(part.startswith(".") or part == "node_modules" for part in rel.parts):
                continue
            modules.add("/" + rel.as_posix().removeprefix("."))
        self.assertEqual(self.directories("gomod"), modules)

    def test_covers_actions(self):
        self.assertEqual(self.directories("github-actions"), {"/"})

    def test_allows_each_tool(self):
        tools = re.search(r"^tool \((.*?)^\)", (ROOT / "tools" / "go.mod").read_text(), re.M | re.S)
        tools_block = next(b for b in re.split(r"\n  - ", self.config) if "directory: /tools" in b)
        allowed = set(re.findall(r"dependency-name: (\S+)", tools_block))
        for path in tools.group(1).split():
            with self.subTest(tool=path):
                self.assertTrue(any(path == m or path.startswith(m + "/") for m in allowed))


if __name__ == "__main__":
    unittest.main()
