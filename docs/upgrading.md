# Upgrading from v0.1.0

## Files written by v0.1.0 must be collected again

**What changes.** This release writes contract version 1.0.0, the first stable version of the file format (see [contract.md](contract.md)), and reads only that version. The report page, `savings-preview export --compact` and `scripts/validate_contract.py` reject data files written by v0.1.0, including the data inside a v0.1.0 `report.html` dropped onto the page. `savings-preview export` without `--compact` still writes the data out of a v0.1.0 `report.html`, as it is, and only v0.1.0 tools read the file it writes.

**Who it affects.** Anyone with data files, zips or `report.html` pages written by v0.1.0 who wants to open them with this release.

**Why.** v0.1.0 labeled its files 1.2.0. With the current format numbered 1.0.0, a check on the major version alone cannot tell a v0.1.0 file from a later 1.x file, so readers check the exact version.

**How to upgrade.** Run this release against the cluster to collect the data again. When the readings directory of a v0.1.0 poll is still on disk, export it instead: `savings-preview export -o savings-preview.otlp.json savings-preview.otlp.json.readings` writes a 1.0.0 file from the readings. Data from Prometheus-compatible stores and Datadog needs a new run.

**How to tell which you have.** The `version` of the `scope` in a v0.1.0 file is `1.2.0`. The page reports "This file is contract version 1.2.0 and this page reads only 1.0.0", `export --compact` reports `the file is contract version "1.2.0" and this release reads only 1.0.0`, and the validator reports `scope version must be 1.0.0`.

**How to roll back.** A v0.1.0 `report.html` still opens on its own, since it carries its own page. The v0.1.0 binary and its page read v0.1.0 files.

## The zip carries its data once, inside report.html

