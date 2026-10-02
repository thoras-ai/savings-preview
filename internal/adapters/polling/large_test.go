package polling

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// largeReader is a cluster of 500 single-pod Deployments with two containers
// each, 1,000 workload-containers in all, on 20 nodes. Usage cycles through
// ten precomputed variants.
type largeReader struct {
	clock    *fakeClock
	base     Cluster
	variants [][]PodMetrics
	reads    int
}

func newLargeReader(clk *fakeClock) *largeReader {
	r := &largeReader{clock: clk}
	one := int32(1)
	for i := 0; i < 500; i++ {
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
	c := r.base
	c.PodMetrics = r.variants[r.reads%len(r.variants)]
	return &c, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func dirSize(t *testing.T, dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// TestLargeClusterStaysBounded polls 1,000 workload-containers every minute for
// 14 days (20,160 readings) and checks that the readings directory and the
// heap stay small, including during the exports.
func TestLargeClusterStaysBounded(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("takes most of a minute, and longer than the default timeout under -race")
	}
	var outBytes atomic.Int64
	createOutput = func(path string, write func(io.Writer) error) error {
		var c countingWriter
		err := write(&c)
		outBytes.Store(c.n)
		return err
	}
	defer func() { createOutput = writeAtomic }()

	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
		}
	}()

	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 14*24*time.Hour - time.Minute
	o.WriteEvery = 6 * 24 * time.Hour // two exports while polling, then the final one
	rd := newLargeReader(clk)
	start := time.Now()
	if err := Run(context.Background(), o, rd, clk, log); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-sampled

	size := dirSize(t, ReadingsDir(out))
	t.Logf("%d readings in %s; readings directory %.1f MB; contract file %.0f MB; peak heap %.0f MB",
		rd.reads, time.Since(start).Round(time.Second), float64(size)/1e6, float64(outBytes.Load())/1e6, float64(peak.Load())/1e6)
	if rd.reads != 20160 {
		t.Errorf("%d reads, want 20160", rd.reads)
	}
	if size > 150e6 {
		t.Errorf("readings directory is %d bytes", size)
	}
	if p := peak.Load(); p > 400<<20 {
		t.Errorf("peak heap %d bytes", p)
	}
	if !strings.Contains(log.String(), "20160 readings covering 14d0h0m") || !strings.Contains(log.String(), "fidelity tier 2") {
		t.Errorf("progress:\n%s", lastLines(log.String(), 3))
	}
	segs, _ := segments(ReadingsDir(out))
	raw := 0
	for _, s := range segs {
		if s.raw {
			raw++
		}
	}
	if raw > 50 {
		t.Errorf("%d raw segments left; older ones should be rolled up", raw)
	}
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}
