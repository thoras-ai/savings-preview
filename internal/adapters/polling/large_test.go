package polling

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// largeReader is a cluster of single-pod Deployments with two containers
// each, on 20 nodes. Usage cycles through ten precomputed variants.
type largeReader struct {
	clock    *fakeClock
	base     Cluster
	variants [][]PodMetrics
	reads    int
	onRead   func(reads int) // called before each read when set
}

func newLargeReader(clk *fakeClock, deployments int) *largeReader {
	r := &largeReader{clock: clk}
	one := int32(1)
	for i := 0; i < deployments; i++ {
		ns := fmt.Sprintf("ns-%d", i%25)
		dep, rs, pod := fmt.Sprintf("app-%d", i), fmt.Sprintf("app-%d-5d9f", i), fmt.Sprintf("app-%d-5d9f-x", i)
		r.base.Deployments = append(r.base.Deployments, appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: dep, Namespace: ns}, Spec: appsv1.DeploymentSpec{Replicas: &one}})
		r.base.ReplicaSets = append(r.base.ReplicaSets, appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: rs, Namespace: ns, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: dep}}}})
		r.base.Pods = append(r.base.Pods, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: ns, OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "500m", "memory", "1Gi"), Limits: rl("cpu", "1", "memory", "2Gi")}},
				{Name: "proxy", Resources: corev1.ResourceRequirements{Requests: rl("cpu", "100m", "memory", "128Mi")}},
			}},
		})
	}
	for i := 0; i < 20; i++ {
		r.base.Nodes = append(r.base.Nodes, corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), Labels: map[string]string{"node.kubernetes.io/instance-type": "m5.4xlarge"}},
			Status:     corev1.NodeStatus{Allocatable: rl("cpu", "15890m", "memory", "62Gi")},
		})
	}
	for v := 0; v < 10; v++ {
		var pms []PodMetrics
		for i, p := range r.base.Pods {
			var pm PodMetrics
			pm.Metadata.Name, pm.Metadata.Namespace = p.Name, p.Namespace
			pm.Containers = []ContainerUsage{
				{Name: "app", Usage: rl("cpu", fmt.Sprintf("%dn", 123456789+v*7654321+i*1013), "memory", fmt.Sprintf("%dKi", 600000+v*12345+i))},
				{Name: "proxy", Usage: rl("cpu", fmt.Sprintf("%dn", 9876543+v*12345), "memory", fmt.Sprintf("%dKi", 50000+v*321))},
			}
			pms = append(pms, pm)
		}
		r.variants = append(r.variants, pms)
	}
	return r
}

func (r *largeReader) Read(ctx context.Context) (*Cluster, error) {
	r.reads++
	if r.onRead != nil {
		r.onRead(r.reads)
	}
	c := r.base
	c.PodMetrics = r.variants[r.reads%len(r.variants)]
	return &c, nil
}

func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// TestLongRunStaysBounded polls every five minutes for 14 days (4,032
// readings) and checks that raw segments older than the retention are rolled
// up, so the readings directory grows at the rollup step rather than the
// polling step, and that the heap does not grow with the readings taken.
func TestLongRunStaysBounded(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Interval = 5 * time.Minute
	o.Rollup = time.Hour
	o.Duration = 14*24*time.Hour - o.Interval
	// Export after the first reading and at the end only, so no export is in
	// flight when the heap is measured. The other run tests export while
	// polling.
	o.WriteEvery = 30 * 24 * time.Hour
	rd := newLargeReader(clk, 10)
	// The live heap after a collection at reading 1,100, once rollups have
	// started, and at reading 4,000: state kept per reading would grow
	// between the two.
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	var early, late uint64
	rd.onRead = func(reads int) {
		if reads != 1100 && reads != 4000 {
			return
		}
		runtime.GC()
		metrics.Read(live)
		if reads == 1100 {
			early = live[0].Value.Uint64()
		} else {
			late = live[0].Value.Uint64()
		}
	}
	if err := Run(context.Background(), o, rd, clk, log); err != nil {
		t.Fatal(err)
	}
	t.Logf("live heap %.2f MB at reading 1100, %.2f MB at reading 4000", float64(early)/1e6, float64(late)/1e6)
	if late > early+256<<10 {
		t.Errorf("live heap grew from %d bytes at reading 1100 to %d at reading 4000", early, late)
	}

	if rd.reads != 4032 {
		t.Errorf("%d reads, want 4032", rd.reads)
	}
	if !strings.Contains(log.String(), "4032 readings covering 14d0h0m") || !strings.Contains(log.String(), "fidelity tier 3") {
		t.Errorf("progress:\n%s", lastLines(log.String(), 3))
	}
	dir := ReadingsDir(out)
	segs, _ := segments(dir)
	var raw, rawBytes int64
	for _, s := range segs {
		if s.raw {
			raw++
			fi, err := os.Stat(filepath.Join(dir, rawName(s.start)))
			if err != nil {
				t.Fatal(err)
			}
			rawBytes += fi.Size()
		}
	}
	// 48 hours of hourly segments are kept raw, plus the one being written.
	if raw > 49 {
		t.Errorf("%d raw segments left; older ones should be rolled up", raw)
	}
	// Twelve rolled-up days hold one point per hour, so the directory is well
	// under half of what 14 days of raw segments would take.
	perRawDay := float64(rawBytes) / (float64(raw) / 24)
	size := dirSize(t, dir)
	t.Logf("%d readings; readings directory %.0f kB; raw segments %.0f kB a day", rd.reads, float64(size)/1e3, perRawDay/1e3)
	if float64(size) > 7*perRawDay {
		t.Errorf("readings directory is %d bytes, raw segments take %.0f a day", size, perRawDay)
	}
}

