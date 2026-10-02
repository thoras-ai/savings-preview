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
| `--url`, `--bearer-token`, `--basic-auth` | none | Query a store directly instead of through the API server. Requires `--cluster-name`. |
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

## History

Before pulling, the adapter finds how far back the container usage series go: it narrows the search with `prometheus_tsdb_lowest_timestamp_seconds` when the store has it, then binary-searches on the usage metric itself. It pulls from that point, never from before the requested window, and reports when the store holds less than asked for.

Each query is split into consecutive ranges of at most one day and at most 10,000 points per series, under the 11,000 point limit of Prometheus. Series are aggregated in the store by namespace, workload and container, and queries run two at a time.

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

Usage queries keep only series with `container!=""`, `container!="POD"` and `image!=""`. The `container=""` series is the pod total, so keeping it would count every pod twice. kube-state-metrics keeps exporting the requests of finished pods, which is why requests are limited to running pods.

Instance type, region and capacity type come from node labels, so kube-state-metrics needs `--metric-labels-allowlist=nodes=[*]` (or the specific labels). Without them the node attributes are `unknown` and the report prices the cluster with its default node.

## Missing data

- Without kube-state-metrics owner series (`kube_pod_owner`) pods cannot be mapped to workloads. The adapter stops and says so.
- When kube-state-metrics exports no requests at all, both request metrics are listed in `thoras.missing_metrics` of every container and the run warns. Nothing is estimated.
- When it exports requests, a container with usage but no request series has no request set. It gets zero requests at the times the workload had a replica count.
- Gaps in a series stay gaps. Values are never filled or carried forward.
- No container usage series (for example when the kubelet cannot label container metrics) stops the run with an explanation.
- Out-of-memory kills are not pulled.

## Managed backends

Credentials plug in through `prometheus.Auth`, which wraps the HTTP client. Bearer token and basic auth are implemented. SigV4 (Amazon Managed Prometheus), Google and Azure OAuth are not; `prometheus.Unsupported` marks where they go.
