# Security

Savings Preview is a command-line program that runs on your machine with a kubeconfig you choose. It installs nothing in the cluster, changes nothing in it, and sends nothing anywhere except requests to your Kubernetes API server (or to a metrics store URL you pass with `--url`, to Google Cloud Monitoring for a project you pass with `--project`, to Entra ID for a token when the `--url` store is an Azure Monitor workspace, to CloudWatch with `--backend cloudwatch`, or to the Datadog API when Datadog keys are given), apart from any credential plugin your kubeconfig runs, such as `aws eks get-token`. A kubeconfig for the ServiceAccount in `deploy/rbac/` holds a token and runs no plugin. Everything it produces stays in local files.

## What it reads

The program sends only reads: `list` on resources, and HTTP `GET` requests to four endpoints of a metrics store through the API server's service proxy. The one exception is `savings-preview check`, which also asks the API server who the identity is and what it may do; see its section below. The access a role grants can be wider than what the program uses; the service proxy section below explains where. [`deploy/rbac/`](../deploy/rbac/) has a ServiceAccount and role for each case.

### `savings-preview prometheus`

Reads usage history from a Prometheus-compatible store (Prometheus, Thanos Query, VictoriaMetrics) in the cluster, reached through the API server.

| API group | Resource | Verb | Why |
| --- | --- | --- | --- |
| core | `services` | `list`, all namespaces | Find the store. Skipped with `--target`. |
| `monitoring.coreos.com` | `prometheuses`, `prometheusagents` | `list`, all namespaces | Optional. Ranks stores run by the Prometheus Operator and reports agent-mode instances. Without this access, or without the operator, discovery still works from service names. |
| core | `services/proxy` | `get` | Send queries to the store. See below. |
| core | `nodes` | `list` | Optional. Read the instance type, region, cloud and capacity type of nodes the store's labels do not give, in one paginated list. Without this access the nodes are priced from the store's labels only. |

Through the proxy it calls only these read endpoints of the store: `/api/v1/query`, `/api/v1/query_range`, `/api/v1/status/buildinfo` and `/api/v1/status/config`. The last one is read only to report where an agent-mode Prometheus sends its data; the URLs are printed with credentials removed. The queries select kube-state-metrics and cAdvisor series: container CPU and memory usage, resource requests and limits, pod owners and phase, replica counts, node allocatable, restarts and OOM kills.

With `--project` (or a `monitoring.googleapis.com` URL) it reads Google Cloud Monitoring instead and needs no Kubernetes access. It uses a bearer token, basic auth or `Authorization` header given as for any store, or else gets an OAuth access token with the `monitoring.read` scope from Google Application Default Credentials or the service account key in `--google-credentials-file`, which means a request to Google's token endpoint or to the metadata server. It sends the credential to `monitoring.googleapis.com`. There it calls `/api/v1/query` and `/api/v1/query_range` for the project, and `/api/v1/status/buildinfo`, which Cloud Monitoring does not serve. When the first query fails with 404 (a wrong project, or the API not enabled), it also calls `/api/v1/status/config`, as for any store, to tell an agent-mode Prometheus from a wrong path. The Monitoring Viewer role (`roles/monitoring.viewer`) is enough; [prometheus-adapter.md](prometheus-adapter.md#google-managed-prometheus-and-gke) shows how to grant it. The token is not printed or logged.

