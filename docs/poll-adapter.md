# Poll adapter

`savings-preview poll` builds the OTel file described in [contract.md](contract.md) from the live cluster when there is no usage history to read. It reads current requests, replica counts, node inventory and metrics-server usage with list calls at an interval, and keeps the readings on disk. `savings-preview snapshot` is the same command.

```
savings-preview poll [--context NAME] [--interval 1m] [--duration 0] [-o savings-preview.otlp.json]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--kubeconfig`, `--context` | standard kubeconfig rules, current context | Which cluster to read. |
| `--cluster-name` | the kubeconfig cluster name | Value of `k8s.cluster.name`. |
| `-o` | `savings-preview.otlp.json` | Contract file to write. Readings are kept in `FILE.readings/`. |
| `--interval` | 1m | Time between readings, at least 15s. metrics-server refreshes every 60s by default, so shorter intervals repeat values. |
| `--duration` | until interrupted | Stop after this long. Ctrl-C or SIGTERM stops the run cleanly at any time. |
| `--write-every` | 10m | How often the contract file is rewritten while polling. Readings continue during the write. |
| `--keep-raw` | 48h | How long readings stay at `--interval` before they are rolled up. 0 keeps them all. |
| `--rollup` | 5m | Step of rolled-up readings, rounded up to a multiple of `--interval`. |
| `--once` | off | Take one reading, write the file and exit. Nothing is saved, and the file is replaced: after `--once` on the output of an earlier poll, `--export` writes the poll's file again. |
| `--export` | off | Write the file from the saved readings and exit, without reading the cluster. Works while a poll is running. |

## Readings

Each reading is appended to `FILE.readings/` and synced before the next one, so a crash or Ctrl-C keeps everything collected so far. Running again with the same `-o` continues the series on the same time grid, leaving a gap for the time it was stopped. The cluster, `--interval` and `--rollup` must match the earlier run. Only one poll may use an `-o` at a time: a second one, or `--once` on the same file, refuses to start.

Readings are stored in hourly segments in a compact binary format, with requests, limits, replicas and node inventory written only when they change. Segments older than `--keep-raw` are rolled up to `--rollup` steps that keep the mean and the peak of usage, because the savings math uses 15-minute peaks. With the defaults, 14 days of readings for 1,000 workload-containers take about 80 MB.

## Output

The contract file is rewritten after the first reading, every `--write-every`, and when the run stops. It is written in passes over the saved readings, one batch of resources at a time, so memory stays bounded whatever the length of the run. Its size is set by the contract: 14 days for 1,000 workload-containers is about 2.8 GB. `savings-preview export --compact FILE.readings` writes the 15-minute compact file the report uses straight from the readings, and `run` writes it after polling.

A resource whose readings include rolled-up steps lists them in `thoras.step_spans` and carries `thoras.container.cpu.usage.max` and `thoras.container.memory.working_set.max`. Each resource's fidelity tier follows its coarsest step and the time its points cover, leaving out gaps between runs. With the default `--keep-raw` of 48h a run can reach tier 2 after 7 days. Tier 1, 14 days at 1 minute, needs `--keep-raw 336h`.

The progress line printed at each write says how long the readings cover and how much longer each tier needs. A single reading understates peaks and is tier 3.

## Mapping

| Contract metric | Source |
| --- | --- |
| `container.cpu.usage`, `container.memory.working_set` | PodMetrics from `metrics.k8s.io/v1beta1`, averaged over the workload's running pods |
| `k8s.container.cpu.request.desired`, `k8s.container.memory.request.desired` and limits | the pod spec of the workload's newest running pod; an unset request is 0 |
| `k8s.deployment.pod.desired`, `k8s.statefulset.pod.desired` | `spec.replicas` |
| `k8s.node.cpu.allocatable`, `k8s.node.memory.allocatable` | node `status.allocatable` |
| node attributes | labels `node.kubernetes.io/instance-type`, `topology.kubernetes.io/region`, `karpenter.sh/capacity-type`, `eks.amazonaws.com/capacityType`, `cloud.google.com/gke-spot`, `cloud.google.com/gke-preemptible`, and the provider ID |

Pods belong to a Deployment through their ReplicaSet's owner reference, and to a StatefulSet directly. Pods owned by anything else are left out and counted in a note. Without metrics-server, the file has everything except usage, which is listed in `thoras.missing_metrics`.

## Access

The kubeconfig identity needs `list` on pods, replicasets, deployments, statefulsets and nodes in all namespaces, and `list` on `pods.metrics.k8s.io`.
