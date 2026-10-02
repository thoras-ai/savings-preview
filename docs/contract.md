# Savings Preview OTel contract

Version 1.0.0. Every adapter writes one OTLP-JSON metrics file in this shape, and the report page reads only that file. `scripts/validate_contract.py` checks a file against this document. `fixtures/savings-preview.otlp.json` is a valid example.

## Pinned conventions

- OpenTelemetry semantic conventions **1.44.0**. Every `resourceMetrics` and `scopeMetrics` entry carries `schemaUrl` = `https://opentelemetry.io/schemas/1.44.0`.
- Most of the Kubernetes and container metrics below are still `development` stability upstream. The pinned version is part of the contract: a later semconv rename does not change this file format until the contract version moves.
- Names with no semantic-convention equivalent live under `thoras.`.
- Instrumentation scope on every `scopeMetrics`: `name` = `thoras.savings_preview`, `version` = `1.0.0`.

## File layout

OTLP/JSON as defined by the OTLP protobuf mapping: 64-bit integers and timestamps are strings, `asInt` is a digit string, `asDouble` is a number. One file describes one cluster. Resources are of three kinds, told apart by their attributes:

| Kind | Identified by | Holds |
| --- | --- | --- |
| container | `k8s.container.name` plus a workload name | usage, requests, limits, OOM kills |
| workload | a workload name, no container | replica count |
| node | `k8s.node.name`, no workload | allocatable CPU and memory |

A workload that has container resources must also have a workload resource. Node resources are optional; without them the report prices the workload with its default node.

## Resource attributes

All kinds:

| Attribute | Type | Meaning |
| --- | --- | --- |
| `k8s.cluster.name` | string | Same value on every resource in the file. |
| `thoras.source` | string | Where the data came from, for example `prometheus`, `datadog`, `gke`, `kubectl-snapshot`. |
| `thoras.step_seconds` | int | Spacing of the data points, in seconds. |
| `thoras.fidelity_tier` | int | 1, 2 or 3, see below. |
| `thoras.missing_metrics` | string[] | Optional. Required metrics the adapter looked for and could not get. |
| `thoras.fidelity_notes` | string[] | Optional. How the series were derived when it differs from the metric definitions, see below. |
| `thoras.step_spans` | string[] | Optional. Set when the points use more than one step, see Mixed steps below. |
| `thoras.source_step_seconds` | int | Optional. Set on a compact resource: the step of the data it was built from, see Compact files below. |
| `thoras.held_metrics` | string[] | Optional. Level metrics whose points are written only where the value changes, see Held metrics below. |

Container and workload resources also take `k8s.namespace.name` and exactly one of `k8s.deployment.name` or `k8s.statefulset.name`. Container resources add `k8s.container.name`.

Node resources add `k8s.node.name`, `host.type` (the instance type), `cloud.provider`, `cloud.region` and `thoras.node.capacity_type` (`on_demand`, `spot` or `unknown`).

## Metrics

All data points are dense series at `thoras.step_seconds`, or at the step of each span when a resource has `thoras.step_spans`, except the metrics a resource lists in `thoras.held_metrics`. Data points carry no attributes. Everything that identifies a series is on the resource.

### Per container (container resource)

| Metric | Instrument | Unit | Value | Required | Meaning |
| --- | --- | --- | --- | --- | --- |
| `container.cpu.usage` | gauge | `{cpu}` | double | yes | CPU used by the container, averaged across the workload's pods. |
| `container.memory.working_set` | updowncounter | `By` | int | yes | Memory working set, averaged across the workload's pods. |
| `thoras.container.cpu.usage.max` | gauge | `{cpu}` | double | no | Highest `container.cpu.usage` reading within the point's step. |
| `thoras.container.memory.working_set.max` | updowncounter | `By` | int | no | Highest `container.memory.working_set` reading within the point's step. |
| `k8s.container.cpu.request.desired` | updowncounter | `{cpu}` | double | yes | CPU request per pod. |
| `k8s.container.memory.request.desired` | updowncounter | `By` | int | yes | Memory request per pod. |
| `k8s.container.cpu.limit.desired` | updowncounter | `{cpu}` | double | no | CPU limit per pod. Omit when no limit is set. |
| `k8s.container.memory.limit.desired` | updowncounter | `By` | int | no | Memory limit per pod. Omit when no limit is set. |
| `thoras.container.oom_kills` | counter, delta | `{oom_kill}` | int | no | OOM kills in the step ending at the point. Steps with no kill may be omitted. |

