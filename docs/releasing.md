# Releasing

Pushing a tag that starts with `v` runs `.github/workflows/release.yml`:

1. **checks** runs the same jobs as CI (`.github/workflows/ci.yml`): `go vet`, `go test -race`, the Python and Node tests, and the fixture check against the contract.
2. **build** fails unless the tagged commit is on `main`. It then runs `scripts/release.sh build <tag>`, which cross-compiles `darwin/amd64`, `darwin/arm64`, `linux/amd64` and `linux/arm64` binaries with `CGO_ENABLED=0` and the tag passed as `-X main.version=<tag>`. It uploads them as the `binaries` workflow artifact. CI and the release use the latest Go 1.26 patch release, not the minimum version in `go.mod`; change `go-version` in both workflows when moving to a new minor version.
3. **publish** downloads the binaries, runs `scripts/release.sh package <tag>`, and creates a GitHub release for the tag with generated notes. A tag with a hyphen, such as `v1.2.0-rc.1`, makes a prerelease.

The `-X main.version` flag sets a `version` variable in `cmd/savings-preview`. The linker ignores it when the variable does not exist, so a binary built from a commit without that variable reports no version.

The release assets are one `savings-preview_<tag>_<os>_<arch>.tar.gz` per platform, holding the `savings-preview` binary and `README.md`, and `checksums.txt` with their SHA-256 sums.

## Cutting a release

Tag a commit on `main` that passed CI and push the tag:

    git tag -a v0.1.0 -m v0.1.0
    git push origin v0.1.0

Follow the run under the repository's Actions tab. If it fails before **publish**, delete the tag, fix the cause and tag again. If **publish** fails after the release exists, delete the release and the tag before retrying.

## Building the release locally

    scripts/release.sh build v0.1.0-local
    scripts/release.sh package v0.1.0-local

The output lands in `dist/release/`. Check a download with `shasum -a 256 -c checksums.txt` in the directory that holds the archives.

## Adding signing or notarization

The binaries pass between the **build** and **publish** jobs as the `binaries` artifact, and `package` archives whatever is in `dist/release/bin/`. To sign them, add a job that runs after **build**, downloads `binaries`, signs the files in place, and uploads them again under the same name with `overwrite: true`. Then make **publish** depend on that job instead of **build**.

macOS code signing and notarization need a `macos` runner with `codesign` and `xcrun notarytool`, the Developer ID certificate, and an App Store Connect API key, all stored as repository secrets and read only by the signing job. A bare binary cannot have a notarization ticket stapled to it, so Gatekeeper checks it online the first time it runs.
