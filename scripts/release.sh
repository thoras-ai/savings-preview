#!/usr/bin/env bash
# Build and package the savings-preview release binaries.
#
#   scripts/release.sh build VERSION     binaries in dist/release/bin/
#   scripts/release.sh package VERSION   archives and checksums.txt in dist/release/
#
# The two steps are separate so the binaries can be signed in between.
set -euo pipefail

PLATFORMS="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"
OUT=dist/release

[[ $# -eq 2 ]] || { echo "usage: $0 build|package VERSION" >&2; exit 2; }
step=$1 version=$2

cd "$(dirname "$0")/.."

case $step in
build)
	rm -rf "$OUT"
	mkdir -p "$OUT/bin"
	for p in $PLATFORMS; do
		os=${p%/*} arch=${p#*/}
		echo "$OUT/bin/savings-preview-$os-$arch"
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$version" \
			-o "$OUT/bin/savings-preview-$os-$arch" ./cmd/savings-preview
	done
	;;
package)
	rm -f "$OUT"/*.tar.gz "$OUT/checksums.txt"
	stage=$(mktemp -d)
	trap 'rm -rf "$stage"' EXIT
	for p in $PLATFORMS; do
		os=${p%/*} arch=${p#*/}
		name=savings-preview_${version}_${os}_${arch}
		mkdir "$stage/$name"
		cp "$OUT/bin/savings-preview-$os-$arch" "$stage/$name/savings-preview"
		# Artifact uploads between jobs drop the executable bit.
		chmod 755 "$stage/$name/savings-preview"
		cp README.md "$stage/$name/"
		tar -C "$stage" -czf "$OUT/$name.tar.gz" "$name"
	done
	(cd "$OUT" && shasum -a 256 -- *.tar.gz > checksums.txt)
	cat "$OUT/checksums.txt"
	;;
*)
	echo "usage: $0 build|package VERSION" >&2
	exit 2
	;;
esac
