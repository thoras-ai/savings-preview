package datadog

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

// TagSet names the tags a producer of Kubernetes metrics puts on its series.
type TagSet struct {
	Name        string
	Namespace   string
	Deployment  string
	StatefulSet string
	Container   string
	Node        string
}

// KubeTags are the tags of the Agent's tagger, the kube-state-metrics core
// check, the legacy kube-state-metrics check (which renames the
// kube-state-metrics labels) and OpenTelemetry data, whose Kubernetes
// resource attributes Datadog maps to the same tags.
var KubeTags = TagSet{Name: "kube_*", Namespace: "kube_namespace", Deployment: "kube_deployment", StatefulSet: "kube_stateful_set",
	Container: "kube_container_name", Node: "node"}

// RawTags are the kube-state-metrics label names, kept by legacy check
// versions that did not rename them.
var RawTags = TagSet{Name: "kube-state-metrics labels", Namespace: "namespace", Deployment: "deployment", StatefulSet: "statefulset",
	Container: "container", Node: "node"}

// ClusterTag tells clusters apart. The Agent sets it from the configured
// cluster name, and OpenTelemetry data from k8s.cluster.name.
const ClusterTag = "kube_cluster_name"

// RegionTag is the node region the kube-state-metrics core check copies from
// the node's topology label.
const RegionTag = "kube_region"

func (t TagSet) workload(kind string) string {
	if kind == StatefulSet {
		return t.StatefulSet
	}
	return t.Deployment
}

// Role is what a metric is used for.
type Role int

const (
	CPUUsage Role = iota
	MemoryUsage
	CPURequest
	MemoryRequest
	CPULimit
	MemoryLimit
	DeploymentReplicas
	StatefulSetReplicas
	NodeCPU
	NodeMemory
)

// Roles lists every role in probe order.
var Roles = []Role{CPUUsage, MemoryUsage, CPURequest, MemoryRequest, CPULimit, MemoryLimit, DeploymentReplicas, StatefulSetReplicas, NodeCPU, NodeMemory}

var roleNames = [...]string{"CPU usage", "memory usage", "CPU requests", "memory requests", "CPU limits", "memory limits",
	"Deployment replicas", "StatefulSet replicas", "node allocatable CPU", "node allocatable memory"}

func (r Role) String() string { return roleNames[r] }

// Unit says how a metric's values convert to the contract's cores or bytes.
type Unit int

const (
	Same      Unit = iota // already cores, bytes or a count
	Nanocores             // divide by 1e9
	// CoresOrNanocores is a name Datadog stores in two units:
	// container.cpu.usage is nanocores from the Agent and cores from the
	// OpenTelemetry kubeletstats receiver. The probe decides from the size of
	// the values.
	CoresOrNanocores
)

// Metric is one Datadog metric that can fill a role.
type Metric struct {
	Name string
	Tags TagSet
	Unit Unit
}

func (m Metric) String() string {
	if m.Tags.Name == KubeTags.Name {
		return m.Name
	}
	return m.Name + " (" + m.Tags.Name + ")"
}

