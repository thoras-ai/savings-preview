package prometheus

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

const t0 = 1_790_000_100 // a multiple of 300

// clusterStore answers like a store holding three days of data for one
// deployment, one statefulset and one node.
func clusterStore(withOwners, withRequests, requestsForContainers bool) func(string, url.Values) (int, any) {
	first := int64(t0) - 3*86400
	return func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		if path == "/api/v1/status/buildinfo" {
			return 200, map[string]any{"status": "success", "data": map[string]any{"version": "3.0.0"}}
		}
		if path == "/api/v1/query" {
			switch {
			case query == "vector(1)":
				return 200, vector(1)
			case strings.Contains(query, "prometheus_tsdb_lowest"):
				return 200, empty()
			case strings.HasPrefix(query, "last_over_time(kube_node_info"):
				return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
					map[string]any{"metric": map[string]string{"node": "n1", "provider_id": "aws:///us-east-1a/i-0abc"}, "value": []any{1.0, "1"}}}}}
			case strings.HasPrefix(query, "last_over_time(kube_node_labels"):
				return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{
					map[string]any{"metric": map[string]string{"node": "n1",
						"label_node_kubernetes_io_instance_type": "m5.xlarge", "label_topology_kubernetes_io_region": "us-east-1",
						"label_eks_amazonaws_com_capacityType": "SPOT"}, "value": []any{1.0, "1"}}}}}
			case query == OwnerProbe:
				if !withOwners {
					return 200, empty()
				}
			case query == RequestsProbe:
				if !withRequests {
					return 200, empty()
				}
			}
			if atoi(q.Get("time")) < first {
				return 200, empty()
			}
			return 200, vector(1)
		}
		s, e, step := atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step"))
		if s < first {
			s = first
		}
		isDep := strings.Contains(query, "kube_replicaset_owner") || strings.Contains(query, "kube_deployment_spec_replicas")
		kind := "web"
		if !isDep {
			kind = "db"
		}
		ns := map[string]string{"namespace": "shop", "workload": kind}
		with := func(c string) map[string]string {
			return map[string]string{"namespace": "shop", "workload": kind, "container": c}
		}
		series := func(l map[string]string, v float64) (int, any) {
			return 200, matrix(l, s, e, step, func(int64) float64 { return v })
		}
		switch {
		case strings.Contains(query, "kube_node_status_allocatable"):
			if strings.Contains(query, `"cpu"`) {
				return series(map[string]string{"node": "n1"}, 4)
			}
			return series(map[string]string{"node": "n1"}, 16*1024*1024*1024)
		case strings.Contains(query, "container_cpu_usage_seconds_total"):
			return series(with("app"), 0.25)
		case strings.Contains(query, "container_memory_working_set_bytes"):
			return series(with("app"), 100*1024*1024)
		case strings.Contains(query, "kube_pod_container_resource_requests") && requestsForContainers:
			if strings.Contains(query, `"cpu"`) {
				return series(with("app"), 0.5)
			}
			return series(with("app"), 256*1024*1024)
		case strings.Contains(query, "kube_pod_container_resource_limits"):
			if strings.Contains(query, `"cpu"`) && isDep {
				return series(with("app"), 1)
			}
		case strings.Contains(query, "kube_deployment_spec_replicas"), strings.Contains(query, "kube_statefulset_replicas"):
			return series(ns, 3)
		}
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
	}
}

