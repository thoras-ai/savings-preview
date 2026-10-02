# Thoras Savings Preview

A read-only CLI that reads a cluster's existing usage history, writes it as one OTel metrics file, and shows what Thoras would have saved.

## Quickstart

    make build                      # bin/savings-preview-{darwin,linux}-{amd64,arm64}
    ./bin/savings-preview-darwin-arm64 --context my-cluster

Or build for this machine only with `go build -o bin/savings-preview ./cmd/savings-preview`. The binary carries the report page, so it needs nothing next to it.

`savings-preview run` (also what runs with no command) uses the current kubeconfig context, or `--context`, and prints what it will read before reading anything. It never changes the cluster. It then:

1. Looks for a Prometheus-compatible store and pulls 14 days of usage from it (`--days`, `--step`, `--target` to name the store).
2. If there is no store, none answers, or the store lacks the per-container usage and request series, it says why and polls metrics-server instead, every `--interval` until `--duration` passes or Ctrl-C. It takes the same `--keep-raw`, `--rollup` and `--write-every` flags as `poll`. Fewer than 7 days of readings give a tier 3 (limited history) report. `--source prometheus` or `--source poll` skips the choice.
3. Writes `savings-preview.otlp.json` (`--out` or `-o`) and checks it against the contract.
4. Serves the report on 127.0.0.1 on a random port and opens the browser; `--no-open` only prints the address. Ctrl-C stops the server.
5. Writes `savings-preview.zip` next to the file: the contract file, `summary.json` (cluster, time span, tier, totals) and `report.html`, a copy of the report with the data inside that opens from disk. Send the zip to Thoras or attach it to an email. Nothing is uploaded.

`savings-preview --version` prints the version.

- `docs/contract.md`: the OTel file every adapter writes and the report reads.
- `fixtures/savings-preview.otlp.json`: a synthetic file that follows the contract. Regenerate it with `python3 scripts/gen_fixture.py`.
- `scripts/validate_contract.py FILE`: checks a file against the contract.
- `src/` and `scripts/build_report.py`: the report page. It reads one contract file and shows spend, savings, a requested-versus-target chart and a workload table.
- `docs/security.md`: what the CLI reads and writes, where its output goes, and how to check. `deploy/rbac/` has read-only ServiceAccounts for each source.
- `docs/prometheus-adapter.md`: the Prometheus adapter. Build it with `go build -o bin/savings-preview ./cmd/savings-preview`.
- `docs/releasing.md`: how a `v*` tag becomes a GitHub release with binaries and checksums.
- `docs/poll-adapter.md`: the poll adapter, for clusters with no usage history. It reads the cluster at an interval and keeps the readings on disk.

Run every test with `make test`, or the Go tests with `go test ./...`, the contract tests with `python3 -m unittest discover -s tests`, and the page tests with `node --test tests/report_model.test.js`. The scripts need Python 3 and the standard library only.

## Report page

`python3 scripts/build_report.py [OUTDIR]` writes `OUTDIR/index.html` (default `dist/`): one file with the scripts, styles and fonts inlined. It makes no network requests, so it works offline.

The page fetches `savings-preview.otlp.json` from the same directory, or the file named in the `data` query parameter. A page that holds the file in a `<script type="application/json" id="report-data">` element, like the `report.html` in the zip, uses that instead of fetching. When the fetch fails, for example when the page is opened from disk, it asks for the file instead.

To try it with the fixture:

    python3 scripts/build_report.py dist
    cp fixtures/savings-preview.otlp.json dist/
    python3 -m http.server -d dist

The savings math is in `src/js/model.js`. The target is 1.1 times the trailing 15-minute maximum of usage, per workload. Waste is the request above the target. Cost is the larger of the CPU and memory cost, priced from the node resources when their instance types are in the built-in AWS price table, and from a 4-core, 16 GiB, $0.192 per hour node otherwise. A container missing a required series is listed but not counted. When a file mixes steps (`thoras.step_spans`), each point counts for its own step, averages are weighted by time, and peaks come from the `.max` series where the file has them.

`src/styles/primitives.css` and `src/styles/semantic.css` are copies of the dashboard's design tokens (`dashboard/src/styles/tokens/`). Re-copy them to pick up token changes; `zinc.css` supplies the Tailwind zinc values they reference.
