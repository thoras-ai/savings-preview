# Getting started

Savings Preview runs on your machine with your existing kubeconfig. It reads the CPU and memory usage history you already keep, or samples usage from metrics-server when there is none, compares it with what your workloads request, and shows what right-sizing would save. Nothing is installed in the cluster and nothing is uploaded.

> **What this reads and writes**
>
> - Reads only: queries to your metrics store or Datadog, `list` on services and Prometheus Operator resources to find a store, `get` on `services/proxy` to query it, and, when it polls, `list` on pods, ReplicaSets, Deployments, StatefulSets, nodes and metrics-server usage. `check` also asks the API server who you are and what you may do. Never Secrets, ConfigMaps or logs.
> - Writes nothing in the cluster. On your machine it writes the data file, a compact copy, `savings-preview.zip` and, when it polls, a `savings-preview.otlp.json.readings/` directory.
> - Sends nothing anywhere except requests to your API server, the store you name with `--url`, or the Datadog API when you give it keys, plus, in a terminal, one lookup of the latest release on `github.com` that carries nothing about your clusters (`SAVINGS_PREVIEW_NO_UPDATE_CHECK=1` turns it off).
>
> [security.md](security.md) lists every request and what the output contains. [deploy/rbac/](../deploy/rbac/) has read-only ServiceAccounts for each source if you would rather not use your own credentials.

## Install

### Homebrew

On macOS or Linux:

    brew install thoras-ai/tap/savings-preview

Homebrew checks the archive against the checksum in the formula. Upgrade with `brew upgrade savings-preview`.

### npx

With Node.js 20 or later, run the CLI without installing it:

    npx @thoras-ai/savings-preview@latest check --context my-cluster

The first run downloads the release binary of the package's version for your machine, checks it against the release's `checksums.txt`, and keeps it in `~/Library/Caches/savings-preview` on macOS or `~/.cache/savings-preview` on Linux. Later runs start it from there. `@latest` makes npx look up the newest version each time; without it, npx can keep running a version it cached earlier. Pin a version with `npx @thoras-ai/savings-preview@0.2.6`, or install it with `npm install -g @thoras-ai/savings-preview` to get a `savings-preview` command, and upgrade that with `npm update -g @thoras-ai/savings-preview`.

| Variable | Use |
| --- | --- |
| `SAVINGS_PREVIEW_CACHE` | Directory to keep the binaries in. |
| `NODE_USE_ENV_PROXY` | Set to `1` to download through the proxy in `HTTPS_PROXY`. Node.js ignores the proxy variables without it, and reads it from version 22.21 and 24. |

### Install script

    curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | sh

`install.sh` picks the archive for your machine (macOS or Linux, amd64 or arm64), checks it against the release's `checksums.txt`, and installs `savings-preview` into `~/.local/bin` when that is on your `PATH`, or `/usr/local/bin` otherwise. It asks before using sudo. When the archive has no checksum or its checksum does not match, it stops without installing anything.

It reads these environment variables:

| Variable | Use |
| --- | --- |
| `VERSION` | Release to install, such as `v0.2.6`. Default: the latest release. |
| `INSTALL_DIR` | Directory to install into. |
| `GITHUB_TOKEN` or `GH_TOKEN` | Optional. Sent to the GitHub API when gh is not installed or not logged in, which raises its limit of 60 requests an hour. |

For example:

    curl -fsSL https://github.com/thoras-ai/savings-preview/releases/latest/download/install.sh | VERSION=v0.2.6 INSTALL_DIR=~/bin sh

Then check the install:

    savings-preview --version

### Install from the archive

To install by hand instead, get the archive for your machine and `checksums.txt` from the release. Archives are `savings-preview_<version>_<os>_<arch>.tar.gz` for `darwin` and `linux`, `amd64` and `arm64`.

