package prometheus

import (
	"fmt"
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
  label_replace(kube_replicaset_owner{owner_kind="Deployment"}, "workload", "$1", "owner_name", "(.+)")
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

// CPUUsage is the mean CPU use per pod, in cores, for each workload container.
func CPUUsage(kind string, step time.Duration) string {
	win := 5 * time.Minute
	if 2*step > win {
		win = 2 * step
	}
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s}[%ds]))
  * on (namespace, pod) group_left (workload) %s
)`, appContainers, seconds(win), podMap(kind))
}

// MemoryUsage is the mean memory working set per pod, in bytes.
func MemoryUsage(kind string) string {
	return fmt.Sprintf(`avg by (namespace, workload, container) (
  max by (namespace, pod, container) (container_memory_working_set_bytes{%s})
  * on (namespace, pod) group_left (workload) %s
)`, appContainers, podMap(kind))
}

// Requests is the mean per-pod request for a resource ("cpu" or "memory").
// kube-state-metrics keeps exporting the requests of finished pods, so the
// series are limited to pods that are running.
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
    * on (namespace, pod) group_left () (kube_pod_status_phase{phase="Running"} == 1)
  )
  * on (namespace, pod) group_left (workload) %s
)`, metric, resource, podMap(kind))
}

// Replicas is the desired replica count of each workload.
func Replicas(kind string) string {
	if kind == StatefulSet {
		return `max by (namespace, workload) (label_replace(kube_statefulset_replicas, "workload", "$1", "statefulset", "(.+)"))`
	}
	return `max by (namespace, workload) (label_replace(kube_deployment_spec_replicas, "workload", "$1", "deployment", "(.+)"))`
}

// NodeAllocatable is the allocatable amount of a resource per node.
func NodeAllocatable(resource string) string {
	return fmt.Sprintf(`max by (node) (kube_node_status_allocatable{resource=%q})`, resource)
}

// Presence probes.
const (
	UsageProbe    = `count(container_memory_working_set_bytes{` + appContainers + `})`
	CPUProbe      = `count(container_cpu_usage_seconds_total{` + appContainers + `})`
	RequestsProbe = `count(kube_pod_container_resource_requests)`
	OwnerProbe    = `count(kube_pod_owner)`
)

// NodeInfoAt returns the last known labels and provider id of every node seen
// in the window ending at the query time.
func NodeInfoAt(window time.Duration) (info, labels string) {
	w := seconds(window)
	return fmt.Sprintf(`last_over_time(kube_node_info[%ds])`, w), fmt.Sprintf(`last_over_time(kube_node_labels[%ds])`, w)
}
