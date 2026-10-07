# Changelog

Each release's section here is its release notes on GitHub. `scripts/release_notes.py` reads the section for a tag, and the release workflow fails when there is none.

## Unreleased

### New

- `run` and `check` read several clusters in one run. Give `--context` more than once, list contexts in a file with `--contexts-file` (one per line, `#` comments), or read every context in the kubeconfig with `--all-contexts`. Each cluster is read as a single run reads it, a store in the cluster first and then polling, up to three at a time, with each progress line prefixed by its context. Clusters that poll do so in the same window. A cluster that cannot be read is reported with the reason and left out; the run fails only when no cluster can be read. Without these flags, `run` and `check` read one cluster as before. See [Many clusters](docs/getting-started.md#many-clusters).
- A run of several clusters writes one `savings-preview.otlp.json`, its compact copy and `savings-preview.zip` holding every cluster, and keeps each cluster's own file in `savings-preview.clusters/`. `summary.json` adds `clusters`, each cluster's own summary or the `error` that kept it from being read; the existing fields keep their names and hold the fleet's figures: totals summed over the clusters, the worst tier, the coarsest step and the earliest start to the latest end.
- `check` with several contexts prints a line per cluster with its source, history, tier and any problem, and a line for the fleet. It exits 0 when every cluster is ready, 2 when none can run and 1 otherwise. `--timeout` applies to each cluster. `--json` prints `status`, `exit_code`, `summary` and `clusters`, each with its context and its full check.
- The report shows a file of several clusters as a fleet. Page one leads with the fleet total, lists each cluster's spend today and with Thoras by saving, and counts the real clusters under Across your fleet. Each cluster is priced from its own nodes. Assumptions adds a Clusters filter that the link keeps (`cluster=`), and Data and method lists each cluster, the clusters that could not be read and why, and any cluster found in more than one file. A file with one cluster shows as before.
- The report page opens several files at once, chosen or dropped together, and shows them as one fleet. A cluster in more than one file counts once, from the file whose data ends latest.
- Contract 1.4.0: a file may hold several clusters, and a cluster resource with `thoras.cluster.error` records a cluster that could not be read. The change is additive. Files with one cluster are still written as 1.0.0, and the validators, `export` and the report read both versions. Releases up to v0.2.0 refuse a 1.4.0 file with a message naming its version.
- The published page lists a Fleet of clusters sample (`#sample=fleet`), written by `go run ./scripts/gen_demo -fleet`.

### Changed messages

- The contract validators (`scripts/validate_contract.py`, and the check `run` makes) report a wrong scope version as `scope version must be 1.0.0 or 1.4.0`, and `export` names both versions when it refuses a file. No flag, field or exit status changed.

### Fixes

- When the kubeconfig sets no current context and `--context` is not given, `run`, `prometheus`, `poll` and `check` say so, list the kubeconfig's contexts and show the command to run with one of them. Before, they printed client-go's `no configuration has been provided, try setting KUBERNETES_MASTER environment variable`. A missing kubeconfig now names the paths looked at, and a `--context` that is not in the kubeconfig lists the contexts that are. `check` prints the message once under Kubernetes and refers to it on the source lines. In `check --json`, `kubernetes.error` holds the new message, which can span several lines, and the `order` notes and `summary` refer to it in a few words; field names and types are unchanged. Exit codes are unchanged.
- The spend chart on page one no longer goes blank when one container of a workload runs only some of the time, such as an init container, a periodic check or a sidecar that exits. A container with no CPU or memory usage at a step is taken as not running there and adds no usage and no request; before, the whole workload had no value at that step, and the chart left out every bucket where any workload had none. A workload now has no value only where none of its containers has usage.
- In the chart, a workload with no value in a bucket inside its span takes its nearest value. The bucket is left empty only when the workloads filled this way make up more than 5% of its spend. Empty buckets at the start and end are dropped, so a store with a shorter memory history than CPU history no longer leaves the first hours blank.
- The poll and Prometheus adapters mark a node as spot the way the product does. A node with `cloud.google.com/gke-preemptible=true`, or with a `karpenter.sh/capacity-type` value that contains `spot` or `preemptible` in any case, is written with `thoras.node.capacity_type` `spot`. Before, such a node was `unknown` unless the value was exactly `spot`. The attribute and its values are unchanged.

### Changed figures

- Totals change for clusters with such containers, in the report and in `summary.json`. A workload with an intermittent container was averaged over only the steps where every container had usage, often one or none, and is now averaged over every step where any container has usage. A workload that had no such step was left out and is now counted. Requests of a container count only at steps where it has usage. Fields and their meaning are unchanged, and files without intermittent containers give the same totals as before.
- **Nodes are priced the way the Thoras product prices them**, in the report and in `summary.json`, so the preview's figures match what the product would show for the same nodes. The price table is the product's, imported from its pricing migrations (AWS us-east-1, Google Cloud us-central1, Azure eastus, and Google Cloud custom machine types); [Node prices](docs/node-prices.md) describes it and how to refresh it.
  - Each node is priced from its own cloud's table, chosen by `cloud.provider`. Before, every node was priced from a short AWS us-east-1 table, and Google Cloud and Azure nodes fell back to the default node unless a reader picked an instance type or used the suggested rates. Google Cloud and Azure clusters now get rates from their own nodes, so their figures change.
  - Spot nodes are priced at the spot price, and a spot node of a type with no spot price is left out of the rates. Before, spot nodes were priced at on-demand. Azure spot nodes stay at on-demand, as in the product.
  - Google Cloud custom machine types, such as `n2d-custom-16-32768`, are priced per vCPU and per GiB.
  - The rates divide the nodes' hourly prices by their listed vCPUs and memory, as the product does, instead of their allocatable CPU and memory. This lowers both rates for AWS clusters too, since allocatable is below the listed size: on-demand AWS figures drop by a few percent (the synthetic sample's annual spend goes from $6,155 to $5,887 with its nodes on-demand, the demo cluster's from $72,778 to $70,860). AWS clusters with spot nodes drop further: the synthetic sample, with one of its two nodes spot, goes to $3,375, and the demo cluster to $63,693.
  - A node needs no allocatable series to be priced. A node whose cloud the file does not name is priced from the table that has its instance type.
  - The default node (4 cores, 16 GiB, $0.192 per hour) applies as before when no node is priced, as on minikube.
  - In a fleet, each cluster is priced from its own nodes with these rules, so a fleet that spans clouds prices each cluster from its own cloud's table. The fleet sample's annual spend is $70,887, with $41,311 in savings.
  - The instance-type picker under Assumptions lists the same table, 3,316 types instead of 128, so a type picked in its cloud's reference region costs what the default pricing charges for it. Every type the old list had is still there, so links that name one keep working, but its price can differ: Google Cloud `n2-standard-4` goes from $0.194 to $0.219 per hour.
  - `pricing` in `summary.json` and the report's pricing line name the clouds and regions used, for example `9 of 13 nodes at AWS us-east-1, Google Cloud us-central1 and Azure eastus on-demand and spot list prices`. An AWS cluster with only on-demand nodes keeps the old wording. Field names and types are unchanged.

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
