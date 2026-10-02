#!/bin/sh
# Install the savings-preview CLI from a GitHub release.
#
#   gh release download -R thoras-ai/savings-preview -p install.sh -O - | sh
#
# Environment:
#   VERSION       release tag to install, such as v0.2.0 (default: the latest release)
#   INSTALL_DIR   directory to install into (default: ~/.local/bin when it is on
#                 PATH, /usr/local/bin otherwise)
#   GITHUB_TOKEN  token for downloading without gh (GH_TOKEN is read too)
#
# The repository is private, so downloads need GitHub credentials. With gh
# installed and logged in, the script downloads through gh. Otherwise it uses
# curl and the GitHub API, with GITHUB_TOKEN or GH_TOKEN when set.
#
# The archive is checked against the release's checksums.txt before anything is
# installed. A missing or wrong checksum stops the install.
set -eu

REPO=thoras-ai/savings-preview
API=https://api.github.com
BINARY=savings-preview

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case $os in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported operating system: $os (supported: macOS and Linux)" ;;
	esac
	case $arch in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch (supported: amd64 and arm64)" ;;
	esac
	# A shell running under Rosetta reports x86_64 on Apple silicon.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
	echo "${os}_${arch}"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "neither sha256sum nor shasum is installed; cannot verify the download"
	fi
}

# verify FILE CHECKSUMS fails unless CHECKSUMS lists FILE's name with FILE's
# SHA-256.
verify() {
	name=$(basename "$1")
	want=$(awk -v n="$name" '$2 == n || $2 == "*" n { print $1 }' "$2")
	[ -n "$want" ] || die "checksums.txt has no entry for $name"
	[ "$(printf '%s\n' "$want" | wc -l | tr -d ' ')" = 1 ] ||
		die "checksums.txt has more than one entry for $name"
	got=$(sha256 "$1")
	[ "$got" = "$want" ] ||
		die "checksum mismatch for $name: expected $want, got $got. Nothing was installed."
}

use_gh() {
	command -v gh >/dev/null 2>&1 && gh auth token >/dev/null 2>&1
}

token() {
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		echo "$GITHUB_TOKEN"
	else
		echo "${GH_TOKEN:-}"
	fi
}

# api_get URL [CURL ARGS...] fetches from the GitHub API with the token, if any.
api_get() {
	url=$1
	shift
	t=$(token)
	if [ -n "$t" ]; then
		curl -fsSL -H "Authorization: Bearer $t" -H "X-GitHub-Api-Version: 2022-11-28" "$@" "$url"
	else
		curl -fsSL -H "X-GitHub-Api-Version: 2022-11-28" "$@" "$url"
	fi
}

# release_json TAG FILE writes the release's API JSON to FILE, one field per
# line.
release_json() {
	if [ -n "$1" ]; then
		path=releases/tags/$1
	else
		path=releases/latest
	fi
	api_get "$API/repos/$REPO/$path" -H "Accept: application/vnd.github+json" -o "$2.raw" || return 1
	tr ',{}[]' '\n\n\n\n\n' <"$2.raw" | sed 's/^[[:space:]]*//' >"$2"
}

# asset_url JSON NAME prints the API URL of the asset called NAME. In each
# asset object the API lists "url" before "name".
asset_url() {
	awk -v n="\"name\":\"$2\"" '
		{ line = $0; gsub(/"[[:space:]]*:[[:space:]]*"/, "\":\"", line) }
		line ~ /^"url":"[^"]*\/releases\/assets\/[0-9]+"$/ { url = line }
		line == n { sub(/^"url":"/, "", url); sub(/"$/, "", url); print url; exit }
	' "$1"
}

