# Thoras Savings Preview

A read-only CLI that reads a cluster's existing usage history, writes it as one OTel metrics file, and shows what Thoras would have saved.

To run it on your cluster, start with [docs/getting-started.md](docs/getting-started.md).

![The report page with the synthetic sample: the summary section](docs/images/report.png)

The report page is also published at https://curly-dollop-gw8yzn1.pages.github.io/. The site is private: it needs a GitHub login with access to the thoras-ai organization. It opens a `savings-preview.otlp.json`, a `savings-preview.zip` or a `report.html` in the browser, and nothing is uploaded.

## Install

The repository is private, so every download needs a GitHub login or token with access to it.

With [gh](https://cli.github.com) logged in:

    gh release download -R thoras-ai/savings-preview -p install.sh -O - | sh

`install.sh` picks the archive for this machine (macOS or Linux, amd64 or arm64), checks it against the release's `checksums.txt`, and installs `savings-preview` into `~/.local/bin` when that is on `PATH`, or `/usr/local/bin` otherwise. It asks before using sudo, and stops without installing anything when a checksum is missing or does not match. Set `VERSION=v0.2.0` to install a release other than the latest, and `INSTALL_DIR` to install somewhere else:

    gh release download -R thoras-ai/savings-preview -p install.sh -O - | VERSION=v0.2.0 INSTALL_DIR=~/bin sh

Without gh, the script downloads through the GitHub API with `curl`, using `GITHUB_TOKEN` or `GH_TOKEN`. The token needs read access to the repository's contents.

With Homebrew, once the tap is published (see `docs/releasing.md`):

    brew tap thoras-ai/tap https://github.com/thoras-ai/homebrew-tap
    export HOMEBREW_GITHUB_API_TOKEN=$(gh auth token)
    brew install thoras-ai/tap/savings-preview

Or download an archive from the release page, check it with `shasum -a 256 -c checksums.txt --ignore-missing`, and put the `savings-preview` binary on `PATH`.

## Quickstart

    make build                      # bin/savings-preview-{darwin,linux}-{amd64,arm64}
    ./bin/savings-preview-darwin-arm64 --context my-cluster

Or build for this machine only with `go build -o bin/savings-preview ./cmd/savings-preview`. The binary carries the report page, so it needs nothing next to it. An installed release runs the same way: `savings-preview --context my-cluster`.

Before the first run, `savings-preview check` with the same flags answers in a few seconds whether the run can work and how well:

    ./bin/savings-preview-darwin-arm64 check --context my-cluster

It prints the identity the kubeconfig gives, the result of `kubectl auth can-i` for each request `prometheus` and `poll` make, whether metrics-server answers, each Prometheus-compatible store it finds with whether it answers and how far back its data goes, whether the Datadog keys are valid, and which source `run` would pick, why, and the fidelity tier to expect. The last line sums it up. `--json` prints the same as JSON, and `--color never` (or `NO_COLOR`) leaves out the colors. It exits with 0 when `run` would read a source at tier 1, 1 when it would read one at a lower tier or with gaps, such as a store with less history than `--days` or polling, and 2 when it has nothing it can read. It pulls no history and writes nothing.

`savings-preview run` (also what runs with no command) uses the current kubeconfig context, or `--context`, and prints what it will read before reading anything. It never changes the cluster. It then:

1. When Datadog keys are given (`DD_API_KEY` and `DD_APP_KEY`, or `--api-key-file` and `--app-key-file`), pulls 14 days of usage from Datadog, taking the `datadog` command's `--site` and `--selector`. If Datadog has no usable data, it says why and goes on to the next step. Several clusters in the organization stop the run until `--selector` picks one.
2. Otherwise looks for a Prometheus-compatible store and pulls 14 days of usage from it (`--days`, `--step`, `--target` to name the store). `--url` reads a store outside the cluster instead, such as Mimir, Grafana Cloud, Amazon Managed Prometheus or Google Managed Prometheus, with the `prometheus` command's `--backend`, `--tenant`, `--bearer-token-file`, `--basic-auth-file`, `--header-file`, `--aws-region`, `--project`, `--google-credentials-file`, `--gke-metrics`, `--max-query-range` and `--selector`, and the same credential environment variables. A store named with `--url` or `--target` that fails stops the run.
3. If there is no store, none answers, or the store lacks the per-container usage and request series, it says why and polls metrics-server instead, every `--interval` until `--duration` passes or Ctrl-C. It takes the same `--keep-raw`, `--rollup` and `--write-every` flags as `poll`. Fewer than 7 days of readings give a tier 3 (limited history) report. `--source datadog`, `--source prometheus` or `--source poll` skips the choice and does not fall back.
4. Writes `savings-preview.otlp.json` (`--out` or `-o`) at full resolution, then a compact copy next to it, `savings-preview.compact.otlp.json`, at 15-minute steps that keep the peak and the mean of usage. The savings math takes its target from 15-minute peaks, so totals from the compact file come within about 1% of the full file's while it stays small enough for a browser: 14 days of 1-minute data for 1,000 workload-containers is about 6.3 GB in full and 330 MB compact. It checks the compact file against the contract.
5. Serves the report on the compact file on 127.0.0.1 on a random port and opens the browser; `--no-open` only prints the address. Ctrl-C stops the server.
6. Writes `savings-preview.zip` next to the file: `summary.json` (cluster, time span, tier, totals, and the name and size of the full file) and `report.html`, a copy of the report with the compact file inside that opens from disk. `savings-preview export -o savings-preview.otlp.json report.html` writes that file back out. The full file stays on disk and is not in the zip. Send the zip to Thoras or attach it to an email. Nothing is uploaded.

`savings-preview export --compact FILE` writes the compact copy of any contract file, of a poll's readings directory (`FILE.readings`), or of the data in a report page, reading it as a stream. `docs/contract.md` describes compact files and how their totals compare with the full file. `docs/upgrading.md` lists what changed in the zip and `summary.json` since v0.1.0.

`savings-preview --version` prints the version.

- `docs/contract.md`: the OTel file every adapter writes and the report reads.
- `fixtures/savings-preview.otlp.json`: a synthetic file that follows the contract. Regenerate it with `python3 scripts/gen_fixture.py`.
- `fixtures/minikube-poll.otlp.json`: one minute of `savings-preview poll` readings from a local minikube cluster, with the cluster, node, namespace and workload names replaced (`kube-system`, `cert-manager`, `coredns` and `metrics-server` kept).
- `scripts/validate_contract.py FILE`: checks a file against the contract.
- `src/` and `scripts/build_report.py`: the report page. It reads one contract file and shows spend, savings, a requested-versus-target chart and a workload table.
- `docs/security.md`: what the CLI reads and writes, where its output goes, and how to check. `deploy/rbac/` has read-only ServiceAccounts for each source.
- `docs/prometheus-adapter.md`: the Prometheus adapter. Build it with `go build -o bin/savings-preview ./cmd/savings-preview`.
- `docs/releasing.md`: how a `v*` tag becomes a GitHub release with binaries, checksums, the install script and the Homebrew formula, and how to publish the formula to the tap.
- `scripts/install.sh` and `packaging/homebrew/savings-preview.rb`: the install script and the Homebrew formula template that each release carries.
- `docs/poll-adapter.md`: the poll adapter, for clusters with no usage history. It reads the cluster at an interval and keeps the readings on disk.
- `docs/datadog-adapter.md`: the Datadog adapter. It reads the history from the Datadog metrics query API with an API key and an application key.
- `docs/design.md`: the design rules the report page follows, and how to re-sync the design tokens in `design/`.

Run every test with `make test`, or the Go tests with `go test ./...`, the contract, install script and formula tests with `python3 -m unittest discover -s tests`, and the page tests with `node --test 'tests/**/*.test.js'`. `make test-all` also runs `go vet` and the Go tests under the race detector. The scripts need Python 3 and the standard library only.

## Report page

`python3 scripts/build_report.py [OUTDIR]` writes `OUTDIR/index.html` (default `dist/`): one file with the scripts, styles and fonts inlined. It makes no network requests, so it works offline.

The page is one report in six numbered sections, read top to bottom: summary, where the money is, workloads, cluster and nodes, assumptions, and data and method. A bar under the header lists them and underlines the one in view. Where the money is maps savings or spend by namespace or workload and ranks the namespaces and the top 10 workloads; with more than 60 workloads the map opens on namespaces, and selecting one shows its workloads. Workloads lists every workload in the file, with a filter and pages of 50; selecting one opens its drill-down: requested, usage and target over time with OOM kills marked, the per-pod requests Thoras would set, and what could make the saving smaller or unsafe (OOM kills, limits below the peak-safe value, provisional data). Cluster and nodes sets the nodes' allocatable capacity against what the workloads request, would need once right-sized and use, lists the instance types with their list prices, and expresses the waste as nodes that could be freed. Assumptions holds every setting: headroom and peak window, pricing (an instance type from the built-in AWS, Google Cloud and Azure list prices, or typed rates, and a spot or committed-use discount), the annual cloud bill to show the savings as a share of, the engineer cost the summary uses, namespaces and time range. Each setting resets on its own or all at once. Data and method describes the file (source, window, resolution, fidelity tier, pricing and contract version), lists the workloads, containers and nodes the totals leave out or price differently and why, and spells out the calculation with the current settings. The page reads the file once and recomputes every section when a setting changes. The settings live in the URL hash, so a link opens the same view, and Copy link in the header copies it; each key is written only when it differs from its default:

| Key | Meaning | Default |
| --- | --- | --- |
| `section` | Section to open: `summary`, `money`, `workloads`, `cluster`, `assumptions` or `method` | `summary` |
| `cpu`, `gib` | Price in USD per core-hour and per GiB-hour, both or neither | From the nodes |
| `node` | Instance type the rates came from, as `cloud/region/type`; read only with `cpu` and `gib` | None |
| `discount` | Spot or committed-use discount on the rates, in percent | `0` |
| `headroom` | Target headroom over the peak, in percent | `10` |
| `peak` | Rolling peak window, in minutes | `15` |
| `ns` | Namespaces to count, comma separated | All |
| `from`, `to` | Time range, in Unix seconds | The whole file |
| `cost` | Loaded cost of an engineer per year, in USD | `200000` |
| `bill` | Annual cloud bill, in USD | None |
| `workload` | Workload whose drill-down the workloads section shows, as `namespace/kind/name` | None |

For example, `index.html#section=workloads&headroom=20&ns=shop,data`. A value that does not parse is ignored. With the defaults, the totals match the CLI's `summary.json`.

Each section is a script in `src/js/section-*.js` that registers itself with `SP.sections` (`src/js/sections.js` describes the contract). Shared pieces are in `src/js/model.js` (`prepare` reads a file once, `compute` applies the settings), `src/js/state.js` (the settings and their hash form), `src/js/levers.js` (the choices and resets the assumptions section offers), `src/js/prices.js` (the instance-type list prices) and `src/js/components.js` (number formats, KPI tile, confidence pill, banner). A new script must be listed in both `scripts/build_report.py` and `page.go`.

The page fetches `savings-preview.otlp.json` from the same directory, or the file named in the `data` query parameter. A page that holds the file in a `<script type="application/json" id="report-data">` element, like the `report.html` in the zip, uses that instead of fetching. When the fetch fails, for example when the page is opened from disk, it asks for a file instead: a contract file, a `savings-preview.zip`, or a `report.html`, chosen or dropped on the page and read in the browser. The header switches between the light and dark themes or follows the system (Auto); the choice is kept in the browser's local storage. Print summary in the header, or the browser's own print command, prints a letter-size summary in the light theme: the headline figures, confidence, spend over the window, the top workloads and how the figure is computed, all under the current settings.

`python3 scripts/build_report.py --site OUTDIR` builds the copy for GitHub Pages: it opens on the upload screen and lists the two fixtures as samples, copied to `OUTDIR/samples/`. `.github/workflows/pages.yml` builds and deploys it on every push to `main`; the repository's Pages source must be set to GitHub Actions. The site holds only the page and the two fixtures, no customer data.

To try it with the fixture:

    python3 scripts/build_report.py dist
    cp fixtures/savings-preview.otlp.json dist/
    python3 -m http.server -d dist

The savings math is in `src/js/model.js`. By default the target is 1.1 times the trailing 15-minute maximum of usage, per workload. Waste is the request above the target. Cost is the larger of the CPU and memory cost, priced from the node resources when their instance types are in the built-in AWS price table, and from a 4-core, 16 GiB, $0.192 per hour node otherwise. A container missing a required series is listed but not counted. When a file mixes steps (`thoras.step_spans`), each point counts for its own step, averages are weighted by time, and peaks come from the `.max` series where the file has them.

The page's design tokens are derived from the dashboard's tokens and live in `design/`. `docs/design.md` describes them, the rules the page follows, and how to re-sync them with `scripts/sync_design_tokens.py`.
