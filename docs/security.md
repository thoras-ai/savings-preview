# Security

Savings Preview is a command-line program that runs on your machine with a kubeconfig you choose. It installs nothing in the cluster, changes nothing in it, and sends nothing anywhere except requests to your Kubernetes API server (or to a metrics store URL you pass with `--url`). Everything it produces stays in local files.

## What it reads

Every request is a read: `list` on resources, or an HTTP `GET` through the API server's service proxy. Each source needs only the access listed for it. [`deploy/rbac/`](../deploy/rbac/) has a ServiceAccount and role for each case.

### `savings-preview prometheus`

Reads usage history from a Prometheus-compatible store (Prometheus, Thanos Query, VictoriaMetrics) in the cluster, reached through the API server.

| API group | Resource | Verb | Why |
| --- | --- | --- | --- |
| core | `services` | `list`, all namespaces | Find the store. Skipped with `--target`. |
| `monitoring.coreos.com` | `prometheuses`, `prometheusagents` | `list`, all namespaces | Optional. Ranks stores run by the Prometheus Operator and reports agent-mode instances. Without this access, or without the operator, discovery still works from service names. |
| core | `services/proxy` | `get` | Send queries to the store. |

Through the proxy it calls only these read endpoints of the store: `/api/v1/query`, `/api/v1/query_range`, `/api/v1/status/buildinfo` and `/api/v1/status/config`. The last one is read only to report where an agent-mode Prometheus sends its data; the URLs are printed with credentials removed. The queries select kube-state-metrics and cAdvisor series: container CPU and memory usage, resource requests and limits, pod owners and phase, replica counts, node allocatable, restarts and OOM kills.

With `--target namespace/service:port`, the only access needed is `get` on `services/proxy` for that one service and port, which a namespaced Role can grant. With `--url`, the program queries the store directly and needs no Kubernetes access at all.

### `savings-preview poll`

Takes readings from the live cluster at an interval when there is no metrics store.

| API group | Resource | Verb | Why |
| --- | --- | --- | --- |
| core | `pods` | `list`, all namespaces | Requests, limits and owners of running containers. |
| `apps` | `replicasets`, `deployments`, `statefulsets` | `list`, all namespaces | Map pods to workloads; replica counts. |
| core | `nodes` | `list` | Allocatable CPU and memory, instance type, region, capacity type. |
| `metrics.k8s.io` | `pods` | `list`, all namespaces | Current CPU and memory usage from metrics-server. |

Polling has no namespaced variant: it reads every namespace in one list call per resource, and nodes are cluster-scoped.

A `list` on pods returns whole pod objects, including any environment variable values written directly into the pod spec. The program keeps only the fields named above and drops the rest in memory. It never reads Secrets, ConfigMaps, logs or exec into containers, and the roles in `deploy/rbac/` do not allow it to.

## What it writes

Nothing in the cluster. None of the roles in `deploy/rbac/` grants a write verb (`create`, `update`, `patch`, `delete`) on any resource, and none grants `get` or `watch` on pods, Secrets or ConfigMaps.

On your machine it writes:

- the output file, `savings-preview.otlp.json` by default (`--out` for `prometheus`, `-o` for `poll`);
- for `poll`, a `FILE.readings/` directory holding the readings taken so far, so a stopped run can continue.

## What the output contains

- Names: cluster, namespaces, Deployments and StatefulSets, containers, nodes. The cluster name is the kubeconfig's cluster name unless `--cluster-name` sets another; on EKS that is often the cluster ARN, which includes the AWS account ID.
- Node attributes: instance type, cloud provider, region, on-demand or spot.
- Numbers over time: CPU and memory usage, requests and limits, replica counts, node allocatable CPU and memory, OOM kills.

It contains no Secrets, environment variables, logs, images, annotations, labels other than the node attributes above, or IP addresses. The format is described in [contract.md](contract.md), and the file is plain JSON you can read before sharing it.

## Where the data goes

Nowhere, unless you send it. The program makes no network requests other than to the API server, or to the `--url` store. The report page (`scripts/build_report.py`) is one offline HTML file that reads the output file in the browser and makes no network requests. Sharing the output file with Thoras is a separate step that you choose to take.

## How to check this

1. Build from source: `go build -o bin/savings-preview ./cmd/savings-preview`. The Kubernetes calls are in `internal/kube`, `internal/adapters/prometheus/discover.go` and `internal/adapters/polling/run.go`.
2. Create the read-only ServiceAccount from [`deploy/rbac/`](../deploy/rbac/) and a kubeconfig for it, as its README describes, and run the program with `--kubeconfig` pointing at that file. A request outside the role fails with a `forbidden` error.
3. List what the identity can do: `kubectl --kubeconfig savings-preview.kubeconfig auth can-i --list`.
4. Add `-v` to `savings-preview prometheus` to log each query it sends.
5. Read the output file before sharing it.
