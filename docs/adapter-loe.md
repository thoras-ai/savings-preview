# Adapter level of effort

Estimates for building one adapter per data source, each emitting the file described in `contract.md`. Every estimate assumes the Prometheus core and the kubectl snapshot already exist, so a PromQL-compatible source costs only its auth, chunking and name handling.

Facts were checked against vendor documentation in October 2026. Where a figure rests on a secondary source and is not confirmed in the official documentation, the text says so. Section "Not verified" lists what could not be confirmed. Vendor docs for these products change often, so re-check any limit before relying on it in code.

## Reading the table

- **Size:** S is up to 5 engineer-days, M is 6 to 10, L is more than 10. A range that straddles a band is labeled with both, for example S to M.
- **Days:** one engineer, including tests against recorded responses and a live smoke test, excluding a design review and the shared report work.
- **Inputs:** U is CPU and memory usage, R is CPU and memory requests, N is replica count, I is node inventory (instance type, region, spot, allocatable). A letter in parentheses means the source has it only under conditions given in the notes.
- **Confidence** is about the estimate, not the product: high means the API and metric names were read in docs and match a client we already have; low means a key limit or field is undocumented.

## Ranked table

Ranked by value for the effort: how likely a prospect uses the source, how few days it takes, and how well it covers the four inputs.

| # | Source | Inputs | History at 1-minute | Customer auth | Size | Days | Confidence |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | Self-hosted Mimir | U R N I as shipped | Customer's retention | Tenant header, plus whatever gateway auth the operator uses | S | 1 | High |
| 2 | Grafana Cloud | U R N I as shipped | 13 months on paid plans | Instance ID and a `metrics:read` token | S | 1.5 to 2 | High |
| 3 | Amazon Managed Prometheus | U R N I as shipped (N and R need kube-state-metrics) | As ingested, up to 1095 days retention | IAM role with query access, SigV4 | S | 2 to 3 | High |
| 4 | Azure Monitor managed Prometheus | U R N (I), usage, requests, replicas and allocatable on by default | 18 months, raw | Entra app registration, Monitoring Data Reader | S | 3 to 4 | Medium to high |
| 5 | Google Managed Prometheus | U N (R missing from the default package) | 1-minute for about 6 weeks, then 10-minute | Service account with `roles/monitoring.viewer` | S to M | 4 to 6 | Medium |
| 6 | GKE system metrics through the PromQL endpoint | U R (N, I partial) | 60-second samples for 6 weeks, then 10-minute | Same as row 5 | M | 4 to 6, plus 2 for node inventory | Medium |
| 7 | Chronosphere | As shipped | Often 5-minute after the first period | Long-lived read-only service account token | S | 2 to 3 | Low |
| 8 | AWS CloudWatch PromQL (OTel Container Insights) | U R N I | 15 months stored | IAM read, SigV4 | S | 3 to 5 | Medium |
| 9 | Datadog | U R N I | 15 months, rollup by range | API key and application key | M to L | 8 to 12 | Medium to low |
| 10 | EC2 CPU, AKS platform metrics (node fallbacks) | I and node-level U only | 5-minute, or 1-minute with detailed monitoring (EC2); 93 days at 1-minute (AKS) | IAM read, Azure Reader | S | 1 to 2 each | Medium |
| 11 | Azure Container Insights (Log Analytics) | U, I, observed replicas, no requests found | 30 days default | Log Analytics Reader | M to L | 8 to 12 | Low |
| 12 | AWS Container Insights, enhanced observability (GetMetricData) | U R N I, per pod | 15 days at 1-minute, then 5-minute | IAM read | L | 10 to 15, plus 3 to 5 for per-container requests | Medium |
| 13 | CloudTrail node reconstruction | Node churn only | 90 days | IAM read | S to M | 4 to 6 | Medium |
| 14 | Elastic | U R N I | Customer's lifecycle policy | API key with index read | S (PromQL) or M (ES\|QL) | 3 to 4, or 8 to 10 | Medium |
| 15 | New Relic | U R N (I unclear) | Not documented for the Kubernetes events | User API key | M | 7 to 10 | Medium |
| 16 | Dynatrace | U R N I | 15 months at 1-minute on the current store | API or platform token | M to L | 8 to 12 | Low to medium |
| 17 | Splunk Observability Cloud | U R N I | 13 months, 1-minute or coarser | Org access token with API scope | M to L | 8 to 11 | Low to medium |

