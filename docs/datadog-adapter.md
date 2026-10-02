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

## Keys

The adapter needs an API key and an application key from the organization that holds the cluster's metrics.

- **API key:** any key of the organization (Organization Settings, API Keys). API keys cannot be scoped.
- **Application key:** an application key acts with the permissions of the user or service account that owns it, unless it is scoped. Scope it to `timeseries_query`, the only permission the adapter's queries need:
  1. Create a service account with the Datadog Read Only Role (Organization Settings, Service Accounts), or use your own user.
  2. Open the account, create an application key, and under Scopes select only `timeseries_query`.
  3. Pass the key as `DD_APP_KEY` or with `--app-key-file`.

Before querying, the adapter checks both keys with `GET /api/v2/validate_keys`, which needs no scope. A 401 or 403 there means a key is wrong or belongs to an organization on another site. A 403 on a query means the application key lacks `timeseries_query`.

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

This writes `testdata/live-SITE.json` with one day at 1 minute, which the replay test then runs too. The recording holds the keys of no request, but it does hold the organization's cluster, namespace, workload, container and node names and their usage, so review it before committing it.

Not yet checked against a live organization: whether the 1,500-point cap applies to this endpoint as it does to graphs, whether `times` come back in milliseconds as the API's example shows (the adapter accepts seconds or milliseconds), how series without a grouping tag are labeled (the adapter treats `N/A` as no value), whether averaging across pods interpolates gaps, the endpoint's rate limit, and how it answers a large cluster: a usage request returns two values per container and step, about 5.8 million for 2,000 containers over a day at 1 minute, and the adapter reads at most 256 MiB of a response.
