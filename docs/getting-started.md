# Getting started

Savings Preview runs on your machine with your existing kubeconfig. It reads the CPU and memory usage history you already keep, or samples usage from metrics-server when there is none, compares it with what your workloads request, and shows what right-sizing would save. Nothing is installed in the cluster and nothing is uploaded.

> **What this reads and writes**
>
> - Reads only: queries to your metrics store or Datadog, `list` on services and Prometheus Operator resources to find a store, `get` on `services/proxy` to query it, and, when it polls, `list` on pods, ReplicaSets, Deployments, StatefulSets, nodes and metrics-server usage. `check` also asks the API server who you are and what you may do. Never Secrets, ConfigMaps or logs.
> - Writes nothing in the cluster. On your machine it writes the data file, a compact copy, `savings-preview.zip` and, when it polls, a `savings-preview.otlp.json.readings/` directory.
> - Sends nothing anywhere except requests to your API server, the store you name with `--url`, or the Datadog API when you give it keys.
>
> [security.md](security.md) lists every request and what the output contains. [deploy/rbac/](../deploy/rbac/) has read-only ServiceAccounts for each source if you would rather not use your own credentials.

## Install

Get the archive for your machine and `checksums.txt` from the release Thoras shares with you. Archives are `savings-preview_<version>_<os>_<arch>.tar.gz` for `darwin` and `linux`, `amd64` and `arm64`.

Check the download and unpack it:

    shasum -a 256 -c --ignore-missing checksums.txt     # Linux: sha256sum -c --ignore-missing checksums.txt
    tar -xzf savings-preview_*_darwin_arm64.tar.gz
    cd savings-preview_*_darwin_arm64
    ./savings-preview --version

Unpacked with `tar` as above, the binary runs as is. If the archive was opened in Finder instead, macOS may refuse to run the binary because it is not notarized. Remove the quarantine attribute Finder copied onto it:

    xattr -d com.apple.quarantine savings-preview

The commands below assume `savings-preview` is on your `PATH`. Move it to a directory such as `/usr/local/bin`, or run it as `./savings-preview`.

## Check before the first run

`savings-preview check` takes the same flags as `run` and answers in a few seconds whether `run` can work and how well. It shows the identity your kubeconfig gives, whether that identity may make each request, whether metrics-server answers, which stores it finds and how much history they hold, whether Datadog keys are valid, and which source `run` would read. It pulls no history and writes nothing.

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

The last line sums it up. The exit status is 0 when `run` would read 14 days at 1-minute steps, 1 when it would read something less, such as polling or a store with less history, and 2 when there is nothing it can read. A `no` in the access table names the request your identity is missing; a role in [deploy/rbac/](../deploy/rbac/README.md) grants it.

## Run

`savings-preview run` uses your current kubeconfig context, or `--context`. It prints what it will read before reading anything, picks a source, writes the files, and opens the report in your browser. Ctrl-C stops it.

### No metrics store

If the cluster has no Prometheus-compatible store, `run` polls metrics-server once a minute. Leave it running: 7 days of readings give a figure you can budget on, and fewer give a provisional one. Set `--duration 168h` for 7 days, or stop it with Ctrl-C at any time; the report is built from what was read. Running the same command again continues where it stopped.

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

## Read the report

The report opens on the summary:

- **Annual savings opportunity**: what right-sizing every counted workload would save a year, and its share of projected spend.
- **Projected annual spend**: what the current requests cost a year.
- **CPU and memory that could be freed**: the requests above the target, against the total requested.
- **Confidence**: how much the history supports the figure. Under 7 days the figures are marked provisional, rounded and shown with `~`. 7 days at 5-minute steps give good confidence, and 14 days at 1-minute steps give high confidence.

The target for each workload is 1.1 times its highest use in any 15 minutes. Waste is the request above the target. Prices come from AWS on-demand list prices (us-east-1) for known instance types, and from a 4-core, 16 GiB, $0.192 per hour node otherwise.

The other sections:

- **Where the money is** maps savings or spend by namespace or by workload, and ranks the namespaces and the top 10 workloads. With more than 60 workloads the map opens on namespaces; select one to see its workloads.
- **Workloads** lists every Deployment and StatefulSet, with a filter. Select one to see its requested, usage and target over time with OOM kills marked, the per-pod requests Thoras would set, and what could make the saving smaller or unsafe: OOM kills, limits below the peak-safe value, or provisional data.
- **Cluster and nodes** sets the nodes' allocatable CPU and memory against what the workloads request, would need once right-sized, and use. It lists the instance types with their list prices and shows the waste as nodes that could be freed.
- **Assumptions** holds the settings every figure uses: headroom above the peak, the peak window, pricing (an instance type from built-in AWS, Google Cloud and Azure list prices, or your own rates per core-hour and GiB-hour), a spot or committed-use discount, an optional annual cloud bill to show the savings as a share of, the loaded engineer cost, namespaces and time range. Changing one recomputes every section. Each setting has its own Reset, and Reset to defaults clears them all.
- **Data and method** describes the file (source, window, resolution, tier, pricing), lists the workloads, containers and nodes the totals leave out or price differently and why, and spells out the calculation with the current settings. DaemonSets and bare pods are not counted.

The page address keeps the settings, so **Copy link** in the header gives a link that opens the same view. **Print summary** prints a one-page summary under the current settings.

## Share the results

Send `savings-preview.zip` to Thoras. It holds `summary.json` (cluster name, time span, confidence tier, totals, the full file's name and size, the tool version and when it was written) and `report.html`, which opens from disk in any browser and carries a 15-minute copy of the data. Open both before sending if you want to see exactly what leaves your machine.

The full-resolution file, `savings-preview.otlp.json`, stays on your machine and is not in the zip. The cluster name on EKS is often the cluster ARN, which includes the AWS account ID; set `--cluster-name` to use another name.

## Troubleshooting

| What you see | What to do |
| --- | --- |
| `Not using a metrics store: ...` and polling starts when you have a store | The reason follows the colon. Name the store with `--target namespace/service:port`, or `--url` for one outside the cluster. |
| `list services: ... forbidden` | Your identity cannot list services. Name the store with `--target`, or use a role from [deploy/rbac/](../deploy/rbac/). |
| `the store holds N clusters` or `the organization reports N clusters` | Pass one of the listed values with `--selector`, as the message shows. |
| `METRIC has series but none with the LABEL label` | A recording or aggregation rule, such as Grafana Cloud Adaptive Metrics, removed a label the queries need. Keep that label on the metric and run again once new data has arrived. |
| `less than the 14 days asked for` | The store or Datadog holds less history. The report covers what it has. |
| `401` or `403` from a store or Datadog | The message says which credential is wrong or missing scope. |
| `another poller is writing ...` | A poll with the same output file is still running. Stop it, or pass another `-o`. |
| `N running pods are not owned by a Deployment or StatefulSet` | Expected: DaemonSets, Jobs and bare pods are not counted. |
| `the store has no per-container usage series` | The container runtime does not label container usage, so each pod's usage is split between its containers by their requests. The figures are still usable. |
| Report figures marked provisional | Less than 7 days of history. Point `run` at a store with more, or poll longer. |
