# Prometheus adapter

`savings-preview prometheus` reads a cluster's usage history from a Prometheus-compatible store and writes the OTel file described in [contract.md](contract.md). It reaches an in-cluster store through the Kubernetes API server, so nothing is installed in the cluster and no port-forward is needed. Stores outside the cluster, such as Mimir, Grafana Cloud, Amazon Managed Prometheus and Google Managed Prometheus, are queried directly by URL; see [Stores outside the cluster](#stores-outside-the-cluster).

```
savings-preview prometheus [--context NAME] [--days 14] [--step 1m] [--out savings-preview.otlp.json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig`, `--context` | standard kubeconfig rules, current context | Which cluster to read. |
| `--cluster-name` | the kubeconfig cluster name | Value of `k8s.cluster.name`. |
| `--days` | 14 | Days of history to pull. Less is pulled if the store holds less. |
| `--step` | 1m | Resolution. Use `5m` for large clusters. |
| `--target` | discovered | `[https:]namespace/service:port[/path-prefix]`, skips discovery. An `http://` or `https://` URL is the same as `--url`. |
| `--url` | none | Query a store directly instead of through the API server. Requires `--cluster-name`. |
| `--backend` | detected from the URL | `prometheus`, `mimir`, `grafana-cloud`, `amp` or `gmp`, for `--url`. |
| `--tenant` | none | Mimir tenant, sent as `X-Scope-OrgID`. Implies `--backend mimir`. |
| `--bearer-token-file`, `--basic-auth-file`, `--header-file` | none | Credentials for `--url`, read from a file so they stay out of shell history and process listings: a token, `user:password`, or `Name: value` lines. The environment variables `SAVINGS_PREVIEW_BEARER_TOKEN`, `SAVINGS_PREVIEW_BASIC_AUTH` and `SAVINGS_PREVIEW_HEADERS` hold the same values; a file takes precedence over its variable. |
| `--aws-region` | from the URL | Region of an Amazon Managed Prometheus workspace whose URL does not name it. |
| `--project` | none | Google Cloud project to read from Google Managed Prometheus, in place of `--url`. Implies `--backend gmp`. |
| `--google-credentials-file` | Application Default Credentials | Service account key for Google Managed Prometheus. |
| `--gke-metrics` | `auto` | What to read from Google Managed Prometheus: `prometheus` (cAdvisor and kube-state-metrics series), `system` (GKE system metrics), or `auto` (the first when the project has both cAdvisor and `kube_pod_owner` series, else the second). |
| `--max-query-range` | the backend's limit | Longest time range the store accepts in one query, counting range selectors. |
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

A Prometheus in agent mode has no query API. The adapter detects it (a `PrometheusAgent` resource, or a 404 from the query API with `remote_write` targets in the running config) and stops with the redacted targets, which is where the data can be read instead.

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
- When a metric the queries join on has series but none of them carries a label the joins need, the run stops and names the metric and the labels. That happens when a recording, relabeling or aggregation rule (Grafana Cloud Adaptive Metrics, for example) removed the label. The metrics and labels checked are `namespace` and `pod` on `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`; `namespace`, `pod`, `owner_kind` and `owner_name` on `kube_pod_owner`; `namespace`, `replicaset`, `owner_kind` and `owner_name` on `kube_replicaset_owner`; `namespace`, `pod`, `container` and `resource` on `kube_pod_container_resource_requests`; `namespace`, `pod` and `phase` on `kube_pod_status_phase`; `namespace` and the workload name on `kube_deployment_spec_replicas` and `kube_statefulset_replicas`; `node` and `resource` on `kube_node_status_allocatable`; and, for the pod-level usage fallback, `namespace`, `pod` and `container` on `kube_pod_container_info`.

  Earlier versions went on in this case. Depending on the label, Deployments were left out with a warning that `kube_replicaset_owner` was missing, requests were reported as unavailable, or no workload was found. Such runs now exit with status 1 and an error of the form `METRIC has series but none with the LABEL label`. There is no compatible way to keep the old behavior, because the figures it produced were incomplete or wrong. To upgrade, restore the label in the store; running the previous build restores the old behavior.

## Stores outside the cluster

`--url` (or a URL in `--target`) queries a store directly, without the API server. The kubeconfig is not used, so `--cluster-name` is required. `savings-preview run` takes the same flags; it names the cluster after the kubeconfig's when it can read one, and a failing `--url` stops it rather than falling back to polling. The backend is detected from the host: `aps-workspaces.REGION.amazonaws.com` is Amazon Managed Prometheus, `*.grafana.net` is Grafana Cloud, `monitoring.googleapis.com` is Google Managed Prometheus, a `--tenant` means Mimir, and anything else is a plain Prometheus-compatible store. `--backend` overrides the detection. The query or remote-write URL a console shows is accepted as well as the base URL.

