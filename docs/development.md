# Development

Notes for building Savings Preview from source and working on it. Pull requests are not accepted; see the [README](../README.md#license).

## Build

    make build                      # bin/savings-preview-{darwin,linux}-{amd64,arm64}
    ./bin/savings-preview-darwin-arm64 --context my-cluster

Or build for this machine only with `go build -o bin/savings-preview ./cmd/savings-preview`. The binary carries the report page, so it needs nothing next to it. An installed release runs the same way: `savings-preview --context my-cluster`.

## Repository layout

- [contract.md](contract.md): the OTel file every adapter writes and the report reads.
- `fixtures/savings-preview.otlp.json`: a synthetic file that follows the contract. Regenerate it with `python3 scripts/gen_fixture.py`.
- `fixtures/intermittent.otlp.json`: two synthetic days at 15-minute steps with containers that run only part of the time (an init container, a check that runs now and then) and workloads whose memory series start six hours after their CPU series. Regenerate it with `python3 scripts/gen_intermittent_fixture.py`.
- `fixtures/fleet-history.otlp.json`: a synthetic fleet of four clusters with 2 days, 1 day, 12 hours and 6 hours of history at 15-minute steps, named by two EKS ARNs with the same cluster name in different regions, a GKE context name and a plain name. Regenerate it with `python3 scripts/gen_fleet_history_fixture.py`.
- `fixtures/minikube-poll.otlp.json`: one minute of `savings-preview poll` readings from a local minikube cluster, with the cluster, node, namespace and workload names replaced (`kube-system`, `cert-manager`, `coredns` and `metrics-server` kept).
- `scripts/gen_demo`: writes `fixtures/demo-cluster.compact.otlp.json`, a 60-workload sample cluster, listed on the site build as Demo cluster. The file is about 20 MB, so it is generated rather than committed: run `go run ./scripts/gen_demo` (or `make demo`) before building the site. The output is the same on every run. `go run ./scripts/gen_demo -fleet` writes `fixtures/fleet.compact.otlp.json`, about 27 MB, listed as Fleet of clusters (`#sample=fleet`): three clusters in two regions built from the demo cluster's workloads, and a fourth recorded as not read.
- `scripts/validate_contract.py FILE`: checks a file against the contract.
- `src/` and `scripts/build_report.py`: the report page. It reads one contract file and shows spend, savings, a requested-versus-target chart and a workload table.
- `deploy/rbac/`: read-only ServiceAccounts for each source; [security.md](security.md) lists the requests behind them.
- `scripts/install.sh` and `packaging/homebrew/savings-preview.rb`: the install script and the Homebrew formula template that each release carries. `packaging/npm/`: the npm package that downloads and runs the release binary.
- `CHANGELOG.md`: what changed in each release; each section is that release's notes. [upgrading.md](upgrading.md) has the upgrade steps.

## Tests

Run every test with `make test`, or the Go tests with `go test ./...`, the contract, install script and formula tests with `python3 -m unittest discover -s tests`, and the page and npm package tests with `node --test 'tests/**/*.test.js'`. `make test-all` also runs `go vet` and the Go tests under the race detector. The scripts need Python 3 and the standard library only.

The Prometheus and Datadog adapter docs describe how their recordings are made: [prometheus-adapter.md](prometheus-adapter.md) and [datadog-adapter.md](datadog-adapter.md#testing).

## Report page

`python3 scripts/build_report.py [OUTDIR]` writes `OUTDIR/index.html` (default `dist/`): one file with the scripts, styles and fonts inlined. It makes no network requests, so it works offline.

To try it with the fixture:

    python3 scripts/build_report.py dist
    cp fixtures/savings-preview.otlp.json dist/
    python3 -m http.server -d dist

The page fetches `savings-preview.otlp.json` from the same directory, or the file named in the `data` query parameter. A page that holds the file in a `<script type="application/json" id="report-data">` element, like the `report.html` in the zip, uses that instead of fetching. When the fetch fails, for example when the page is opened from disk, it asks for a file instead: a contract file, a `savings-preview.zip`, or a `report.html`, chosen or dropped on the page and read in the browser. The page opens in the light theme; the header switches to dark or to following the system (Auto), and the choice is kept in the browser's local storage. Print summary in the header, or the browser's own print command, prints page one on a letter-size sheet in the light theme, under the current settings.

What each section of the page shows, and the keys of the settings in the link, are in [Read the report](getting-started.md#read-the-report).

Each section is a script in `src/js/section-*.js` that registers itself with `SP.sections` (`src/js/sections.js` describes the contract). Shared pieces are in `src/js/model.js` (`prepare` reads a file once, `compute` applies the settings), `src/js/state.js` (the settings and their hash form), `src/js/levers.js` (the choices and resets the assumptions section offers), `src/js/prices.js` (the instance-type list prices) and `src/js/components.js` (number formats, KPI tile, confidence pill, banner). A new script must be listed in both `scripts/build_report.py` and `page.go`. The page reads the file once and recomputes every section when a setting changes.

`python3 scripts/build_report.py --site OUTDIR` builds the hosted copy: it opens on the upload screen and lists the two fixtures and the demo cluster as samples, copied to `OUTDIR/samples/`. Generate the demo cluster first. `.github/workflows/pages.yml` builds and deploys it to GitHub Pages on every push to `main`; the repository's Pages source must be set to GitHub Actions. The workflow generates the demo cluster before the build. The site holds only the page and the three samples, no customer data.

The page's design tokens are derived from the Thoras dashboard's tokens and live in `design/`. [design.md](design.md) describes them, the rules the page follows, and how to re-sync them with `scripts/sync_design_tokens.py`.

## Savings math

The savings math is in `src/js/model.js`, and the CLI's `summary.json` computes the same figures in `internal/savings`. By default the target is 1.1 times the trailing 15-minute maximum of usage, per workload. Waste is the request above the target. Cost is the larger of the CPU and memory cost, priced the way the Thoras product prices nodes: each node from its cloud's list price table (AWS us-east-1, Google Cloud us-central1, Azure eastus) by instance type, spot nodes at the spot price, and the rates are the average priced node's hourly cost over its vCPUs and over its GiB. With no node priced, a 4-core, 16 GiB, $0.192 per hour node is used. A container missing a required series is listed but not counted. Workloads in Thoras's own namespaces (`thoras`, `thoras-*`) are left out unless the `includeThoras` lever (`savings.Options.IncludeThoras` in Go) is set, and add no covered time. Each workload's averages are over the time its cluster has data, the steps where at least one workload has complete data, so a workload counts as 0 while it is not running, and time with no data from any workload, such as an outage of the metrics source, is left out. When a file mixes steps (`thoras.step_spans`), each point counts for its own step, averages are weighted by time, and peaks come from the `.max` series where the file has them.

## Node price table

The price table is `internal/savings/node_prices.json`, a copy of the Thoras product's price table. The Go summary embeds it, and both page builders inline it ahead of `src/js/model.js`, so the page and the CLI read the same prices. [node-prices.md](node-prices.md) describes how a node is priced.

`scripts/import_node_prices.py` regenerates the table from the product's pricing migrations, which are not public. It reads the newest file of each kind:

| Migration | What it holds |
| --- | --- |
| `*_aws_pricing.sql` | AWS on-demand prices and spot discounts, us-east-1 |
| `*_gcp_pricing.sql` | Google Cloud on-demand prices and spot discounts, us-central1 |
| `*_azure_pricing.sql` | Azure on-demand prices and spot discounts, eastus |
| `*_gcp_custom_node_pricing.sql` | Google Cloud custom machine types, per vCPU and per GiB, on-demand and spot |

The import keeps Linux rows only, since the product prices every node as Linux, and computes each spot price as the migrations do: on-demand × (100 − discount) / 100, rounded to 10 decimal places. `source` in the JSON names the files it read. When the product's prices change, import them and commit the result with the changed figures noted in the CHANGELOG:

    python3 scripts/import_node_prices.py /path/to/product-source
    make test

The script writes `internal/savings/node_prices.json` and prints how many types each cloud has. Tests that check figures against the table (`internal/savings/savings_test.go`, `tests/report_model.test.js`) name the instance types and prices they use, so a price change shows up there.

## Releasing

[releasing.md](releasing.md) describes how a `v*` tag becomes a GitHub release with binaries, SBOMs, checksums, the install script and the Homebrew formula, and how to publish the formula to the tap.
