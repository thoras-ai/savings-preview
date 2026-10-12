# Savings Preview quickstart

Savings Preview shows what Thoras would save on a Kubernetes cluster, using the usage history the cluster already keeps, or a live sample when it keeps none. It is read-only and runs on your laptop: nothing is installed in the cluster and nothing is uploaded.

## 1. Install

With [Homebrew](https://brew.sh):

    brew install thoras-ai/tap/savings-preview

Without Homebrew, with Node.js 20 or later, put `npx @thoras-ai/savings-preview` wherever the steps below say `savings-preview`:

    npx @thoras-ai/savings-preview --version

Or use the install script:

    curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | sh

Check it worked:

    $ savings-preview --version
    savings-preview v0.2.5

## 2. Before you run

You need the same cluster access you already use for `kubectl`. `savings-preview check` tells you whether that is enough.

Find the context name for the cluster:

    kubectl config get-contexts

See what it will read, in a few seconds. This reads no history and writes nothing:

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

If the cluster keeps no usage history, it says `Falling back to polling metrics-server` and samples the cluster once a minute instead, so no report opens yet. Press Ctrl-C once to build the report from what it has read so far, and again to close it. On a call, add `--duration 10m` to have the report open by itself after ten minutes.

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
| `savings-preview.compact.otlp.json` | The data, small enough to open on the upload page. |
| `savings-preview.otlp.json` | The full data. It stays on the laptop. |
| `savings-preview.clusters/` | With several clusters, each cluster's own files. |

## 4. Share the result

- **Send `savings-preview.zip`**, or just the `report.html` inside it. `report.html` opens in any browser, with no install.
- **Open it on the upload page**: https://curly-dollop-gw8yzn1.pages.github.io/ takes `savings-preview.compact.otlp.json`, the zip or `report.html`. Drop several at once to combine clusters into one report. The page needs a GitHub login in the thoras-ai organization.
- **Show the demo without a cluster**: https://curly-dollop-gw8yzn1.pages.github.io/#sample=demo opens a 60-workload sample cluster. [demo.md](demo.md) walks through it in five minutes.
- **Show a fleet without a cluster**: https://updog.link/thoras-savings-preview opens a sample report across several clusters.

## Sending us your results

When a customer runs it on their own, without a call, have them add `--collect-only`. It reads and writes the same files, does not open a browser or serve the report, and exits when done. Its last lines say what to send:

    savings-preview --context <their context> --collect-only

    Send savings-preview.zip to your Thoras contact. It holds the report and the data, and nothing else from your environment.

The zip holds `report.html`, `summary.json` and the compact data: `savings-preview.compact.otlp.json` for one cluster, or one `NAME.compact.otlp.json` per cluster under `savings-preview.clusters/`. Drop the zip on the upload page as it is, or take the JSON files out and open them there. The full-resolution data stays on their laptop.

## 5. On a call with a prospect's engineer

The download needs a thoras-ai GitHub login, so send them the file for their laptop from the [latest release](https://github.com/thoras-ai/savings-preview/releases/latest): `savings-preview_<version>_darwin_arm64.tar.gz` for an Apple silicon Mac, `darwin_amd64` for an Intel Mac, `linux_amd64` or `linux_arm64` for Linux.

Then have them paste these two commands in the folder where they saved it, with only one copy of the file there. The first unpacks it (and clears the macOS download flag, which otherwise stops it from running):

    xattr -c savings-preview_*.tar.gz 2>/dev/null; tar -xzf savings-preview_*.tar.gz && cd savings-preview_*/
    ./savings-preview --context <their context>

The report opens on their screen; ask them to share it. If it says it is polling, see the note on polling in step 3. Afterwards, ask them to send you `savings-preview.zip`.

Access they need: read access to the cluster through their own `kubectl` login. With Prometheus in the cluster, it lists services to find it and queries it through the services proxy. Without one, it lists pods, deployments, statefulsets, replicasets and nodes, and reads metrics-server. It never reads secrets, config maps or logs. If their security team wants the full list, send [security.md](security.md).

## 6. Common problems

| What you see | What to do |
| --- | --- |
| `the kubeconfig sets no current context`, or `no configuration has been provided` | Pass `--context`, with a name from `kubectl config get-contexts`. |
| An `aws` error about an expired token or SSO session | Run `aws sso login`, then try again. |
| A `gcloud` error about credentials or reauthentication | Run `gcloud auth login`, then try again. |
| `forbidden` | The login can't read something the run needs. `savings-preview check --context <context>` shows which line says `no`. |
