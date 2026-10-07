# Savings Preview quickstart

Savings Preview shows what Thoras would save on a Kubernetes cluster, using the usage history the cluster already keeps, or a live sample when it keeps none. It is read-only and runs on your laptop: nothing is installed in the cluster and nothing is uploaded.

## 1. Install

With [gh](https://cli.github.com) logged in to an account in the thoras-ai organization:

    gh release download -R thoras-ai/savings-preview -p install.sh -O - | sh

If gh has several accounts and says `release not found`, run it as your thoras-ai account instead (replace `<account>` with your GitHub login):

    (export GH_TOKEN="$(gh auth token -h github.com -u <account>)"; gh release download -R thoras-ai/savings-preview -p install.sh -O - | sh)

Check it worked:

    $ savings-preview --version
    savings-preview v0.2.0

## 2. Before you run

You need the same cluster access you already use for `kubectl`. If `kubectl get nodes` works, Savings Preview will too.

Find the context name for the cluster:

    kubectl config get-contexts

See what it will read, in a few seconds. This reads no history and writes nothing:

    savings-preview check --context <context>

The last line is the answer. `ready` or `limited` means it will work; `cannot run` says what is missing. For example, on a test cluster with an hour of history:

    limited: run reads prometheus demo-prom/demo-prom-prometheus-server:80 (service labels); expect tier 3 (limited history): the store holds about 1 h of history, less than the 14 days asked for

## 3. Run it

One cluster:

    savings-preview --context <context>

It prints what it will read, reads 14 days of usage, and opens the report in your browser. Leave the terminal open while you look at the report; Ctrl-C closes it.

    $ savings-preview --context minikube
    Context minikube (cluster minikube, API server https://127.0.0.1:32771)
    This run only reads; it creates, changes and deletes nothing. It will:
    ...
    minikube: $1,602 a year in requests, $1,424 a year in savings (24 of 24 workloads counted, tier 3, 1h)
    wrote savings-preview.zip with a summary and report.html, a copy of the report that carries the compact file and opens without this tool
    the full-resolution file stays at savings-preview.otlp.json (904 KB); it is not in the zip

    Report: http://127.0.0.1:62351/
    Press Ctrl-C to stop serving the report.

Several clusters, in one report:

    savings-preview --context prod-us --context prod-eu --context staging

Every cluster in your kubeconfig:

    savings-preview --all-contexts

The end of the output shows each cluster and the total. Here the third context did not exist:

    $ savings-preview --context minikube --context minikube-copy --context staging
    ...
    cluster        source      tier  requests/yr  savings/yr
    minikube       prometheus  3     $4,921       $4,736
    minikube-copy  prometheus  3     $4,921       $4,736
    staging        not read
    not read: staging: the kubeconfig has no context "staging". ...

    Fleet, 2 of 3 clusters: $9,842 a year in requests, $9,473 a year in savings (48 of 48 workloads counted)

`savings-preview check` takes the same flags and prints one line per cluster.

The report opens on the total across all clusters, with one row per cluster. A cluster that can't be read is listed with the reason, and the others still run.

The files land in the folder you ran it from:

| File | What it is |
| --- | --- |
| `savings-preview.zip` | The one to send. Holds `report.html` and a short summary. |
| `savings-preview.compact.otlp.json` | The data, small enough to open on the upload page. |
| `savings-preview.otlp.json` | The full data. It stays on the laptop. |
| `savings-preview.clusters/` | With several clusters, each cluster's own files. |

## 4. Share the result

- **Send `savings-preview.zip`**, or just the `report.html` inside it. `report.html` opens in any browser, with no install.
- **Open it on the upload page**: https://curly-dollop-gw8yzn1.pages.github.io/ takes `savings-preview.compact.otlp.json`, the zip or `report.html`. Drop several at once to combine clusters into one report. The page needs a GitHub login in the thoras-ai organization.
- **Show the demo without a cluster**: https://curly-dollop-gw8yzn1.pages.github.io/#sample=demo opens a 60-workload sample cluster. [demo.md](demo.md) walks through it in five minutes.
- **Show a fleet without a cluster**: https://curly-dollop-gw8yzn1.pages.github.io/#sample=fleet opens a sample report across several clusters.

## 5. On a call with a prospect's engineer

The download needs a thoras-ai GitHub login, so send them the file for their laptop from the [latest release](https://github.com/thoras-ai/savings-preview/releases/latest): `savings-preview_<version>_darwin_arm64.tar.gz` for an Apple silicon Mac, `darwin_amd64` for an Intel Mac, `linux_amd64` or `linux_arm64` for Linux.

Then have them paste these two commands in the folder where they saved it. The first unpacks it (and clears the macOS download flag, which otherwise stops it from running):

    xattr -c savings-preview_*.tar.gz 2>/dev/null; tar -xzf savings-preview_*.tar.gz && cd savings-preview_*/
    ./savings-preview --context <their context>

The report opens on their screen; ask them to share it. Afterwards, ask them to send you `savings-preview.zip`.

Access they need: read access to the cluster through their own `kubectl` login. It lists pods, deployments, statefulsets, replicasets, nodes and metrics-server metrics. If they run Prometheus in the cluster, it lists services to find it and queries it through the services proxy. It never reads secrets, config maps or logs. If their security team wants the full list, send [security.md](security.md).

## 6. Common problems

| What you see | What to do |
| --- | --- |
| `release not found` when installing | gh is using an account outside thoras-ai. Use the `GH_TOKEN` line in step 1. |
| `the kubeconfig sets no current context`, or `no configuration has been provided` | Pass `--context`, with a name from `kubectl config get-contexts`. |
| An `aws` error about an expired token or SSO session | Run `aws sso login`, then try again. |
| A `gcloud` error about credentials or reauthentication | Run `gcloud auth login`, then try again. |
| `forbidden` | The login can't read something the run needs. `savings-preview check --context <context>` shows which line says `no`. |
