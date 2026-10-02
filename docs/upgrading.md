# Upgrading from v0.1.0

## The zip carries its data once, inside report.html

**What changes.** In v0.1.0 the zip written by `run` held `savings-preview.otlp.json`, the full contract file, as well as `summary.json` and `report.html`, which carried the same data again. The zip now holds only `summary.json` and `report.html`, and the data inside `report.html` is a compact copy at 15-minute steps (contract 1.3.0, see [contract.md](contract.md#compact-files)). The full file is not in the zip. It stays next to the zip on the machine that ran the tool, and `summary.json` names it in `full_data`.

**Who it affects.** Anyone who takes `savings-preview.otlp.json` out of a zip: their tools find no such entry. Anyone who reads compact data with the v0.1.0 report page or validator, or with their own scripts: readers that do not handle `thoras.held_metrics` treat the requests and replica counts of a compact file as mostly missing and misprice it, and the v0.1.0 validator rejects the fidelity tier of compact resources built from data finer than 300 seconds. People who open `report.html` or read `summary.json` keep working: `report.html` carries a page that reads compact files, and `summary.json` keeps its fields, though its totals now come from the compact file (see below).

**Why.** Fourteen days of 1-minute data for 1,000 workload-containers is about 6.3 GB, which a browser cannot load and an email cannot carry. The compact copy is about 330 MB, and the zip holding it twice was 91 MB; with the data once it is 46 MB. A gzipped copy of the data file inside the zip would not help, since the zip already compresses each entry.

**How to upgrade.** Write the data file out of the page with `savings-preview export -o savings-preview.otlp.json report.html`; the zip comment gives the same command. The file it writes is the compact copy. Read it with the report page or `scripts/validate_contract.py` from this release or later, and have scripts handle `thoras.held_metrics` (a held point's value lasts until the next point) and `thoras.source_step_seconds`, as [contract.md](contract.md#held-metrics) describes. For full-resolution data, ask for the file named in `summary.json` `full_data.file`, or run `savings-preview export` on the poll readings.

**How to tell which you have.** A new zip has no `savings-preview.otlp.json` entry, its `summary.json` has `compact_step_seconds` and `full_data`, and its archive comment (`unzip -z`) says that `report.html` carries a compact copy. A v0.1.0 zip has the data file and none of these.

**How to roll back.** Run v0.1.0 to get a zip with the full file, or send the full file from disk alongside the zip. `savings-preview export` also takes a v0.1.0 `report.html` and writes out the full file it carries.

## summary.json

`step_seconds` still gives the coarsest step the data was collected at, as in v0.1.0. New fields: `compact_step_seconds` (900 when the data in the zip is compact) and `full_data` (`file` and `bytes` of the full-resolution file). Totals are computed from the compact file, so they can differ from the full file's by up to about 1%; [contract.md](contract.md#compact-files) explains why. `start`, `end`, `span_seconds` and `snapshot` also come from the compact file: its first and last steps count in full, so a run of a few minutes reports a 15-minute span and `snapshot` true.

## Commands

`run` writes `savings-preview.compact.otlp.json` next to the output file in addition to the files it wrote before, and serves the compact file to the report. It reads every file it checks as a stream, so its memory stays at a few hundred MB whatever the size of the cluster: under 300 MB for 14 days of 1,000 workload-containers. The new `export` command writes compact copies and takes a report page's data back out. Existing commands and flags are unchanged.

## run reads Datadog when its keys are set

**What changes.** `run` without `--source` reads Datadog first when both keys are given: `DD_API_KEY` and `DD_APP_KEY` (or `DD_APPLICATION_KEY`) in the environment, or `--api-key-file` and `--app-key-file`. Before, it ignored these variables and looked for a store in the cluster. A store named with `--url` or `--target` comes first. When Datadog has no usable data, `run` prints why and goes on as before; only a Datadog organization with several clusters stops it, with the `--selector` to pass.

**Who it affects.** People who have both Datadog keys in their shell environment for other tools and run `run` without `--source`. Their report now comes from Datadog, and `--selector` is read as Datadog tag filters. `--step` must then be a whole number of minutes. When Datadog has no usable data and the run falls back to a store in the cluster that holds several clusters, label matchers for that store can be passed only with `--source prometheus`.

**Why.** A prospect that already sends metrics to Datadog gets 14 days of history in one command instead of starting a poll.

**How to keep the old behavior.** Unset either key for the run, for example `env -u DD_API_KEY savings-preview run`; the run then looks for a store and polls when there is none, as before. `--source prometheus` and `--source poll` also skip Datadog, but each reads only one source and does not fall back to the other.

**How to tell which you have.** The access list `run` prints first starts with "query the Datadog metrics API" when it will read Datadog, and the line after the pull says `from Datadog`. The file's `thoras.source` is `datadog`.
