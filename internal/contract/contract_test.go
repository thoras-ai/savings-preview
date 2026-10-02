package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func points(n int, step int64, v float64) []Point {
	p := make([]Point, n)
	for i := range p {
		p[i] = Point{T: 1_790_000_000 + int64(i)*step, V: v}
	}
	return p
}

func TestWriteIsValid(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	c := Common{Cluster: "c", Source: "prometheus", StepSeconds: 60, Tier: 3}
	f := File{Resources: []Resource{
		{Common: c, Namespace: "ns", Kind: "deployment", Workload: "web", Container: "app", Metrics: []Series{
			{CPUUsage, points(5, 60, 0.25)},
			{MemoryWorkingSet, points(5, 60, 1e8)},
			{CPURequest, points(5, 60, 0.5)},
			{MemoryRequest, points(5, 60, 2e8)},
		}},
		{Common: c, Namespace: "ns", Kind: "deployment", Workload: "web", Metrics: []Series{{DeploymentPods, points(5, 60, 2)}}},
		{Common: c, Namespace: "ns", Kind: "statefulset", Workload: "db", Container: "pg",
			Missing: []string{CPURequest, MemoryRequest},
			Metrics: []Series{{CPUUsage, points(5, 60, 1)}, {MemoryWorkingSet, points(5, 60, 5e8)}}},
		{Common: c, Namespace: "ns", Kind: "statefulset", Workload: "db", Metrics: []Series{{StatefulSetPods, points(5, 60, 1)}}},
		{Common: c, Node: &Node{Name: "n1", InstanceType: "m5.large", Provider: "aws", Region: "us-east-1", CapacityType: "spot"},
			Metrics: []Series{{NodeCPU, points(5, 60, 2)}, {NodeMemory, points(5, 60, 8e9)}}},
	}}
	path := filepath.Join(t.TempDir(), "out.json")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Write(fh); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	out, err := exec.Command(py, "../../scripts/validate_contract.py", path).CombinedOutput()
	if err != nil {
		t.Fatalf("validator rejected the file:\n%s", out)
	}
}

func TestWriteRejectsBadValues(t *testing.T) {
	c := Common{Cluster: "c", Source: "s", StepSeconds: 60, Tier: 3}
	f := File{Resources: []Resource{{Common: c, Namespace: "n", Kind: "deployment", Workload: "w",
		Metrics: []Series{{DeploymentPods, []Point{{T: 60, V: -1}}}}}}}
	if err := f.Write(os.Stderr); err == nil {
		t.Fatal("expected an error for a negative value")
	}
}

func TestTierFor(t *testing.T) {
	const d = 86400
	for _, c := range []struct {
		step int
		span int64
		want int
	}{{60, 14 * d, 1}, {60, 13 * d, 2}, {300, 14 * d, 2}, {300, 6 * d, 3}, {600, 30 * d, 3}} {
		if got := TierFor(c.step, c.span); got != c.want {
			t.Errorf("TierFor(%d,%d)=%d want %d", c.step, c.span, got, c.want)
		}
	}
}

func TestStreamWithStepSpansIsValid(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	// Twelve 5-minute points, then an hour of 1-minute points.
	var coarse, fine []Point
	for i := int64(0); i < 12; i++ {
		coarse = append(coarse, Point{T: 1_790_000_000 + i*300, V: 0.25})
	}
	for i := int64(0); i < 60; i++ {
		fine = append(fine, Point{T: 1_790_003_600 + i*60, V: 0.25})
	}
	all := append(append([]Point(nil), coarse...), fine...)
	c := Common{Cluster: "c", Source: "kubectl-snapshot", StepSeconds: 60, Tier: 3}
	spans := []StepSpan{{1_790_000_000, 1_790_003_600, 300}, {1_790_003_600, 1_790_007_200, 60}}
	path := filepath.Join(t.TempDir(), "out.json")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriter(fh)
	for _, r := range []Resource{
		{Common: c, Namespace: "ns", Kind: "deployment", Workload: "web", StepSpans: spans,
			Metrics: []Series{{DeploymentPods, all}}},
		{Common: c, Namespace: "ns", Kind: "deployment", Workload: "web", Container: "app", StepSpans: spans, Metrics: []Series{
			{CPUUsage, all}, {CPUUsageMax, all}, {MemoryWorkingSet, all}, {MemoryWorkingSetMax, all},
			{CPURequest, all}, {MemoryRequest, all},
		}},
	} {
		if err := w.Resource(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	out, err := exec.Command(py, "../../scripts/validate_contract.py", path).CombinedOutput()
	if err != nil {
		t.Fatalf("validator rejected the file:\n%s", out)
	}
}