The semantic conventions define usage and requests per pod or per container instance. The contract reports one series per workload and container, so usage is the mean over the pods running at each step and requests are the per-pod value. The pod attribute is deliberately dropped. Total demand is the per-pod value times the replica count.

A request that is not set is reported as `0`, not as a missing series. A zero request means no reserved cost.

### Per workload (workload resource)

| Metric | Instrument | Unit | Value | Required | Meaning |
| --- | --- | --- | --- | --- | --- |
| `k8s.deployment.pod.desired` | updowncounter | `{pod}` | int | yes, for deployments | Desired replica count. |
| `k8s.statefulset.pod.desired` | updowncounter | `{pod}` | int | yes, for statefulsets | Desired replica count. |

### Per node (node resource)

| Metric | Instrument | Unit | Value | Required | Meaning |
| --- | --- | --- | --- | --- | --- |
| `k8s.node.cpu.allocatable` | updowncounter | `{cpu}` | double | yes | Allocatable CPU. |
| `k8s.node.memory.allocatable` | updowncounter | `By` | int | yes | Allocatable memory. |

Node count at a time is the number of node resources that have a data point at that time. There is no separate count metric, so nodes that come and go over the window are represented as they were.

### OTLP encoding

- `gauge` metrics use `gauge.dataPoints`.
- `updowncounter` metrics are a `sum` with `isMonotonic` false and cumulative temporality (`aggregationTemporality` 2). Because the values are levels, each point is the level at that time, not an accumulation.
- The delta counter is a `sum` with `isMonotonic` true and delta temporality (`aggregationTemporality` 1).
- `timeUnixNano` is the sample time, a whole second. `startTimeUnixNano` is optional and ignored.
- Values are finite and not negative. Points are strictly increasing in time and spaced by a whole multiple of `thoras.step_seconds`.

## Resolution, source and fidelity

Each resource records how its series were produced: `thoras.source`, `thoras.step_seconds` and `thoras.fidelity_tier`. Different resources in one file may differ, for example when a source has coarser data for older history.

The target is 14 days at 60 seconds, which is what the forecaster trains on. 300 seconds is acceptable.

| Tier | Meaning |
| --- | --- |
| 1 | Step of 60 seconds or less over at least 14 days. |
| 2 | Step of 300 seconds or less over at least 7 days. |
| 3 | Anything coarser or shorter, including a single snapshot. |

### Mixed steps

An adapter that keeps recent data at a fine step and older data at a coarser one records the spans in `thoras.step_spans`. Each entry is `START/END/STEP` in unix seconds, START inclusive and END exclusive: points in that range are STEP seconds apart. Entries are in time order, do not overlap, and every point lies in one of them on its grid (START plus a multiple of STEP). Each STEP is a multiple of `thoras.step_seconds`, which is the finest of them. For example, `["1789200000/1790236800/300", "1790236800/1790409600/60"]` is 5-minute data followed by two days of 1-minute data.

A point summarizing several readings carries their mean in the usage metric. When an adapter has the peaks, it adds them as `thoras.container.cpu.usage.max` and `thoras.container.memory.working_set.max`, with a point wherever the usage metric has one. Readers that compute peaks should prefer the max series when it is present.

The fidelity tier of a resource with step spans follows its coarsest step.

A reader weights each point by the step of the span it lies in, not by `thoras.step_seconds`. Taking peaks from the usage means instead of the max series understates them, which overstates savings.

An adapter may claim a lower tier than its data supports but not a higher one. The validator rejects a claim the step and span cannot support. For a resource with `thoras.source_step_seconds` the tier is judged on that step instead of the coarsest step of the points.

`thoras.fidelity_notes` lists derivations a reader should know about. Readers ignore values they do not recognize.

| Note | Meaning |
| --- | --- |
| `usage_split_from_pod_by_requests` | The source had no per-container usage. Container usage is the pod's usage split between its containers in proportion to their requests (evenly when none has a request, so a single container gets all of it). |

