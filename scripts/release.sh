#!/usr/bin/env bash
# Build and package the savings-preview release binaries.
#
#   scripts/release.sh build VERSION [OS/ARCH...]     binaries in dist/release/bin/
#   scripts/release.sh package VERSION [OS/ARCH...]   archives, SBOMs, install.sh and checksums.txt in dist/release/
#   scripts/release.sh checksums VERSION              install.sh and checksums.txt for the archives and SBOMs in dist/release/
#
# The build and package steps are separate so the binaries can be signed in
# between. Given platforms, build and package handle only those, and package
# writes neither install.sh nor checksums.txt, so each platform can be built in
# its own job and the checksums step run once over all of them.
set -euo pipefail

PLATFORMS="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64"
OUT=dist/release

usage() {
	echo "usage: $0 build|package VERSION [OS/ARCH...]" >&2
	echo "       $0 checksums VERSION" >&2
	exit 2
}

[[ $# -ge 2 ]] || usage
step=$1 version=$2
shift 2
case $step in
build | package) ;;
checksums) [[ $# -eq 0 ]] || usage ;;
*) usage ;;
esac
platforms=${*:-$PLATFORMS}
for p in $platforms; do
	[[ " $PLATFORMS " == *" $p "* ]] || { echo "$0: unknown platform $p (one of: $PLATFORMS)" >&2; exit 2; }
done

cd "$(dirname "$0")/.."

checksums() {
	local p files=()
	for p in $PLATFORMS; do files+=("savings-preview_${version}_${p%/*}_${p#*/}.tar.gz"); done
	for p in $PLATFORMS; do files+=("savings-preview_${version}_${p%/*}_${p#*/}.cdx.json"); done
	rm -f "$OUT/install.sh" "$OUT/checksums.txt"
	(cd "$OUT" && shasum -a 256 -- "${files[@]}" > checksums.txt) || { rm -f "$OUT/checksums.txt"; exit 1; }
	cp scripts/install.sh "$OUT/install.sh"
	cat "$OUT/checksums.txt"
}

case $step in
build)
	if [[ $# -eq 0 ]]; then rm -rf "$OUT"; fi
	mkdir -p "$OUT/bin"
	for p in $platforms; do
		os=${p%/*} arch=${p#*/}
		echo "$OUT/bin/savings-preview-$os-$arch"
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$version" \
			-o "$OUT/bin/savings-preview-$os-$arch" ./cmd/savings-preview
	done
	;;
package)
	if [[ $# -eq 0 ]]; then
		rm -f "$OUT"/*.tar.gz "$OUT"/*.cdx.json "$OUT/install.sh" "$OUT/checksums.txt"
	fi
	stage=$(mktemp -d)
	trap 'rm -rf "$stage"' EXIT
	go build -modfile=tools/go.mod -o "$stage/cyclonedx-gomod" github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod
	for p in $platforms; do
		os=${p%/*} arch=${p#*/}
		name=savings-preview_${version}_${os}_${arch}
		rm -f "$OUT/$name.tar.gz" "$OUT/$name.cdx.json"
		mkdir "$stage/$name"
		cp "$OUT/bin/savings-preview-$os-$arch" "$stage/$name/savings-preview"
		# Artifact uploads between jobs drop the executable bit.
		chmod 755 "$stage/$name/savings-preview"
		cp README.md "$stage/$name/"
		tar -C "$stage" -czf "$OUT/$name.tar.gz" "$name"
		# The SBOM lists the modules and Go version recorded in the binary.
		# GOOS and GOARCH set the platform in its package URLs, which
		# otherwise name the machine running the release.
		GOOS=$os GOARCH=$arch "$stage/cyclonedx-gomod" bin -json -std -version "$version" \
			-output "$OUT/$name.cdx.json" "$stage/$name/savings-preview"
	done
	if [[ $# -eq 0 ]]; then checksums; fi
	;;
checksums)
	checksums
	;;
esac
