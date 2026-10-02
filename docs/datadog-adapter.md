# Datadog adapter

`savings-preview datadog` reads a cluster's usage history from the Datadog metrics query API and writes the OTel file described in [contract.md](contract.md). It needs no Kubernetes access: everything comes from the metrics the Datadog Agent (or an OpenTelemetry Collector) already sends to Datadog.

```
DD_API_KEY=... DD_APP_KEY=... savings-preview datadog --site us1 [--selector kube_cluster_name:prod-us] [--days 14] [--step 1m]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--site` | `DD_SITE`, else `us1` | The organization's site: `us1`, `us3`, `us5`, `eu1`, `ap1`, `ap2`, `uk1`, `us1-fed` or `us2-fed`. The site domain (`datadoghq.eu`, `us3.datadoghq.com`), as `DD_SITE` takes it, works too, and so does an `https://` URL for a proxy in front of the API. |
| `--api-key-file`, `--app-key-file` | none | Files holding the API key and the application key. The environment variables `DD_API_KEY` and `DD_APP_KEY` (or `DD_APPLICATION_KEY`) hold the same values; a file takes precedence. Keys are never taken from flags, so they stay out of shell history and process listings. |
| `--selector` | none | Tag filters added to every query, such as `kube_cluster_name:prod-us`. Several filters are separated by commas and must all match. `!tag:value` excludes and `*` matches any characters. |
| `--cluster-name` | the `kube_cluster_name` of the series | Value of `k8s.cluster.name`. Required when the series carry no cluster tag. |
| `--out` | `savings-preview.otlp.json` | Output file. |
| `--days` | 14 | Days of history to pull. |
| `--step` | 1m | Resolution, a whole number of minutes. |
| `--list` | off | Print the metric found for each input and the clusters, then exit. |
| `--concurrency` | 2 | Requests in flight at once. |
| `-v` | off | Log the metric chosen for each input and each pull with its series count and duration. |

## Setup for an organization that already uses Datadog

1. **Find the site.** It is the host in the browser's address bar when logged in to Datadog: `app.datadoghq.com` is `us1`, `us3.datadoghq.com` is `us3`, `us5.datadoghq.com` is `us5`, `app.datadoghq.eu` is `eu1`, `ap1.datadoghq.com` is `ap1`. `--site` takes the host as it appears there, so `--site us5.datadoghq.com` works as well as `--site us5`.
2. **Copy the API key.** Organization Settings, API Keys. Open any key and copy it. API keys cannot be scoped.
3. **Create an application key scoped to `timeseries_query`.** An unscoped application key acts with every permission of the user or service account that owns it. `timeseries_query` is the only permission the adapter's queries need.
   1. Organization Settings, Application Keys (or Personal Settings, Application Keys), New Key. Name it `savings-preview`. To keep the key off a person's account, create a service account with the Datadog Read Only Role first (Organization Settings, Service Accounts) and create the key on it.
   2. Copy the key when it is shown.
   3. Open the key, edit its scopes, and select only `timeseries_query`.

   The scope of a key on your own user can also be set through the API, with the key itself, while it is still unscoped. Save both keys to files (step 4), find the key's ID on its page in Datadog, then:

   ```
   K=~/.config/savings-preview
   printf 'DD-API-KEY: %s\nDD-APPLICATION-KEY: %s\n' "$(cat $K/dd-api-key)" "$(cat $K/dd-app-key)" |
     curl -sS -X PATCH -H @- -H 'Content-Type: application/json' \
       https://api.datadoghq.com/api/v2/current_user/application_keys/KEY_ID \
       -d '{"data":{"type":"application_keys","id":"KEY_ID","attributes":{"scopes":["timeseries_query"]}}}'
   ```

   Use the API host of your site (`api.datadoghq.eu`, `api.us5.datadoghq.com`). For a key on a service account, the path is `/api/v2/service_accounts/SERVICE_ACCOUNT_ID/application_keys/KEY_ID`, and the call needs the application key of a user who can manage service accounts, because the Read Only Role cannot change the service account's keys. The UI is simpler there. Headers read from standard input stay out of the process list. Once the key is scoped, other APIs answer it with 403 Forbidden, which is expected.
4. **Save both keys to files.** Copy each key, then on macOS:

   ```
   mkdir -p -m 700 ~/.config/savings-preview
   pbpaste > ~/.config/savings-preview/dd-api-key && chmod 600 ~/.config/savings-preview/dd-api-key
   # copy the application key
   pbpaste > ~/.config/savings-preview/dd-app-key && chmod 600 ~/.config/savings-preview/dd-app-key
   ```

