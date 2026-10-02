package prometheus

import (
	"fmt"
	"strings"
	"time"
)

// Workload kinds the contract can describe.
const (
	Deployment  = "deployment"
	StatefulSet = "statefulset"
)

// Pod to workload mapping. A pod owned by a ReplicaSet belongs to the
// Deployment that owns the ReplicaSet. Pods owned by anything else (bare
// ReplicaSets, DaemonSets, Jobs) are left out.
const podToDeployment = `max by (namespace, pod, workload) (
  label_replace(kube_pod_owner{owner_kind="ReplicaSet"}, "replicaset", "$1", "owner_name", "(.+)")
  * on (namespace, replicaset) group_left (workload)
  max by (namespace, replicaset, workload) (label_replace(kube_replicaset_owner{owner_kind="Deployment"}, "workload", "$1", "owner_name", "(.+)"))
)`

const podToStatefulSet = `max by (namespace, pod, workload) (
  label_replace(kube_pod_owner{owner_kind="StatefulSet"}, "workload", "$1", "owner_name", "(.+)")
)`

func podMap(kind string) string {
	if kind == StatefulSet {
		return podToStatefulSet
	}
	return podToDeployment
}

// The container="" series is the pod total and container="POD" is the pause
// container, so both are excluded to avoid counting a pod twice.
const appContainers = `container!="",container!="POD",image!=""`

func seconds(d time.Duration) int { return int(d / time.Second) }

// The pod cgroup series: no container label, but namespace and pod are set.
const podCgroup = `container="",pod!=""`

func rateWindow(step time.Duration) time.Duration {
	win := 5 * time.Minute
	if 2*step > win {
		win = 2 * step
	}
	return win
}

// CPUUsage is the mean CPU use per pod, in cores, for each workload container.
func CPUUsage(kind string, step time.Duration) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s}[%ds]))
  * on (namespace, pod) group_left (workload) %s
)`, appContainers, seconds(rateWindow(step)), podMap(kind))
}

// MemoryUsage is the mean memory working set per pod, in bytes.
func MemoryUsage(kind string) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod, container) (container_memory_working_set_bytes{%s})
  * on (namespace, pod) group_left (workload) %s
)`, appContainers, podMap(kind))
}

// PodCPUUsage is CPUUsage for stores whose cAdvisor series carry no container
// labels. The pod cgroup's usage is split between the pod's containers by
// their share of the pod's CPU request.
func PodCPUUsage(kind string, step time.Duration) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod) (rate(container_cpu_usage_seconds_total{%s}[%ds]))
  * on (namespace, pod) group_right () %s
  * on (namespace, pod) group_left (workload) %s
)`, podCgroup, seconds(rateWindow(step)), requestShare("cpu"), podMap(kind))
}

// PodMemoryUsage is MemoryUsage split from the pod cgroup like PodCPUUsage.
func PodMemoryUsage(kind string) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod) (container_memory_working_set_bytes{%s})
  * on (namespace, pod) group_right () %s
  * on (namespace, pod) group_left (workload) %s
)`, podCgroup, requestShare("memory"), podMap(kind))
}

// requestShare is each container's fraction of its pod's request for a
// resource. A container without a request has a share of zero. When no
// container in the pod has a request, the pod is split evenly, which gives a
// single-container pod all of it.
func requestShare(resource string) string {
	r := fmt.Sprintf(`(
    max by (namespace, pod, container) (kube_pod_container_resource_requests{resource=%q})
    or on (namespace, pod, container) 0 * max by (namespace, pod, container) (kube_pod_container_info{})
  )`, resource)
	return fmt.Sprintf(`(
  %[1]s / on (namespace, pod) group_left () (sum by (namespace, pod) (%[1]s) > 0)
  or on (namespace, pod, container)
  (%[1]s * 0 + 1) / on (namespace, pod) group_left () count by (namespace, pod) (%[1]s)
)`, r)
}

// OOMKills is the number of restarts in each step whose last termination was
// an out-of-memory kill, summed over the workload's pods. Steps without a
// kill have no sample.
func OOMKills(kind string, step time.Duration) string {
	restarts := `max by (namespace, pod, container) (kube_pod_container_status_restarts_total{}%s)`
	return fmt.Sprintf(`sum by (namespace, workload, container) (
  (%s - %s > 0)
  * on (namespace, pod, container) group_left () max by (namespace, pod, container) (kube_pod_container_status_last_terminated_reason{reason="OOMKilled"})
  * on (namespace, pod) group_left (workload) %s
)`, fmt.Sprintf(restarts, ""), fmt.Sprintf(restarts, fmt.Sprintf(" offset %ds", seconds(step))), podMap(kind))
}

