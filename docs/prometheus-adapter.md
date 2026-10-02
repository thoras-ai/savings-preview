# Prometheus adapter

`savings-preview prometheus` reads a cluster's usage history from a Prometheus-compatible store and writes the OTel file described in [contract.md](contract.md). It reaches the store through the Kubernetes API server, so nothing is installed in the cluster and no port-forward is needed.

```
savings-preview prometheus [--context NAME] [--days 14] [--step 1m] [--out savings-preview.otlp.json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig`, `--context` | standard kubeconfig rules, current context | Which cluster to read. |
| `--cluster-name` | the kubeconfig cluster name | Value of `k8s.cluster.name`. |
| `--days` | 14 | Days of history to pull. Less is pulled if the store holds less. |
| `--step` | 1m | Resolution. Use `5m` for large clusters. |
| `--target` | discovered | `[https:]namespace/service:port[/path-prefix]`, skips discovery. |
| `--url`, `--bearer-token-file`, `--basic-auth-file` | none | Query a store directly instead of through the API server. Requires `--cluster-name`. Credentials are read from a file (`user:password` for basic auth) so they stay out of shell history. |
| `--selector` | none | Label matchers added to every query, such as `cluster="prod-us"`, to read one cluster from a store that holds several. Values may be left unquoted (`cluster=prod-us`). |
| `--list` | off | Print what discovery finds and exit. |
| `--concurrency` | 2 | Queries in flight at once. |

## Discovery

The adapter lists the cluster's services and Prometheus Operator resources and picks a store in this order:

1. Thanos Query (`thanos-query`, not the frontend or gRPC service).
2. VictoriaMetrics (`vmselect`, with the `/select/0/prometheus` prefix, or `vmsingle`).
3. The `prometheus-operated` service next to a `Prometheus` resource.
4. Other Prometheus services, matched by the `app.kubernetes.io/name: prometheus` label or common names such as `prometheus-server`, `prometheus-k8s` and `*-prometheus`.

Thanos and VictoriaMetrics come first because they usually keep more history than the Prometheus in front of them. The first store that answers a test query is used. `--list` shows every candidate and `--target` overrides the choice.

A Prometheus in agent mode has no query API. The adapter detects it (a `PrometheusAgent` resource, or a 404 from the query API) and stops with the redacted `remote_write` targets, which is where the data can be read instead.

## Access

The kubeconfig identity needs:

- `get` on `services/proxy` in the namespace of the store. This is all that is needed with `--target`.
- `list` on `services` in all namespaces, for discovery.
- `list` on `prometheuses` and `prometheusagents` (`monitoring.coreos.com`), optional.

[`deploy/rbac/`](../deploy/rbac/) has ServiceAccounts and roles with exactly this access, and [security.md](security.md) describes what the program reads and writes.

## History

Before pulling, the adapter finds how far back the container usage series go. When the store has `prometheus_tsdb_lowest_timestamp_seconds` and the usage metric has no data before that time, the search starts there; otherwise (for example behind Thanos, where the metric describes a sidecar) it is ignored. The adapter then binary-searches on the usage metric itself, which assumes the data is continuous: an outage at a probe point makes the start look later than it is. It pulls from that point, never from before the requested window, and reports when the store holds less than asked for.

Each query is split into consecutive ranges of at most one day and at most 10,000 points per series, under the 11,000 point limit of Prometheus. Series are aggregated in the store by namespace, workload and container, and queries run two at a time. Every join deduplicates its right-hand side, so a second copy of a kube-state-metrics series (two replicas, or two scrape jobs) does not break the queries.

## Queries

Per-pod series are joined to the owning workload in PromQL. A pod belongs to a Deployment through `kube_pod_owner` and `kube_replicaset_owner`, and to a StatefulSet through `kube_pod_owner`. Pods owned by anything else are not reported.

| Contract metric | Source |
| --- | --- |
| `container.cpu.usage` | mean over pods of `rate(container_cpu_usage_seconds_total[5m])` |
| `container.memory.working_set` | mean over pods of `container_memory_working_set_bytes` |
| `k8s.container.cpu.request.desired`, `k8s.container.memory.request.desired` | mean over pods of `kube_pod_container_resource_requests`, for pods whose `kube_pod_status_phase{phase="Running"}` is 1 |
| `k8s.container.cpu.limit.desired`, `k8s.container.memory.limit.desired` | the same for `kube_pod_container_resource_limits` |
| `k8s.deployment.pod.desired`, `k8s.statefulset.pod.desired` | `kube_deployment_spec_replicas`, `kube_statefulset_replicas` |
| `k8s.node.cpu.allocatable`, `k8s.node.memory.allocatable` | `kube_node_status_allocatable` |
| node attributes | `kube_node_info` and `kube_node_labels` |
| `thoras.container.oom_kills` | difference of `kube_pod_container_status_restarts_total` from one step to the next, for containers whose `kube_pod_container_status_last_terminated_reason` is `OOMKilled`, summed over pods |

