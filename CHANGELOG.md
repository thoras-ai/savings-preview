# Changelog

Each release's section here is its release notes on GitHub. `scripts/release_notes.py` reads the section for a tag, and the release workflow fails when there is none.

## v0.2.0

### Breaking changes

- **The zip carries its data once, inside `report.html`.** `savings-preview.zip` no longer holds `savings-preview.otlp.json`. It holds `summary.json` and `report.html`, and `report.html` carries a compact copy of the data at 15-minute steps. The full-resolution file stays on disk next to the zip, and `summary.json` names it in `full_data`. Take the data back out with `savings-preview export -o savings-preview.otlp.json report.html`. See [Upgrading](docs/upgrading.md#the-zip-carries-its-data-once-inside-reporthtml).
- **Contract 1.3.0.** Compact files use two new optional attributes, `thoras.held_metrics` and `thoras.source_step_seconds`. The v0.1.0 report page and its `report.html` misprice compact files, and the v0.1.0 validator rejects compact resources that claim tier 1 or 2 (data at 300 seconds or finer over 7 days or more), since it judges the tier on the 900-second points. Read compact files with the report page or `scripts/validate_contract.py` from this release. Full files read as before, and 1.2.0 files are valid 1.3.0 files. See [contract.md](docs/contract.md#readers-before-130).
- **`run` reads Datadog first when both keys are set.** Without `--source`, `run` now reads Datadog when `DD_API_KEY` and `DD_APP_KEY` (or `DD_APPLICATION_KEY`), or `--api-key-file` and `--app-key-file`, are given. In v0.1.0 it ignored them. Naming a store with `--url` or `--target` turns Datadog off. With Datadog, `--step` must be a whole number of minutes, and an organization with several clusters stops the run until `--selector` picks one. To keep the v0.1.0 behavior, unset a key for the run: `env -u DD_API_KEY savings-preview run`. See [Upgrading](docs/upgrading.md#run-reads-datadog-when-its-keys-are-set).
- **`summary.json` totals come from the compact file.** They can differ from the full file's by up to about 1%. `start`, `end`, `span_seconds` and `snapshot` also come from the compact file, so a run of a few minutes reports a 15-minute span. New fields: `compact_step_seconds` and `full_data`. See [Upgrading](docs/upgrading.md#summaryjson).

No flag was removed or renamed, and no flag default changed.

### New

- `run` reads a store outside the cluster with `--url`, taking the `prometheus` command's `--backend`, `--tenant`, `--bearer-token-file`, `--basic-auth-file`, `--header-file`, `--aws-region`, `--max-query-range` and `--selector`. `--target` also takes a URL. A store named with `--url` or `--target` that fails stops the run.
- `run --source datadog` reads Datadog, with the `datadog` command's `--site`, `--api-key-file`, `--app-key-file` and `--selector`.
- `run` writes `savings-preview.compact.otlp.json` next to the full file, serves it to the report, and reads every file it checks as a stream: after collection, under 300 MB of memory for 14 days of 1,000 workload-containers. It checks only the compact file against the contract, not the full one.
- `savings-preview export` writes the compact copy of a contract file or a poll's readings directory (`--compact`), and writes the data in a `report.html` back out to a file.
- `savings-preview check` says in a few seconds whether `run` can read usage data and how well: the identity the kubeconfig gives, the RBAC checks for each source, metrics-server, the stores it finds and how far back their data goes, whether the Datadog keys are valid, and which source `run` would pick at which fidelity tier. It takes `run`'s flags plus `--json`, `--color` and `--timeout`, and exits 0, 1 or 2 by the expected tier.
- The report is laid out as six numbered sections: summary, where the money is (by namespace and workload), workloads with a drill-down for each, cluster and nodes, assumptions, and data and method. The assumptions section recomputes every section for headroom, peak window, instance-type pricing on AWS, GCP and Azure, rates, discount, namespaces and time range, and shows the savings as a share of an annual cloud bill and in engineers at a loaded yearly cost. Settings live in the URL, and "Copy link" shares the current view. "Print summary" prints a letter-size summary.
- Reports from limited history or a single snapshot are marked provisional, with a banner and figures rounded and marked with ~.
- The report draws peak and mean usage, shows fidelity notes and short spans, has an Auto, Light and Dark theme switch, and opens a dropped `.otlp.json`, `savings-preview.zip` or `report.html`. It is also published on GitHub Pages with two samples.
- Each release carries `install.sh`, which installs the right binary after checking it against `checksums.txt`, and a Homebrew formula, `savings-preview.rb`. Since the repository is private, both need a logged-in `gh` or a GitHub token. See the README and [releasing.md](docs/releasing.md).
- Setup guides for Datadog ([datadog-adapter.md](docs/datadog-adapter.md)) and Grafana Cloud ([prometheus-adapter.md](docs/prometheus-adapter.md)), each checked against a live account, with scrubbed recordings that the tests replay.

### Fixes

- `poll --export` reports the rollup age it used.
- When cAdvisor is collected but its per-container series were dropped, the error says so and names the fix.
- The `datadog` command's messages say "1 day" and "1 node" instead of "1 days" and "1 nodes".

### Upgrading

[docs/upgrading.md](docs/upgrading.md) lists each change from v0.1.0 with who it affects, how to upgrade, how to tell which version wrote a zip, and how to roll back.

**Full changes:** https://github.com/thoras-ai/savings-preview/compare/v0.1.0...v0.2.0

## v0.1.0

First release.

- The OTel metrics contract, version 1.2.0 ([contract.md](docs/contract.md)), a 14-day synthetic fixture, and `scripts/validate_contract.py`.
- The report page: spend, savings, a requested-versus-target chart and a workload table, built into one offline HTML file.
- `savings-preview prometheus` reads 14 days of usage from a Prometheus-compatible store in the cluster, or from Mimir, Grafana Cloud or Amazon Managed Prometheus by URL. It takes a cluster selector, falls back to pod-level usage, and records OOM kills.
- `savings-preview poll` reads metrics-server at an interval and keeps the readings on disk, for clusters with no usage history.
- `savings-preview datadog` reads usage history from the Datadog metrics query API.
- `savings-preview run` finds usage data, checks the file against the contract, opens the report, and writes `savings-preview.zip`.
- [security.md](docs/security.md) and read-only RBAC manifests for each source in `deploy/rbac/`.
- Binaries for macOS and Linux on amd64 and arm64, with `checksums.txt`.

**Full changes:** https://github.com/thoras-ai/savings-preview/commits/v0.1.0