5. **Run the adapter.** List what it finds first, then pull:

   ```
   K=~/.config/savings-preview
   savings-preview datadog --site us1 --api-key-file $K/dd-api-key --app-key-file $K/dd-app-key --list
   savings-preview datadog --site us1 --api-key-file $K/dd-api-key --app-key-file $K/dd-app-key \
     --selector kube_cluster_name:CLUSTER
   ```

   `--list` prints the metric chosen for each input and one `cluster:` line per cluster. Pass the cluster to `--selector`. The pull writes `savings-preview.otlp.json`.
6. **Open the report.** `savings-preview run` does not read Datadog yet, so build the page and serve it next to the file:

   ```
   python3 scripts/build_report.py dist
   cp savings-preview.otlp.json dist/
   python3 -m http.server -d dist
   ```

Before querying, the adapter checks both keys with `GET /api/v2/validate_keys`, which needs no scope. A 401 or 403 there means a key is wrong or belongs to an organization on another site. A 403 on a query means the application key lacks `timeseries_query`.

## Setup for testing without a customer organization

1. **Start a trial** at datadoghq.com and pick the US1 site. The trial lasts 14 days.
2. **Create the keys and save them to files** as in steps 2 to 4 above. The trial creates an API key at sign-up.
3. **Install the Agent on minikube.** Save these values as `datadog-minikube.yaml`:

   ```yaml
   datadog:
     site: datadoghq.com
     clusterName: minikube
     apiKeyExistingSecret: datadog-secret
     kubeStateMetricsCore:
       enabled: true
     kubelet:
       tlsVerify: false
     logs:
       enabled: false
     apm:
       portEnabled: false
     processAgent:
       enabled: false
     orchestratorExplorer:
       enabled: false
   clusterAgent:
     enabled: true
   ```

   `clusterName` becomes the `kube_cluster_name` tag. minikube's kubelet serves a self-signed certificate, hence `tlsVerify: false`. The API key goes in a Secret created from its file, never in the values:

   ```
   helm repo add datadog https://helm.datadoghq.com && helm repo update
   kubectl --context minikube create namespace datadog
   kubectl --context minikube -n datadog create secret generic datadog-secret \
     --from-file=api-key=$HOME/.config/savings-preview/dd-api-key
   helm --kube-context minikube install datadog-agent datadog/datadog -n datadog -f datadog-minikube.yaml
   kubectl --context minikube -n datadog get pods
   ```
4. **Wait for history.** Metrics show up in `--list` a few minutes after the Agent pods are ready. Give it at least 30 minutes before a pull, and expect a warning that Datadog holds less than the days asked for.
5. **Run the adapter** with a one-day window:

   ```
   K=~/.config/savings-preview
   savings-preview datadog --site us1 --api-key-file $K/dd-api-key --app-key-file $K/dd-app-key \
     --selector kube_cluster_name:minikube --days 1
   python3 scripts/validate_contract.py savings-preview.otlp.json
   ```

## Removing the test setup

```
helm --kube-context minikube uninstall datadog-agent -n datadog
kubectl --context minikube delete namespace datadog
kubectl --context minikube get crd -o name | grep datadoghq.com
```

The chart installs the Datadog Operator's CRDs as part of the release, so the uninstall removes them and the last command prints nothing. Delete the `savings-preview` application key in Datadog, and the key files, when the test is over.

## Troubleshooting

- **`CPU usage: container.cpu.usage` instead of `kubernetes.cpu.usage.total`.** The kubelet check reports per-container CPU only when the kubelet's cAdvisor metrics carry container labels, which minikube's do not. The Agent's container check sends `container.cpu.usage` in nanocores, and the adapter uses it.
- **Memory lower than `kubectl top` for databases.** Datadog's working set is memory use minus inactive file cache, the kubelet's usual definition. Where metrics-server reports the container's whole memory use, as on minikube, `kubectl top` is higher for containers with a large page cache, such as PostgreSQL. CPU, requests and memory of other containers agree to within the rollup.
- **DaemonSet pods, static pods (`etcd`, `kube-apiserver`) and bare pods are missing.** Only containers of Deployments and StatefulSets are reported.
- **Containers with zero requests.** These containers set no request. The kubelet check reports no request series for them, and the adapter writes zero while the workload has replicas.
- **`Forbidden` from other Datadog APIs** with the scoped application key is expected. The adapter needs `timeseries_query` only.
- **`less than the 1 day asked for`.** The Agent has been running for less than `--days`. The file covers what Datadog holds.
- **`the organization reports N clusters`.** Pass one of the listed clusters to `--selector`.