download() {
	tag=$1 archive=$2 dir=$3
	if use_gh; then
		if [ -z "$tag" ]; then
			tag=$(gh release view -R "$REPO" --json tagName --jq .tagName) ||
				die "gh could not read the latest release of $REPO. Check that the account gh uses can read it (gh auth status), or set GH_TOKEN to a token that can."
		fi
		archive=$(printf '%s' "$archive" | sed "s/@TAG@/$tag/")
		gh release download "$tag" -R "$REPO" -D "$dir" -p "$archive" -p checksums.txt >&2 ||
			die "gh could not download $archive from release $tag of $REPO. Check that the account gh uses can read it (gh auth status), or set GH_TOKEN to a token that can."
	else
		command -v curl >/dev/null 2>&1 ||
			die "install gh (https://cli.github.com) or curl to download releases"
		[ -n "$(token)" ] ||
			say "gh is not installed or not logged in, and GITHUB_TOKEN is not set; trying without credentials."
		release_json "$tag" "$dir/release.json" ||
			die "could not read release ${tag:-latest} of $REPO from the GitHub API. The repository is private: log in with gh, or set GITHUB_TOKEN to a token that can read it."
		if [ -z "$tag" ]; then
			tag=$(sed -n 's/^"tag_name"[[:space:]]*:[[:space:]]*"\(.*\)"$/\1/p' "$dir/release.json" | head -n 1)
			[ -n "$tag" ] || die "the latest release of $REPO has no tag name"
		fi
		archive=$(printf '%s' "$archive" | sed "s/@TAG@/$tag/")
		for name in "$archive" checksums.txt; do
			url=$(asset_url "$dir/release.json" "$name")
			[ -n "$url" ] || die "release $tag of $REPO has no asset named $name"
			api_get "$url" -H "Accept: application/octet-stream" -o "$dir/$name" ||
				die "could not download $name from release $tag of $REPO"
		done
	fi
	echo "$tag"
}

install_dir() {
	if [ -n "${INSTALL_DIR:-}" ]; then
		echo "$INSTALL_DIR"
		return
	fi
	case ":$PATH:" in
	*":$HOME/.local/bin:"*) echo "$HOME/.local/bin" ;;
	*) echo /usr/local/bin ;;
	esac
}

# put SRC DIR installs SRC as DIR/savings-preview, asking before using sudo.
put() {
	src=$1 dir=$2
	if mkdir -p "$dir" 2>/dev/null && [ -w "$dir" ]; then
		cp "$src" "$dir/$BINARY.tmp.$$"
		chmod 755 "$dir/$BINARY.tmp.$$"
		mv -f "$dir/$BINARY.tmp.$$" "$dir/$BINARY"
		return
	fi
	command -v sudo >/dev/null 2>&1 ||
		die "$dir is not writable. Set INSTALL_DIR to a directory you can write to, such as \$HOME/.local/bin."
	# stdin may be the script itself when it is piped to sh, so ask on the terminal.
	# A failed redirection on a special builtin exits some shells, so try it in
	# a subshell.
	if ! (: </dev/tty) 2>/dev/null; then
		die "$dir is not writable and there is no terminal to ask about sudo. Set INSTALL_DIR to a directory you can write to, such as \$HOME/.local/bin."
	fi
	printf '%s is not writable. Install with sudo? [y/N] ' "$dir" >/dev/tty
	read -r answer </dev/tty || answer=
	case $answer in
	y | Y | yes | YES) ;;
	*) die "not installed. Set INSTALL_DIR to a directory you can write to, such as \$HOME/.local/bin." ;;
	esac
	sudo mkdir -p "$dir"
	sudo cp "$src" "$dir/$BINARY.tmp.$$"
	sudo chmod 755 "$dir/$BINARY.tmp.$$"
	sudo mv -f "$dir/$BINARY.tmp.$$" "$dir/$BINARY"
}

main() {
	tag=${VERSION:-}
	case $tag in
	"" | v*) ;;
	*) tag=v$tag ;;
	esac
	plat=$(platform)

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t savings-preview)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	say "Downloading $BINARY ${tag:-(latest release)} for $plat"
	tag=$(download "$tag" "${BINARY}_@TAG@_${plat}.tar.gz" "$tmp")
	archive=$tmp/${BINARY}_${tag}_${plat}.tar.gz

	verify "$archive" "$tmp/checksums.txt"
	say "Checksum verified"

	mkdir "$tmp/x"
	tar -xzf "$archive" -C "$tmp/x"
	bin=$tmp/x/${BINARY}_${tag}_${plat}/$BINARY
	[ -f "$bin" ] || die "$(basename "$archive") does not contain ${BINARY}_${tag}_${plat}/$BINARY"

	dir=$(install_dir)
	put "$bin" "$dir"
	say "Installed $BINARY $tag to $dir/$BINARY"

	case ":$PATH:" in
	*":$dir:"*) run=$BINARY ;;
	*)
		run=$dir/$BINARY
		say ""
		say "$dir is not on PATH. Add it with:"
		say "  export PATH=\"$dir:\$PATH\""
		;;
	esac
	say ""
	say "Next steps:"
	say "  $run --version"
	say "  $run --context <kube-context>    read the cluster's usage history and open the report"
	say "  $run --help"
}

main "$@"
