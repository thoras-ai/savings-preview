# Savings Preview quickstart

Savings Preview shows what Thoras would save on your Kubernetes clusters, using the usage history the clusters already keep, or a live sample when they keep none. It is read-only and runs on your machine: nothing is installed in the cluster and nothing is uploaded.

This page is the short version. [getting-started.md](getting-started.md) covers each source, the report and troubleshooting in full.

## 1. Install

With [Homebrew](https://brew.sh):

    brew install thoras-ai/tap/savings-preview

Without Homebrew, with Node.js 20 or later, put `npx @thoras-ai/savings-preview@latest` wherever the steps below say `savings-preview`:

    npx @thoras-ai/savings-preview@latest --version

Or use the install script:

    curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | sh

To install from a release archive by hand instead, see [Install from the archive](getting-started.md#install-from-the-archive).

Check it worked:

    $ savings-preview --version
    savings-preview v0.2.7

## 2. Before you run

You need the same cluster access you already use for `kubectl`. `savings-preview check` tells you whether that is enough.

Find the context name for the cluster:

    kubectl config get-contexts

See what it will read, in a few seconds. This reads no history and writes nothing. It lists what it will ask and waits for Enter first:

    savings-preview check --context <context>

The last line is the answer. `ready` or `limited` means it will work; `cannot run` says what is missing. For example, on a test cluster with an hour of history:

    limited: run reads prometheus demo-prom/demo-prom-prometheus-server:80 (service labels); expect tier 3 (limited history): the store holds about 2 h of history, less than the 14 days asked for

## 3. Run it

One cluster:

    savings-preview --context <context>

It first prints what it found and what it will do, and waits:

    $ savings-preview --context dev
    Looking for usage data...

    dev: Prometheus monitoring/prometheus-server:80, 2.9 days of history

    This run only reads; it creates, changes and deletes nothing. It will:
    ...
    It writes savings-preview.otlp.json, savings-preview.compact.otlp.json and savings-preview.zip, in /Users/you. Nothing is uploaded.
    Then it opens the report in your browser.

    Press Enter to continue, or q to quit.

Press Enter, and it reads the usage and opens the report in your browser. Leave the terminal open while you look at the report; Ctrl-C closes it. In a script, add `--yes` to start without waiting.

If the cluster keeps no usage history, it says `Falling back to polling metrics-server` and samples the cluster once a minute instead, so no report opens yet. Press Ctrl-C once to build the report from what it has read so far, and again to close it. Add `--duration 10m` to have the report open by itself after ten minutes. A longer poll gives a better report: 7 days of readings cover a full week of demand.

    $ savings-preview --context minikube
    ...
    minikube: no store found, will poll metrics-server until you press Ctrl-C
    ...
    minikube: $4,862 a year in requests, $4,678 a year in savings (27 of 27 workloads counted, tier 3, 1h30m)
    wrote savings-preview.zip with a summary, report.html, a copy of the report that opens without this tool, and the compact file
    the full-resolution file stays at savings-preview.otlp.json (1.5 MB); it is not in the zip

    Report: http://127.0.0.1:55060/
    Press Ctrl-C to stop serving the report.

Several clusters, in one report:

    savings-preview --context prod-us --context prod-eu --context staging

Every cluster in your kubeconfig:

    savings-preview --all-contexts

The plan lists each cluster before anything is read, including any that cannot be read and why. The end of the output shows each cluster and the total. Here the third context did not exist:

    $ savings-preview --context minikube --context minikube-copy --context staging
    ...
    cluster        source      tier  requests/yr  savings/yr
    minikube       prometheus  3     $4,862       $4,678
    minikube-copy  prometheus  3     $4,862       $4,678
    staging        not read
    not read: staging: the kubeconfig has no context "staging". ...

    Fleet, 2 of 3 clusters: $9,724 a year in requests, $9,357 a year in savings (54 of 54 workloads counted)

`savings-preview check` takes the same flags and prints one row per cluster and a total.

The report opens on the total across all clusters, with one row per cluster. A cluster that can't be read is listed with the reason, and the others still run.

The files land in the folder you ran it from:

| File | What it is |
| --- | --- |
| `savings-preview.zip` | The one to send. Holds `report.html`, a short summary and the compact data. |
| `savings-preview.compact.otlp.json` | The data at 15-minute steps, small enough for the report page to open in a browser. |
| `savings-preview.otlp.json` | The full data. It stays on your machine and is not in the zip. |
| `savings-preview.clusters/` | With several clusters, each cluster's own files. |

## 4. Look at the result

The report opens in your browser while the command runs. To open it again later, unzip `savings-preview.zip` and open the `report.html` inside it. It opens in any browser, with no install, and makes no network requests. **Open another file** in its header, or dropping files on the page, opens other `.json`, `.zip` or `report.html` files from runs of one cluster each, together as one report.

[Read the report](getting-started.md#read-the-report) describes each section.

## 5. Access you need

Read access to the cluster through your own `kubectl` login. With Prometheus in the cluster, it lists services to find it and queries it through the services proxy. Without one, it lists pods, deployments, statefulsets, replicasets and nodes, and reads metrics-server. It never reads secrets, config maps or logs. [security.md](security.md) has the full list, and [deploy/rbac/](../deploy/rbac/README.md) has read-only ServiceAccounts if you would rather not use your own credentials.

## 6. Common problems

| What you see | What to do |
| --- | --- |
| `the kubeconfig sets no current context`, or `no configuration has been provided` | Pass `--context`, with a name from `kubectl config get-contexts`. |
| An `aws` error about an expired token or SSO session | Run `aws sso login`, then try again. |
| A `gcloud` error about credentials or reauthentication | Run `gcloud auth login`, then try again. |
| `forbidden` | The login can't read something the run needs. `savings-preview check --context <context>` shows which line says `no`. |

## Sending results to Thoras

To send the result to Thoras, add `--collect-only`: it reads and writes the same files, then exits without opening a browser, and its last line says which file to send. [send-to-thoras.md](send-to-thoras.md) walks through installing, checking access, collecting and what `savings-preview.zip` contains.
