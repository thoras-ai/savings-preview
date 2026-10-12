#!/usr/bin/env bash
# Commit a release's Homebrew formula to the tap repository.
#
#   scripts/update_tap.sh v0.3.0 dist/release/savings-preview.rb
#
# Clones the tap, writes Formula/savings-preview.rb on its default branch and
# pushes. TAP_URL names the tap (default git@github.com:thoras-ai/homebrew-tap.git);
# git needs push access to it, which the release workflow gives with a deploy
# key through GIT_SSH_COMMAND. Does nothing when the tap already has the same
# formula. Commits as the caller's git identity.
set -euo pipefail

TAP_URL=${TAP_URL:-git@github.com:thoras-ai/homebrew-tap.git}
FILE=Formula/savings-preview.rb

die() { echo "update_tap.sh: $*" >&2; exit 1; }

[[ $# -eq 2 ]] || die "usage: scripts/update_tap.sh TAG FORMULA"
tag=$1 formula=$2
[[ -f $formula ]] || die "$formula does not exist"
# Formulas of v0.2.3 and earlier download through the GitHub API with a token,
# from when the repository was private. Never put one of those in the tap.
if grep -q 'api.github.com' "$formula"; then
	die "$formula downloads through api.github.com; render it again with scripts/homebrew_formula.py"
fi

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
git clone -q --depth 1 "$TAP_URL" "$dir" || die "cannot clone $TAP_URL; check that it exists and that the key can read it"

mkdir -p "$dir/Formula"
cp "$formula" "$dir/$FILE"
git -C "$dir" add "$FILE"
if git -C "$dir" diff --cached --quiet; then
	echo "$TAP_URL already has the formula for $tag"
	exit 0
fi
git -C "$dir" commit -q -m "savings-preview ${tag#v}"
git -C "$dir" push -q origin HEAD || die "cannot push to $TAP_URL; check that the key can write to it"
echo "pushed $(git -C "$dir" rev-parse --short HEAD) to $TAP_URL"
