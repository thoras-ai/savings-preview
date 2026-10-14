# Impact on your cluster

Savings Preview does not affect your production workloads. It runs on your laptop, installs nothing in the cluster, and never connects to your pods, nodes or applications. It only reads monitoring data that already exists, the same way a dashboard does.

## What the tool does

A run makes read-only requests to one of three places, depending on where your usage history lives:

| Source | Where the reads go | What they look like |
| --- | --- | --- |
| Prometheus-compatible store (Prometheus, Thanos, Mimir, managed services) | Your metrics store, through the Kubernetes API server | About 250 range queries over 14 days, two at a time, like opening a few 14-day dashboards one after another |
| No store (polling) | The Kubernetes API server and metrics-server | A handful of `list` calls once a minute, like running `kubectl get pods -A` |
| Datadog | The Datadog API | About 110 queries to Datadog; nothing is sent to your cluster |

## What the tool does not do

- It does not create, change or delete anything in the cluster. Every request is a read: `list` on resources, and `GET` on the store's query API. [security.md](security.md) lists every request.
- It does not run anything in the cluster: no agent, DaemonSet, sidecar or job.
- It does not contact your workloads or nodes. Requests go to the API server, the metrics store or the Datadog API only.
- It does not keep running. A run against a metrics store takes a minute or two and then stops. Polling runs only as long as you leave it running, and Ctrl-C stops it.

## Why production workloads are not affected

- **The reads go to your monitoring stack, not to your applications.** Your workloads serve traffic exactly as before; the tool never sends them a request.
- **The load is small and bounded.** Against a metrics store, the tool runs at most two queries at a time, each covering one day of history, and every result is aggregated in the store, so only one series per workload container comes back. When several clusters are read in one run, three are read at a time.
- **Your store protects itself.** Prometheus and Thanos refuse any query that would load more than their configured limit (`--query.max-samples`, 50 million points by default). If a query is too large, the store returns an error, the run stops and says which query failed, and nothing else is affected. Managed stores enforce their own limits the same way.
- **You can stop it at any time.** Ctrl-C ends a run immediately. Every command says what it will read and waits for Enter before it starts.

## Very large clusters

On most clusters the defaults are fine. On a cluster with tens of thousands of containers, or a metrics store that is already close to its memory limit, you can make the reads lighter. These options can be combined:

| Option | Effect |
| --- | --- |
| `--selector 'namespace=~"team-a\|team-b"'` | Reads only the matching namespaces. Run once per group to cover the cluster in parts. |
| `--step 5m` | One fifth of the data per query, at a coarser resolution. |
| `--days 3` | Three days of history instead of 14, so fewer queries. |
| `--interval 5m` (polling) | One reading every 5 minutes instead of every minute. The savings math uses 15-minute peaks, so this still gives three readings per window. |
| `savings-preview prometheus --concurrency 1` | One query at a time instead of two. |

A good start on a very large cluster is one or two namespaces with `--days 3`. Check the store's query latency and memory, then widen.

### How the store's work grows

For each query, the store reads the per-container usage and request series for the day it covers and groups them by workload. Its work is about the number of running containers × 1,440 points (one day at 1-minute steps): about 7 million points for 5,000 containers, and about 72 million for 50,000. That is why the largest clusters can reach the store's default 50-million-point limit, and why the options above help.

For polling, each reading is five cluster-wide `list` calls (Pods, ReplicaSets, Deployments, StatefulSets, Nodes) and one metrics-server read. Kubernetes 1.31 and later answer these from the API server's cache. On clusters with tens of thousands of pods, use a metrics store if you have one, or a longer `--interval`.
