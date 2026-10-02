# Thoras Savings Preview

A read-only CLI that reads a cluster's existing usage history, writes it as one OTel metrics file, and shows what Thoras would have saved.

- `docs/contract.md`: the OTel file every adapter writes and the report reads.
- `fixtures/savings-preview.otlp.json`: a synthetic file that follows the contract. Regenerate it with `python3 scripts/gen_fixture.py`.
- `scripts/validate_contract.py FILE`: checks a file against the contract.
- `src/` and `scripts/build_report.py`: the report page. It reads one contract file and shows spend, savings, a requested-versus-target chart and a workload table.

Run the tests with `python3 -m unittest discover -s tests`. Python 3, standard library only.

## Report page

`python3 scripts/build_report.py [OUTDIR]` writes `OUTDIR/index.html` (default `dist/`): one file with the scripts, styles and fonts inlined. It makes no network requests, so it works offline.

The page fetches `savings-preview.otlp.json` from the same directory, or the file named in the `data` query parameter. When the fetch fails, for example when the page is opened from disk, it asks for the file instead.

To try it with the fixture:

    python3 scripts/build_report.py dist
    cp fixtures/savings-preview.otlp.json dist/
    python3 -m http.server -d dist

The savings math is in `src/js/model.js`. The target is 1.1 times the trailing 15-minute maximum of usage, per workload. Waste is the request above the target. Cost is the larger of the CPU and memory cost, priced from the node resources when their instance types are in the built-in AWS price table, and from a 4-core, 16 GiB, $0.192 per hour node otherwise. A container missing a required series is listed but not counted.

`src/styles/primitives.css` and `src/styles/semantic.css` are copies of the dashboard's design tokens (`dashboard/src/styles/tokens/`). Re-copy them to pick up token changes; `zinc.css` supplies the Tailwind zinc values they reference.

Run the page tests with `node --test tests/report_model.test.js`.