**What changes.** In v0.1.0 the zip written by `run` held `savings-preview.otlp.json`, the full contract file, as well as `summary.json` and `report.html`, which carried the same data again. The zip then held only `summary.json` and `report.html` (later releases add the compact files; see [The zip carries the compact files](#the-zip-carries-the-compact-files)), and the data inside `report.html` is a compact copy at 15-minute steps (see [contract.md](contract.md#compact-files)). The full file is not in the zip. It stays next to the zip on the machine that ran the tool, and `summary.json` names it in `full_data`.

**Who it affects.** Anyone who takes `savings-preview.otlp.json` out of a zip: their tools find no such entry. People who open `report.html` or read `summary.json` keep working: `report.html` carries a page that reads compact files, and `summary.json` keeps its fields, though its totals now come from the compact file (see below).

**Why.** Fourteen days of 1-minute data for 1,000 workload-containers is about 6.3 GB, which a browser cannot load and an email cannot carry. The compact copy is about 330 MB, and the zip holding it twice was 91 MB; with the data once it is 46 MB. A gzipped copy of the data file inside the zip would not help, since the zip already compresses each entry.

**How to upgrade.** Write the data file out of the page with `savings-preview export -o savings-preview.otlp.json report.html`; the zip comment gives the same command. The file it writes is the compact copy. For full-resolution data, ask for the file named in `summary.json` `full_data.file`, or run `savings-preview export` on the poll readings.

**How to tell which you have.** A new zip has no `savings-preview.otlp.json` entry, its `summary.json` has `compact_step_seconds` and `full_data`, and its archive comment (`unzip -z`) says that `report.html` carries a compact copy. A v0.1.0 zip has the data file and none of these.

**How to roll back.** Run v0.1.0 to get a zip with the full file, or send the full file from disk alongside the zip.

## summary.json

`step_seconds` still gives the coarsest step the data was collected at, as in v0.1.0. New fields: `compact_step_seconds` (900 when the data in the zip is compact) and `full_data` (`file` and `bytes` of the full-resolution file). Totals are computed from the compact file, so they can differ from the full file's by up to about 1%; [contract.md](contract.md#compact-files) explains why. `start`, `end`, `span_seconds` and `snapshot` also come from the compact file: its first and last steps count in full, so a run of a few minutes reports a 15-minute span and `snapshot` true.

`display_name` is the short name the report shows for `cluster`, such as `dev` for `arn:aws:eks:us-east-1:111122223333:cluster/dev`. A file of several clusters has it at the top level, listing the clusters read, and in each entry of `clusters`. `cluster` still holds the full name.

## Commands

`run` writes `savings-preview.compact.otlp.json` next to the output file in addition to the files it wrote before, and serves the compact file to the report. It reads every file it checks as a stream, so its memory stays at a few hundred MB whatever the size of the cluster: under 300 MB for 14 days of 1,000 workload-containers. The new `export` command writes compact copies and takes a report page's data back out. Existing commands and flags were unchanged in that release; [run waits for Enter in a terminal](#run-waits-for-enter-in-a-terminal) describes a later change.

## check, prometheus, poll and datadog wait for Enter in a terminal

**What changes.** Like `run`, the commands that reach a cluster or a metrics API now print what they are about to read and write and wait for Enter: `check`, `prometheus`, `poll` (and `snapshot`) and `datadog`. `q`, Ctrl-C or the end of input exits with status 0 having read nothing. They wait only when stdin, stdout and stderr are all terminals. `poll --export` and `export` read only local files and do not wait.

**Who it affects.** Anyone who drives these commands through a terminal from a program. `check` used to answer at once. Scripts, pipes, cron and CI without a terminal are not affected, and `check --json` piped to another program does not wait.

**Why.** Every command says what it will do to your clusters before it does it.

**How to keep the old behavior.** Pass `--yes` (`-y`), which every one of these commands now accepts.

**How to tell which you have.** The command prints `Press Enter to continue, or q to quit.` after the list of what it will do.

**How to roll back.** Run v0.2.5, or add `--yes`.

## run waits for Enter in a terminal

**What changes.** Before it reads any history, `run` prints a plan and waits for Enter; `q`, Ctrl-C or the end of input exits with status 0 having read nothing. It waits only when stdin, stdout and stderr are all terminals.

**Who it affects.** Anyone who drives `run` through a terminal from a program: expect scripts, `tmux send-keys`, terminal recordings. The run sits at the prompt. Scripts, pipes, cron and CI without a terminal are not affected: they print the plan and go on.

**Why.** The first thing you see is what the tool is about to read and write, before it reads anything.

**How to keep the old behavior.** Pass `--yes` (`-y`).

**How to tell which you have.** The run prints `Press Enter to continue, or q to quit.` after the plan.

**How to roll back.** Run the previous release, or add `--yes`.

## The zip carries the compact files

**What changes.** Besides `summary.json` and `report.html`, the zip holds the compact data as plain files: `savings-preview.compact.otlp.json` for one cluster, or `savings-preview.clusters/NAME.compact.otlp.json` for each cluster read in a run of several. They are the files the run leaves on disk. The full-resolution files stay out of the zip. `summary.json` and `report.html` are unchanged.

**Who it affects.** Tools that list a zip's entries and expect exactly two. A report page from an earlier release also opens a new fleet zip wrongly: it reads the first `.otlp.json` entry, so it shows one cluster's figures as if they were the whole report, with no error. A single-cluster zip opens correctly there.

**Why.** The data can be taken out of the zip without running `export`, and either the zip or the files can be opened on the report page.

**How to upgrade.** Open a new fleet zip with the report page of this release or later, or open the `report.html` inside it.

**How to tell which you have.** `unzip -l` lists `.compact.otlp.json` entries in a new zip. A fleet zip shown on an older page has one cluster in the header, where it should say the number of clusters, and no Clusters filter under Assumptions.

**How to roll back.** Open `report.html` from the zip; it carries all the clusters and its own page.

## run reads Datadog when its keys are set

**What changes.** `run` without `--source` reads Datadog first when both keys are given: `DD_API_KEY` and `DD_APP_KEY` (or `DD_APPLICATION_KEY`) in the environment, or `--api-key-file` and `--app-key-file`. Before, it ignored these variables and looked for a store in the cluster. A store named with `--url` or `--target` comes first. When Datadog has no usable data, `run` prints why and goes on as before; only a Datadog organization with several clusters stops it, with the `--selector` to pass.

**Who it affects.** People who have both Datadog keys in their shell environment for other tools and run `run` without `--source`. Their report now comes from Datadog, and `--selector` is read as Datadog tag filters. `--step` must then be a whole number of minutes. When Datadog has no usable data and the run falls back to a store in the cluster that holds several clusters, label matchers for that store can be passed only with `--source prometheus`.

**Why.** If you already send metrics to Datadog, you get 14 days of history in one command instead of starting a poll.

**How to keep the old behavior.** Unset either key for the run, for example `env -u DD_API_KEY savings-preview run`; the run then looks for a store and polls when there is none, as before. `--source prometheus` and `--source poll` also skip Datadog, but each reads only one source and does not fall back to the other.

**How to tell which you have.** The access list `run` prints first starts with "query the Datadog metrics API" when it will read Datadog, and the line after the pull says `from Datadog`. The file's `thoras.source` is `datadog`.

## Azure Monitor workspace URLs are read as Azure Monitor managed Prometheus

**What changes.** A `--url` under `prometheus.monitor.azure.com` (or the `.us` and `.cn` domains) was read as a plain Prometheus-compatible store. It is now read as Azure Monitor managed Prometheus: each query reads one metric and the adapter joins the results, queries are limited to 32 days, and without a token given directly the run gets an Entra ID token from an app registration or the Azure CLI.

**Who it affects.** People who already ran against an Azure Monitor workspace with a token in `--bearer-token-file`, `SAVINGS_PREVIEW_BEARER_TOKEN` or an `Authorization` header. The token is still used as is. The figures are the same as from the joined queries, except as [prometheus-adapter.md](prometheus-adapter.md#one-metric-per-query) describes. A workspace without per-container usage series, which the joined queries handled by splitting pod-level usage, now stops the run. An Azure Monitor URL with `--tenant` used to be read as Mimir and is now refused.

**How to keep the old behavior.** Pass `--backend prometheus`.

**How to tell which you have.** The run prints `using azure store at ...`, and `run` lists "query the Azure Monitor workspace".