Credentials never come from flags. Bearer tokens, basic auth and extra headers are read from files or environment variables, Amazon credentials from the standard AWS chain, and Google credentials from Application Default Credentials or a key file. They are not printed or logged; the store URL is printed without user info or query string.

Credentials plug in through `prometheus.Auth`, which wraps the HTTP client: `Header`, `Basic`, `Bearer`, `SigV4` and `Google`, combined with `Chain`. Azure OAuth is not implemented; `prometheus.Unsupported` marks where it goes.

Every store is checked for the labels the queries join on, as described in [Missing data](#missing-data). On Grafana Cloud the check matters most, because Adaptive Metrics can aggregate labels away.

### Mimir

```
savings-preview prometheus --url https://mimir.example.com --tenant TENANT --cluster-name prod-us
```

- **URL:** the gateway, query-frontend or monolithic Mimir. The Prometheus API is under `/prometheus`, which is added when the URL has no path. Give the path explicitly when the operator changed `prometheus_http_prefix` or a gateway maps it elsewhere. Cortex serves the API under `/api/prom`, so a Cortex URL needs that path.
- **Tenant:** `--tenant` sets `X-Scope-OrgID`. It is not needed when multi-tenancy is off. A multi-tenant Mimir answers `401 no org id` without it.
- **Gateway auth:** whatever the gateway in front of Mimir uses, as basic auth, a bearer token or headers. Mimir itself does no authentication.
- **What the customer provides:** the URL, the tenant ID that holds the cluster's metrics, and gateway credentials with read access to that tenant.
- **Limits:** Mimir has no range limit by default. When the operator set `max_total_query_length` or `max_partial_query_length`, the store rejects queries longer than that with `err-mimir-max-query-length`; pass the same limit as `--max-query-range`. It counts range selectors in instant queries too.

### Grafana Cloud

```
savings-preview prometheus --url https://prometheus-prod-NN-prod-REGION.grafana.net \
  --basic-auth-file ~/.config/savings-preview/grafana-basic --cluster-name prod-us --selector 'cluster="prod-us"'
```

- **URL:** the Prometheus endpoint of the stack, from the stack's Prometheus details page. `/api/prom` is added when the URL has no path, and the remote-write URL (`/api/prom/push`) is accepted.
- **Credentials:** basic auth, the Prometheus instance ID (the numeric user on the same page) as user and an access policy token as password.
- **What the customer creates:** an access policy scoped to the stack with the `metrics:read` scope, and a token for it. No other scope is needed.
- **Limits:** a range selector may cover at most 32 days; a longer one fails with `err-mimir-max-query-length`. Range queries are split by day, and the instant queries that look back over the whole `--days` window are split into pieces of at most 32 days, so `--days` beyond 32 works.
- **Adaptive Metrics:** an aggregation rule can drop the `pod`, `container` or other labels from cAdvisor and kube-state-metrics series. The run then stops and names the metric and the labels, instead of producing usage that cannot be attributed to workloads. A cluster whose container runtime does not label cAdvisor series (older containerd setups, minikube) stops the same way on Grafana Cloud, since its pod-level series cannot be told apart from aggregated ones. On Grafana Cloud usage is never split from pod-level series: there a series without a `container` label may be the sum of all of a pod's containers and the pod total, which would double the usage. The fix is to exclude the metric from Adaptive Metrics recommendations, or to keep the labels in its rule, and run again once new data has arrived.
- **Container series dropped at collection:** the Grafana k8s-monitoring chart drops cAdvisor series without a `container` label by default, so on such a runtime the store has no container usage at all. The run stops and says so when it finds `machine_memory_bytes` but no container series. The kubelet's resource endpoint labels containers on such runtimes; the minikube values below show how to collect it, although its series carry no `image` label, which the adapter needs and those values add.

#### Setup for a stack that already holds the cluster

1. **Find the URL and the instance ID.** In the Grafana Cloud portal (grafana.com, My Account), open the stack and click **Details** on the Prometheus card. Copy the **URL** (`https://prometheus-prod-NN-prod-REGION.grafana.net/api/prom`) and the **Username / Instance ID** (a number).
2. **Create a read-only token.** In the portal, **Security**, **Access Policies**, **Create access policy**. Set the realm to the stack, select only the `metrics:read` scope, and create it. On the new policy, **Add token**, and copy the token when it is shown.
3. **Save the credentials to a file.** With the token on the clipboard, on macOS:

   ```
   mkdir -p -m 700 ~/.config/savings-preview
   pbpaste > ~/.config/savings-preview/grafana-token && chmod 600 ~/.config/savings-preview/grafana-token
   (umask 077; printf '%s:%s' INSTANCE_ID "$(cat ~/.config/savings-preview/grafana-token)" > ~/.config/savings-preview/grafana-basic)
   ```

4. **Run the adapter.** A stack usually holds several clusters. Without `--selector` the run stops and lists them by label, for example `the store holds 3 clusters (label cluster: "prod-eu", "prod-us", "staging")`. Run again with the one to read:

   ```
   savings-preview prometheus --url https://prometheus-prod-NN-prod-REGION.grafana.net \
     --basic-auth-file ~/.config/savings-preview/grafana-basic --cluster-name prod-us --selector 'cluster="prod-us"'
   python3 scripts/validate_contract.py savings-preview.otlp.json
   ```

#### Setup for testing without a customer stack

1. **Start a trial** at grafana.com. It creates a stack with a Prometheus endpoint.
2. **Create a token and save it** as in steps 1 to 3 above, with the scopes `metrics:read` and `metrics:write`: the collector writes with it and the adapter reads with it. Keep the instance ID.
3. **Create the Secret** the collector reads, from the file. These steps use the namespace `monitoring`. When the cluster already has one, as with kube-prometheus-stack, pick another name and use it throughout, since step 7 deletes the namespace.

   ```
   kubectl --context minikube create namespace monitoring
   kubectl --context minikube -n monitoring create secret generic grafana-cloud-metrics \
     --from-literal=username=INSTANCE_ID --from-file=password=$HOME/.config/savings-preview/grafana-token
   ```

4. **Install k8s-monitoring.** Save these values as `grafana-cloud.yaml`, with the stack's URL and the remote-write path:

   ```yaml
   cluster:
     name: minikube
   destinations:
     grafanaCloudMetrics:
       type: prometheus
       url: https://prometheus-prod-NN-prod-REGION.grafana.net/api/prom/push
       auth:
         type: basic
       secret:
         create: false
         name: grafana-cloud-metrics
   clusterMetrics:
     enabled: true
   hostMetrics:
     enabled: true
     linuxHosts:
       enabled: true
   collectors:
     alloy-metrics:
       presets: [clustered, statefulset]
   telemetryServices:
     kube-state-metrics:
       deploy: true
     node-exporter:
       deploy: true
   ```

   `cluster.name` becomes the `cluster` label. The credentials stay in the Secret. minikube's container runtime does not label cAdvisor series, so the chart drops them all. On minikube, also save these values as `grafana-cloud-minikube.yaml`. They take container usage from the kubelet's resource endpoint, which is what metrics-server reads and which does label containers, and set the `image` label the adapter selects on:

   ```yaml
   clusterMetrics:
     kubeletResource:
       metricsTuning:
         includeMetrics:
           - container_cpu_usage_seconds_total
           - container_memory_working_set_bytes
       extraMetricProcessingRules: |
         rule {
           source_labels = ["__name__"]
           regex         = "container_cpu_usage_seconds_total|container_memory_working_set_bytes"
           target_label  = "image"
           replacement   = "unknown"
         }
   ```

   ```
   helm repo add grafana https://grafana.github.io/helm-charts && helm repo update
   helm --kube-context minikube install grafana-cloud grafana/k8s-monitoring --version 4.5.2 -n monitoring \
     -f grafana-cloud.yaml -f grafana-cloud-minikube.yaml --wait
   kubectl --context minikube -n monitoring get pods
   ```

   The chart's Fleet Management install (the Kubernetes app's setup wizard, `collectorCommon.alloy.remoteConfig`) can leave the collector with an empty remote configuration, so nothing is sent and every source shows as offline. These values write to the stack directly and need no remote configuration.
