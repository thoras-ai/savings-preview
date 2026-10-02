"""Tests for the package and checksums steps of scripts/release.sh.

The script runs in a copy of the repository layout with a fake go command,
which builds a fake cyclonedx-gomod that records how it was called. Nothing is
compiled and nothing touches the network.
"""

import hashlib
import json
import os
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PLATFORMS = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"]
VERSION = "v1.2.3"

FAKE_GO = """\
#!/bin/sh
# Handles only: go build -modfile=tools/go.mod -o OUT PACKAGE
echo "go $*" >>"$FAKE_LOG"
[ "$1 $2 $3" = "build -modfile=tools/go.mod -o" ] || exit 1
cat >"$4" <<'TOOL'
#!/bin/sh
out= version=
while [ $# -gt 1 ]; do
	case $1 in
	-output) out=$2; shift ;;
	-version) version=$2; shift ;;
	esac
	shift
done
printf '{"goos":"%s","goarch":"%s","version":"%s","binary":"%s","bytes":"%s"}' \\
	"$GOOS" "$GOARCH" "$version" "$1" "$(cat "$1")" >"$out"
TOOL
chmod 755 "$4"
"""


class PackageTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = Path(tempfile.mkdtemp())
        cls.addClassCleanup(shutil.rmtree, cls.tmp)
        (cls.tmp / "scripts").mkdir()
        shutil.copy(ROOT / "scripts" / "release.sh", cls.tmp / "scripts")
        shutil.copy(ROOT / "scripts" / "install.sh", cls.tmp / "scripts")
        (cls.tmp / "README.md").write_text("readme\n")
        bindir = cls.tmp / "dist" / "release" / "bin"
        bindir.mkdir(parents=True)
        for p in PLATFORMS:
            (bindir / f"savings-preview-{p.replace('_', '-')}").write_text(f"binary {p}\n")
        fake = cls.tmp / "fakebin"
        fake.mkdir()
        (fake / "go").write_text(FAKE_GO)
        (fake / "go").chmod(0o755)
        cls.log = cls.tmp / "go.log"
        cls.env = dict(os.environ, PATH=f"{fake}:{os.environ['PATH']}", FAKE_LOG=str(cls.log))
        cls.env.pop("GOOS", None)
        cls.env.pop("GOARCH", None)
        cls.out = cls.tmp / "dist" / "release"
        cls.package()

    @classmethod
    def package(cls):
        cls.release("package", VERSION)

    @classmethod
    def release(cls, *args):
        return subprocess.run(
            [cls.tmp / "scripts" / "release.sh", *args],
            env=cls.env, check=True, capture_output=True, text=True,
        )

    def test_one_sbom_per_archive(self):
        archives = sorted(p.name for p in self.out.glob("*.tar.gz"))
        sboms = sorted(p.name for p in self.out.glob("*.cdx.json"))
        self.assertEqual(archives, [f"savings-preview_{VERSION}_{p}.tar.gz" for p in PLATFORMS])
        self.assertEqual(sboms, [f"savings-preview_{VERSION}_{p}.cdx.json" for p in PLATFORMS])

    def test_sbom_describes_the_archived_binary_for_its_platform(self):
        for p in PLATFORMS:
            name = f"savings-preview_{VERSION}_{p}"
            sbom = json.loads((self.out / f"{name}.cdx.json").read_text())
            os_, arch = p.split("_")
            self.assertEqual((sbom["goos"], sbom["goarch"]), (os_, arch))
            self.assertEqual(sbom["version"], VERSION)
            self.assertTrue(sbom["binary"].endswith(f"/{name}/savings-preview"))
            with tarfile.open(self.out / f"{name}.tar.gz") as tar:
                archived = tar.extractfile(f"{name}/savings-preview").read().decode()
            self.assertEqual(sbom["bytes"] + "\n", archived)

    def test_checksums_cover_archives_and_sboms(self):
        lines = (self.out / "checksums.txt").read_text().splitlines()
        sums = dict(reversed(line.split()) for line in lines)
        files = list(self.out.glob("*.tar.gz")) + list(self.out.glob("*.cdx.json"))
        self.assertEqual(sorted(sums), sorted(f.name for f in files))
        for f in files:
            self.assertEqual(sums[f.name], hashlib.sha256(f.read_bytes()).hexdigest())

    def test_builds_the_generator_pinned_in_tools(self):
        self.assertEqual(
            self.log.read_text().split("\n")[0].split()[-1],
            "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod",
        )
        self.assertTrue((self.out / "install.sh").exists())


class PlatformTest(PackageTest):
    """Packages each platform on its own, as the release jobs do, then writes the checksums."""

    @classmethod
    def package(cls):
        for p in PLATFORMS:
            r = cls.release("package", VERSION, p.replace("_", "/"))
            assert r.stdout == "", r.stdout
        assert not (cls.out / "checksums.txt").exists()
        assert not (cls.out / "install.sh").exists()
        cls.release("checksums", VERSION)

    def test_checksums_list_archives_then_sboms_by_platform(self):
        names = [line.split()[1] for line in (self.out / "checksums.txt").read_text().splitlines()]
        self.assertEqual(
            names,
            [f"savings-preview_{VERSION}_{p}.tar.gz" for p in PLATFORMS]
            + [f"savings-preview_{VERSION}_{p}.cdx.json" for p in PLATFORMS],
        )

    def test_packaging_one_platform_keeps_the_others(self):
        self.addCleanup(self.release, "checksums", VERSION)
        self.release("package", VERSION, "linux/arm64")
        self.assertEqual(len(list(self.out.glob("*.tar.gz"))), 4)
        self.assertTrue((self.out / "checksums.txt").exists())

    def test_checksums_fail_when_an_archive_is_missing(self):
        archive = self.out / f"savings-preview_{VERSION}_darwin_arm64.tar.gz"
        moved = self.tmp / archive.name
        archive.rename(moved)
        self.addCleanup(self.release, "checksums", VERSION)
        self.addCleanup(moved.rename, archive)
        with self.assertRaises(subprocess.CalledProcessError):
            self.release("checksums", VERSION)

    def test_rejects_an_unknown_platform(self):
        with self.assertRaises(subprocess.CalledProcessError) as e:
            self.release("package", VERSION, "windows/amd64")
        self.assertEqual(e.exception.returncode, 2)

if __name__ == "__main__":
    unittest.main()