func runAgainst(t *testing.T, f *fakeStore) (*contract.File, *Report, error) {
	t.Helper()
	return Run(context.Background(), Options{Client: f.client(t), Cluster: "test", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
}

func validate(t *testing.T, f *contract.File) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(py, "../../../scripts/validate_contract.py", path).CombinedOutput(); err != nil {
		t.Fatalf("validator rejected the file:\n%s", out)
	}
}

func find(f *contract.File, ns, kind, wl, container string) *contract.Resource {
	for i := range f.Resources {
		r := &f.Resources[i]
		if r.Node == nil && r.Namespace == ns && r.Kind == kind && r.Workload == wl && r.Container == container {
			return r
		}
	}
	return nil
}

func series(r *contract.Resource, name string) []contract.Point {
	for _, s := range r.Metrics {
		if s.Name == name {
			return s.Points
		}
	}
	return nil
}

func TestRunProducesValidContract(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.Containers != 2 || rep.Workloads != 2 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	if rep.FullHistory {
		t.Error("the store has 3 days, not 7")
	}
	web := find(file, "shop", Deployment, "web", "app")
	if web == nil {
		t.Fatal("no web/app resource")
	}
	if got := series(web, contract.CPURequest); len(got) == 0 || got[0].V != 0.5 {
		t.Errorf("cpu request: %v", got)
	}
	if got := series(web, contract.CPULimit); len(got) == 0 {
		t.Error("the cpu limit that exists must be reported")
	}
	if got := series(web, contract.MemoryLimit); len(got) != 0 {
		t.Error("a limit that is not set must be left out")
	}
	if web.Tier != 3 {
		t.Errorf("3 days at 5 minutes is tier 3, got %d", web.Tier)
	}
	if find(file, "shop", StatefulSet, "db", "app") == nil || find(file, "shop", StatefulSet, "db", "") == nil {
		t.Error("the statefulset and its workload resource must both be present")
	}
	for _, r := range file.Resources {
		if r.Node != nil && (r.Node.InstanceType != "m5.xlarge" || r.Node.Provider != "aws" || r.Node.CapacityType != "spot" || r.Node.Region != "us-east-1") {
			t.Errorf("node attributes: %+v", r.Node)
		}
	}
}

func TestMissingRequestsAreZeroWhenKubeStateMetricsAnswers(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, false)}
	file, _, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	r := find(file, "shop", Deployment, "web", "app")
	got := series(r, contract.CPURequest)
	if len(got) == 0 {
		t.Fatalf("an unset request must be reported as zero, missing=%v", r.Missing)
	}
	for _, p := range got {
		if p.V != 0 {
			t.Fatalf("want zeros, got %v", p)
		}
	}
	if len(r.Missing) != 0 {
		t.Errorf("nothing is missing, got %v", r.Missing)
	}
}

func TestWithoutRequestMetricsRequestsAreDeclaredMissing(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, false, false)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	r := find(file, "shop", Deployment, "web", "app")
	if len(r.Missing) != 2 || len(series(r, contract.CPURequest)) != 0 {
		t.Errorf("requests must be listed as missing, not invented: missing=%v", r.Missing)
	}
	var warned bool
	for _, w := range rep.Warnings {
		warned = warned || strings.Contains(w, "requests are unavailable")
	}
	if !warned {
		t.Errorf("the report must say requests are unavailable: %v", rep.Warnings)
	}
}

func TestWithoutKubeStateMetricsIsAClearError(t *testing.T) {
	f := &fakeStore{handle: clusterStore(false, false, false)}
	_, _, err := runAgainst(t, f)
	if err == nil || !strings.Contains(err.Error(), "kube-state-metrics is not in the store") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNoContainerMetricsIsAClearError(t *testing.T) {
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if q.Get("query") == "vector(1)" {
			return 200, vector(1)
		}
		return 200, empty()
	}}
	_, _, err := runAgainst(t, f)
	if err == nil || !strings.Contains(err.Error(), "no per-container usage metrics") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestQueriesAvoidDoubleCounting(t *testing.T) {
	for _, q := range []string{CPUUsage(Deployment, time.Minute), MemoryUsage(StatefulSet)} {
		if !strings.Contains(q, `container!=""`) || !strings.Contains(q, `container!="POD"`) || !strings.Contains(q, `image!=""`) {
			t.Errorf("query must exclude pod totals and pause containers:\n%s", q)
		}
	}
	if !strings.Contains(Requests(Deployment, "cpu"), `kube_pod_status_phase{phase="Running"} == 1`) {
		t.Error("requests must be limited to running pods")
	}
}