5. **Check that metrics arrive.** A minute after the pods are ready:

   ```
   printf 'user = "%s"\n' "$(cat ~/.config/savings-preview/grafana-basic)" | curl -sS -K - --get \
     https://prometheus-prod-NN-prod-REGION.grafana.net/api/prom/api/v1/query \
     --data-urlencode 'query=count by (job) ({cluster="minikube"})'
   ```

   Expect the `integrations/kubernetes/kube-state-metrics`, `integrations/kubernetes/kubelet`, `integrations/kubernetes/resources` and `integrations/node_exporter` jobs. Reading the credentials from standard input keeps them out of the process list.
6. **Run the adapter** after 10 minutes or more, with a one-day window:

   ```
   savings-preview prometheus --url https://prometheus-prod-NN-prod-REGION.grafana.net \
     --basic-auth-file ~/.config/savings-preview/grafana-basic --cluster-name minikube --selector 'cluster="minikube"' --days 1
   python3 scripts/validate_contract.py savings-preview.otlp.json
   ```

   Expect a warning that the store holds less than the day asked for. Memory, CPU and requests agree with `kubectl top pods -A --containers` and the pod specs, CPU to within its 5-minute rate window.
7. **Remove the test setup:**

   ```
   helm --kube-context minikube uninstall grafana-cloud -n monitoring
   kubectl --context minikube delete namespace monitoring
   kubectl --context minikube delete crd alloys.collectors.grafana.com
   ```

   Helm leaves the Alloy CRD in place, hence the last command; skip it when another Alloy Operator install uses the CRD. Delete the access policy in the portal, and the two credential files, when the test is over.