// Candidates lists, per role, the metrics to look for in order of preference.
// The Agent's kubelet check comes first. Its request metrics cover running
// containers only and carry the workload tags, which the legacy
// kube-state-metrics check does not put on container metrics.
var Candidates = map[Role][]Metric{
	CPUUsage: {
		{"kubernetes.cpu.usage.total", KubeTags, Nanocores},
		{"container.cpu.usage", KubeTags, CoresOrNanocores},
	},
	MemoryUsage: {
		{"kubernetes.memory.working_set", KubeTags, Same},
		{"container.memory.working_set", KubeTags, Same},
	},
	CPURequest: {
		{"kubernetes.cpu.requests", KubeTags, Same},
		{"kubernetes_state.container.cpu_requested", KubeTags, Same},
		{"k8s.container.cpu_request", KubeTags, Same},
	},
	MemoryRequest: {
		{"kubernetes.memory.requests", KubeTags, Same},
		{"kubernetes_state.container.memory_requested", KubeTags, Same},
		{"k8s.container.memory_request", KubeTags, Same},
	},
	CPULimit: {
		{"kubernetes.cpu.limits", KubeTags, Same},
		{"kubernetes_state.container.cpu_limit", KubeTags, Same},
		{"k8s.container.cpu_limit", KubeTags, Same},
	},
	MemoryLimit: {
		{"kubernetes.memory.limits", KubeTags, Same},
		{"kubernetes_state.container.memory_limit", KubeTags, Same},
		{"k8s.container.memory_limit", KubeTags, Same},
	},
	DeploymentReplicas: {
		{"kubernetes_state.deployment.replicas_desired", KubeTags, Same},
		{"kubernetes_state.deployment.replicas_desired", RawTags, Same},
		{"k8s.deployment.desired", KubeTags, Same},
	},
	StatefulSetReplicas: {
		{"kubernetes_state.statefulset.replicas_desired", KubeTags, Same},
		{"kubernetes_state.statefulset.replicas_desired", RawTags, Same},
		{"k8s.statefulset.desired_pods", KubeTags, Same},
	},
	NodeCPU: {
		{"kubernetes_state.node.cpu_allocatable", KubeTags, Same},
	},
	NodeMemory: {
		{"kubernetes_state.node.memory_allocatable", KubeTags, Same},
	},
}

// NanocoreFloor is the CPU reading above which a CoresOrNanocores metric is
// taken to be in nanocores. No container runs on 10,000 cores, and a cluster
// whose busiest container peaks under 10 microcores is idle.
const NanocoreFloor = 1e4

// scope is the query scope: the filters plus a wildcard on each tag the
// query groups by, so that series without those tags are left out.
func scope(filters []string, need ...string) string {
	parts := append([]string(nil), filters...)
	for _, t := range need {
		parts = append(parts, t+":*")
	}
	if len(parts) == 0 {
		return "{*}"
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func rollup(fn string, step time.Duration) string {
	return fmt.Sprintf(".rollup(%s, %d)", fn, int(step/time.Second))
}

// needs returns the tags a role's queries group by.
func needs(r Role, t TagSet, kind string) []string {
	switch r {
	case DeploymentReplicas:
		return []string{t.Namespace, t.Deployment}
	case StatefulSetReplicas:
		return []string{t.Namespace, t.StatefulSet}
	case NodeCPU, NodeMemory:
		return []string{t.Node}
	}
	if kind == "" {
		return []string{t.Namespace, t.Container}
	}
	return []string{t.Namespace, t.workload(kind), t.Container}
}

// ContainerQuery is the per-pod mean of a container metric for each workload
// container of a kind, rolled up over each step with fn. Datadog rolls up
// each pod's series before it averages across pods, so with fn "max" this is
// the mean over pods of each pod's peak within the step.
func ContainerQuery(m Metric, r Role, kind string, filters []string, fn string, step time.Duration) string {
	by := needs(r, m.Tags, kind)
	return fmt.Sprintf("avg:%s%s by {%s}%s", m.Name, scope(filters, by...), strings.Join(by, ","), rollup(fn, step))
}

// ReplicasQuery is the desired replica count of each workload.
func ReplicasQuery(m Metric, r Role, filters []string, step time.Duration) string {
	by := needs(r, m.Tags, "")
	return fmt.Sprintf("max:%s%s by {%s}%s", m.Name, scope(filters, by...), strings.Join(by, ","), rollup("max", step))
}

// NodeQuery is a node metric per node, with the node's region.
func NodeQuery(m Metric, r Role, filters []string, step time.Duration) string {
	by := needs(r, m.Tags, "")
	return fmt.Sprintf("max:%s%s by {%s,%s}%s", m.Name, scope(filters, by...), strings.Join(by, ","), RegionTag, rollup("avg", step))
}

// ProbeQuery is the largest value of a metric among the series that carry
// the tags its role needs, per cluster.
func ProbeQuery(r Role, m Metric, filters []string, window time.Duration) string {
	return fmt.Sprintf("max:%s%s by {%s}%s", m.Name, scope(filters, needs(r, m.Tags, "")...), ClusterTag, rollup("max", window))
}
