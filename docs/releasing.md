# Releasing

Pushing a tag that starts with `v` runs `.github/workflows/release.yml`:

1. **preflight** fails unless the tagged commit is on `main` and `CHANGELOG.md` has a section for the tag. It then looks up the CI runs on `main` for the tagged commit with `scripts/main_ci_result.py`: a push to `main`, the nightly run or a manual run, all of which run every check. When one is still running it waits for it, up to 15 minutes.
2. **checks** runs the CI workflow (`.github/workflows/ci.yml`): `go vet`, `go test`, `go test -race`, `govulncheck`, the Python and Node tests, and the fixture check against the contract. It runs only when CI on `main` did not pass for the tagged commit: it failed, was cancelled, never ran, or was still running after the wait. Pull requests run the same jobs except `go test -race`, which runs on pushes to `main`, nightly, on demand and here, so a release never skips it.
3. **build** runs once per platform, `darwin/amd64`, `darwin/arm64`, `linux/amd64` and `linux/arm64`, at the same time as **preflight**. Each job restores the Go build cache that CI saves on `main`, without ever saving one. The CI `vet` job compiles the release binaries into that cache with `scripts/release.sh build`, so a release build mostly only links. The job runs `scripts/release.sh build <tag> <os>/<arch>`, which cross-compiles the binary with `CGO_ENABLED=0` and the tag passed as `-X main.version=<tag>`. It then runs `scripts/release.sh package <tag> <os>/<arch>`, which writes the platform's archive and its SBOM, and uploads both as the `release-<os>-<arch>` workflow artifact. CI and the release use the latest Go 1.26 patch release, not the minimum version in `go.mod`; change `go-version` in both workflows when moving to a new minor version.
4. **govulncheck** runs `govulncheck ./...` on the tagged commit with the vulnerability database downloaded from `vuln.go.dev` at that moment. It runs alongside the builds, whether or not CI on `main` is reused.
5. **publish** runs once **build** passed for every platform, **govulncheck** passed, and either CI passed on `main` or **checks** passed. It downloads the archives and SBOMs, runs `scripts/release.sh checksums <tag>`, which writes `checksums.txt` and copies `install.sh`, and creates a draft GitHub release for the tag. `scripts/release_notes.py` takes the release notes from the tag's `CHANGELOG.md` section, rewriting relative links to point at the tag; a prerelease tag such as `v1.2.0-rc.1` uses its own section when there is one and the `v1.2.0` section otherwise. It then fills in the Homebrew formula from the draft's asset URLs and `checksums.txt` with `scripts/homebrew_formula.py`, attaches it, and publishes the release. A tag with a hyphen, such as `v1.2.0-rc.1`, makes a prerelease.

A tag pushed after CI passed on `main` skips **checks**, and the four builds and **govulncheck** run side by side while **preflight** looks up the CI result.

The `-X main.version` flag sets a `version` variable in `cmd/savings-preview`. The linker ignores it when the variable does not exist, so a binary built from a commit without that variable reports no version.

The release assets are:

- one `savings-preview_<tag>_<os>_<arch>.tar.gz` per platform, holding the `savings-preview` binary and `README.md`;
- `install.sh`, a copy of `scripts/install.sh`;
- one `savings-preview_<tag>_<os>_<arch>.cdx.json` per archive, the SBOM of the binary in it (see below);
- `checksums.txt`, with the SHA-256 sums of the archives and the SBOMs;
- `savings-preview.rb`, the Homebrew formula for this release.

## Cutting a release

Add a `## vX.Y.Z` section to `CHANGELOG.md` on `main`: what is new, breaking changes with how to upgrade, and fixes. Check what the release page will show with `scripts/release_notes.py vX.Y.Z`.

Tag a commit on `main` that passed CI and has that section, and push the tag:

    git tag -a v0.1.0 -m v0.1.0
    git push origin v0.1.0

Follow the run under the repository's Actions tab. If it fails before **publish**, delete the tag, fix the cause and tag again. If **publish** fails after creating the draft release, delete the draft and the tag before retrying.

Then update the Homebrew tap as described below.

## Building the release locally

    scripts/release.sh build v0.1.0-local
    scripts/release.sh package v0.1.0-local

Naming platforms, as the release jobs do, builds and packages only those, and `checksums` then writes `checksums.txt` and `install.sh` once every platform is packaged:

    scripts/release.sh build v0.1.0-local linux/amd64
    scripts/release.sh package v0.1.0-local linux/amd64
    scripts/release.sh checksums v0.1.0-local

The output lands in `dist/release/`. The package step builds the SBOM generator pinned in `tools/go.mod`, which downloads its modules the first time. Check a download with `shasum -a 256 -c checksums.txt` in the directory that holds the archives, adding `--ignore-missing` when only some of them are there.

## SBOMs