With an Azure Monitor workspace URL (`*.prometheus.monitor.azure.com`) it needs no Kubernetes access either. It uses a bearer token or `Authorization` header given as for any store, or else gets an Entra ID access token for `https://prometheus.monitor.azure.com`: from the login endpoint with an app registration's client secret, or by running `az account get-access-token`, which uses the Azure CLI's own login. It sends the token only to the workspace, where it calls `/api/v1/query`, `/api/v1/query_range` and `/api/v1/status/buildinfo`, and `/api/v1/status/config` after a 404 as for any store. The Monitoring Data Reader role on the workspace is enough; [prometheus-adapter.md](prometheus-adapter.md#azure-monitor-managed-prometheus) shows how to grant it. Neither the secret nor the token is printed or logged.

With CloudWatch (`--backend cloudwatch`, or a `monitoring.REGION.amazonaws.com` URL) it needs no Kubernetes access either. It signs each request with SigV4 using AWS credentials from the standard chain (environment, shared config and credentials files, SSO, web identity, or the instance or container role), which may mean a request to AWS STS or SSO, and sends them only to `monitoring.REGION.amazonaws.com`. There it calls `/api/v1/query`, `/api/v1/query_range` and `/api/v1/status/buildinfo`, and `/api/v1/status/config` after a 404 as for any store. `cloudwatch:GetMetricData` and `cloudwatch:ListMetrics` are enough; [prometheus-adapter.md](prometheus-adapter.md#amazon-cloudwatch-promql-otel-container-insights) shows the policy. The credentials are not printed or logged.

The `list` on nodes is the same request polling makes, so `poll.yaml` and `poll-and-prometheus.yaml` already grant it; `prometheus.yaml`, `discovery.yaml` and `prometheus-namespaced.yaml` do not, and with them the run goes on without it. It is made only when the store leaves some node's instance type, cloud or capacity type unknown. The program keeps each node's name, the labels named in [Node prices](node-prices.md#where-the-node-attributes-come-from) and the provider ID's cloud, and drops the rest.

The exact PromQL is in `internal/adapters/prometheus/queries.go`, for GKE system metrics in `internal/adapters/prometheus/gke.go`, and for Azure Monitor in `internal/adapters/prometheus/split.go`. Each series comes back with all of its labels, some of which carry container images; the program keeps only namespace, pod, container, workload and node and drops the rest.

#### The service proxy grant

`get` on `services/proxy` lets the identity send an HTTP `GET` to any path on a service, not only to the store's query API. Granted cluster-wide, as in `prometheus.yaml` and `poll-and-prometheus.yaml`, it reaches every port of every service in every namespace, including any that return sensitive data or act on a `GET`. RBAC cannot limit the path. It can limit the service and port, so the narrow setup is:

1. Apply `discovery.yaml`, which grants `list` on services and the optional Prometheus Operator resources and no proxy access, and run `savings-preview prometheus --list` to find the store. Skip this step if you already know the store's service and port.
2. Delete the discovery binding (`kubectl delete clusterrolebinding savings-preview-discovery`). Apply `prometheus-namespaced.yaml`, a Role in the store's namespace that allows `get` on `services/proxy` for that one service and port, and run `savings-preview prometheus --target namespace/service:port`.

With `--url`, the program queries the store directly and needs no Kubernetes access at all.

### `savings-preview poll`

Takes readings from the live cluster at an interval when there is no metrics store.

| API group | Resource | Verb | Why |
| --- | --- | --- | --- |
| core | `pods` | `list`, all namespaces | Requests, limits and owners of running containers. |
| `apps` | `replicasets`, `deployments`, `statefulsets` | `list`, all namespaces | Map pods to workloads; replica counts. |
| core | `nodes` | `list` | Allocatable CPU and memory, instance type, region, capacity type. |
| `metrics.k8s.io` | `pods` | `list`, all namespaces | Current CPU and memory usage from metrics-server. |

Polling has no namespaced variant: it reads every namespace in one list call per resource, and nodes are cluster-scoped.

A `list` on pods returns whole pod objects: container images, commands and arguments, environment variable values written into the spec, labels and annotations (which can include `kubectl.kubernetes.io/last-applied-configuration`). The program keeps only the fields named above and drops the rest in memory. It never reads Secrets, ConfigMaps, logs or exec into containers, and the roles in `deploy/rbac/` do not allow it to.

### `savings-preview run`

The default command. It needs the access of `prometheus` (without `--target`, discovery) and, when it falls back to polling, the access of `poll`; `poll-and-prometheus.yaml` grants both. With `--source poll` it needs only `poll.yaml`, and with `--source prometheus` only the `prometheus` roles. With `--url` it needs no Kubernetes access, and reads the kubeconfig only for the cluster name when `--cluster-name` is not given. When Datadog keys are given it first makes the requests of `datadog`; with `--source datadog` it makes only those and needs no Kubernetes access. Without `--source datadog`, it may also list the nodes, as `prometheus` does, to fill in node attributes Datadog does not have. When a request is forbidden during discovery, it reports why and polls instead. Before reading anything it prints which of these reads it will make.

### `savings-preview check`

Answers, before a run, whether `run` with the same flags can read usage data. It makes the requests below only for the sources `run` would try with those flags: none to the cluster with `--url` or `--source datadog`, and none to Datadog without keys.

| API group | Resource | Verb | Why |
| --- | --- | --- | --- |
| `authentication.k8s.io` | `selfsubjectreviews` | `create` | The user and groups the API server sees, as `kubectl auth whoami`. Servers before Kubernetes 1.28 may not answer; the kubeconfig user is shown instead. |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create` | Whether the identity may make each request in the `prometheus` and `poll` tables above, as `kubectl auth can-i`. |
| core | `services` | `list`, all namespaces | Find stores, as `run` does. Skipped with `--target`. |
| `monitoring.coreos.com` | `prometheuses`, `prometheusagents` | `list`, all namespaces | Optional, as for `prometheus`. |
| core | `services/proxy` | `get` | Ask each store found whether it answers and how far back its data goes. |
| core | `nodes` | `list` | Optional. When a usable store does not label every node with an instance type, count the live nodes that have one, to say where `run` would take instance types from. |
| `metrics.k8s.io` | `pods` | `list`, all namespaces | Whether metrics-server answers. It asks for one pod; metrics-server may return them all. |

The two reviews are `create` requests in name only: the API server evaluates them and stores nothing, and the default `system:basic-user` role allows them for every authenticated user, so the roles in `deploy/rbac/` need no extra rule. A row the identity is denied shows as `no`; the check itself goes on.

Each store gets instant queries only (`/api/v1/query`, `/api/v1/status/buildinfo`, and `/api/v1/status/config` when it may run in agent mode), the same checks `prometheus` makes before it pulls, plus a search for the oldest sample of container usage and an instant query of `kube_node_labels` to count the nodes it labels with an instance type. It never calls `/api/v1/query_range`. With Datadog keys it calls only `GET /api/v2/validate_keys`. It writes nothing, locally or in the cluster.

### `savings-preview datadog`

Reads usage history from the Datadog metrics query API and needs no Kubernetes access. It sends an API key and an application key, read from files or environment variables, to the Datadog site you choose, and calls only `GET /api/v2/validate_keys` and `POST /api/v2/query/timeseries`. An application key scoped to `timeseries_query` is enough; [datadog-adapter.md](datadog-adapter.md#keys) shows how to create one. The metric queries are in `internal/adapters/datadog/metrics.go`. The keys are not printed or logged.

## What it writes

Nothing in the cluster. None of the roles in `deploy/rbac/` grants a write verb (`create`, `update`, `patch`, `delete`) on any resource, and none grants `get` or `watch` on pods, Secrets or ConfigMaps.

On your machine it writes:

- the output file, `savings-preview.otlp.json` by default (`--out` for `prometheus`, `datadog` and `run`, `-o` for `poll` and `run`);
- for `poll`, and for `run` when it polls, a `FILE.readings/` directory holding the readings taken so far, so a stopped run can continue;
- for `run` and `export --compact`, a compact copy of the output file next to it (`savings-preview.compact.otlp.json` by default): the same resources at 15-minute steps;
- for `run`, a zip next to the output file (`savings-preview.zip` by default) holding `summary.json` (cluster name, time span, fidelity tier, the savings totals, and the file name and size of the full output file), `report.html`, the report page with the compact file inside it, and the compact file itself, or each cluster's compact file under `savings-preview.clusters/` with several contexts;
- for `run` with several contexts, a `savings-preview.clusters/` directory next to the output file with each cluster's own file, its compact copy and, for a cluster that polls, its readings. The output file, the compact file and the zip hold every cluster, and `summary.json` lists them with the reason any could not be read.

## What the output contains

- Names: cluster, namespaces, Deployments and StatefulSets, containers, nodes. The cluster name is the kubeconfig's cluster name unless `--cluster-name` sets another; on EKS that is often the cluster ARN, which includes the AWS account ID.
- Node attributes: instance type, cloud provider, region, on-demand or spot, node pool, and whether the instance type came from the store or the Kubernetes API.
- Numbers over time: CPU and memory usage, requests and limits, replica counts, node allocatable CPU and memory, OOM kills.

It contains no Secrets, environment variables, logs, images, annotations, labels other than the node attributes above, or IP addresses. The format is described in [contract.md](contract.md), and the file is plain JSON you can read before sharing it.

## Where the data goes

Nowhere, unless you send it. The program makes no network requests other than to the API server, the `--url` store, Google Cloud Monitoring and Google's token endpoint with `--project`, Entra ID's token endpoint for an Azure Monitor workspace, CloudWatch with `--backend cloudwatch`, or the Datadog API. The report page (`scripts/build_report.py`, also built into the binary) is one offline HTML file that reads the output file in the browser and makes no network requests. The copy published on GitHub Pages (`build_report.py --site`) also fetches its sample files from the same site when one is picked; a file opened there is read in the browser and not uploaded. `run` serves the page and the compact file on 127.0.0.1 on a random port until you press Ctrl-C; the server answers only requests addressed to that address, so it cannot be reached from other machines or read by a web page under another name. Sharing the output file or the zip with Thoras is a separate step that you choose to take.

The npm package's `savings-preview` command also downloads the release archive and `checksums.txt` from `github.com`, or through `api.github.com` with a token, the first time it runs a version. It takes the token from `GITHUB_TOKEN`, `GH_TOKEN` or `gh auth token`, and sends it only to `api.github.com`. Later runs start the cached binary and make no requests of their own.

## How releases are built

- Release binaries are built by GitHub Actions from a tag on `main`, and published only after the full CI run passed for that commit, on `main` or on the tag. `checksums.txt` on each release has the SHA-256 of every archive and SBOM, and `install.sh`, the Homebrew formula and the npm package check the archive against it before installing or running it. The npm package holds no binary: it downloads the release archive of its own version from GitHub, and keeps the checked binary in the user cache directory.
- Each archive has a CycloneDX SBOM, `savings-preview_<tag>_<os>_<arch>.cdx.json`, listing the Go modules and the Go release built into its binary. Feed it to your vulnerability scanner, or run `govulncheck -mode binary` on the binary itself.
- CI runs `govulncheck` on every pull request, nightly, and on each release tag with that day's vulnerability database, and fails when the code calls a function with a known vulnerability, in a dependency or in the Go standard library.
- Every GitHub Action the workflows use is pinned to a commit, and Dependabot proposes updates to the actions, the Go modules and the CI tools each week.

[releasing.md](releasing.md#sboms) has the details.

## How to check this

1. Build from source: `go build -o bin/savings-preview ./cmd/savings-preview`. The Kubernetes calls are in `internal/kube`, `internal/adapters/prometheus/discover.go` and `internal/adapters/polling/run.go`.
2. Create the read-only ServiceAccount from [`deploy/rbac/`](../deploy/rbac/) and a kubeconfig for it, as its README describes, and run the program with `--kubeconfig` pointing at that file. A request outside the role fails with a `forbidden` error.
3. List what the identity can do: `kubectl --kubeconfig savings-preview.kubeconfig auth can-i --list`, or only what the program uses: `savings-preview check --kubeconfig savings-preview.kubeconfig`.
4. Add `-v` to `savings-preview prometheus` to log each pull by name, with its series count and duration. The PromQL behind each name is in `internal/adapters/prometheus/queries.go`.
5. Read the output file before sharing it.