Rows 1 to 7 are one family: the PromQL client with a different auth step. Rows 1 to 4 and 7 total about 10 to 13 days. Adding Google (row 5) brings the group to about 14 to 19 days.

## Recommended build order

1. **Auth abstraction on the PromQL client, then Mimir and Grafana Cloud.** Header, basic and bearer auth are about a day of shared work and cover rows 1, 2 and 7 with almost nothing extra. Any Prometheus-compatible store behind a plain endpoint comes free.
2. **Amazon Managed Prometheus.** SigV4 signing around the same client. Most AWS shops that run Prometheus use it.
3. **Azure managed Prometheus.** The only managed service where usage, requests, replicas and allocatable are all collected by default, so it is the cheapest to get a complete file from. Instance type, region and spot still need node labels or the compute API.
4. **Google Managed Prometheus together with GKE system metrics.** Build them as one piece of work. The managed kube-state-metrics package on GKE does not include requests, so a GKE customer's requests usually have to come from the system metrics. The two share auth and name mapping.
5. **AWS CloudWatch PromQL.** Cheap once SigV4 exists, but it covers only customers that enabled the newer OTel Container Insights add-on. Ask the first prospects which Container Insights they run before building the enhanced observability path (row 12).
6. **Datadog.** A new client with its own chunking and rate limiting, so it is the first adapter that needs its own design.
7. **Node fallbacks (EC2, AKS platform metrics)** take 1 to 2 days each when a deal has only node metrics and the report is better with some data than none.
8. **Everything else, on demand:** Azure Log Analytics, classic Container Insights, New Relic, Dynatrace, Elastic, Splunk, CloudTrail reconstruction. Build one only for a deal that needs it. Elastic is the exception: if the prospect is on a version with the PromQL endpoint, it is a few days.

## Cross-cutting findings

