// Package polling reads a cluster's current requests, replica counts, nodes and
// metrics-server usage with read-only API calls at an interval, keeps the
// readings on disk with older ones rolled up, and writes them as the contract
// file.
package polling

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Source is recorded as thoras.source on every resource.
const Source = "kubectl-snapshot"

const unknown = "unknown"

// ContainerUsage is one container's usage in a PodMetrics item.
type ContainerUsage struct {
	Name  string              `json:"name"`
	Usage corev1.ResourceList `json:"usage"`
}

// PodMetrics is the part of a metrics.k8s.io/v1beta1 PodMetrics item that is used.
type PodMetrics struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Containers []ContainerUsage `json:"containers"`
}

// Cluster is one read of everything a reading needs.
type Cluster struct {
	Pods         []corev1.Pod
	ReplicaSets  []appsv1.ReplicaSet
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	Nodes        []corev1.Node
	// PodMetrics is nil when metrics-server could not be read; MetricsErr says why.
	PodMetrics []PodMetrics
	MetricsErr error
}

// WorkloadKey names a Deployment or StatefulSet.
type WorkloadKey struct {
	Namespace string
	Kind      string // "deployment" or "statefulset"
	Name      string
}

// ContainerKey names one container of a workload.
type ContainerKey struct {
	WorkloadKey
	Container string
}

// CPU values are in nanocores and memory values in bytes throughout this
// package, so readings are stored exactly.

// NodeAttrs are the attributes of a node resource.
type NodeAttrs struct {
	Name, InstanceType, Provider, Region, CapacityType string
}

// NodeInfo is a node's inventory at one reading.
type NodeInfo struct {
	NodeAttrs
	CPU, Memory int64
}

// Levels are one container's values at one reading. HasUsage is false when
// metrics-server had nothing for it.
type Levels struct {
	CPURequest, MemoryRequest int64
	HasCPULimit, HasMemLimit  bool
	CPULimit, MemoryLimit     int64
	HasUsage                  bool
	CPUUsage, MemoryUsage     int64
}

// Reading is the cluster reduced to the contract's resources at one instant.
type Reading struct {
	Containers map[ContainerKey]Levels
	Replicas   map[WorkloadKey]int
	Nodes      []NodeInfo
	// Skipped counts running pods not owned by a Deployment or StatefulSet.
	Skipped    int
	MetricsErr string
	// NoUsage is true when no container had usage, including when
	// metrics-server answered with an empty list.
	NoUsage bool
}

func nanocores(l corev1.ResourceList) (int64, bool) {
	q, ok := l[corev1.ResourceCPU]
	if !ok {
		return 0, false
	}
	return q.ScaledValue(resource.Nano), true
}

func memBytes(l corev1.ResourceList) (int64, bool) {
	q, ok := l[corev1.ResourceMemory]
	if !ok {
		return 0, false
	}
	return q.Value(), true
}

func owner(refs []metav1.OwnerReference, kinds ...string) (string, string, bool) {
	for _, r := range refs {
		for _, k := range kinds {
			if r.Kind == k {
				return r.Kind, r.Name, true
			}
		}
	}
	return "", "", false
}