## What it reads

The adapter calls only `GET /api/v2/validate_keys` and `POST /api/v2/query/timeseries`. For each input it looks, in the order below, for the first metric that has series with the tags it needs in the requested window, and uses that metric. `--list` shows the choice.

| Contract input | Datadog metrics, in order | Unit |
| --- | --- | --- |
| CPU usage | `kubernetes.cpu.usage.total` (kubelet check), `container.cpu.usage` | nanocores, converted to cores; see below for `container.cpu.usage` |
| Memory working set | `kubernetes.memory.working_set`, `container.memory.working_set` | bytes |
| CPU and memory requests | `kubernetes.cpu.requests` and `kubernetes.memory.requests` (kubelet check), `kubernetes_state.container.cpu_requested` and `memory_requested` (kube-state-metrics core check), `k8s.container.cpu_request` and `k8s.container.memory_request` (OpenTelemetry) | cores, bytes |
| CPU and memory limits | the same three families: `kubernetes.cpu.limits`, `kubernetes_state.container.cpu_limit`, `k8s.container.cpu_limit`, and their memory counterparts | cores, bytes |
| Deployment replicas | `kubernetes_state.deployment.replicas_desired`, `k8s.deployment.desired` | count |
| StatefulSet replicas | `kubernetes_state.statefulset.replicas_desired`, `k8s.statefulset.desired_pods` | count |
| Node allocatable CPU and memory | `kubernetes_state.node.cpu_allocatable`, `kubernetes_state.node.memory_allocatable` | cores, bytes |

Container series are grouped by `kube_namespace`, `kube_deployment` or `kube_stateful_set`, and `kube_container_name`. The Agent's tagger puts these tags on kubelet metrics, the kube-state-metrics core check puts them on its container metrics, and Datadog maps the OpenTelemetry resource attributes `k8s.namespace.name`, `k8s.deployment.name`, `k8s.statefulset.name` and `k8s.container.name` to the same tags. The kubeletstats and k8s_cluster receivers do not set `k8s.deployment.name` or `k8s.statefulset.name` themselves: the collector needs the `k8sattributes` processor, or the run stops with no workload containers found. Series without one of the grouping tags are left out, so pods of DaemonSets, Jobs and bare pods are not reported. Replica metrics are grouped by `kube_namespace` and the workload tag; legacy kube-state-metrics check versions that kept the `namespace`, `deployment` and `statefulset` label names are tried as well. Nodes are grouped by `node`, with the region from `kube_region`.

Usage is the mean over the workload's pods at each step, rolled up per pod with `avg`. The peaks (`thoras.container.cpu.usage.max` and `thoras.container.memory.working_set.max`) come from the same query rolled up with `max`. Datadog rolls up each pod's series before it averages across pods, so a peak is the mean of each pod's highest reading within the step. That is at least the highest reading of the mean, which errs toward smaller savings. Requests and limits are the mean over pods; replicas and node allocatable the highest value in the step.

`container.cpu.usage` holds nanocores when the Agent's container check sends it and cores when the OpenTelemetry kubeletstats receiver does, under the same name. The adapter reads the largest value in the window: above 10,000 it is nanocores, otherwise cores. No container uses 10,000 cores, and nanocore readings stay below that only in a cluster that is idle.

## Requests and pulls

Datadog returns at most 1,500 points per series per query, and picks a coarse interval for long ranges unless told otherwise. Every query names the step as its rollup interval and covers at most 1,440 steps: one day at 1 minute, five days at 5 minutes. A 14-day pull at 1 minute is 14 requests per query. Requests group the queries that share a shape: the usage means of one workload kind, its usage peaks, its requests and limits, its replicas, and the nodes. A 14-day run at 1 minute sends about 110 requests, plus one probe per metric looked for.

When Datadog answers with points further apart than the step, the adapter splits the range in half and asks again, down to 60 steps, and then stops with an error that suggests a larger `--step`.

Every response's rate-limit headers are read. When `X-RateLimit-Remaining` reaches 0, or a request is refused with 429, no request starts until `X-RateLimit-Reset` seconds have passed. Up to 100 such waits per request do not count as retries. A 429 without the headers, a 5xx response or a network error is retried after 1, 2, 4 and up to 32 seconds, six times. Datadog publishes no limit for the query endpoint; lower `--concurrency` if a run spends much of its time waiting.

