"""Tests for scripts/install.sh.

Each test runs the script with a PATH that holds only the tools it needs plus
fake uname, sysctl, gh, curl and sudo commands, which serve a release built in a
temporary directory. Nothing touches the network or the real install
locations.
"""

import fcntl
import hashlib
import io
import json
import os
import pty
import shutil
import subprocess
import tarfile
import tempfile
import termios
import threading
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "install.sh"
REPO = "thoras-ai/savings-preview"
PLATFORMS = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"]

TOOLS = [
    "awk", "basename", "cat", "chmod", "cp", "cut", "head", "mkdir", "mktemp",
    "mv", "rm", "sed", "sh", "tar", "gzip", "tr", "wc",
]

FAKE_UNAME = """\
#!/bin/sh
case $1 in
-s) echo "$FAKE_UNAME_S" ;;
-m) echo "$FAKE_UNAME_M" ;;
esac
"""

FAKE_SYSCTL = """\
#!/bin/sh
[ "$*" = "-n sysctl.proc_translated" ] && [ -n "${FAKE_TRANSLATED:-}" ] || exit 1
echo "$FAKE_TRANSLATED"
"""

FAKE_GH = """\
#!/bin/sh
echo "gh $*" >>"$FAKE_LOG"
case "$1 $2" in
"auth token")
	[ "${FAKE_GH_LOGGED_IN:-}" = 1 ] && echo gho_fake && exit 0
	exit 1
	;;
"release view")
	cat "$FAKE_RELEASES/latest"
	;;
"release download")
	tag=$3
	shift 3
	while [ $# -gt 0 ]; do
		case $1 in
		-R) shift 2 ;;
		-D) dir=$2; shift 2 ;;
		-p) cp "$FAKE_RELEASES/$tag/$2" "$dir/" || exit 1; shift 2 ;;
		*) exit 2 ;;
		esac
	done
	;;
*) exit 2 ;;
esac
"""

FAKE_CURL = """\
#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
out= url= auth=
while [ $# -gt 0 ]; do
	case $1 in
	-o) out=$2; shift 2 ;;
	-H) case $2 in Authorization:*) auth=$2 ;; esac; shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
if [ -n "${FAKE_REQUIRE_TOKEN:-}" ] && [ "$auth" != "Authorization: Bearer $FAKE_REQUIRE_TOKEN" ]; then
	exit 22
fi
api=https://api.github.com/repos/thoras-ai/savings-preview
case $url in
"$api/releases/latest") file=$FAKE_RELEASES/$(cat "$FAKE_RELEASES/latest")/release.json ;;
"$api/releases/tags/"*) file=$FAKE_RELEASES/${url#"$api/releases/tags/"}/release.json ;;
"$api/releases/assets/"*) file=$FAKE_RELEASES/assets/${url#"$api/releases/assets/"} ;;
*) exit 6 ;;
esac
[ -f "$file" ] || exit 22
if [ -n "$out" ]; then cp "$file" "$out"; else cat "$file"; fi
"""

FAKE_SUDO = """\
#!/bin/sh
echo "sudo $*" >>"$FAKE_LOG"
chmod u+w "$FAKE_SUDO_DIR"
"$@"
"""


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def archive(tag, platform):
    name = f"savings-preview_{tag}_{platform}"
    body = f"#!/bin/sh\necho savings-preview {tag} {platform}\n".encode()
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        info = tarfile.TarInfo(f"{name}/savings-preview")
        info.size = len(body)
        info.mode = 0o755
        tar.addfile(info, io.BytesIO(body))
        info = tarfile.TarInfo(f"{name}/README.md")
        info.size = 0
        tar.addfile(info, io.BytesIO(b""))
    return f"{name}.tar.gz", buf.getvalue()


class InstallScriptTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.releases = self.tmp / "releases"
        (self.releases / "assets").mkdir(parents=True)
        self.log = self.tmp / "log"
        self.log.touch()
        self.next_asset = 100
        self.add_release("v0.1.0")
        self.add_release("v0.2.0")
        (self.releases / "latest").write_text("v0.2.0\n")

        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        for tool in TOOLS:
            self.link_tool(tool)
        self.link_tool("sha256sum") or self.link_tool("shasum")
        for name, body in [("uname", FAKE_UNAME), ("sysctl", FAKE_SYSCTL), ("gh", FAKE_GH),
                           ("curl", FAKE_CURL), ("sudo", FAKE_SUDO)]:
            self.write_tool(name, body)
        self.dest = self.tmp / "dest"

    def link_tool(self, name):
        path = shutil.which(name)
        if path is None:
            return False
        (self.bin / name).symlink_to(path)
        return True

    def write_tool(self, name, body):
        p = self.bin / name
        p.write_text(body)
        p.chmod(0o755)

    def add_release(self, tag, pretty=False):
        d = self.releases / tag
        d.mkdir()
        assets, sums = [], []
        for platform in PLATFORMS:
            name, data = archive(tag, platform)
            (d / name).write_bytes(data)
            sums.append(f"{sha256(data)}  {name}\n")
        (d / "checksums.txt").write_text("".join(sums))
        for name in sorted(p.name for p in d.iterdir()):
            self.next_asset += 1
            shutil.copy(d / name, self.releases / "assets" / str(self.next_asset))
            assets.append({
                "url": f"https://api.github.com/repos/{REPO}/releases/assets/{self.next_asset}",
                "id": self.next_asset,
                "name": name,
                "label": "",
                "uploader": {"login": "github-actions[bot]", "url": "https://api.github.com/users/x"},
                "browser_download_url": f"https://github.com/{REPO}/releases/download/{tag}/{name}",
            })
        release = {
            "url": f"https://api.github.com/repos/{REPO}/releases/1",
            "tag_name": tag,
            "name": tag,
            "body": 'Fixes, "name": "decoy", and more',
            "assets": assets,
        }
        (d / "release.json").write_text(json.dumps(release, indent=2 if pretty else None,
                                                   separators=None if pretty else (",", ":")))

    def run_script(self, uname=("Darwin", "arm64"), gh=False, extra_path=(), new_session=True, **env):
        e = {
            "PATH": os.pathsep.join([*map(str, extra_path), str(self.bin)]),
            "HOME": str(self.home),
            "TMPDIR": str(self.tmp),
            "FAKE_UNAME_S": uname[0],
            "FAKE_UNAME_M": uname[1],
            "FAKE_RELEASES": str(self.releases),
            "FAKE_LOG": str(self.log),
            "FAKE_GH_LOGGED_IN": "1" if gh else "",
            "INSTALL_DIR": str(self.dest),
        }
        e.update(env)
        e = {k: v for k, v in e.items() if v is not None}
        return subprocess.run(["sh", str(SCRIPT)], env=e, capture_output=True, text=True,
                              stdin=subprocess.DEVNULL, start_new_session=new_session, timeout=60)

    def installed(self, dest=None):
        p = (dest or self.dest) / "savings-preview"
        if not p.exists():
            return None
        return subprocess.run([str(p)], capture_output=True, text=True, check=True).stdout.strip()

    def assertInstalled(self, r, want, dest=None):
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.installed(dest), want)

    def assertFailed(self, r, message):
        self.assertNotEqual(r.returncode, 0)
        self.assertIn(message, r.stderr)
        self.assertIsNone(self.installed())

    def calls(self, prefix):
        return [l for l in self.log.read_text().splitlines() if l.startswith(prefix)]

    # Platform detection

    def test_platforms(self):
        cases = [
            (("Darwin", "arm64"), "darwin_arm64"),
            (("Darwin", "x86_64"), "darwin_amd64"),
            (("Linux", "x86_64"), "linux_amd64"),
            (("Linux", "amd64"), "linux_amd64"),
            (("Linux", "aarch64"), "linux_arm64"),
            (("Linux", "arm64"), "linux_arm64"),
        ]
        for uname, platform in cases:
            with self.subTest(uname=uname):
                r = self.run_script(uname=uname)
                self.assertInstalled(r, f"savings-preview v0.2.0 {platform}")

    def test_rosetta_shell_gets_the_arm64_binary(self):
        r = self.run_script(uname=("Darwin", "x86_64"), FAKE_TRANSLATED="1")
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")
        r = self.run_script(uname=("Darwin", "x86_64"), FAKE_TRANSLATED="0")
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_amd64")

    def test_unsupported_platforms(self):
        cases = [
            (("FreeBSD", "amd64"), "unsupported operating system: FreeBSD"),
            (("MINGW64_NT-10.0", "x86_64"), "unsupported operating system: MINGW64_NT-10.0"),
            (("Linux", "armv7l"), "unsupported architecture: armv7l"),
            (("Linux", "i686"), "unsupported architecture: i686"),
        ]
        for uname, message in cases:
            with self.subTest(uname=uname):
                r = self.run_script(uname=uname)
                self.assertFailed(r, message)
                self.assertEqual(self.calls("curl") + self.calls("gh release"), [])

    # Checksums

    def corrupt(self, tag, platform):
        name, _ = archive(tag, platform)
        for p in [self.releases / tag / name, *self.asset_files(name, tag)]:
            p.write_bytes(p.read_bytes() + b"\0")

    def asset_files(self, name, tag):
        release = json.loads((self.releases / tag / "release.json").read_text())
        return [self.releases / "assets" / str(a["id"]) for a in release["assets"] if a["name"] == name]

    def set_checksums(self, tag, text):
        for p in [self.releases / tag / "checksums.txt", *self.asset_files("checksums.txt", tag)]:
            p.write_text(text)

    def test_checksum_mismatch_installs_nothing(self):
        self.corrupt("v0.2.0", "darwin_arm64")
        for gh in (True, False):
            with self.subTest(gh=gh):
                r = self.run_script(gh=gh)
                self.assertFailed(r, "checksum mismatch for savings-preview_v0.2.0_darwin_arm64.tar.gz")

    def test_checksum_mismatch_keeps_the_installed_binary(self):
        self.dest.mkdir()
        (self.dest / "savings-preview").write_text("#!/bin/sh\necho old\n")
        (self.dest / "savings-preview").chmod(0o755)
        self.corrupt("v0.2.0", "darwin_arm64")
        r = self.run_script()
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(self.installed(), "old")

    def test_missing_checksum_entry(self):
        sums = (self.releases / "v0.2.0" / "checksums.txt").read_text().splitlines(keepends=True)
        self.set_checksums("v0.2.0", "".join(l for l in sums if "darwin_arm64" not in l))
        self.assertFailed(self.run_script(), "checksums.txt has no entry for savings-preview_v0.2.0_darwin_arm64.tar.gz")

    def test_duplicate_checksum_entry(self):
        sums = (self.releases / "v0.2.0" / "checksums.txt").read_text()
        name, _ = archive("v0.2.0", "darwin_arm64")
        self.set_checksums("v0.2.0", sums + f"{'0' * 64}  {name}\n")
        self.assertFailed(self.run_script(), "more than one entry")

    def test_empty_checksums(self):
        self.set_checksums("v0.2.0", "")
        self.assertFailed(self.run_script(), "checksums.txt has no entry")

    def test_checksum_of_another_file_does_not_match(self):
        sums = (self.releases / "v0.2.0" / "checksums.txt").read_text()
        name, _ = archive("v0.2.0", "darwin_arm64")
        other, _ = archive("v0.2.0", "linux_arm64")
        swapped = {l.split()[1]: l.split()[0] for l in sums.splitlines()}
        swapped[name], swapped[other] = swapped[other], swapped[name]
        self.set_checksums("v0.2.0", "".join(f"{h}  {n}\n" for n, h in swapped.items()))
        self.assertFailed(self.run_script(), "checksum mismatch")

    def test_name_prefix_does_not_match(self):
        sums = (self.releases / "v0.2.0" / "checksums.txt").read_text()
        name, _ = archive("v0.2.0", "darwin_arm64")
        self.set_checksums("v0.2.0", sums.replace(f"  {name}", f"  x{name}"))
        self.assertFailed(self.run_script(), "checksums.txt has no entry")

    def test_binary_mode_checksum_lines(self):
        sums = (self.releases / "v0.2.0" / "checksums.txt").read_text()
        self.set_checksums("v0.2.0", sums.replace("  ", " *"))
        self.assertInstalled(self.run_script(), "savings-preview v0.2.0 darwin_arm64")

    def use_hash_tool(self, tool):
        for t in ("sha256sum", "shasum"):
            (self.bin / t).unlink(missing_ok=True)
        self.link_tool(tool)

    def test_each_hash_tool(self):
        tools = [t for t in ("sha256sum", "shasum") if shutil.which(t)]
        for tool in tools:
            with self.subTest(tool=tool):
                self.use_hash_tool(tool)
                self.assertInstalled(self.run_script(), "savings-preview v0.2.0 darwin_arm64")
                (self.dest / "savings-preview").unlink()
        self.corrupt("v0.2.0", "darwin_arm64")
        for tool in tools:
            with self.subTest(tool=tool, corrupt=True):
                self.use_hash_tool(tool)
                self.assertFailed(self.run_script(), "checksum mismatch")

    def test_no_hash_tool(self):
        for t in ("sha256sum", "shasum"):
            (self.bin / t).unlink(missing_ok=True)
        self.assertFailed(self.run_script(), "neither sha256sum nor shasum is installed")

    # Downloads

    def test_version_selects_the_release(self):
        for version in ("v0.1.0", "0.1.0"):
            with self.subTest(version=version):
                r = self.run_script(VERSION=version)
                self.assertInstalled(r, "savings-preview v0.1.0 darwin_arm64")
                self.assertIn("Installed savings-preview v0.1.0", r.stderr)

    def test_unknown_version(self):
        for gh in (True, False):
            with self.subTest(gh=gh):
                self.assertFailed(self.run_script(gh=gh, VERSION="v9.9.9"), "v9.9.9")

    def test_gh_is_used_when_logged_in(self):
        r = self.run_script(gh=True)
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")
        self.assertEqual(self.calls("curl"), [])
        self.assertIn(f"gh release download v0.2.0 -R {REPO} -D", self.log.read_text())

    def test_curl_with_token(self):
        for var in ("GITHUB_TOKEN", "GH_TOKEN"):
            with self.subTest(var=var):
                r = self.run_script(FAKE_REQUIRE_TOKEN="t0k", **{var: "t0k"})
                self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")
                self.assertNotIn("trying without credentials", r.stderr)

    def test_github_token_wins_over_gh_token(self):
        r = self.run_script(FAKE_REQUIRE_TOKEN="a", GITHUB_TOKEN="a", GH_TOKEN="b")
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")

    def test_curl_without_token(self):
        r = self.run_script()
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")
        self.assertIn("trying without credentials", r.stderr)
        self.assertFalse(any("Authorization" in c for c in self.calls("curl")))

    def test_private_repository_without_credentials(self):
        r = self.run_script(FAKE_REQUIRE_TOKEN="t0k")
        self.assertFailed(r, "log in with gh, or set GITHUB_TOKEN")

    def test_gh_not_logged_in_falls_back_to_curl(self):
        r = self.run_script(gh=False, GITHUB_TOKEN="t0k", FAKE_REQUIRE_TOKEN="t0k")
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64")
        self.assertEqual(self.calls("gh release"), [])

    def test_pretty_printed_release_json(self):
        shutil.rmtree(self.releases / "v0.2.0")
        self.add_release("v0.2.0", pretty=True)
        self.assertInstalled(self.run_script(), "savings-preview v0.2.0 darwin_arm64")

    def test_missing_asset(self):
        name, _ = archive("v0.2.0", "darwin_arm64")
        p = self.releases / "v0.2.0" / "release.json"
        release = json.loads(p.read_text())
        release["assets"] = [a for a in release["assets"] if a["name"] != name]
        p.write_text(json.dumps(release))
        self.assertFailed(self.run_script(), f"release v0.2.0 of {REPO} has no asset named {name}")

    # Install location

    def test_default_is_local_bin_when_on_path(self):
        local = self.home / ".local" / "bin"
        r = self.run_script(INSTALL_DIR=None, extra_path=[local])
        self.assertInstalled(r, "savings-preview v0.2.0 darwin_arm64", dest=local)
        self.assertIn("Next steps:\n  savings-preview --version", r.stderr)

    def test_install_dir_not_on_path(self):
        r = self.run_script()
        self.assertIn(f'export PATH="{self.dest}:$PATH"', r.stderr)
        self.assertIn(f"  {self.dest}/savings-preview --version", r.stderr)

    def test_replaces_an_existing_binary(self):
        self.assertInstalled(self.run_script(VERSION="v0.1.0"), "savings-preview v0.1.0 darwin_arm64")
        self.assertInstalled(self.run_script(), "savings-preview v0.2.0 darwin_arm64")
        self.assertEqual(sorted(p.name for p in self.dest.iterdir()), ["savings-preview"])

    def read_only_dest(self):
        if os.geteuid() == 0:
            self.skipTest("root can write to read-only directories")
        self.dest.mkdir()
        self.dest.chmod(0o555)
        self.addCleanup(self.dest.chmod, 0o755)

    def test_unwritable_dir_without_a_terminal(self):
        self.read_only_dest()
        r = self.run_script(FAKE_SUDO_DIR=str(self.dest))
        self.assertFailed(r, "there is no terminal to ask about sudo")
        self.assertEqual(self.calls("sudo"), [])

    def run_with_terminal(self, answer, **env):
        master, slave = pty.openpty()

        def controlling_tty():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)

        e = {
            "PATH": str(self.bin), "HOME": str(self.home), "TMPDIR": str(self.tmp),
            "FAKE_UNAME_S": "Linux", "FAKE_UNAME_M": "x86_64", "FAKE_RELEASES": str(self.releases),
            "FAKE_LOG": str(self.log), "INSTALL_DIR": str(self.dest), "FAKE_SUDO_DIR": str(self.dest),
            **env,
        }
        p = subprocess.Popen(["sh", str(SCRIPT)], env=e, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                             preexec_fn=controlling_tty, pass_fds=(slave,))
        os.close(slave)
        prompt = b""
        deadline = time.monotonic() + 30
        while b"[y/N]" not in prompt and time.monotonic() < deadline:
            try:
                chunk = os.read(master, 1024)
            except OSError:
                break
            if not chunk:
                break
            prompt += chunk
        os.write(master, answer.encode() + b"\n")

        # The script cannot exit while terminal output, such as the echoed
        # answer, is left unread.
        def drain():
            try:
                while os.read(master, 1024):
                    pass
            except OSError:
                pass

        reader = threading.Thread(target=drain, daemon=True)
        reader.start()
        out, err = p.communicate(timeout=30)
        reader.join(timeout=5)
        os.close(master)
        return p.returncode, prompt.decode(), err

    def test_asks_before_sudo_and_stops_on_no(self):
        self.read_only_dest()
        code, prompt, err = self.run_with_terminal("n")
        self.assertIn(f"{self.dest} is not writable. Install with sudo? [y/N]", prompt)
        self.assertNotEqual(code, 0)
        self.assertIn("not installed", err)
        self.assertEqual(self.calls("sudo"), [])
        self.assertIsNone(self.installed())

    def test_asks_before_sudo_and_uses_it_on_yes(self):
        self.read_only_dest()
        code, prompt, err = self.run_with_terminal("y")
        self.assertIn("Install with sudo? [y/N]", prompt)
        self.assertEqual(code, 0, err)
        self.assertTrue(self.calls("sudo"))
        self.assertEqual(self.installed(), "savings-preview v0.2.0 linux_amd64")

    def test_script_is_posix_sh(self):
        for shell in ("dash", "bash"):
            path = shutil.which(shell)
            if path is None:
                continue
            with self.subTest(shell=shell):
                r = subprocess.run([path, "-n", str(SCRIPT)], capture_output=True, text=True)
                self.assertEqual(r.returncode, 0, r.stderr)


if __name__ == "__main__":
    unittest.main()
