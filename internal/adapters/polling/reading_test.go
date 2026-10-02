package polling

import (
	"os"
	"strings"
	"testing"
	"time"

	validator "github.com/thoras-ai/savings-preview/internal/validate"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rl(kv ...string) corev1.ResourceList {
	l := corev1.ResourceList{}
	for i := 0; i < len(kv); i += 2 {
		l[corev1.ResourceName(kv[i])] = resource.MustParse(kv[i+1])
	}
	return l
}

func meta(name string, owner ...string) metav1.ObjectMeta {
	m := metav1.ObjectMeta{Name: name, Namespace: "shop", CreationTimestamp: metav1.NewTime(time.Unix(1_790_000_000, 0))}
	if len(owner) == 2 {
		m.OwnerReferences = []metav1.OwnerReference{{Kind: owner[0], Name: owner[1]}}
	}
	return m
}

func pod(name, ownerKind, ownerName string, requests, limits corev1.ResourceList, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: meta(name, ownerKind, ownerName),
		Status:     corev1.PodStatus{Phase: phase},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{Requests: requests, Limits: limits}},
			{Name: "side"},
		}},
	}
}

func podMetrics(name, cpu, mem string) PodMetrics {
	var pm PodMetrics
	pm.Metadata.Name, pm.Metadata.Namespace = name, "shop"
	pm.Containers = []ContainerUsage{
		{Name: "app", Usage: rl("cpu", cpu, "memory", mem)},
		{Name: "side", Usage: rl("cpu", "1m", "memory", "10Mi")},
	}
	return pm
}

func testCluster(withMetrics bool) *Cluster {
	two := int32(2)
	webLimits := rl("cpu", "1", "memory", "512Mi")
	c := &Cluster{
		Pods: []corev1.Pod{
			pod("web-abc-1", "ReplicaSet", "web-abc", rl("cpu", "250m", "memory", "256Mi"), webLimits, corev1.PodRunning),
			pod("web-abc-2", "ReplicaSet", "web-abc", rl("cpu", "250m", "memory", "256Mi"), webLimits, corev1.PodRunning),
			pod("db-0", "StatefulSet", "db", rl("cpu", "2", "memory", "4Gi"), nil, corev1.PodRunning),
			pod("job-1", "Job", "job", nil, nil, corev1.PodRunning),
			pod("web-old", "ReplicaSet", "web-abc", nil, nil, corev1.PodSucceeded),
		},
		ReplicaSets:  []appsv1.ReplicaSet{{ObjectMeta: meta("web-abc", "Deployment", "web")}},
		Deployments:  []appsv1.Deployment{{ObjectMeta: meta("web"), Spec: appsv1.DeploymentSpec{Replicas: &two}}},
		StatefulSets: []appsv1.StatefulSet{{ObjectMeta: meta("db")}},
		Nodes: []corev1.Node{{
			ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.large",
				"topology.kubernetes.io/region":    "us-east-1",
				"karpenter.sh/capacity-type":       "spot",
			}},
			Spec:   corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-1"},
			Status: corev1.NodeStatus{Allocatable: rl("cpu", "1930m", "memory", "7000000Ki")},
		}},
	}
	if withMetrics {
		c.PodMetrics = []PodMetrics{
			podMetrics("web-abc-1", "100m", "100Mi"),
			podMetrics("web-abc-2", "300m", "300Mi"),
			podMetrics("db-0", "250000000n", "1Gi"),
		}
	} else {
		c.MetricsErr = errString("the server could not find the requested resource")
	}
	return c
}

type errString string

func (e errString) Error() string { return string(e) }

var (
	web = ContainerKey{WorkloadKey{"shop", "deployment", "web"}, "app"}
	db  = ContainerKey{WorkloadKey{"shop", "statefulset", "db"}, "app"}
)

func TestReduce(t *testing.T) {
	r := Reduce(testCluster(true))
	w := r.Containers[web]
	if w.CPURequest != 250e6 || w.MemoryRequest != 256<<20 {
		t.Errorf("web requests = %v, %v", w.CPURequest, w.MemoryRequest)
	}
	if !w.HasMemLimit || w.MemoryLimit != 512<<20 || !w.HasCPULimit || w.CPULimit != 1e9 {
		t.Errorf("web limits = %+v", w)
	}
	if !w.HasUsage || w.CPUUsage != 200e6 || w.MemoryUsage != 200<<20 {
		t.Errorf("web usage = %+v, want the mean over both pods", w)
	}
	if d := r.Containers[db]; d.HasCPULimit || d.CPUUsage != 250e6 {
		t.Errorf("db = %+v", d)
	}
	if got := r.Replicas; len(got) != 2 || got[web.WorkloadKey] != 2 || got[db.WorkloadKey] != 1 {
		t.Errorf("replicas = %v", got)
	}
	if r.Skipped != 1 || r.NoUsage {
		t.Errorf("skipped = %d, noUsage = %v", r.Skipped, r.NoUsage)
	}
	n := r.Nodes[0]
	if n.CapacityType != "spot" || n.Provider != "aws" || n.InstanceType != "m5.large" || n.Region != "us-east-1" ||
		n.CPU != 1930e6 || n.Memory != 7000000*1024 {
		t.Errorf("node = %+v", n)
	}
}

func TestUnsetRequestIsZero(t *testing.T) {
	side := Reduce(testCluster(true)).Containers[ContainerKey{web.WorkloadKey, "side"}]
	if side.CPURequest != 0 || side.MemoryRequest != 0 || side.HasCPULimit {
		t.Errorf("side = %+v", side)
	}
}

func TestNoMetrics(t *testing.T) {
	r := Reduce(testCluster(false))
	if !r.NoUsage || r.MetricsErr == "" || r.Containers[web].HasUsage {
		t.Errorf("reading = %+v", r)
	}
	c := testCluster(true)
	c.PodMetrics = []PodMetrics{}
	if r := Reduce(c); !r.NoUsage || r.MetricsErr != "" {
		t.Errorf("empty metrics list: %+v", r)
	}
}

func TestNodeCapacityLabels(t *testing.T) {
	for labels, want := range map[[2]string]string{
		{"eks.amazonaws.com/capacityType", "ON_DEMAND"}: "on_demand",
		{"eks.amazonaws.com/capacityType", "SPOT"}:      "spot",
		{"karpenter.sh/capacity-type", "on-demand"}:     "on_demand",
		{"cloud.google.com/gke-spot", "true"}:           "spot",
		{"some/label", "x"}:                             "unknown",
	} {
		n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labels[0]: labels[1]}}}
		if got := nodeInfo(&n); got.CapacityType != want || got.Provider != "unknown" || got.Region != "unknown" {
			t.Errorf("%v: %+v, want capacity %s", labels, got, want)
		}
	}
	gce := corev1.Node{Spec: corev1.NodeSpec{ProviderID: "gce://proj/zone/vm"}}
	if p := nodeInfo(&gce).Provider; p != "gcp" {
		t.Errorf("gce provider = %s", p)
	}
}

// validate checks the file at path with the Go port of
// scripts/validate_contract.py, whose agreement with the script
// internal/validate tests.
func validate(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if errs := validator.Validate(b); len(errs) > 0 {
		t.Fatalf("validator rejected %s:\n%s", path, strings.Join(errs[:min(len(errs), 10)], "\n"))
	}
}
