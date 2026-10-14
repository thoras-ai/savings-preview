# Load on your cluster

Savings Preview never touches your workloads. Its reads land in one of three places, depending on the source: your metrics store, your Kubernetes API server, or the Datadog API. This page explains how much each read costs, how that cost grows with cluster size, and how to limit it on a large cluster.

| Source | What carries the load | How it grows | Worst case |
| --- | --- | --- | --- |
| `prometheus` | The metrics store (Prometheus, Thanos Query, Mimir, managed stores) | Number of running containers in the cluster | The store refuses an oversized query and the run stops |
| `poll` | The API server and metrics-server | Number of pods and ReplicaSets, once per `--interval` | Repeated cluster-wide list calls on a very large cluster |
| `datadog` | The Datadog API | Number of workload containers | Datadog rate limits slow the run |

## Prometheus-compatible stores

A run with the defaults (`--days 14`, `--step 1m`, `--concurrency 2`) sends about 250 range queries:

- Up to 18 queries: 8 for Deployments, 8 for StatefulSets, 2 for nodes.
- Each query split into one-day chunks, which is 14 or 15 chunks.
- A few instant queries to find the store and check which metrics exist.

Two queries run at a time, one after another, so the store never sees more than two of them at once.

What comes back is small. Every result is aggregated in the store by namespace, workload and container, so the run receives one series per workload container and never one per pod.

What the store has to read is larger. To attribute pods to their workloads, each query reads the per-pod container series (`container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `kube_pod_container_resource_requests` and their limits) and joins them to `kube_pod_owner`. The work for one query is about:

```
containers running in the cluster × 1,440 points (one day at 1 minute)
```

For example, a cluster with 5,000 containers needs about 7 million points per query. A cluster with 50,000 containers needs about 72 million.

Prometheus and Thanos Query stop any query that would load more than `--query.max-samples` points (50 million by default). When that happens, the store returns an error, the run stops and says which query failed, and nothing else is affected. Below that limit, the store holds the query in memory while it runs. On a Prometheus that is already close to its memory limit, run on part of the cluster first; see below.

Stores that cap the series a query may return (CloudWatch) are split into groups of namespaces automatically, so each query stays under the cap. A result that reaches a store's series limit fails the run rather than giving incomplete figures.

### Running on a large cluster

Each of these flags reduces the work per query. They can be combined.

| Flag | Effect |
| --- | --- |
| `--selector 'namespace=~"team-a\|team-b"'` | Reads only the matching namespaces. Run once per group of namespaces to cover the cluster in parts. |
| `--step 5m` | One fifth of the points per query, at a coarser resolution. |
| `--days 3` | Fewer chunks, so fewer queries in total. The work per query is the same. |
| `--concurrency 1` | One query at a time. |

On a cluster with tens of thousands of containers, start with one or two namespaces and `--days 3`, watch the store's query latency and memory, then widen.

## Polling (no metrics store)

`savings-preview poll` reads the cluster once per `--interval` (1 minute by default). Each reading makes five list calls across all namespaces (Pods, ReplicaSets, Deployments, StatefulSets, Nodes) and reads metrics-server's pod usage list. Each call returns every object of its kind in one response.

On small and medium clusters this is light: about the same as a few `kubectl get pods -A` per minute. On a cluster with tens of thousands of pods, or many old ReplicaSets kept for rollback history, each reading is several large list responses per minute for as long as the poll runs. Kubernetes 1.31 and later serve these lists from the API server's cache. Older versions read them from etcd.

On a large cluster:

- Use a metrics store if one exists. It reads history that is already there instead of collecting it.
- Otherwise, raise `--interval`, for example `--interval 5m`. The savings math uses 15-minute peaks, so 5-minute readings still give each window three samples.

## Datadog

`savings-preview datadog` sends about 110 requests to the Datadog metrics query API for a 14-day run at 1 minute, plus one probe per metric. Queries are grouped in Datadog by namespace, workload and container. Nothing is sent to your cluster. Large accounts may hit Datadog's per-organization rate limits; the run waits and retries.