`internal/adapters/prometheus/testdata/grafana-cloud.json` is a recording of a run against a trial stack set up this way. To record another:

```
RECORD_GRAFANA_CLOUD_URL=https://prometheus-prod-NN-prod-REGION.grafana.net \
RECORD_GRAFANA_CLOUD_AUTH_FILE=$HOME/.config/savings-preview/grafana-basic RECORD_SELECTOR='cluster="minikube"' \
  go test -run TestRecordGrafanaCloud ./internal/adapters/prometheus
```

It holds one day at 1 minute, ending at the current minute. Cluster, namespace, workload, pod, container and node names are replaced with numbered ones (`cluster-1`, `namespace-3`) in the queries, the selector and the responses alike, labels the adapter does not read are dropped, and provider IDs keep only their scheme. Usage values are kept. Review the file before committing it.

### Amazon Managed Prometheus

```
AWS_PROFILE=readonly savings-preview prometheus \
  --url https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-0123abcd --cluster-name prod-us
```

- **URL:** the workspace endpoint, `https://aps-workspaces.REGION.amazonaws.com/workspaces/WORKSPACE_ID`, or the query URL the console shows. The region comes from the host. VPC endpoint host names are recognized too. For any other host, pass `--backend amp --aws-region REGION`.
- **Credentials:** every request is signed with SigV4 for the `aps` service. Credentials come from the standard AWS chain: environment variables, `AWS_PROFILE` with the shared config and credentials files (including SSO and assumed roles), web identity, and the container or instance role. Bearer tokens and basic auth are refused for this backend.
- **What the customer grants:** an IAM identity with this policy. The adapter calls only `/api/v1/query` and `/api/v1/query_range`, both covered by `aps:QueryMetrics`.

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": ["aps:QueryMetrics"],
      "Resource": "arn:aws:aps:REGION:ACCOUNT_ID:workspace/WORKSPACE_ID"
    }]
  }
  ```

- **Limits:** a query may cover at most 95 days. Range queries are split by day, which also keeps each one within the per-24-hour sample and series budgets.
- **Metrics:** the workspace holds whatever was remote-written. The AWS managed scraper's sample configuration has no kube-state-metrics job, and without kube-state-metrics the run stops (see [Missing data](#missing-data)).

### Google Managed Prometheus and GKE

```
savings-preview prometheus --project PROJECT_ID --cluster-name prod-us
savings-preview run --project PROJECT_ID --selector 'cluster_name="prod-us"'
```

Google Cloud Monitoring answers PromQL for a whole project at `https://monitoring.googleapis.com/v1/projects/PROJECT_ID/location/global/prometheus`. `--project` builds that URL; the URL itself, or the query URL the console shows, works as `--url` too. The same endpoint serves two kinds of data, and `--gke-metrics` picks one:

- **Managed Prometheus series** (`prometheus`): cAdvisor and kube-state-metrics series collected by Managed Prometheus, read with the same queries as any other store. GKE's managed kube-state-metrics package does not include `kube_pod_owner` or `kube_pod_container_resource_requests`, so this needs a kube-state-metrics the customer runs and scrapes with Managed Prometheus.
- **GKE system metrics** (`system`): the `kubernetes.io/container/*` and `kubernetes.io/node/*` metrics GKE writes to Cloud Monitoring, free and on by default. In PromQL they are named `kubernetes_io:container_cpu_core_usage_time` and so on.

With `auto`, the default, the run reads Managed Prometheus series when the project has both cAdvisor usage and `kube_pod_owner`, and GKE system metrics otherwise. Most GKE projects end up on system metrics.