Check the download, clear the macOS download flag, and unpack it:

    shasum -a 256 -c --ignore-missing checksums.txt     # Linux: sha256sum -c --ignore-missing checksums.txt
    xattr -c savings-preview_*_darwin_arm64.tar.gz 2>/dev/null
    tar -xzf savings-preview_*_darwin_arm64.tar.gz
    cd savings-preview_*_darwin_arm64
    ./savings-preview --version

On macOS, the `xattr` line clears the download flag a browser sets on the archive, which otherwise stops the binary from running because it is not notarized. It does nothing to an archive without the flag. If the binary was unpacked before the flag was cleared, or by Finder, clear it on the binary:

    xattr -c savings-preview

The commands below assume `savings-preview` is on your `PATH`. Move it to a directory such as `/usr/local/bin`, or run it as `./savings-preview`.

## Check before the first run

`savings-preview check` takes the same flags as `run` and answers in a few seconds whether `run` can work and how well. It shows the identity your kubeconfig gives, whether that identity may make each request, whether metrics-server answers, which stores it finds and how much history they hold, whether Datadog keys are valid, and which source `run` would read. It pulls no history and writes nothing. In a terminal it first lists what it will ask and waits for Enter; `--yes` skips the wait.

    $ savings-preview check --context minikube
    Kubernetes
      context minikube, cluster minikube, API server https://127.0.0.1:32771
      identity: minikube-user (groups system:masters, system:authenticated)

    Access (kubectl auth can-i)
      prometheus  list services -A                                yes
      prometheus  list prometheuses.monitoring.coreos.com -A      yes
      prometheus  list prometheusagents.monitoring.coreos.com -A  yes
      prometheus  get services/proxy -A                           yes
      poll        list pods -A                                    yes
      poll        list replicasets.apps -A                        yes
      poll        list deployments.apps -A                        yes
      poll        list statefulsets.apps -A                       yes
      poll        list nodes                                      yes
      poll        list pods.metrics.k8s.io -A                     yes

    metrics-server: answers

    Prometheus-compatible stores
      no Prometheus-compatible store found in the cluster

    Datadog: no keys (DD_API_KEY and DD_APP_KEY, or --api-key-file and --app-key-file)

    Source
      1. no  Datadog: no keys given
      2. no  a store in the cluster: no Prometheus-compatible store found in the cluster
      3. yes polling metrics-server: access allowed and metrics-server answers
      run would read: metrics-server, by polling
      why: no other source can be read
      expected tier: 3 (limited history)
      limit: polling gives tier 3 until it has 7 days of readings

    limited: run reads metrics-server, by polling; expect tier 3 (limited history): polling gives tier 3 until it has 7 days of readings

The last line sums it up. `--json` prints the same as JSON, and `--color never` (or `NO_COLOR`) leaves out the colors. The exit status is 0 when `run` would read 14 days at 1-minute steps, 1 when it would read something less, such as polling or a store with less history, and 2 when there is nothing it can read. A `no` in the access table names the request your identity is missing; a role in [deploy/rbac/](../deploy/rbac/README.md) grants it.

## Run

`savings-preview run` uses your current kubeconfig context, or `--context`. Before it reads any history it finds each cluster's source, as `check` does, and prints a plan: where it will read each cluster from, or why it cannot, what it reads, the files it writes and where, and whether it opens a browser. Then it waits: Enter goes on, and `q` or Ctrl-C quits without reading anything. It then reads, writes the files, and opens the report in your browser. Ctrl-C stops it.

It waits only when stdin, stdout and stderr are all terminals. Run from a script, a pipe or CI, or with its output sent to a file, it prints the plan and goes on. `--yes` (`-y`) skips the wait in a terminal too.

### What run does

After the plan, `run`:

1. When Datadog keys are given (`DD_API_KEY` and `DD_APP_KEY`, or `--api-key-file` and `--app-key-file`), pulls 14 days of usage from Datadog, taking the `datadog` command's `--site` and `--selector`. If Datadog has no usable data, it says why and goes on to the next step. Several clusters in the organization stop the run until `--selector` picks one.
2. Otherwise looks for a Prometheus-compatible store and pulls 14 days of usage from it (`--days`, `--step`, `--target` to name the store). `--url` reads a store outside the cluster instead, such as Mimir, Grafana Cloud, Amazon Managed Prometheus, Amazon CloudWatch (OTel Container Insights), Azure Monitor managed Prometheus or Google Managed Prometheus, with the `prometheus` command's `--backend`, `--tenant`, `--bearer-token-file`, `--basic-auth-file`, `--header-file`, `--aws-region`, `--project`, `--google-credentials-file`, `--gke-metrics`, `--azure-tenant-id`, `--azure-client-id`, `--azure-client-secret-file`, `--max-query-range` and `--selector`, and the same credential environment variables. A store named with `--url` or `--target` that fails stops the run.
3. If there is no store, none answers, or the store lacks the per-container usage and request series, it says why and polls metrics-server instead, every `--interval` until `--duration` passes or Ctrl-C. It takes the same `--keep-raw`, `--rollup` and `--write-every` flags as `poll`. Fewer than 7 days of readings give a tier 3 (limited history) report. `--source datadog`, `--source prometheus` or `--source poll` skips the choice and does not fall back.
4. Writes `savings-preview.otlp.json` (`--out` or `-o`) at full resolution, then a compact copy next to it, `savings-preview.compact.otlp.json`, at 15-minute steps that keep the peak and the mean of usage. The savings math takes its target from 15-minute peaks, so totals from the compact file come within about 1% of the full file's while it stays small enough for a browser: 14 days of 1-minute data for 1,000 workload-containers is about 6.3 GB in full and 330 MB compact. It checks the compact file against the [contract](contract.md).
5. Serves the report on the compact file on 127.0.0.1 on a random port and opens the browser; `--no-open` only prints the address. Ctrl-C stops the server.
6. Writes `savings-preview.zip` next to the file: `summary.json` (cluster, time span, tier, totals, the name and size of the full file, and `excluded_namespaces`, Thoras's own namespaces the totals leave out), `report.html`, a copy of the report with the compact file inside that opens from disk, and the compact file itself (each cluster's, in `savings-preview.clusters/`, for several clusters). The full file stays on disk and is not in the zip. Nothing is uploaded.