The pull starts `--days` before now. When usage begins later, the run warns with the date it begins.

## Organizations with several clusters

Most organizations send several clusters to Datadog. The probe groups the usage series by `kube_cluster_name`, and when there is more than one value, counting series without the tag as a value of their own, the run stops, lists the clusters and suggests a `--selector`. `--list` prints them too. The selector applies to every query, so the tag must be on kube-state-metrics series as well as on the kubelet series; `kube_cluster_name` is.

The Agent sets `kube_cluster_name` from its configured cluster name, and OpenTelemetry data from `k8s.cluster.name`. When a cluster has no name configured, its series have no cluster tag: select it with another tag the Agent sets on it, such as `env`, and pass `--cluster-name`.

## Missing data

- No CPU or memory usage metric with the namespace and container tags stops the run with the names it looked for.
- No request metric: both request metrics are listed in `thoras.missing_metrics` of every container and the run warns. Nothing is estimated.
- When a request metric exists, a container with usage but no request series has no request set: the kubelet check reports requests only for containers that set one. It gets zero requests at the times its workload has a replica count.
- No replica metric for a kind: the replica metric is listed as missing on each of its workloads, and the run warns.
- No node allocatable metric: node resources are left out and the report prices workloads with its default node.
- Gaps the series has at every pod stay gaps; the adapter never fills or carries values forward. When Datadog averages pods, it may interpolate a short gap in one pod's series from its neighbors, as it does for graphs.
- The kube-state-metrics core check reports requests for every pod it lists, including pods that are not running. The kubelet metrics, which come first, cover running containers only.

## Not covered

- **OOM kills.** The kubelet check has no OOM kill count, and the kube-state-metrics core check reports only the current termination reason, not kills over time, so `thoras.container.oom_kills` is left out.
- **Node instance type, provider and capacity type** are not on the node metrics, so they are `unknown` and the report prices nodes with its default node size. The region comes from `kube_region` when the core check sets it.
- **OpenTelemetry node metrics.** `k8s.node.allocatable_cpu` is off by default in the collector, and Datadog does not map `k8s.node.name` to a tag, so OpenTelemetry-only organizations get no node resources.

## Testing

`go test ./internal/adapters/datadog` runs the adapter against a fake organization that evaluates the queries the adapter sends, with the point cap, interval override and rate-limit headers, and replays every recording in `internal/adapters/datadog/testdata`. `fake-org.json` was recorded from the fake organization (`UPDATE_RECORDINGS=1 go test -run TestRecordFakeOrg ./internal/adapters/datadog`).

To record a live organization:

```
RECORD_DATADOG=1 DD_SITE=us1 DD_API_KEY=... DD_APP_KEY=... RECORD_DATADOG_SELECTOR=kube_cluster_name:NAME \
  go test -run TestRecordDatadog ./internal/adapters/datadog
```

This writes `testdata/live-SITE.json` with one day at 1 minute ending at the current minute, which the replay test then runs too. The recording holds the keys of no request. Cluster, namespace, workload, container, node and region names are replaced with numbered ones (`cluster-1`, `namespace-3`) in the queries, filters and responses alike, so the run replays unchanged. The usage values are kept. Values of other tags are not rewritten: a `RECORD_DATADOG_SELECTOR` on `env`, `team` or a custom cluster tag is written as is into the filters and every query. Review the file before committing it. `live-api.datadoghq.com.json` is a recording of a one-node minikube cluster with the Agent installed as above.

Checked against a live organization:

- `times` come back in milliseconds.
- Series without a grouping tag are grouped under the value `N/A`. The container queries exclude them with a wildcard on each grouping tag. On minikube, nodes have no `kube_region`, which comes back as `N/A` and is read as no region.
- The endpoint's rate limit is `query_timeseries_public`, 300 requests per 10 seconds, reported in the `X-RateLimit-*` headers.
- The Agent reports every 15 seconds. `times` list only the steps where some series has a point, so a gap in every series is a gap in `times` too.
- Usage, requests and memory agree with `kubectl top` and the pod specs, apart from the memory definition noted under Troubleshooting.

Not yet checked: whether the 1,500-point cap applies to this endpoint as it does to graphs (a check needs more than a day of history at 1 minute), whether averaging across pods interpolates gaps, and how the endpoint answers a large cluster: a usage request returns two values per container and step, about 5.8 million for 2,000 containers over a day at 1 minute, and the adapter reads at most 256 MiB of a response.