Each archive has a [CycloneDX](https://cyclonedx.org) 1.6 JSON SBOM next to it, written by `cyclonedx-gomod bin` from the build information Go records in the binary. It lists the Go modules linked into the binary with their versions, and the Go release as a `std` component, all with package URLs for the archive's platform, plus the binary's hashes and build settings. The binary is the one inside the archive, so a signing job added between **build** and **publish** is covered.

Check an SBOM with any CycloneDX tool, for example `grype sbom:savings-preview_v0.3.0_linux_amd64.cdx.json`. To check a binary against the Go vulnerability database directly, run `go tool -modfile=tools/go.mod govulncheck -mode binary path/to/savings-preview` from a checkout.

## Vulnerability checks

The `govulncheck` CI job runs `govulncheck ./...` with the latest Go 1.26 patch release, as the release build does. It fails when the code calls a function with a known vulnerability in a dependency or in the standard library, and lists, without failing, vulnerabilities in packages that are imported but not called. It runs alongside the other jobs on every pull request, push to `main` and nightly run, and in a release's **checks** when that runs. Every release also runs it in its own **govulncheck** job, with a database downloaded when the tag is pushed.

Pull requests read a copy of the vulnerability database that the nightly run saves in the Actions cache each day, and download it from `vuln.go.dev` when there is none for the day. All other runs download it. The govulncheck binary is cached by `tools/go.sum`.

To run the same check locally: `make vulncheck`. When it fails on the standard library, update Go: the fix is in a newer patch release, which CI picks up on its own. When it fails on a module, upgrade it with `go get module@version` and `go mod tidy`.

## Tools and dependency updates

`tools/go.mod` pins the Go tools CI and the release use, `govulncheck` and `cyclonedx-gomod`, as `tool` directives. It is a separate module so that the tools' dependencies never change the versions linked into `savings-preview`. Run them with `go tool -modfile=tools/go.mod <tool>`.

Every action in `.github/workflows/` is pinned to a commit, with its release tag in a comment. `tests/test_workflows.py` fails on an action referenced by tag or branch.

Dependabot (`.github/dependabot.yml`) opens pull requests each week for the actions, the modules in `go.mod`, and the tools in `tools/go.mod`. Action updates come as one pull request, as do the Kubernetes and the AWS SDK modules. When adding a tool, add its module to the `allow` list of the `/tools` entry as well: Dependabot skips modules a `tool` directive lists unless they are allowed by name.

## The install script

`scripts/install.sh` is attached to every release as `install.sh`. Users run it with:

    gh release download -R thoras-ai/savings-preview -p install.sh -O - | sh

It detects the OS and architecture, downloads the matching archive and `checksums.txt` from the latest release (or the tag in `VERSION`), verifies the archive, and installs the binary into `INSTALL_DIR`, `~/.local/bin` when that is on `PATH`, or `/usr/local/bin`, asking before it uses sudo. It downloads through gh when gh is installed and logged in, and otherwise through the GitHub API with `curl` and `GITHUB_TOKEN` or `GH_TOKEN`. Any checksum problem stops it before it installs anything. The script needs only POSIX `sh` and the usual base tools (`awk`, `sed`, `tar`, and `sha256sum` or `shasum`).

`tests/test_install_script.py` runs it against a fake release with fake `gh`, `curl`, `uname` and `sudo` commands, so it needs no network.

## Homebrew

`packaging/homebrew/savings-preview.rb` is the formula template. For each release, `scripts/homebrew_formula.py` replaces its `@...@` placeholders with the version, the API URL of each platform's archive and the archive's SHA-256, and fails if any archive or checksum is missing. The formula downloads through the GitHub API because the repository is private; it sends `HOMEBREW_GITHUB_API_TOKEN` when that is set.

To render it by hand for an existing release:

    gh release download v0.2.0 -p checksums.txt
    gh release view v0.2.0 --json tagName,assets > release.json
    scripts/homebrew_formula.py v0.2.0 checksums.txt release.json > savings-preview.rb

### Publishing to the tap

The formula is published from a separate tap repository, `thoras-ai/homebrew-tap`. It does not exist yet. To set it up once:

1. Create `thoras-ai/homebrew-tap` as a private repository with a `Formula/` directory. Homebrew maps `brew tap thoras-ai/tap` to a repository named `homebrew-tap`.
2. Give the people who will install read access to both it and `thoras-ai/savings-preview`.

For each release:

1. Download the formula: `gh release download v0.2.0 -R thoras-ai/savings-preview -p savings-preview.rb`.
2. Copy it to `Formula/savings-preview.rb` in the tap, commit and push.
3. Check it: `brew update`, then `HOMEBREW_GITHUB_API_TOKEN=$(gh auth token) brew upgrade savings-preview` (or `brew install thoras-ai/tap/savings-preview`), then `brew test savings-preview`.

Users install with:

    brew tap thoras-ai/tap https://github.com/thoras-ai/homebrew-tap
    export HOMEBREW_GITHUB_API_TOKEN=$(gh auth token)
    brew install thoras-ai/tap/savings-preview

`brew tap` clones over HTTPS, so git needs credentials for GitHub; `gh auth setup-git` configures them. Without `HOMEBREW_GITHUB_API_TOKEN`, the download fails with a 404.

The tap update can later move into the release workflow as a job that pushes the formula to the tap. It would need a token or deploy key with write access to `thoras-ai/homebrew-tap`, stored as a secret in this repository.

## Adding signing or notarization

Each **build** job builds its platform's binary into `dist/release/bin/` and then packages it, and `package` archives whatever is there. **build** starts before **preflight** has checked that the tag is on `main`, so it must never see signing secrets. To sign the binaries, split **build** in two: one job that builds and uploads the binaries, and one that needs it and **preflight**, signs the binaries that need it, and runs `package`. Only the second job gets the signing secrets. The SBOM is written from the archived binary, so it describes the signed one.

macOS code signing and notarization need a `macos` runner with `codesign` and `xcrun notarytool`, the Developer ID certificate, and an App Store Connect API key, all stored as repository secrets and read only by the signing job. A bare binary cannot have a notarization ticket stapled to it, so Gatekeeper checks it online the first time it runs.