Usage queries keep only series with `container!=""`, `container!="POD"` and `image!=""`. The `container=""` series is the pod total, so keeping it would count every pod twice. kube-state-metrics keeps exporting the requests of finished pods, which is why requests are limited to running pods.

An out-of-memory kill counts when the container restarts and its last termination reason is `OOMKilled`. A kill of a child process that does not restart the container is not counted, and neither is a restart in the first step of a container's restart series, which has no earlier value to compare with. Without `kube_pod_container_status_restarts_total` no kills are reported.

## Containers without usage labels

Some container runtimes (older containerd setups, and minikube) give cAdvisor series no `container`, `pod` or `namespace` labels for containers; only the pod cgroup series (`container=""`) carries `namespace` and `pod`. When the store has no per-container usage, the adapter uses the pod cgroup series instead and splits it between the pod's containers in proportion to their requests, CPU by CPU request and memory by memory request. A container without a request gets none of the pod's usage, unless no container in the pod has one, in which case the pod is split evenly. A single-container pod gets all of it. The containers list comes from `kube_pod_container_info`, which the fallback needs.

The pod cgroup includes the pause container and any sidecar started as an init container, so their use is attributed to the regular containers. Every container resource produced this way carries `thoras.fidelity_notes` = `usage_split_from_pod_by_requests`, and the CLI prints a note.

The choice is made for the whole store. When some nodes label container series and others do not, the store has per-container usage and the pods on unlabeled nodes are left out of the usage averages; the run warns with their number.

## Stores with several clusters

Thanos, Mimir, Amazon Managed Prometheus and Grafana Cloud often hold several clusters, told apart by a label. Before pulling, the adapter counts series by `cluster`, `cluster_name`, `cluster_id`, `k8s_cluster`, `k8s_cluster_name` and `kubernetes_cluster` in `kube_node_info` over the window, and in `kube_pod_owner` and `container_memory_working_set_bytes` now. When one of these metrics has more than one value of one label (series without the label count as a value of their own), the run stops, lists the values and suggests a `--selector`. It does the same when a selector still matches more than one value.

`--selector` is added to every series selector in every query, so the label must be on the cAdvisor and kube-state-metrics series alike. `--selector 'cluster=""'` selects the series that have no cluster label.

Earlier versions only looked at the `cluster` label on `kube_pod_owner`, and when it had several values they warned and averaged the clusters together. Runs against such a store now exit with status 1 and an error that starts with `the store holds N clusters` and names the label and its values. There is no compatible way to keep the old behavior: averaging workloads across clusters gives wrong usage and savings figures. To upgrade, add `--selector` with one of the listed values, or `--selector 'cluster=""'` for the series without the label. Running the previous build restores the warning.

Instance type, region and capacity type come from node labels, so kube-state-metrics needs `--metric-labels-allowlist=nodes=[*]` (or the specific labels). Without them the node attributes are `unknown` and the report prices the cluster with its default node.

## Missing data

- Without kube-state-metrics owner series (`kube_pod_owner`) pods cannot be mapped to workloads. The adapter stops and says so.
- Without `kube_replicaset_owner`, Deployments cannot be mapped and are left out with a warning. StatefulSets are still reported.
- When kube-state-metrics exports no requests, or no `kube_pod_status_phase` to limit them to running pods, both request metrics are listed in `thoras.missing_metrics` of every container and the run warns. Nothing is estimated.
- When it exports requests, a container with usage but no request series has no request set, or had none yet. It gets zero requests at the times the workload had a replica count.
- Gaps in a series stay gaps. Values are never filled or carried forward.
- No container usage series at all, neither per container nor per pod, stops the run with an explanation. So does a store with only pod-level usage and no `kube_pod_container_info`.

## Managed backends

Credentials plug in through `prometheus.Auth`, which wraps the HTTP client. Bearer token and basic auth are implemented. SigV4 (Amazon Managed Prometheus), Google and Azure OAuth are not; `prometheus.Unsupported` marks where they go.