- **Requests and replicas are the weak spot, not usage.** Usage comes from cAdvisor everywhere. Requests and desired replicas come from kube-state-metrics (or the vendor's equivalent), and managed offerings do not guarantee it:
  - GKE's managed kube-state-metrics package lists `kube_deployment_spec_replicas` but not `kube_pod_container_resource_requests`, `kube_node_status_allocatable` or `kube_pod_owner`.
  - The AWS managed scraper sample config has no kube-state-metrics job.
  - Azure's default minimal ingestion keeps all of them.
  
  The contract already handles a missing required series with `thoras.missing_metrics`, so each adapter should probe for the series first and report what is missing.
- **Node inventory mostly needs a cloud API.** Instance type, region and spot status are not in the usage metrics of GKE, AWS Container Insights or Azure. Budget about 2 days per cloud for node labels or the compute API, or fall back to the default node.
- **Chunking is driven by the point cap, not the range cap.** 14 days at 1 minute is 20,160 points per series. Datadog caps at 1,500, New Relic at 366 buckets, Dynatrace at 10,080 per tuple, and CloudWatch PromQL at 7 days per request. Prometheus-style stores allow wide ranges but have cost caps per query (Azure, AMP), so chunk by namespace or workload there.
- **Resolution degrades with age on some sources.** GKE and Google Managed Prometheus drop to 10-minute after about 6 weeks, CloudWatch metrics go to 5-minute after 15 days and 1-hour after 63, Datadog rolls up by query range. A 14-day pull at 1 minute is fine on all of them as long as the run is on recent data, but the adapter should record the step it got.
- **Counters and units.** CPU is a cumulative counter on Prometheus and GKE (`rate()`), nanocores on Datadog and Azure Log Analytics, nanoseconds per minute on Dynatrace's current store, and a percentage on several classic cloud metrics. Each adapter needs a unit test per metric.

## Managed Prometheus on the existing PromQL client

All of these use the standard `/api/v1/query_range` and differ in auth, limits and which metrics are present.

| Service | Auth | Retention | Per-query limits | Metrics present by default |
| --- | --- | --- | --- | --- |
| [Amazon Managed Prometheus](https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-compatible-APIs.html) | SigV4, service name `aps`, query access IAM policy | 150 days default, up to 1095 ([docs](https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-workspace-configuration.html)) | 95 days of range; 50M samples, 12M series and 5 GB scanned per 24-hour slice of a range query; 300 queries per second ([quotas](https://docs.aws.amazon.com/prometheus/latest/userguide/AMP_quotas.html)) | Whatever was remote-written. The managed scraper sample covers cAdvisor but not kube-state-metrics ([collector setup](https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-collector-how-to.html)) |
| [Azure Monitor managed Prometheus](https://learn.microsoft.com/en-us/azure/azure-monitor/metrics/prometheus-api-promql) | Entra client-credentials token for `https://prometheus.monitor.azure.com`, role Monitoring Data Reader | 18 months | 32 days including range selectors; minimum step 60 s for ranges of 48 hours or more; query cost cap 15,000; 50M samples per query ([limits](https://learn.microsoft.com/en-us/azure/azure-monitor/fundamentals/service-limits)) | cAdvisor and kube-state-metrics with a minimal keep list that includes usage, requests, replicas, allocatable and pod owner ([default targets](https://learn.microsoft.com/en-us/azure/azure-monitor/containers/prometheus-metrics-scrape-default)) |
| [Google Managed Prometheus](https://docs.cloud.google.com/stackdriver/docs/managed-prometheus/query) | OAuth2 access token, `roles/monitoring.viewer` | 24 months; 1-minute then 10-minute after about 6 weeks ([quotas](https://docs.cloud.google.com/monitoring/quotas)) | 200 MB per response; range and point caps not documented | cAdvisor/kubelet and kube-state-metrics packages are on by default on current GKE ([cAdvisor](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/cadvisor-kubelet-metrics), [kube-state-metrics](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/kube-state-metrics)) |
| [Grafana Cloud](https://grafana.com/docs/grafana-cloud/send-data/metrics/metrics-prometheus/query-http-api/) | Basic auth, instance ID and a token with `metrics:read` | 13 months on paid plans, 14 days on free (pricing pages, not read in full) | 32 days per query ([usage limits](https://grafana.com/docs/grafana-cloud/cost-management-and-billing/manage-invoices/understand-your-invoice/usage-limits/)) | Depends on the Kubernetes monitoring allow-list. Adaptive Metrics can drop pod and container labels, so check that the labels exist |
| [Mimir](https://grafana.com/docs/mimir/latest/references/http-api/) | `X-Scope-OrgID` header, not needed with a single tenant | Operator's choice | No default range cap; operators often set one | Whatever was shipped |
| [Chronosphere](https://docs-xcor.paloaltonetworks.com/tooling/prometheus-api) | Bearer token from a read-only service account | Set per customer; long-term data is downsampled to 5-minute by default (secondary source, unconfirmed) | Not found | Depends on collector settings; cAdvisor and kubelet scraping are off unless enabled |

Notes that drive the estimates:

- **AMP:** the signer from the AWS SDK does most of the work. The credential chain (SSO, assumed roles) takes most of the time. The 95-day range cap is not a problem for 14 days; chunk by day for the per-slice budgets.
- **Azure:** every fetching query must carry a `__name__` matcher, and names and labels are case-insensitive, so two series that differ only by case can collapse. The docs' own join example uses two metric names, so test what "one `__name__` matcher" allows before writing joins in PromQL. If joins are rejected, do the joins client-side.
- **Google:** the 24-month retention applies to Prometheus data as well as system metrics, but resolution degrades earlier than for Azure. The docs do not state a points or range cap for the PromQL endpoint, so find it by testing and chunk by day to stay far from it.
- **Grafana Cloud:** cheapest of the group. The label-presence check for Adaptive Metrics belongs in the shared probe, not here.
- **Chronosphere:** the product is documented under Palo Alto Networks as Cortex XCOR. Limits are not documented on a page available in October 2026, and no free tier was found, so the estimate has the lowest confidence in the group. The token is long-lived and shown once, which some security teams will resist.

## GKE system metrics through the PromQL endpoint

Source: [Cloud Monitoring Kubernetes metrics](https://docs.cloud.google.com/monitoring/api/metrics_kubernetes), [GKE metrics configuration](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-metrics), [PromQL in Cloud Monitoring](https://docs.cloud.google.com/monitoring/promql).

- **Inputs:** `kubernetes.io/container/cpu/core_usage_time` (cumulative seconds, use `rate()`), `container/memory/used_bytes` (label `memory_type`, `evictable` or `non-evictable`), `container/cpu/request_cores`, `container/memory/request_bytes`, matching `limit_*`, and `node/cpu/allocatable_cores` and `node/memory/allocatable_bytes`. In PromQL the names become `kubernetes_io:container_cpu_core_usage_time` and so on.
- **Working set:** there is no metric called working set. The non-evictable series is the likely match, but the docs do not say so. Validate it against a real cluster before relying on it.
- **Replicas:** not in system metrics. They come from `kube_deployment_spec_replicas` and `kube_statefulset_replicas` in the managed kube-state-metrics package, which is on by default on Standard 1.29.2-gke.2000 and later and Autopilot 1.27.4-gke.900 and later. Older clusters need the customer to enable it. These are billed as managed Prometheus samples.
- **Default and cost:** system metrics are on by default and free to ingest. They can be disabled on Standard but not on Autopilot.
- **History:** retained 24 months. The quotas page says system metric data is stored for 6 weeks at its original sampling frequency, then downsampled to 10-minute intervals. Fourteen days at 1 minute works.
- **Requests lag:** the docs say request data is not visible for up to 120 seconds after sampling, so expect a gap at the end of the window.
- **Quirks of the PromQL endpoint:** no staleness handling, strongly typed metrics (`rate()` on a gauge errors), a `monitored_resource` selector when a metric maps to several resource types, and a lookback widened to the step.
- **Node inventory:** instance type, region and spot are not in these metrics. Use the compute and GKE APIs or node labels from the kubectl snapshot. That is the extra 2 days.
- **Testing:** needs a real GKE cluster, and Autopilot is the cheapest. Minikube does not emit these metrics.

## Datadog

Source: [OTLP metrics ingest](https://docs.datadoghq.com/opentelemetry/setup/otlp_ingest/metrics/), [rollups](https://docs.datadoghq.com/dashboards/functions/rollup/), [rate limits](https://docs.datadoghq.com/api/latest/rate-limits/), [kubelet metrics](https://github.com/DataDog/integrations-core/blob/master/kubelet/metadata.csv), [kube-state-metrics core](https://docs.datadoghq.com/integrations/kubernetes_state_core/), [API specification](https://github.com/DataDog/datadog-api-client-python/blob/master/.generator/schemas/v2/openapi.yaml).

- **No export of stored history:** confirmed. The OpenTelemetry documentation covers ingest of OTLP metrics into Datadog, and OTLP export of traces from Datadog SDKs. No path returns stored metric history as OTLP, so an adapter must query the API and write the contract file itself.
- **Query API:** `POST /api/v2/query/timeseries` (preferred) and `GET /api/v1/query`.
- **Points per series:** 1,500 per query. The rollup interval defaults to 20 seconds for the past hour, 1 minute for 4 hours, 5 minutes for a day, 10 minutes for 2 days, 1 hour for a week and 4 hours for a month. A 14-day query with defaults returns 4-hour points, so the adapter must set the interval explicitly. At 1 minute that is about 25 hours per request, about 14 requests per series per 14 days. At 5 minutes it is about 5 days per request. The v2 `interval` can be overridden by a larger one when the query would return too many points, so check the spacing of what comes back.
- **Rate limits:** the documentation lists response headers (`X-RateLimit-Limit`, `Period`, `Remaining`, `Reset`) and says the list of limits is not complete and can be raised through support. No number for the metrics query endpoints is published. The 1,600-per-hour figure could not be confirmed in any official page. Read the headers at run time and back off on HTTP 429.
- **Retention:** 15 months is stated across several Datadog pages, but no single page was found that states it for metrics. Whether 1-minute points are returned for ranges older than a few days was not verified.
- **Metrics:**
  - Usage: `kubernetes.cpu.usage.total` (nanocores) and `kubernetes.memory.working_set` (bytes).
  - Requests: `kubernetes.cpu.requests` (cores) and `kubernetes.memory.requests`, or `kubernetes_state.container.cpu_requested` and `memory_requested`.
  - Replicas: `kubernetes_state.deployment.replicas_desired`, `kubernetes_state.statefulset.replicas_desired`.
  - Nodes: `kubernetes_state.node.cpu_allocatable` and `memory_allocatable`.
  - Tags: the core check and the current legacy check both rename the kube-state-metrics labels to `kube_namespace`, `pod_name`, `kube_deployment`, `kube_stateful_set` and `kube_container_name`; `node` keeps its name. The Agent's tagger puts the same tags on kubelet metrics. The legacy check puts no workload tag on its container metrics, so requests are best read from the kubelet's `kubernetes.cpu.requests` and `kubernetes.memory.requests`, which have them, or from the core check, which adds the pod's owner.
  - Customers who ship through OpenTelemetry keep the OTel names (`k8s.container.cpu_request`, `k8s.deployment.desired`), and Datadog maps the `k8s.*` resource attributes to the same `kube_*` tags ([attribute mapping](https://github.com/DataDog/opentelemetry-mapping-go/blob/main/pkg/otlp/attributes/attributes.go)). `container.cpu.usage` is stored in nanocores by the Agent and in cores by the kubeletstats receiver, under the same name.
- **Credentials:** an API key and an application key. The API specification lists `timeseries_query` as the OAuth scope and permission for both query endpoints. A `metrics_read` scope does not appear for these endpoints. `GET /api/v2/validate_keys` checks both keys and needs no scope. The application key inherits its owner's permissions unless scoped, so a security team will want a dedicated read-only user and a scoped key. The customer also has to state their Datadog site.
- **Client choice:** plain REST with two endpoints is enough. The official clients add retries and site handling but also a dependency.
- **Testing:** the Datadog agent on minikube with the Helm chart and the core check enabled. A trial account exists, but its terms were not checked.
- **Estimate:** 8 to 12 days. A new client, the chunking and rollup control, rate-limit handling and the tag and name variants account for most of it.

## AWS Container Insights

Source: [Container Insights comparison](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/container-insights-eks-compare.html), [enhanced metrics](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Container-Insights-metrics-enhanced-EKS.html), [OTel Container Insights](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/container-insights-eks-otel.html), [CloudWatch PromQL](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-PromQL.html), [GetMetricData](https://docs.aws.amazon.com/AmazonCloudWatch/latest/APIReference/API_GetMetricData.html).

There are three surfaces, and a customer may have any of them or none. All are opt-in.

| Surface | Metrics | Query API | Size and days |
| --- | --- | --- | --- |
| OTel Container Insights (add-on 6.2.0 or later), original Prometheus names | cAdvisor and kube-state-metrics | CloudWatch PromQL, `/api/v1/query_range`, SigV4 service `monitoring` | S, 3 to 5 |
| Enhanced observability, `ContainerInsights` namespace | `pod_cpu_usage_total`, `pod_memory_working_set`, `pod_cpu_request`, `pod_memory_request`, `replicas_desired` | `GetMetricData` | L, 10 to 15 |
| Classic (maintenance mode) | Mostly percentages of node capacity | `GetMetricData` | Not worth building |

- **Requests:** the enhanced list does include `pod_cpu_request` and `pod_memory_request` as metrics, but at pod level only. Per-container requests exist only as fields in the performance log events, which means a Logs Insights query against the cluster's `performance` log group (3 to 5 more days). One row of the enhanced table still says requests are log fields only, which contradicts the rest of the page, so confirm on a live cluster.
- **Workload mapping:** the metrics carry pod and `Service` dimensions but no deployment or owner dimension. The adapter has to map pods to workloads, for example by stripping the ReplicaSet hash. The OTel route avoids this because kube-state-metrics labels are present.
- **Replicas:** `replicas_desired` is documented as a workload-level count, but what its `PodName` dimension holds is not documented. Check on a live cluster.
- **Resolution:** classic CloudWatch metrics keep 1-minute data for 15 days, 5-minute data until day 63, then 1-hour until 455 days. A 14-day pull is inside the 1-minute window with little margin. For the OTel metrics the documentation states 15 months of storage and says nothing about rollups.
- **`GetMetricData` limits:** 500 queries and 100,800 data points per call, so at most 5 series at 20,160 points per page, and a large cluster needs many paginated calls. The documented default service quota is 500 requests per second. The cost is $0.01 per 1,000 metrics requested ([pricing](https://aws.amazon.com/cloudwatch/pricing/), not confirmed against the official price list, so confirm the figure before quoting it).
- **CloudWatch PromQL limits:** 7 days per request, so 14 days is at least 2 requests; 500 series per query with a `warnings` field and no error when truncated; 20 second timeout; 300 queries per second and 30 concurrent. OTel Container Insights keeps the cAdvisor and kube-state-metrics names and their own labels, which PromQL reads bare, so the usual queries apply; OpenTelemetry resource attributes are added as labels under a `@resource.` prefix (the cluster is `@resource.k8s.cluster.name`) and need PromQL's quoted label names. The product was announced as generally available in June 2026. The add-on's documentation lists Deployment and StatefulSet among the kube-state-metrics it collects, but not which metrics, so confirm `kube_deployment_spec_replicas`, `kube_statefulset_replicas` and `kube_node_labels` on a live cluster.
- **Units:** `pod_cpu_usage_total` is in millicores according to the performance log reference, but the metrics page says only "CPU units". Check the unit on a live cluster.
- **Customer access:** `cloudwatch:GetMetricData` and `cloudwatch:ListMetrics`, plus `logs:StartQuery`, `logs:GetQueryResults` and `logs:DescribeLogGroups` for container requests. Low approval difficulty. The metrics may live in a different account from the cluster.
- **Testing:** a small EKS cluster. The enhanced metrics are easy to mock because `GetMetricData` returns plain JSON.

## Azure Container Insights

Source: [data collection](https://learn.microsoft.com/en-us/azure/azure-monitor/containers/container-insights-data-collection-configure), [log queries](https://learn.microsoft.com/en-us/azure/azure-monitor/containers/container-insights-log-query), [Log Analytics limits](https://learn.microsoft.com/en-us/azure/azure-monitor/fundamentals/service-limits), [retention](https://learn.microsoft.com/en-us/azure/azure-monitor/logs/data-retention-configure).

Microsoft now describes the managed Prometheus path as the preferred one, so for most Azure prospects the adapter in the managed Prometheus table is the right one. The Log Analytics path is for clusters that only have the legacy tables.

- **Default collection:** the portal's default preset ("Logs and Events") collects `ContainerLogV2`, `KubeEvents` and `KubePodInventory` only. It does not collect `Perf`, `InsightsMetrics` or `KubeNodeInventory`. The Standard and Cost-optimized presets do collect them. The cost-optimized preset collects every five minutes and excludes some namespaces. Microsoft says `Perf` queries return nothing when that collection is off.
- **Tables:** `Perf` has `cpuUsageNanoCores`, `memoryWorkingSetBytes`, limit and capacity counters, per container and per node. `KubePodInventory` has controller name and kind. The deployment name comes from stripping the ReplicaSet hash from the controller name.
- **Gaps:** no request counters were found in any documentation page (for example `cpuRequestNanoCores`), and `KubePodInventory` has no desired replica count, so a count of observed pods is only an approximation. Test on a real workspace before committing to an estimate. Without requests the adapter cannot produce a savings figure for the container.
- **Limits:** 500,000 rows and about 104 MB per response, 3 minute default and 10 minute maximum timeout, 200 requests per 30 seconds per user, 5 concurrent queries. At 20,160 rows per container per counter, the row cap is reached with about 25 series, so aggregate in KQL with `summarize ... by bin(TimeGenerated, 1m)` and chunk by time.
- **Retention:** 30 days by default, which puts 14 days of data inside the window but leaves little spare.
- **Access:** Log Analytics Reader, or Reader on the cluster resource in resource-context mode. The direct `api.loganalytics.azure.com` endpoint rejects client-credential service principals in some configurations, so use the Azure Resource Manager route or another flow.

## Node-level fallbacks

| Source | What it gives | Limits | Days |
| --- | --- | --- | --- |
| EC2 `CPUUtilization` ([docs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/viewing_metrics_with_cloudwatch.html)) | Percent CPU per instance, 5-minute by default, 1-minute with paid detailed monitoring | No memory, no requests, no pods. 1-minute data is subject to the 15-day rollup | 1 to 2 |
| CloudTrail `LookupEvents` ([docs](https://docs.aws.amazon.com/awscloudtrail/latest/APIReference/API_LookupEvents.html)) | Instance launch and terminate events over 90 days, which reconstructs node count and instance types | 2 requests per second per account and Region, 50 results per page, one lookup attribute per call. Busy autoscaling accounts take many minutes | 4 to 6 |
| AKS platform metrics ([reference](https://learn.microsoft.com/en-us/azure/aks/monitor-aks-reference)) | `node_cpu_usage_millicores`, `node_memory_working_set_bytes`, cluster-level allocatable CPU and memory | 93 days, 1-minute, node level only, no requests | 2 |

None of these provides requests or per-workload data, so a file built only from them has `thoras.missing_metrics` on every container and cannot produce savings. Use them to size the cluster, not to price waste.

## Later sources

| Source | How to query | Inputs and names | Limits that force chunking | Customer must provide |
| --- | --- | --- | --- | --- |
| [New Relic](https://docs.newrelic.com/docs/kubernetes-pixie/kubernetes-integration/installation/k8s-otel-metrics/) | NRQL through NerdGraph | `K8sContainerSample`: `cpuUsedCores`, `memoryWorkingSetBytes`, `cpuRequestedCores`, `memoryRequestedBytes`. `K8sDeploymentSample`: `podsDesired` | `TIMESERIES` is capped at 366 buckets ([syntax](https://docs.newrelic.com/docs/query-your-data/nrql-new-relic-query-language/get-started/nrql-syntax-clauses-functions/)), so 1-minute needs about 56 six-hour requests per series. `LIMIT` is capped at 5,000. 3,000 queries per account per minute ([NRQL limits](https://docs.newrelic.com/docs/nrql/using-nrql/rate-limits-nrql-queries/)). 25 concurrent NerdGraph requests per user | A user API key, which inherits the user's access. No read-only key type, so ask for a dedicated service user |
| [Dynatrace](https://docs.dynatrace.com/docs/analyze-explore-automate/metrics/built-in-metrics-on-grail) | Metrics API v2 or Grail DQL | `dt.kubernetes.container.cpu_usage`, `memory_working_set`, `requests_cpu`, `requests_memory`, `dt.kubernetes.workload.pods_desired`, node allocatable. The classic `builtin:kubernetes.workload.*` keys are a second set | Metrics API v2: 10,080 points per tuple, so 14 days at 1 minute needs two or more windows ([API](https://docs.dynatrace.com/docs/discover-dynatrace/references/dynatrace-api/environment-api/metric-v2/get-data-points)). DQL limits not verified | `metrics.read` for v2. Grail scope names not verified |
| [Elastic](https://www.elastic.co/docs/reference/query-languages/promql/promql-http-api) | PromQL endpoint on Serverless and GA in stack 9.5 (preview in 9.4); otherwise ES\|QL `TS` (GA in 9.4) | Kubernetes integration fields such as `kubernetes.container.cpu.usage.nanocores`, `kubernetes.container.memory.workingset.bytes`; kube-state-metrics fields for requests and replicas | PromQL: time series data streams only, no numeric point cap documented, some join operators unsupported ([limitations](https://www.elastic.co/docs/reference/query-languages/promql/promql-limitations)). ES\|QL `TS`: 10,000 rows by default ([limitations](https://www.elastic.co/docs/reference/query-languages/esql/limitations)) | URL and an API key with index read on `metrics-*` |
| [Splunk Observability Cloud](https://help.splunk.com/en/splunk-observability-cloud/monitor-infrastructure/resolution-and-data-retention) | SignalFlow over REST or websocket | `container.cpu.time` (cumulative, so derive cores), `container.memory.working_set`, `k8s.container.cpu_request`, `k8s.container.memory_request`, `k8s.deployment.desired` | 250,000 input series per job, 5,000 jobs per minute per org ([limits](https://help.splunk.com/en/splunk-observability-cloud/administer/org-reference-info/per-product-system-limits-in-splunk-observability-cloud/infrastructure-system-limits-details)). Whether 1-minute resolution is honored over 14 days was not verified | An org access token with API scope and a read-only role. Default expiry is 30 days |

- **New Relic:** most customers on the OpenTelemetry path land in the `Metric` event type rather than the `K8s*Sample` events, so a second schema may be needed. Retention for the Kubernetes sample events is not stated; the infrastructure integrations line on the [retention page](https://docs.newrelic.com/docs/data-apis/manage-data/manage-data-retention/) gives 395 days on the original data plan.
- **Dynatrace:** the current store keeps 1-minute data for 15 months with no documented degradation ([limits](https://docs.dynatrace.com/docs/analyze-explore-automate/metrics/limits)). The classic metric set degrades after 14 days, which sits exactly at the 14-day target. The two documentation pages disagree on the key for desired replicas, so check against a live tenant. CPU is nanoseconds per minute on the current store and millicores on the classic one.
- **Elastic:** three routes (PromQL, ES|QL, and plain aggregations on older stacks) mean the adapter has to detect the version and the data stream type first. The 3 to 4 day estimate holds only when the PromQL endpoint is available.
- **Splunk:** SignalFlow is a streaming protocol with its own message framing, so the PromQL client does not carry over. Utilization metrics are relative, so derive cores from the cumulative CPU time.

## Testing without a customer

| Source | Test setup |
| --- | --- |
| Mimir | Single-binary Mimir in Docker, with Prometheus or Grafana Alloy writing to it from minikube |
| Grafana Cloud | Free-tier stack; retention is short but enough for a 14-day pull |
| Amazon Managed Prometheus | A workspace on a pay-per-use account, with a local Prometheus remote-writing using SigV4 |
| Azure managed Prometheus | A small AKS cluster with the metrics add-on enabled. Only the auth step is Azure-specific; test the rest against any Prometheus |
| Google Managed Prometheus and GKE system metrics | A small GKE cluster, Autopilot being cheapest. No local equivalent |
| Chronosphere | No free tier found. Mock the standard Prometheus API |
| AWS Container Insights | A small EKS cluster with the add-on; mock `GetMetricData` with recorded JSON |
| Azure Log Analytics | A small AKS cluster and a workspace on pay-as-you-go |
| Node fallbacks | A small EKS or AKS cluster; both sources are on by default except EC2 detailed monitoring |
| Datadog, New Relic, Dynatrace, Splunk | Free or trial account with the vendor's agent chart on minikube. Trial terms were not checked, except Dynatrace's 15-day trial |
| Elastic | Local Elasticsearch in Docker with the Elastic Agent or OpenTelemetry collector on minikube, or a cloud trial |

Replaying recorded responses is enough for unit tests on every source. A live smoke test is needed for the items listed under "Not verified".

## Not verified

- **Google Managed Prometheus and GKE:** the maximum range and point count of the PromQL endpoint, and the query rate quota beyond the documented 100 requests per second default for the metrics scope. That the non-evictable memory series equals the kubelet working set. Whether the system request metrics are queryable under usable PromQL names (the metric list says so, but a live query was not run). The documentation describes resolution for Google Managed Prometheus (full resolution for a week, 1-minute for five weeks, then 10-minute) differently from system metrics (full resolution for six weeks, then 10-minute), both on the same quotas page. Treat 14 days at 1 minute as available for both and confirm on a real project.
- **Datadog:** the query API rate limit, whether the 1,500-point cap documented for graphs applies to `/api/v2/query/timeseries`, the retention of 1-minute points for old ranges, whether the kube-state-metrics core check is on by default in current Helm charts, and free trial terms. The OpenTelemetry names and units and the `metrics_read` question are settled in the Datadog section above.
- **AWS:** whether `PodName` on `replicas_desired` is the workload or the pod, whether OTel Container Insights metrics roll up in resolution with age, whether the add-on exports the deployment and statefulset metrics, the default retention of the performance log group, the unit string on `pod_cpu_usage_total`, the 60-second default collection interval for classic Container Insights (secondary source, unconfirmed), and the pricing figures (not confirmed against the official price list). For OTel Container Insights in CloudWatch PromQL: the exact series kube-state-metrics sends, that `@resource.k8s.cluster.name` is set on every series, and how a query that reaches the 20-second execution timeout over a day of a large cluster behaves.
- **Azure:** whether any request counters exist in the `Perf` table, and what exactly the single `__name__` matcher rule allows. Whether `kube_node_info` carries the instance type. Whether resolution degrades with age on managed Prometheus (not documented).
- **Grafana Cloud:** the default Kubernetes metrics allow-list (the default-metrics page was not available in October 2026), and the paid and free retention figures (pricing pages, secondary source, unconfirmed).
- **Chronosphere:** query limits, retention and the series and label endpoint paths. The query limits page was not available in October 2026.
- **New Relic:** node instance type, region and spot attributes, retention of the Kubernetes events, the maximum `SINCE` range, whether `cpuUsedCores` is already in cores, free tier limits.
- **Dynatrace:** Grail scope names, DQL limits and rate limits, which replica key is correct, node properties, and the classic resolution ladder (1 minute for 14 days, 5 minutes to day 28, 1 hour to day 400, secondary source, unconfirmed).
- **Splunk:** the default Kubernetes metrics page was not available in October 2026, so usage metric names are from secondary sources and unconfirmed. Exact node allocatable names, the maximum time range per job, and trial length.
- **Elastic:** PromQL behavior before 9.4, which Kubernetes integration data streams are time series data streams, privileges beyond index read, and the field names of the OpenTelemetry route.
- **Free trials and sandboxes:** apart from Dynatrace's 15-day trial, trial terms were not checked for any vendor.
