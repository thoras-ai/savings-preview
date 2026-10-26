# Building a report from data files

This page is for the Thoras team and for anyone who has the data files from a run and wants to build the report from them again, without reading a cluster.

## What a run writes

`savings-preview run` (with or without `--collect-only`) writes these files in the folder it runs in:

- `savings-preview.zip`: `summary.json`, `report.html` and the compact data. `report.html` is a copy of the report with the compact data inside; it opens from disk in any browser.
- `savings-preview.compact.otlp.json`: the usage data at 15-minute steps, which the report reads. It holds the peak and the mean of usage in each step, so its totals come within about 1% of the full file's. [contract.md](contract.md#compact-files) describes it.
- `savings-preview.otlp.json`: the full-resolution data, often at 1-minute steps. For 14 days of a large cluster it can be several GB, too large for the report page in a browser.
- For several clusters, `savings-preview.clusters/`, with each cluster's own files.

Send the zip. It is small, and it holds the report and the compact data, which is everything Thoras needs. The full-resolution `.otlp.json` file is not needed.

## When you have only the full-resolution files

If you have only the full `.otlp.json` files, of any size, build the report from them with `report`:

    savings-preview report savings-preview.otlp.json

`report` reads each file as a stream and compacts it with the same code as `export --compact`, so it never holds a full file in memory and the size of the file does not matter. It checks the compact data against the contract, prints the totals as `run` does, and opens the report in your browser. It reads only the files named; it connects to no cluster and uploads nothing.

Each file can be:

- a full-resolution or compact contract file (`.otlp.json`), of one cluster or several;
- a `report.html` that carries its data;
- a zip that `run` wrote, or a zip that holds one `.otlp.json` file.

In a terminal, `report` first lists the files it will read with their sizes and what it will write, and waits for Enter; `q` quits. `--yes` (`-y`) starts without waiting. `--no-open` writes the files and exits without serving the report or opening a browser, for scripts:

    savings-preview report --no-open --yes -o out savings-preview.otlp.json

## Several clusters in one report

Give one file per cluster, or files that already hold several clusters:

    savings-preview report us-east-1.otlp.json eu-west-1.otlp.json

The clusters are joined into one file of several clusters (contract version 1.4.0), the same way `run --context a --context b` writes a fleet, and the report shows each cluster and the fleet's totals. Clusters are told apart by the cluster name in the data (`k8s.cluster.name`). If two files hold the same cluster, `report` stops and names both files; leave one of them out.

## Where the output goes

`report` writes to the current directory, or to the directory given with `-o`, which it creates if needed:

- `savings-preview.compact.otlp.json`: the compact data of every cluster.
- `savings-preview.zip`: `summary.json`, `report.html` and the compact data, as `run` writes it.
- For several clusters, `savings-preview.clusters/`, with each cluster's compact file. The zip carries the same files under the same folder name.

`report` does not write or copy a full-resolution file. For that reason `summary.json` has no `full_data` field, which in a zip from `run` names the full-resolution file left next to it.

## How long it takes

Compacting reads about 130 MB a second on a laptop (Apple silicon): about 20 seconds for a 2.5 GB file. Two clusters of 2.5 GB and 2.6 GB took 42 seconds in all, and used at most 140 MB of memory. The compact data was 89 MB, and the zip 25 MB.

## Only the compact file

`savings-preview export --compact FILE` still writes the compact copy of one file, next to it as `NAME.compact.otlp.json`, or to `-o FILE`. It writes no zip and does not join clusters.

## Opening compact files in the report page

The report page also opens several files together. In an existing `report.html`, choose **Open another file** in the header, or drop files on the page, and pick the compact `.json`, `.zip` or `report.html` files, one cluster per file: the page shows them as one fleet. A cluster that appears in more than one file is counted once, from the file whose data ends latest. This works only for files small enough for the browser, such as compact files; use `report` for full-resolution files.