`--collect-only` reads and writes the same files, then exits instead of serving the report or opening a browser; see [Sending us your results](#sending-us-your-results).

`savings-preview export --compact FILE` writes the compact copy of any contract file, of a poll's readings directory (`FILE.readings`), or of the data in a report page, reading it as a stream. [contract.md](contract.md#compact-files) describes compact files and how their totals compare with the full file. [upgrading.md](upgrading.md) lists what changed in the zip and `summary.json` since v0.1.0. `savings-preview --version` prints the version.

### No metrics store

If the cluster has no Prometheus-compatible store, `run` polls metrics-server once a minute. Leave it running: 7 days of readings cover a full week of demand. Set `--duration 168h` for 7 days, or stop it with Ctrl-C at any time; the report is built from what was read. Running the same command again continues where it stopped.

    $ savings-preview run --context minikube --duration 4m
    Context minikube (cluster minikube, API server https://127.0.0.1:32771)
    This run only reads; it creates, changes and deletes nothing. It will:
      - list Services, and Prometheus Operator resources if installed, to find a metrics store
      - query that store through the API server's service proxy (GET requests only)
      - if no usable store is found, read Pods, ReplicaSets, Deployments, StatefulSets, Nodes and metrics-server pod usage at each interval
    Output goes to savings-preview.otlp.json, a 15-minute copy for the report in savings-preview.compact.otlp.json, and savings-preview.zip. Nothing is uploaded.

    Not using a metrics store: no Prometheus-compatible store found in the cluster.
    Falling back to polling metrics-server.

    Polling every 1m for 4m (Ctrl-C stops early and builds the report from what was read).
    ...
    minikube: $3,634 a year in requests, $3,078 a year in savings (31 of 31 workloads counted, tier 3, 15m)
    wrote savings-preview.zip with a summary and report.html, a copy of the report that carries the compact file and opens without this tool
    the full-resolution file stays at savings-preview.otlp.json (116 KB); it is not in the zip

    Report: http://127.0.0.1:53015/
    Press Ctrl-C to stop serving the report.

The machine has to stay awake and connected for the whole poll. For a poll of several days, run it from a host that stays up, with a token that outlives it ([deploy/rbac/](../deploy/rbac/README.md) shows how).

### Prometheus in the cluster

Prometheus, Thanos Query or VictoriaMetrics in the cluster is found automatically and queried through the API server, so no port-forward is needed. `savings-preview run` reads 14 days from it, or `--days`:

    $ savings-preview run --context minikube --days 1
    ...
    pulling 1 day of history from prometheus sp-guide/sp-guide-prom-prometheus-server:80 (service labels)
    wrote savings-preview.otlp.json from prometheus sp-guide/sp-guide-prom-prometheus-server:80 (service labels): 36 containers in 31 workloads, 1 nodes; data from 2026-10-02T11:25:31Z to 2026-10-02T11:57:34Z
    note: the store has no per-container usage series, so container usage is the pod's usage split between its containers by their requests
    warning: the store holds about 1 h of history, less than the 1 days asked for
    ...
    minikube: $3,634 a year in requests, $3,427 a year in savings (31 of 31 workloads counted, tier 3, 15m)

If it finds the wrong store, list what it sees with `savings-preview prometheus --list` and name one with `--target namespace/service:port`. A store that holds several clusters stops the run with the list of cluster labels; pass one with `--selector`, for example `--selector 'cluster="prod-us"'`.

### Grafana Cloud, Mimir, Amazon Managed Prometheus, CloudWatch or Azure Monitor

Pass the store's URL. Credentials are read from a file or the environment, never from a flag.

    # Grafana Cloud: instance ID and a token with the metrics:read scope, saved as ID:TOKEN
    savings-preview run --url https://prometheus-prod-NN-prod-REGION.grafana.net \
      --basic-auth-file ~/.config/savings-preview/grafana-basic --selector 'cluster="prod-us"'

    # Mimir
    savings-preview run --url https://mimir.example.com --tenant TENANT --bearer-token-file token

    # Amazon Managed Prometheus: the AWS identity needs aps:QueryMetrics on the workspace
    AWS_PROFILE=readonly savings-preview run \
      --url https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-12345678-abcd-1234-abcd-123456789012

    # CloudWatch with OTel Container Insights: the AWS identity needs cloudwatch:GetMetricData and cloudwatch:ListMetrics
    AWS_PROFILE=readonly savings-preview run --backend cloudwatch --aws-region us-east-1 \
      --selector '"@resource.k8s.cluster.name"="prod-us"'

    # Azure Monitor managed Prometheus: the az login identity needs Monitoring Data Reader on the workspace
    savings-preview run --url https://WORKSPACE.REGION.prometheus.monitor.azure.com --selector 'cluster="aks-prod"'

[prometheus-adapter.md](prometheus-adapter.md#stores-outside-the-cluster) covers where to find each URL and how to create read-only credentials.

### Datadog

Give `run` an API key and an application key scoped to `timeseries_query`. If Datadog has no usable data, `run` says why and goes on to the cluster; with `--source datadog` it stops instead and needs no Kubernetes access.

    K=~/.config/savings-preview
    savings-preview datadog --site us1 --api-key-file $K/dd-api-key --app-key-file $K/dd-app-key --list
    savings-preview run --site us1 --api-key-file $K/dd-api-key --app-key-file $K/dd-app-key \
      --selector kube_cluster_name:CLUSTER

`--list` prints the clusters Datadog knows about. [datadog-adapter.md](datadog-adapter.md#setup-for-an-organization-that-already-uses-datadog) shows how to find the site and create the scoped key.

## Many clusters

One run can read many clusters and write one report for all of them. Name the kubeconfig contexts with `--context` once for each, list them in a file with `--contexts-file`, or read every context in the kubeconfig with `--all-contexts`:

    savings-preview check --context prod-us --context prod-eu --context staging
    savings-preview run --context prod-us --context prod-eu --context staging
    savings-preview run --contexts-file clusters.txt
    savings-preview run --all-contexts

The contexts file has one context name per line; `#` starts a comment. Without any of these flags the run reads one cluster, the current context, as before.

Each cluster needs the same read access as a single-cluster run, in that cluster: the identity each context uses needs the access listed at the top of this page, and [deploy/rbac/](../deploy/rbac/) has the same read-only roles for each. Each cluster is read the way a single run reads it: a store found in the cluster first, then polling metrics-server. `--target`, `--days`, `--step` and the polling flags apply to every cluster. Up to three clusters are read at a time, and each line of progress starts with its context in brackets. Clusters that poll all poll in the same window, so `--duration 168h` takes a week, not a week per cluster.

A cluster that cannot be read, because its context is missing, its credentials have expired or it has no usage data, does not stop the others. The run lists it with the reason at the end and in the report's Data and method section, and fails only when no cluster can be read. Two contexts that point at the same kubeconfig cluster are each read and named after their context.

The run writes the same files as a single-cluster run, holding every cluster: `savings-preview.otlp.json`, `savings-preview.compact.otlp.json` and `savings-preview.zip`. `summary.json` in the zip keeps its fields, which now hold the fleet's totals, and adds `clusters`, one entry per cluster with its own figures, or with `error` for a cluster that was not read. `display_name` gives the short name the report shows for each cluster, such as `dev` for an EKS ARN that ends in `cluster/dev`. Each cluster's own file is kept in `savings-preview.clusters/`, and the zip carries each cluster's compact file under the same folder name.

Datadog, `--url`, `--cluster-name` and `--source datadog` name one cluster, so they work only with one context. To read clusters that live in Datadog or in one shared store, run once per cluster with `--selector`, and open the files together on the report page.

`check` with several contexts prints one line per cluster (its source, how much history the store holds, the tier and any problem) and a line for the fleet. It exits 0 when every cluster is ready, 2 when none can run, and 1 otherwise. `--timeout` applies to each cluster on its own. `--json` prints each cluster's full check.

The report page also opens several files at once: choose or drop the `.json`, `.zip` or `report.html` files from runs of one cluster each, and the page shows them as one fleet. A cluster that appears in more than one file is counted once, from the file whose data ends latest, and Data and method says which file was left out.

## Read the report

The report opens on one page:

- **The headline**: what right-sizing every workload would save a year, and its share of what the current requests cost. The line under it says where the data came from.
- **Today and with Thoras**: annual spend, utilization (usage over requests, CPU and memory weighted by price) and requested cores and GiB on each side.
- **The gap chart**: requested spend today against spend with Thoras over the window, with usage under them and the waste hatched.
- **Three workloads, before and after**: monthly spend today and with Thoras for the biggest saver that runs all day, a job that runs in bursts, and one that is already right-sized. Select one to open its drill-down.
- **Across your fleet**: the saving times the number of clusters like this one, in dollars and in loaded engineers.
- **Next step**: Book a demo, and Copy summary, which copies a few plain lines for chat or email, with a line per cluster in a report of several. With less than 7 days of data it also suggests a week-long run when the cluster was polled, or 14 days of history in the store when the data came from one.

A report of several clusters opens on the fleet: the headline and Today and with Thoras add up every cluster, each priced from its own nodes, the gap chart sums them over the window, a table lists each cluster's spend today and with Thoras, the biggest saving first, and Across your fleet counts the clusters that have workloads in the figures. Select a cluster to see page one for it alone; Show all clusters goes back. Under Assumptions, Clusters includes or excludes clusters from every figure, and the link keeps the choice. Namespaces are named `cluster/namespace`, since clusters often share namespace names.

The target for each workload is 1.1 times its highest use in any 15 minutes. Waste is the request above the target. A workload counts only while it runs, and each cluster's yearly figures are its average over the time observed. Workloads in Thoras's own namespaces, `thoras` and any starting with `thoras-`, are left out, so a cluster that already runs Thoras shows only its own workloads; Assumptions can include them. A workload costs the larger of its CPU cost and its memory cost, at per-core and per-GiB rates taken from the cluster's nodes. Utilization is usage over requests, CPU and memory weighted by price, with each workload's usage counted up to its request. A container missing a required series is listed but not counted. Nodes are priced the way the Thoras product prices them: by instance type from the list prices of the node's cloud (AWS us-east-1, Google Cloud us-central1 or Azure eastus), with spot nodes at the spot price. With no node priced, the report uses a 4-core, 16 GiB, $0.192 per hour node. See [Node prices](node-prices.md).

Details for engineers, under page one, opens the other sections:

- **Where the money is** maps savings or spend by namespace or by workload, and ranks the namespaces and the top 10 workloads. With more than 60 workloads the map opens on namespaces; select one to see its workloads. In a file with several clusters the map adds a cluster level above namespaces and opens on it past 60 workloads, a By cluster ranking comes first, and each namespace is named with its cluster, so `default` in two clusters is two namespaces.
- **Workloads** lists every Deployment and StatefulSet, with a filter. In a file with several clusters a Cluster column gives each workload's cluster; sorting by it lists each cluster as a block under its own heading, and the filter matches cluster names too. Select one to see its requested, usage and target over time with OOM kills marked, the per-pod requests Thoras would set, and what could make the saving smaller or unsafe: OOM kills, limits below the peak-safe value, or provisional data.
- **Cluster and nodes** sets the nodes' allocatable CPU and memory against what the workloads request, would need once right-sized, and use. It lists the instance types with their list prices and shows the waste as nodes that could be freed. In a file with several clusters it first shows each cluster on its own (allocatable, requested, used, nodes that could be freed and instance mix), with the figures the cluster shows when the Clusters filter is set to it alone, then the fleet total.
- **Assumptions** holds the settings every figure uses: headroom above the peak, the peak window, pricing (an instance type from the same AWS, Google Cloud and Azure list prices the default pricing uses, or your own rates per core-hour and GiB-hour), a spot or committed-use discount, an optional annual cloud bill to show the savings as a share of, the loaded engineer cost, namespaces and time range. Changing one recomputes every section. Each setting has its own Reset, and Reset to defaults clears them all.
- **Data and method** describes the file (source, window, resolution, tier, pricing), lists the workloads, containers and nodes the totals leave out or price differently and why, and spells out the calculation with the current settings. DaemonSets and bare pods are not counted.

The page address keeps the settings, so **Copy link** in the header gives a link that opens the same view. **Print summary** prints page one under the current settings.

### Links to a view

The settings live in the part of the address after `#`, so a link opens the same view. Each key is written only when it differs from its default:

| Key | Meaning | Default |
| --- | --- | --- |
| `section` | Section to open: `summary`, `money`, `workloads`, `cluster`, `assumptions` or `method` | `summary` |
| `cpu`, `gib` | Price in USD per core-hour and per GiB-hour, both or neither | From the nodes |
| `node` | Instance type the rates came from, as `cloud/region/type`; read only with `cpu` and `gib` | None |
| `discount` | Spot or committed-use discount on the rates, in percent | `0` |
| `headroom` | Target headroom over the peak, in percent | `10` |
| `peak` | Rolling peak window, in minutes | `15` |
| `ns` | Namespaces to count, comma separated | All |
| `thoras` | `1` to count Thoras's own namespaces, `thoras` and `thoras-*` | Left out |
| `cluster` | Clusters to count, in a report of several, comma separated | All |
| `from`, `to` | Time range, in Unix seconds | The whole file |
| `cost` | Loaded cost of an engineer per year, in USD | `200000` |
| `bill` | Annual cloud bill, in USD | None |
| `clusters` | Clusters like this one, for the fleet figure on page one of a single-cluster report | `1` |
| `workload` | Workload whose drill-down the workloads section shows, as `namespace/kind/name` | None |

For example, `report.html#section=workloads&headroom=20&ns=shop,data`. A value that does not parse is ignored. With the defaults, the totals match the CLI's `summary.json`.

## Share the results

Send `savings-preview.zip` to Thoras. It holds `summary.json` (cluster name, time span, confidence tier, totals, the full file's name and size, the tool version and when it was written), `report.html`, which opens from disk in any browser and carries a 15-minute copy of the data, and that same copy as `savings-preview.compact.otlp.json`, or one file per cluster in `savings-preview.clusters/` for several clusters. Open them before sending if you want to see exactly what leaves your machine.

The full-resolution file, `savings-preview.otlp.json`, stays on your machine and is not in the zip. The cluster name on EKS is often the cluster ARN, which includes the AWS account ID; set `--cluster-name` to use another name.

## Sending us your results

If you are running Savings Preview to send the result to Thoras, add `--collect-only`:

    savings-preview --context <context> --collect-only

It prints its plan and waits for Enter, reads and writes the same files as `run`, then exits without opening a browser or serving the report. Its last lines say what to send:

    Send savings-preview.zip to your Thoras contact. It holds the report and the data, and nothing else from your environment.

To run it from a script, add `--yes`, which starts without waiting for Enter. Without a terminal it never waits. `check`, `prometheus`, `poll` and `datadog` print what they will do and wait the same way, and take `--yes` too.

## Troubleshooting

| What you see | What to do |
| --- | --- |
| `the kubeconfig sets no current context` | Your kubeconfig has contexts but none is selected. Pass one of the listed contexts with `--context`, as the message shows, or select one with `kubectl config use-context`. |
| `the kubeconfig has no context "NAME"` | The context given with `--context` is not in the kubeconfig. Use one of the listed names. |
| `the kubeconfig's current context "NAME" does not exist` | The kubeconfig selects a context it does not define. Pass one of the listed contexts with `--context`, or select one with `kubectl config use-context`. |
| `the kubeconfig (PATH) has no contexts` | The file holds no cluster access. Pass another file with `--kubeconfig`, or set up access to the cluster first. |
| `no kubeconfig found` | No file at `--kubeconfig`, at the paths in `KUBECONFIG`, or at `~/.kube/config`. Pass the file with `--kubeconfig`, or set up access to the cluster first, so that `kubectl get nodes` works. |
| `Not using a metrics store: ...` and polling starts when you have a store | The reason follows the colon. Name the store with `--target namespace/service:port`, or `--url` for one outside the cluster. |
| `list services: ... forbidden` | Your identity cannot list services. Name the store with `--target`, or use a role from [deploy/rbac/](../deploy/rbac/). |
| `the store holds N clusters` or `the organization reports N clusters` | Pass one of the listed values with `--selector`, as the message shows. |
| `METRIC has series but none with the LABEL label` | A recording or aggregation rule, such as Grafana Cloud Adaptive Metrics, removed a label the queries need. Keep that label on the metric and run again once new data has arrived. |
| `less than the 14 days asked for` | The store or Datadog holds less history. The report covers what it has. |
| `401` or `403` from a store or Datadog | The message says which credential is wrong or missing scope. |
| `another poller is writing ...` | A poll with the same output file is still running. Stop it, or pass another `-o`. |
| `N running pods are not owned by a Deployment or StatefulSet` | Expected: DaemonSets, Jobs and bare pods are not counted. |
| `the store has no per-container usage series` | The container runtime does not label container usage, so each pod's usage is split between its containers by their requests. The figures are still usable. |
| Next step suggests a week-long run, or 14 days of history in the store | Less than 7 days of history. Poll longer, or keep more history in the store. |
