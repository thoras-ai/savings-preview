# Savings Preview OTel contract

Version 1.1.0. Every adapter writes one OTLP-JSON metrics file in this shape, and the report page reads only that file. `scripts/validate_contract.py` checks a file against this document. `fixtures/savings-preview.otlp.json` is a valid example.

## Pinned conventions

- OpenTelemetry semantic conventions **1.44.0**. Every `resourceMetrics` and `scopeMetrics` entry carries `schemaUrl` = `https://opentelemetry.io/schemas/1.44.0`.
- Most of the Kubernetes and container metrics below are still `development` stability upstream. The pinned version is part of the contract: a later semconv rename does not change this file format until the contract version moves.
- Names with no semantic-convention equivalent live under `thoras.`.
- Instrumentation scope on every `scopeMetrics`: `name` = `thoras.savings_preview`, `version` = `1.x.y`. A change that old readers cannot handle bumps the major version.

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

Container and workload resources also take `k8s.namespace.name` and exactly one of `k8s.deployment.name` or `k8s.statefulset.name`. Container resources add `k8s.container.name`.

Node resources add `k8s.node.name`, `host.type` (the instance type), `cloud.provider`, `cloud.region` and `thoras.node.capacity_type` (`on_demand`, `spot` or `unknown`).

## Metrics

All data points are dense series at `thoras.step_seconds`. Data points carry no attributes. Everything that identifies a series is on the resource.

### Per container (container resource)

| Metric | Instrument | Unit | Value | Required | Meaning |
| --- | --- | --- | --- | --- | --- |
| `container.cpu.usage` | gauge | `{cpu}` | double | yes | CPU used by the container, averaged across the workload's pods. |
| `container.memory.working_set` | updowncounter | `By` | int | yes | Memory working set, averaged across the workload's pods. |
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

An adapter may claim a lower tier than its data supports but not a higher one. The validator rejects a claim the step and span cannot support.

`thoras.fidelity_notes` lists derivations a reader should know about. Readers ignore values they do not recognize.

| Note | Meaning |
| --- | --- |
| `usage_split_from_pod_by_requests` | The source had no per-container usage. Container usage is the pod's usage split between its containers in proportion to their requests (evenly when none has a request, so a single container gets all of it). |

## Missing data

- A series that could not be retrieved is left out of the file, and its metric name is listed in the resource's `thoras.missing_metrics`. Every required metric must be either present or listed, never both and never neither. Only required metrics can be listed.
- A gap inside a series is represented by omitting the points for those steps. Zero, `NaN` and carried-forward values are never used to fill a gap.
- A container with a required series missing is shown in the report but excluded from savings, because cost is the larger of the CPU and memory cost and cannot be computed from one of them.
- Optional metrics are simply absent when unavailable, with no declaration.

## Versions

- 1.1.0 adds the optional `thoras.fidelity_notes` attribute. 1.0.0 files are valid 1.1.0 files.

## Out of scope

HPA and external metrics, JVM heap, and realized savings.
