# Upgrading from v0.1.0

## The zip holds a compact file

**What changes.** In v0.1.0, `savings-preview.otlp.json` in the zip written by `run` was the full contract file. It is now a compact copy at 15-minute steps (contract 1.3.0, see [contract.md](contract.md#compact-files)). The entry keeps its name. The full file is no longer in the zip. It stays next to the zip on the machine that ran the tool, and `summary.json` names it in `full_data`.

**Who it affects.** Anyone who reads the data file from a zip with something other than the report page and `summary.json` of the same release: the v0.1.0 report page or validator, or their own scripts. The v0.1.0 page and other readers that do not handle `thoras.held_metrics` treat the requests and replica counts of a compact file as mostly missing and misprice it. The v0.1.0 validator rejects the fidelity tier of compact resources built from data finer than 300 seconds. People who only open `report.html` or read `summary.json` keep working: `report.html` carries a page that reads compact files, and `summary.json` keeps its fields, though its totals now come from the compact file (see below).

**Why.** Fourteen days of 1-minute data for 1,000 workload-containers is about 6.3 GB, which a browser cannot load and an email cannot carry. A zip with both files would be larger than the full file alone, so the compact copy replaces it rather than joining it.

**How to upgrade.** Read zip data files with the report page or `scripts/validate_contract.py` from this release or later. Scripts that read the file should handle `thoras.held_metrics` (a held point's value lasts until the next point) and `thoras.source_step_seconds`, as [contract.md](contract.md#held-metrics) describes. For full-resolution data, ask for the file named in `summary.json` `full_data.file`, or run `savings-preview export` on the poll readings.

**How to tell which you have.** A compact zip's `summary.json` has `compact_step_seconds` and `full_data`, its archive comment (`unzip -z`) says the file is a compact copy, and its data file's scope version is 1.3.0 with `thoras.source_step_seconds` on the compacted resources. A v0.1.0 zip has none of these.

**How to roll back.** Run v0.1.0 to get a zip with the full file, or send the full file from disk alongside the zip.

## summary.json

`step_seconds` still gives the coarsest step the data was collected at, as in v0.1.0. New fields: `compact_step_seconds` (900 when the file in the zip is compact) and `full_data` (`file` and `bytes` of the full-resolution file). Totals are computed from the compact file, so they can differ from the full file's by up to about 1%; [contract.md](contract.md#compact-files) explains why. `start`, `end`, `span_seconds` and `snapshot` also come from the compact file: its first and last steps count in full, so a run of a few minutes reports a 15-minute span and `snapshot` true.

## Commands

`run` writes `savings-preview.compact.otlp.json` next to the output file in addition to the files it wrote before, and serves the compact file to the report. The new `export` command writes compact copies. Existing commands and flags are unchanged.