// Requests is the mean per-pod request for a resource ("cpu" or "memory").
// kube-state-metrics keeps exporting the requests of finished pods, so the
// series are limited to pods that are running. Every right-hand side of a
// join is deduplicated so that a second copy of a series, from two scrapes or
// two kube-state-metrics replicas, does not fail the query.
func Requests(kind, resource string) string {
	return perPod("kube_pod_container_resource_requests", kind, resource)
}

// Limits is the mean per-pod limit for a resource.
func Limits(kind, resource string) string {
	return perPod("kube_pod_container_resource_limits", kind, resource)
}

func perPod(metric, kind, resource string) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod, container) (
    %s{resource=%q}
    * on (namespace, pod) group_left () max by (namespace, pod) (kube_pod_status_phase{phase="Running"} == 1)
  )
  * on (namespace, pod) group_left (workload) %s
)`, metric, resource, podMap(kind))
}

// Replicas is the desired replica count of each workload.
func Replicas(kind string) string {
	if kind == StatefulSet {
		return `max by (namespace, workload) (label_replace(kube_statefulset_replicas{}, "workload", "$1", "statefulset", "(.+)"))`
	}
	return `max by (namespace, workload) (label_replace(kube_deployment_spec_replicas{}, "workload", "$1", "deployment", "(.+)"))`
}

// NodeAllocatable is the allocatable amount of a resource per node.
func NodeAllocatable(resource string) string {
	return fmt.Sprintf(`max by (node) (kube_node_status_allocatable{resource=%q})`, resource)
}

// Presence probes.
const (
	UsageProbe      = `count(container_memory_working_set_bytes{` + appContainers + `})`
	CPUProbe        = `count(container_cpu_usage_seconds_total{` + appContainers + `})`
	RequestsProbe   = `count(kube_pod_container_resource_requests{})`
	OwnerProbe      = `count(kube_pod_owner{})`
	ReplicaSetProbe = `count(kube_replicaset_owner{owner_kind="Deployment"})`
	PhaseProbe      = `count(kube_pod_status_phase{phase="Running"})`
	PodUsageProbe   = `count(container_memory_working_set_bytes{` + podCgroup + `})`
	ContainerProbe  = `count(kube_pod_container_info{})`
	RestartsProbe   = `count(kube_pod_container_status_restarts_total{})`
	CAdvisorProbe   = `count(machine_memory_bytes{})`
)

// UnlabeledPods counts workload pods that have a pod cgroup series but no
// per-container series, as on nodes whose runtime does not label container
// metrics.
const UnlabeledPods = `count(
  max by (namespace, pod) (container_memory_working_set_bytes{` + podCgroup + `})
  unless on (namespace, pod) max by (namespace, pod) (container_memory_working_set_bytes{` + appContainers + `})
  and on (namespace, pod) max by (namespace, pod) (kube_pod_owner{owner_kind=~"ReplicaSet|StatefulSet"})
)`

// ClusterLabels are labels that multi-cluster stores (Thanos, Mimir, Amazon
// Managed Prometheus, Grafana Cloud) commonly use to tell clusters apart.
var ClusterLabels = []string{"cluster", "cluster_name", "cluster_id", "k8s_cluster", "k8s_cluster_name", "kubernetes_cluster"}

// ClusterValues returns queries that count series by the cluster labels: node
// info over the window, and pod owners and container memory at the query time.
func ClusterValues(window time.Duration) []string {
	by := strings.Join(ClusterLabels, ", ")
	return []string{
		fmt.Sprintf(`count by (%s) (last_over_time(kube_node_info{}[%ds]))`, by, seconds(window)),
		fmt.Sprintf(`count by (%s) (kube_pod_owner{})`, by),
		fmt.Sprintf(`count by (%s) (container_memory_working_set_bytes{})`, by),
	}
}

// NodeInfoAt returns the last known labels and provider id of every node seen
// in the window ending at the query time.
func NodeInfoAt(window time.Duration) (info, labels string) {
	w := seconds(window)
	return fmt.Sprintf(`last_over_time(kube_node_info{}[%ds])`, w), fmt.Sprintf(`last_over_time(kube_node_labels{}[%ds])`, w)
}