func nodeInfo(n *corev1.Node) NodeInfo {
	l := n.Labels
	capacity := unknown
	karpenter := strings.ToLower(l["karpenter.sh/capacity-type"])
	eks := strings.ToLower(l["eks.amazonaws.com/capacityType"])
	gke := l["cloud.google.com/gke-spot"]
	switch {
	case karpenter == "spot" || eks == "spot" || gke == "true" || l["cloud.google.com/gke-preemptible"] == "true":
		capacity = "spot"
	case karpenter == "on-demand" || eks == "on_demand" || gke == "false":
		capacity = "on_demand"
	}
	provider := unknown
	if p, _, ok := strings.Cut(n.Spec.ProviderID, "://"); ok && p != "" {
		provider = p
		if p == "gce" {
			provider = "gcp"
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := l[k]; v != "" {
				return v
			}
		}
		return unknown
	}
	cpu, _ := nanocores(n.Status.Allocatable)
	mem, _ := memBytes(n.Status.Allocatable)
	return NodeInfo{
		NodeAttrs: NodeAttrs{
			Name:         n.Name,
			InstanceType: first("node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
			Provider:     provider,
			Region:       first("topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"),
			CapacityType: capacity,
		},
		CPU:    cpu,
		Memory: mem,
	}
}

// Reduce maps pods to their Deployment or StatefulSet through owner
// references, takes requests and limits from the newest running pod of each
// workload, averages usage over the workload's running pods, and takes replica
// counts from the controllers.
func Reduce(c *Cluster) Reading {
	rsOwner := map[[2]string]string{}
	for i := range c.ReplicaSets {
		rs := &c.ReplicaSets[i]
		if _, name, ok := owner(rs.OwnerReferences, "Deployment"); ok {
			rsOwner[[2]string{rs.Namespace, rs.Name}] = name
		}
	}
	desired := map[WorkloadKey]int{}
	for _, d := range c.Deployments {
		desired[WorkloadKey{d.Namespace, "deployment", d.Name}] = replicas(d.Spec.Replicas)
	}
	for _, s := range c.StatefulSets {
		desired[WorkloadKey{s.Namespace, "statefulset", s.Name}] = replicas(s.Spec.Replicas)
	}

	type usage struct{ cpu, mem int64 }
	podUsage := map[[2]string]map[string]usage{}
	for _, pm := range c.PodMetrics {
		m := map[string]usage{}
		for _, cu := range pm.Containers {
			cpu, ok1 := nanocores(cu.Usage)
			mem, ok2 := memBytes(cu.Usage)
			if ok1 && ok2 {
				m[cu.Name] = usage{cpu, mem}
			}
		}
		podUsage[[2]string{pm.Metadata.Namespace, pm.Metadata.Name}] = m
	}

	r := Reading{Containers: map[ContainerKey]Levels{}, Replicas: map[WorkloadKey]int{}, NoUsage: true}
	if c.MetricsErr != nil {
		r.MetricsErr = c.MetricsErr.Error()
	}
	newest := map[WorkloadKey]*corev1.Pod{}
	running := map[WorkloadKey][]*corev1.Pod{}
	for i := range c.Pods {
		p := &c.Pods[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		wl, ok := workloadOf(p, rsOwner)
		if _, known := desired[wl]; !ok || !known {
			r.Skipped++
			continue
		}
		running[wl] = append(running[wl], p)
		if n := newest[wl]; n == nil || n.CreationTimestamp.Before(&p.CreationTimestamp) {
			newest[wl] = p
		}
	}
	for wl, pod := range newest {
		r.Replicas[wl] = desired[wl]
		for _, ctr := range pod.Spec.Containers {
			var lv Levels
			lv.CPURequest, _ = nanocores(ctr.Resources.Requests)
			lv.MemoryRequest, _ = memBytes(ctr.Resources.Requests)
			lv.CPULimit, lv.HasCPULimit = nanocores(ctr.Resources.Limits)
			lv.MemoryLimit, lv.HasMemLimit = memBytes(ctr.Resources.Limits)
			var n int64
			for _, p := range running[wl] {
				if u, ok := podUsage[[2]string{p.Namespace, p.Name}][ctr.Name]; ok {
					lv.CPUUsage += u.cpu
					lv.MemoryUsage += u.mem
					n++
				}
			}
			if n > 0 {
				lv.HasUsage = true
				lv.CPUUsage = divRound(lv.CPUUsage, n)
				lv.MemoryUsage = divRound(lv.MemoryUsage, n)
				r.NoUsage = false
			}
			r.Containers[ContainerKey{wl, ctr.Name}] = lv
		}
	}
	for i := range c.Nodes {
		r.Nodes = append(r.Nodes, nodeInfo(&c.Nodes[i]))
	}
	return r
}

func divRound(sum, n int64) int64 { return (sum + n/2) / n }

func replicas(p *int32) int {
	if p == nil {
		return 1
	}
	return int(*p)
}

func workloadOf(p *corev1.Pod, rsOwner map[[2]string]string) (WorkloadKey, bool) {
	kind, name, ok := owner(p.OwnerReferences, "ReplicaSet", "StatefulSet")
	if !ok {
		return WorkloadKey{}, false
	}
	if kind == "StatefulSet" {
		return WorkloadKey{p.Namespace, "statefulset", name}, true
	}
	dep, ok := rsOwner[[2]string{p.Namespace, name}]
	return WorkloadKey{p.Namespace, "deployment", dep}, ok
}