### Compact files

A compact file holds the same resources as a full file at a 15-minute step (900 seconds), which is the window the report takes peaks over. `savings-preview export --compact` writes one from a full file, and `run` writes one for the report and the zip. Each point summarizes the source points in its step:

- `container.cpu.usage` and `container.memory.working_set` carry their mean, weighted by the step of each source point.
- `thoras.container.cpu.usage.max` and `thoras.container.memory.working_set.max` carry the highest reading in the step, from the source's usage and peak metrics. A compact file always has them where it has usage.
- Requests, limits, replica counts and node allocatable carry their weighted mean, rounded to a whole number for integer metrics, and are held (see below) when that gives the same value at every time a reader uses them.
- `thoras.container.oom_kills` carries the sum of the kills in the step.

Steps start at the first point of the file, so data on one grid stays on one grid. A compact resource sets `thoras.step_seconds` to 900, keeps its `thoras.fidelity_tier`, and records the coarsest step of its source data in `thoras.source_step_seconds`. The tier follows the source step: the 15-minute peaks of 1-minute data are those of the 1-minute data. `thoras.source_step_seconds` is at most the coarsest step of the points. When the source has step spans coarser than 900 seconds that start and end on the 15-minute grid, as a poll rolled up to 15 minutes or an hour does, those spans are kept and only the finer ones are compacted; `thoras.step_spans` then lists both. A resource whose data is all at 900 seconds or coarser, or whose step spans are off the grid, is copied as it is, without the attribute. Since the grid starts at the first point of the file, a resource that appears partway through a poll and comes first in the file can put the rolled-up spans of the others off the grid; they are then copied as they are, which is correct but larger.

Totals from a compact file come close to the full file's but are not always equal:

- The target follows the peak over the 15 minutes before each point. In a full file that window straddles two compact steps; in a compact file it is the point's own step. This moves the savings in either direction: down when a step's peak comes early, up when the step before had a higher peak.
- A workload's peak is that of its containers' summed usage. A compact file can only bound it by the sum of each container's peak, which lowers the savings for workloads with several containers.
- A replica count that changes within a step is written as its mean rounded to a whole pod, which moves requests and spend in either direction.
- The first and last steps count as whole steps even when the data covers part of them, which matters only for runs of a few hours or less.

Requests and spend match exactly when replica counts are steady within each step. On the test data, savings from the compact file were 0.03% below the full file for single-container workloads with steady replicas, and 0.9% below for two-container workloads whose replica counts change every minute.

### Held metrics

A resource may list level metrics in `thoras.held_metrics`: `k8s.container.cpu.request.desired`, `k8s.container.memory.request.desired`, `k8s.container.cpu.limit.desired`, `k8s.container.memory.limit.desired`, `k8s.deployment.pod.desired`, `k8s.statefulset.pod.desired`, `k8s.node.cpu.allocatable` and `k8s.node.memory.allocatable`. A listed metric must be present. Its points are written at its first time, at its last time, and wherever its value changes. Each point's value holds until the next point, and the last point holds for one step. A held series cannot show a gap, so a writer holds a metric only when the series has a value at every step from its first point to its last that a reader prices: for a container, every step with usage; for a workload or node, every step.

A reader averaging a held series over time weights each point by the time until the next one.

## Missing data

- A series that could not be retrieved is left out of the file, and its metric name is listed in the resource's `thoras.missing_metrics`. Every required metric must be either present or listed, never both and never neither. Only required metrics can be listed.
- A gap inside a series is represented by omitting the points for those steps. Zero, `NaN` and carried-forward values are never used to fill a gap. Held metrics are the exception: their points are only the changes, so they have no gaps.
- A container with a required series missing is shown in the report but excluded from savings, because cost is the larger of the CPU and memory cost and cannot be computed from one of them.
- Optional metrics are simply absent when unavailable, with no declaration.

## Versioning

The scope `version` names the contract a file follows. Each release reads only the versions it knows and rejects other files with a message naming the version. A change that keeps every existing file valid with the same meaning bumps the minor version; any other change bumps the major version. Minor versions 1.1 to 1.3 are not used, because files from development builds before 1.0.0 carry them.

## Out of scope

HPA and external metrics, JVM heap, and realized savings.