// synthSource yields records readings at a five-minute step for containers
// containers, building each record as it goes so the source holds nothing.
func synthSource(containers, records int) source {
	keys := make([]WorkloadKey, containers)
	for c := range keys {
		keys[c] = WorkloadKey{Namespace: "ns", Kind: "deployment", Name: fmt.Sprintf("app-%03d", c)}
	}
	return func(until int64, _ func(string) bool, fn func(*record) error) error {
		for r := 0; r < records; r++ {
			rec := &record{T: 1_790_000_000 + int64(r)*300, Step: 300, Readings: 1,
				Containers: make([]cEntry, 0, containers), Workloads: make([]wEntry, 0, containers)}
			if rec.T > until {
				break
			}
			for c, wk := range keys {
				rec.Containers = append(rec.Containers, cEntry{Key: ContainerKey{wk, "app"}, Levels: Levels{
					CPURequest: 500e6, MemoryRequest: 1 << 30, HasUsage: true,
					CPUUsage: int64(100e6 + r*1000 + c), MemoryUsage: int64(600e6 + r*10 + c),
				}})
				rec.Workloads = append(rec.Workloads, wEntry{wk, 1})
			}
			if err := fn(rec); err != nil {
				return err
			}
		}
		return nil
	}
}

// exportHeap exports src under budget and returns how far the live heap grew
// above where it started, sampled as records are read. A low GC target makes
// collections frequent, so the live heap of the last collection stays close
// to what the export holds.
func exportHeap(t *testing.T, src source, budget int64) uint64 {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtime.GC()
	metrics.Read(live)
	base, peak := live[0].Value.Uint64(), live[0].Value.Uint64()
	n := 0
	sampled := func(until int64, want func(string) bool, fn func(*record) error) error {
		return src(until, want, func(rec *record) error {
			if n++; n%16 == 0 {
				metrics.Read(live)
				peak = max(peak, live[0].Value.Uint64())
			}
			return fn(rec)
		})
	}
	if _, err := Export(sampled, "test", io.Discard, budget); err != nil {
		t.Fatal(err)
	}
	return peak - base
}

// TestExportHeapStaysFlat checks that export memory follows the budget rather
// than the number of resources. Exporting the larger cluster in one pass shows
// how much the heap would grow without the budget.
func TestExportHeapStaysFlat(t *testing.T) {
	const budget = 1 << 20
	few := exportHeap(t, synthSource(8, 2000), budget)
	many := exportHeap(t, synthSource(64, 2000), budget)
	whole := exportHeap(t, synthSource(64, 2000), math.MaxInt64)
	t.Logf("heap growth: %.1f MB for 8 containers, %.1f MB for 64, %.1f MB for 64 in one pass",
		float64(few)/1e6, float64(many)/1e6, float64(whole)/1e6)
	if limit := few + 2*budget; many > limit || whole <= limit {
		t.Errorf("heap grew %d bytes for 8 containers, %d for 64, %d for 64 in one pass; want the batched export under %d and the single pass over it",
			few, many, whole, limit)
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
