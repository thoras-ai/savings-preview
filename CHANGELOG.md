# Changelog

Each release's section here is its release notes on GitHub. `scripts/release_notes.py` reads the section for a tag, and the release workflow fails when there is none.

## v0.2.0

### Breaking changes

- **The zip carries its data once, inside `report.html`.** `savings-preview.zip` no longer holds `savings-preview.otlp.json`. It holds `summary.json` and `report.html`, and `report.html` carries a compact copy of the data at 15-minute steps. The full-resolution file stays on disk next to the zip, and `summary.json` names it in `full_data`. Take the data back out with `savings-preview export -o savings-preview.otlp.json report.html`. See [Upgrading](docs/upgrading.md#the-zip-carries-its-data-once-inside-reporthtml).
- **Contract 1.0.0.** This is the first stable contract. Files written by v0.1.0 cannot be read and must be collected again. See [Upgrading](docs/upgrading.md#files-written-by-v010-must-be-collected-again).
- **`run` reads Datadog first when both keys are set.** Without `--source`, `run` now reads Datadog when `DD_API_KEY` and `DD_APP_KEY` (or `DD_APPLICATION_KEY`), or `--api-key-file` and `--app-key-file`, are given. In v0.1.0 it ignored them. Naming a store with `--url` or `--target` turns Datadog off. With Datadog, `--step` must be a whole number of minutes, and an organization with several clusters stops the run until `--selector` picks one. To keep the v0.1.0 behavior, unset a key for the run: `env -u DD_API_KEY savings-preview run`. See [Upgrading](docs/upgrading.md#run-reads-datadog-when-its-keys-are-set).
- **Azure Monitor workspace URLs use the `azure` backend.** A `--url` under `prometheus.monitor.azure.com` was read as a plain Prometheus store and now gets the Azure Monitor managed Prometheus backend. A workspace without per-container usage series now stops the run, and an Azure Monitor URL with `--tenant` is refused. Pass `--backend prometheus` to keep the v0.1.0 behavior. See [Upgrading](docs/upgrading.md#azure-monitor-workspace-urls-are-read-as-azure-monitor-managed-prometheus).
- **`summary.json` totals come from the compact file.** They can differ from the full file's by up to about 1%. `start`, `end`, `span_seconds` and `snapshot` also come from the compact file, so a run of a few minutes reports a 15-minute span. New fields: `compact_step_seconds` and `full_data`. See [Upgrading](docs/upgrading.md#summaryjson).

No flag was removed or renamed, and no flag default changed.

### New

- [Getting started](docs/getting-started.md) takes a platform engineer from download to a report they can share: what the tool reads and writes, install, `check`, running against each source, reading the report, sharing the results, and troubleshooting.
- GKE clusters: the `prometheus` command and `run` read Google Cloud Monitoring, which serves Google Managed Prometheus and GKE system metrics. `--project PROJECT_ID` (or a `monitoring.googleapis.com` URL in `--url`) selects the new `gmp` backend. Without a bearer token, basic auth or header, it authenticates with Google Application Default Credentials or a service account key in `--google-credentials-file`, and needs `roles/monitoring.viewer`. `--gke-metrics auto|prometheus|system` picks the usual cAdvisor and kube-state-metrics queries or GKE system metrics, which are on by default on GKE; `auto` uses system metrics when the project has no container usage or `kube_pod_owner`. A `--url` with a token keeps working as in v0.1.0. See [prometheus-adapter.md](docs/prometheus-adapter.md#google-managed-prometheus-and-gke).
- `run` reads a store outside the cluster with `--url`, taking the `prometheus` command's `--backend`, `--tenant`, `--bearer-token-file`, `--basic-auth-file`, `--header-file`, `--aws-region`, `--max-query-range` and `--selector`. `--target` also takes a URL. A store named with `--url` or `--target` that fails stops the run.
- `run --source datadog` reads Datadog, with the `datadog` command's `--site`, `--api-key-file`, `--app-key-file` and `--selector`.
- `run` writes `savings-preview.compact.otlp.json` next to the full file, serves it to the report, and reads every file it checks as a stream: after collection, under 300 MB of memory for 14 days of 1,000 workload-containers. It checks only the compact file against the contract, not the full one.
- `savings-preview export` writes the compact copy of a contract file or a poll's readings directory (`--compact`), and writes the data in a `report.html` back out to a file.
- `savings-preview check` says in a few seconds whether `run` can read usage data and how well: the identity the kubeconfig gives, the RBAC checks for each source, metrics-server, the stores it finds and how far back their data goes, whether the Datadog keys are valid, and which source `run` would pick at which fidelity tier. It takes `run`'s flags plus `--json`, `--color` and `--timeout`, and exits 0, 1 or 2 by the expected tier.
- The report is laid out as six numbered sections: summary, where the money is (by namespace and workload), workloads with a drill-down for each, cluster and nodes, assumptions, and data and method. The assumptions section recomputes every section for headroom, peak window, instance-type pricing on AWS, GCP and Azure, rates, discount, namespaces and time range, and shows the savings as a share of an annual cloud bill and in engineers at a loaded yearly cost. Settings live in the URL, and "Copy link" shares the current view. "Print summary" prints page one on a letter-size sheet.
- The report opens on one page that makes the case: the annual saving, today next to Thoras with utilization on each side, requested spend against Thoras over the window, three workloads before and after, the saving across a fleet of similar clusters, and the next step, with Book a demo and Copy summary. A line under the headline says where the data came from. A short poll or a single reading changes only the next step, which suggests a week-long run. The other sections sit under a collapsed Details for engineers. The page opens in the light theme; Auto and Dark stay in the theme switch.
- The report draws peak and mean usage, shows fidelity notes and short spans, has an Auto, Light and Dark theme switch, and opens a dropped `.otlp.json`, `savings-preview.zip` or `report.html`. It is also published on GitHub Pages with three samples.
- Azure Monitor managed Prometheus: the `prometheus` command, `run --url` and `check` read an Azure Monitor workspace with `--backend azure`, which a `prometheus.monitor.azure.com` URL selects. Each query reads one metric, as the workspace requires, and the adapter joins the results by workload. Without a token given directly, the run gets an Entra ID token from an app registration (`--azure-tenant-id`, `--azure-client-id` and `--azure-client-secret-file`, or the `AZURE_*` variables) or the Azure CLI. See [prometheus-adapter.md](docs/prometheus-adapter.md#azure-monitor-managed-prometheus).
- Amazon CloudWatch: the `prometheus` command, `run --url` and `check` read CloudWatch's PromQL endpoint for EKS clusters with OTel Container Insights, with `--backend cloudwatch --aws-region REGION` or a `monitoring.REGION.amazonaws.com` URL. Requests are signed with SigV4 from the standard AWS credential chain. Queries stay within CloudWatch's 7-day range and 500-series limits, and a truncated result stops the run with a suggested `--selector`. See [prometheus-adapter.md](docs/prometheus-adapter.md#amazon-cloudwatch-promql-otel-container-insights).
- A demo cluster sample: `go run ./scripts/gen_demo` (or `make demo`) writes a 60-workload, 14-day compact file, and the published page lists it first. [demo.md](docs/demo.md) is a five-minute walk through the report with it.
- Each release carries `install.sh`, which installs the right binary after checking it against `checksums.txt`, and a Homebrew formula, `savings-preview.rb`. Since the repository is private, `install.sh` needs a logged-in `gh` or `GITHUB_TOKEN` or `GH_TOKEN`, and the formula needs `HOMEBREW_GITHUB_API_TOKEN`. The Homebrew tap is not published yet. See the README and [releasing.md](docs/releasing.md).
- Each archive has a CycloneDX SBOM next to it, `savings-preview_<tag>_<os>_<arch>.cdx.json`, listing the Go modules and Go release in its binary. `checksums.txt` lists the SBOMs as well as the archives, so check a partial download with `shasum -a 256 -c --ignore-missing checksums.txt`. See [releasing.md](docs/releasing.md#sboms).
- CI runs `govulncheck` on every pull request, push to `main`, nightly run and release, and `make vulncheck` runs it locally. Dependabot opens weekly updates for the actions and Go modules. The Go jobs share one build cache, so a pull request's checks take about 30 seconds instead of two and a half minutes.
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