- **Credentials:** an OAuth access token from Google Application Default Credentials: `GOOGLE_APPLICATION_CREDENTIALS` (a service account key, or a workload identity federation configuration), the `gcloud auth application-default login` user, or the metadata server, which is how workload identity on GKE and a VM's service account are reached. `--google-credentials-file` names a service account key instead; it must be a key of type `service_account`. The token has the `monitoring.read` scope. Bearer tokens, basic auth and `--tenant` are refused for this backend. Headers from `--header-file` are still sent, for example `X-Goog-User-Project` to bill queries to another project.
- **What the customer grants:** the Monitoring Viewer role (`roles/monitoring.viewer`) on the project that holds the cluster's metrics, for a service account or a user. For a key file:

  ```
  gcloud iam service-accounts create savings-preview --project PROJECT_ID
  gcloud projects add-iam-policy-binding PROJECT_ID \
    --member serviceAccount:savings-preview@PROJECT_ID.iam.gserviceaccount.com --role roles/monitoring.viewer
  gcloud iam service-accounts keys create key.json \
    --iam-account savings-preview@PROJECT_ID.iam.gserviceaccount.com
  savings-preview run --project PROJECT_ID --google-credentials-file key.json
  ```

  A user who already has the role can run `gcloud auth application-default login` and skip the key. Delete the key when the run is done.
- **Several clusters:** a project often holds several clusters. Managed Prometheus series name the cluster in the `cluster` label and system metrics in `cluster_name`; `--selector` accepts either name and applies it to both kinds of series. With system metrics, the run stops when the selected series hold more than one `cluster_name` or `location`, as described in [Stores with several clusters](#stores-with-several-clusters).
- **Resolution:** Cloud Monitoring keeps about six weeks at full resolution and older data at 10-minute steps. With `--days` above 42 and a step under 10 minutes, the older days have gaps, and the run warns; pull at most 42 days or use `--step 10m`.
- **Limits:** the documentation states no range or point cap for the PromQL API, only a 200 MB response cap. Range queries are split by day.

#### GKE system metrics

| Contract metric | System metric |
| --- | --- |
| `container.cpu.usage` | `rate(kubernetes_io:container_cpu_core_usage_time[5m])` |
| `container.memory.working_set` | `kubernetes_io:container_memory_used_bytes{memory_type="non-evictable"}` |
| `k8s.container.cpu.request.desired`, `k8s.container.memory.request.desired` | `kubernetes_io:container_cpu_request_cores`, `kubernetes_io:container_memory_request_bytes` |
| `k8s.container.cpu.limit.desired`, `k8s.container.memory.limit.desired` | `kubernetes_io:container_cpu_limit_cores`, `kubernetes_io:container_memory_limit_bytes` |
| `k8s.deployment.pod.desired`, `k8s.statefulset.pod.desired` | `kube_deployment_spec_replicas`, `kube_statefulset_replicas` from GKE's managed kube-state-metrics |
| `k8s.node.cpu.allocatable`, `k8s.node.memory.allocatable` | `kubernetes_io:node_cpu_allocatable_cores`, `kubernetes_io:node_memory_allocatable_bytes` |

- Container queries select `monitored_resource="k8s_container"` and node queries `monitored_resource="k8s_node"`. A container belongs to the Deployment or StatefulSet named by its `metadata_system_top_level_controller_name` and `metadata_system_top_level_controller_type` labels; containers of other controllers are left out. Without these labels the run stops.
- The kubelet's working set has no system metric of its own. The non-evictable part of used memory is the closest match.
- Replica counts come from GKE's managed kube-state-metrics, on by default on Standard clusters from 1.29.2-gke.2000 and Autopilot clusters from 1.27.4-gke.900. Without it, the run warns and lists the replica counts as missing. When Managed Prometheus renamed the series' `namespace` label to `exported_namespace`, that label is used.
- Request metrics can take up to two minutes to become queryable, so the pull ends three minutes before now.
- Node resources take `cloud.provider` `gcp` and `cloud.region` from the node's `location` label (a zone's region for a zonal cluster). The instance type and capacity type are not in the metrics and are `unknown`.
- OOM kills are not read.
- Resources carry `thoras.source` `gke`.

With Managed Prometheus series, the run stops when `kube_pod_owner` has an `exported_namespace` label: Managed Prometheus replaced the pods' namespace with that of kube-state-metrics, so pods cannot be mapped to workloads. Scrape kube-state-metrics with a ClusterPodMonitoring, which keeps the label, or read system metrics.
