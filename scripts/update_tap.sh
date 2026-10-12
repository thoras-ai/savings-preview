#!/usr/bin/env bash
# Commit a release's Homebrew formula to the tap repository.
#
#   GH_TOKEN=<token> scripts/update_tap.sh v0.3.0 dist/release/savings-preview.rb
#
# Writes Formula/savings-preview.rb on the tap's default branch through the
# GitHub contents API, so no clone is needed. The token needs write access to
# the tap's contents. TAP names the tap repository (default
# thoras-ai/homebrew-tap). Does nothing when the formula there is already the
# same.
set -euo pipefail

TAP=${TAP:-thoras-ai/homebrew-tap}
FILE=Formula/savings-preview.rb

die() { echo "update_tap.sh: $*" >&2; exit 1; }

[[ $# -eq 2 ]] || die "usage: scripts/update_tap.sh TAG FORMULA"
tag=$1 formula=$2
[[ -f $formula ]] || die "$formula does not exist"

content=$(base64 <"$formula" | tr -d '\n')
args=(-X PUT "repos/$TAP/contents/$FILE" -f message="savings-preview ${tag#v}" -f content="$content")

if current=$(gh api "repos/$TAP/contents/$FILE" --jq '.sha + " " + .content' 2>/dev/null); then
	sha=${current%% *}
	if [[ $(tr -d '\n' <<<"${current#* }") == "$content" ]]; then
		echo "$TAP already has the formula for $tag"
		exit 0
	fi
	args+=(-f sha="$sha")
elif ! gh api "repos/$TAP" --jq .full_name >/dev/null; then
	die "cannot read $TAP; check that it exists and that the token can write to it"
fi

gh api "${args[@]}" --jq .commit.html_url
