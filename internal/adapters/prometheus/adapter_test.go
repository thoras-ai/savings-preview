package prometheus

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
	validator "github.com/thoras-ai/savings-preview/internal/validate"
)

const t0 = 1_790_000_100 // a multiple of 300

// clusterStore answers like a store holding three days of data for one
// deployment, one statefulset and one node.
func clusterStore(withOwners, withRequests, requestsForContainers bool, drop ...string) func(string, url.Values) (int, any) {
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
			case dropped(drop, query), query == UnlabeledPods:
				return 200, empty()
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

// validate checks f with the Go port of scripts/validate_contract.py, whose
// agreement with the script internal/validate tests.
func validate(t *testing.T, f *contract.File) {
	t.Helper()
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	if errs := validator.Validate(buf.Bytes()); len(errs) > 0 {
		t.Fatalf("validator rejected the file:\n%s", strings.Join(errs[:min(len(errs), 10)], "\n"))
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
	if err == nil || !strings.Contains(err.Error(), "no container usage metrics") {
		t.Errorf("unexpected error: %v", err)
	}
	c := f.client(t)
	c.Selector = `cluster="b"`
	_, _, err = Run(context.Background(), Options{Client: c, Cluster: "test", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	if err == nil || !strings.Contains(err.Error(), "match the selector") {
		t.Errorf("unexpected error with a selector: %v", err)
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

func dropped(drop []string, query string) bool {
	for _, d := range drop {
		if query == d {
			return true
		}
	}
	return false
}

func warnings(rep *Report) string { return strings.Join(rep.Warnings, "\n") }

func TestMissingPhaseMetricMakesRequestsUnavailable(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true, PhaseProbe)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	r := find(file, "shop", Deployment, "web", "app")
	if len(r.Missing) != 2 || len(series(r, contract.CPURequest)) != 0 {
		t.Errorf("without the phase metric requests must be missing, not zero: missing=%v", r.Missing)
	}
	if !strings.Contains(warnings(rep), "kube_pod_status_phase") {
		t.Errorf("warnings: %s", warnings(rep))
	}
}

func TestMissingReplicaSetOwnerLeavesDeploymentsOutWithAWarning(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true, ReplicaSetProbe)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if find(file, "shop", Deployment, "web", "app") != nil {
		t.Error("deployments cannot be mapped without kube_replicaset_owner")
	}
	if find(file, "shop", StatefulSet, "db", "app") == nil {
		t.Error("statefulsets do not need it")
	}
	if !strings.Contains(warnings(rep), "kube_replicaset_owner") {
		t.Errorf("warnings: %s", warnings(rep))
	}
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), "kube_replicaset_owner") && q.Get("start") != "" {
			t.Error("no Deployment query should be sent")
			break
		}
	}
}

// groups answers an instant query with one series per label set.
func groups(sets ...map[string]string) map[string]any {
	var res []any
	for _, l := range sets {
		res = append(res, map[string]any{"metric": l, "value": []any{1.0, "1"}})
	}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}}
}

func TestMultipleClustersInOneStoreStop(t *testing.T) {
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if strings.HasPrefix(q.Get("query"), "count by (cluster, ") && strings.Contains(q.Get("query"), "kube_pod_owner") {
			return 200, groups(map[string]string{"cluster": "prod-us"}, map[string]string{"cluster": "prod-eu"}, map[string]string{"cluster": "dev"})
		}
		return h(path, q)
	}}
	_, _, err := runAgainst(t, f)
	var ce *ClusterError
	if !errors.As(err, &ce) {
		t.Fatalf("want a ClusterError, got %v", err)
	}
	if ce.Label != "cluster" || strings.Join(ce.Values, ",") != "dev,prod-eu,prod-us" || ce.Selected {
		t.Errorf("error: %+v", ce)
	}
	for _, want := range []string{"3 clusters", `"prod-eu"`, `--selector 'cluster="dev"'`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %s: %v", want, err)
		}
	}
	for _, q := range f.requests {
		if q.Get("start") != "" {
			t.Fatal("no history may be pulled from a store with several clusters")
		}
	}
}

func TestSeriesWithAndWithoutAClusterLabelCountAsTwoClusters(t *testing.T) {
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if strings.HasPrefix(q.Get("query"), "count by (cluster, ") && strings.Contains(q.Get("query"), "kube_node_info") {
			return 200, groups(map[string]string{"k8s_cluster": "a"}, map[string]string{})
		}
		return h(path, q)
	}}
	_, _, err := runAgainst(t, f)
	var ce *ClusterError
	if !errors.As(err, &ce) || ce.Label != "k8s_cluster" || len(ce.Values) != 2 || ce.Values[0] != "" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), `--selector 'k8s_cluster="a"'`) {
		t.Errorf("the suggestion must name a real value: %v", err)
	}
}

func TestOneLabelPerMetricIsOneCluster(t *testing.T) {
	// kube-state-metrics labeled, cAdvisor not: one cluster, not two.
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		if strings.HasPrefix(query, "count by (cluster, ") {
			if strings.Contains(query, "container_memory_working_set_bytes") {
				return 200, groups(map[string]string{})
			}
			return 200, groups(map[string]string{"cluster": "prod"})
		}
		return h(path, q)
	}}
	if _, _, err := runAgainst(t, f); err != nil {
		t.Fatal(err)
	}
}

func TestSelectorIsAppliedToEveryQuery(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true)}
	c := f.client(t)
	c.Selector = `cluster="prod-us"`
	file, _, err := Run(context.Background(), Options{Client: c, Cluster: "test", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	sent := 0
	for _, q := range f.requests {
		query := q.Get("query")
		if query == "" || query == "vector(1)" || strings.Contains(query, "prometheus_tsdb_lowest_timestamp_seconds") {
			continue
		}
		sent++
		if n, m := strings.Count(query, "{"), strings.Count(query, `{cluster="prod-us"`); n == 0 || n != m {
			t.Errorf("%d of %d selectors carry the matcher:\n%s", m, n, query)
		}
	}
	if sent < 10 {
		t.Errorf("only %d queries checked", sent)
	}
}

func TestSelectorStillMatchingSeveralClustersStops(t *testing.T) {
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if strings.HasPrefix(q.Get("query"), "count by (cluster, ") {
			return 200, groups(map[string]string{"cluster": "prod-us"}, map[string]string{"cluster": "prod-eu"})
		}
		return h(path, q)
	}}
	c := f.client(t)
	c.Selector = `cluster=~"prod-.*"`
	_, _, err := Run(context.Background(), Options{Client: c, Cluster: "test", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	var ce *ClusterError
	if !errors.As(err, &ce) || !ce.Selected || !strings.Contains(err.Error(), "the selector matches 2 clusters") {
		t.Fatalf("got %v", err)
	}
}

// podLevelStore answers like a store whose cAdvisor series have no container
// labels, only the pod cgroup series.
func podLevelStore(h func(string, url.Values) (int, any), withContainerInfo bool) func(string, url.Values) (int, any) {
	return func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		switch {
		case query == UsageProbe:
			return 200, empty()
		case query == ContainerProbe && !withContainerInfo:
			return 200, empty()
		case path == "/api/v1/query_range" && strings.Contains(query, "container_") && strings.Contains(query, `container!=""`):
			return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
		}
		return h(path, q)
	}
}

func TestPodLevelFallback(t *testing.T) {
	f := &fakeStore{handle: podLevelStore(clusterStore(true, true, true), true)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if !rep.UsageFromPods || rep.Containers != 2 {
		t.Fatalf("report: %+v", rep)
	}
	web := find(file, "shop", Deployment, "web", "app")
	if web == nil || len(series(web, contract.CPUUsage)) == 0 || len(series(web, contract.MemoryWorkingSet)) == 0 {
		t.Fatalf("usage must come from the pod series: %+v", web)
	}
	if len(web.Notes) != 1 || web.Notes[0] != UsageFromPodsNote {
		t.Errorf("notes: %v", web.Notes)
	}
	if find(file, "shop", Deployment, "web", "").Notes != nil {
		t.Error("the note belongs on container resources only")
	}
	usageQueries := map[string]bool{}
	for _, q := range f.requests {
		query := q.Get("query")
		if q.Get("start") != "" && strings.Contains(query, "container_cpu_usage_seconds_total") {
			usageQueries[query] = true
			if !strings.Contains(query, podCgroup) || !strings.Contains(query, `kube_pod_container_resource_requests{resource="cpu"}`) {
				t.Errorf("cpu usage must be split from the pod series by cpu requests:\n%s", query)
			}
		}
		if q.Get("start") == "" && q.Get("time") != "" && strings.Contains(query, "container_memory_working_set_bytes") &&
			!strings.Contains(query, "count by") && query != UsageProbe && query != PodUsageProbe &&
			query != `count(container_memory_working_set_bytes{})` && query != `count(container_memory_working_set_bytes{namespace!="",pod!=""})` {
			t.Errorf("unexpected instant query: %s", query)
		}
	}
	if len(usageQueries) != 2 {
		t.Errorf("want one cpu usage query per workload kind, got %d", len(usageQueries))
	}
}

func TestPodLevelFallbackNeedsContainerInfo(t *testing.T) {
	f := &fakeStore{handle: podLevelStore(clusterStore(true, true, true), false)}
	_, _, err := runAgainst(t, f)
	if err == nil || !strings.Contains(err.Error(), "kube_pod_container_info") {
		t.Fatalf("got %v", err)
	}
}

func TestContainerLevelStoreDoesNotFallBack(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true)}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.UsageFromPods || find(file, "shop", Deployment, "web", "app").Notes != nil {
		t.Error("a store with container labels must not be marked")
	}
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), "kube_pod_container_info") && q.Get("start") != "" {
			t.Error("no request split is needed")
		}
	}
}

func TestPodsWithoutContainerSeriesWarn(t *testing.T) {
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if q.Get("query") == UnlabeledPods {
			return 200, vector(4)
		}
		return h(path, q)
	}}
	_, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings(rep), "4 workload pods have pod-level usage but no per-container usage") {
		t.Errorf("warnings: %s", warnings(rep))
	}
}

func TestOOMKills(t *testing.T) {
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		if path == "/api/v1/query_range" && strings.Contains(query, "kube_pod_container_status_restarts_total") {
			if !strings.Contains(query, "kube_replicaset_owner") {
				return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
			}
			kill := int64(t0) - 86400
			s, e := atoi(q.Get("start")), atoi(q.Get("end"))
			if kill < s || kill > e {
				return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
			}
			return 200, matrix(map[string]string{"namespace": "shop", "workload": "web", "container": "app"}, kill, kill, 300, func(int64) float64 { return 2 })
		}
		return h(path, q)
	}}
	file, rep, err := runAgainst(t, f)
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	got := series(find(file, "shop", Deployment, "web", "app"), contract.OOMKills)
	if len(got) != 1 || got[0].V != 2 || rep.OOMKills != 2 {
		t.Errorf("oom kills: %v, report %d", got, rep.OOMKills)
	}
	if series(find(file, "shop", StatefulSet, "db", "app"), contract.OOMKills) != nil {
		t.Error("no kills, no series")
	}
}

func TestOOMKillsAreSkippedWithoutRestartMetrics(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true, RestartsProbe)}
	if _, _, err := runAgainst(t, f); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), "restarts_total") && q.Get("start") != "" {
			t.Error("no OOM query without restart metrics")
		}
	}
}

func TestOOMQueryCountsEachRestartOnce(t *testing.T) {
	q := OOMKills(Deployment, 5*time.Minute)
	for _, want := range []string{
		`max by (namespace, pod, container) (kube_pod_container_status_restarts_total{}) - max by (namespace, pod, container) (kube_pod_container_status_restarts_total{} offset 300s) > 0`,
		`kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`,
		"sum by (namespace, workload, container)",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s:\n%s", want, q)
		}
	}
}

func TestRequestSetPartwayThroughTheWindowIsZeroBefore(t *testing.T) {
	usage := []contract.Point{{T: 300}, {T: 600}, {T: 900}, {T: 1200}}
	req := []contract.Point{{T: 900, V: 0.5}, {T: 1200, V: 0.5}}
	replicas := []contract.Point{{T: 300, V: 1}, {T: 600, V: 1}, {T: 900, V: 1}, {T: 1200, V: 1}}
	got := fillRequests(req, usage, replicas)
	want := []contract.Point{{T: 300}, {T: 600}, {T: 900, V: 0.5}, {T: 1200, V: 0.5}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("point %d: got %v want %v", i, got[i], want[i])
		}
	}
	// No replica count means kube-state-metrics was not answering: leave the gap.
	if got := fillRequests(req, usage, replicas[2:]); len(got) != 2 {
		t.Errorf("gaps without replica counts must stay gaps, got %v", got)
	}
}

func TestJoinsDeduplicateTheirRightHandSide(t *testing.T) {
	for _, q := range []string{CPUUsage(Deployment, time.Minute), Requests(Deployment, "cpu"), Limits(StatefulSet, "memory")} {
		if strings.Contains(q, "* on (namespace, replicaset) group_left (workload)\n  label_replace") {
			t.Errorf("the ReplicaSet owner side must be wrapped in max by:\n%s", q)
		}
	}
	if !strings.Contains(podToDeployment, "max by (namespace, replicaset, workload) (label_replace(kube_replicaset_owner") {
		t.Error("kube_replicaset_owner must be deduplicated before the join")
	}
	if !strings.Contains(Requests(Deployment, "cpu"), `group_left () max by (namespace, pod) (kube_pod_status_phase{phase="Running"} == 1)`) {
		t.Error("kube_pod_status_phase must be deduplicated before the join")
	}
}

func TestFirstFailedQueryCancelsTheRest(t *testing.T) {
	var mu sync.Mutex
	rangeCalls := 0
	h := clusterStore(true, true, true)
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if path == "/api/v1/query_range" {
			mu.Lock()
			rangeCalls++
			mu.Unlock()
			return 422, map[string]any{"status": "error", "errorType": "execution", "error": "boom"}
		}
		return h(path, q)
	}}
	_, _, err := Run(context.Background(), Options{Client: f.client(t), Cluster: "x", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0), Concurrency: 1})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
	if rangeCalls > 2 {
		t.Errorf("queries kept running after the first failure: %d range calls", rangeCalls)
	}
}

func TestCapacityTypeLabels(t *testing.T) {
	for labels, want := range map[[2]string]string{
		{"eks.amazonaws.com/capacityType", "SPOT"}:        "spot",
		{"karpenter.sh/capacity-type", "on-demand"}:       "on_demand",
		{"karpenter.sh/capacity-type", "preemptible"}:     "spot",
		{"karpenter.sh/capacity-type", "Spot-Instance"}:   "spot",
		{"karpenter.sh/capacity-type", "reserved"}:        "unknown",
		{"cloud.google.com/gke-spot", "TRUE"}:             "spot",
		{"cloud.google.com/gke-spot", "true"}:             "spot",
		{"cloud.google.com/gke-preemptible", "true"}:      "spot",
		{"cloud.google.com/gke-preemptible", "false"}:     "unknown",
		{"kubernetes.azure.com/scalesetpriority", "Spot"}: "spot",
	} {
		if got := capacityType(map[string]string{labelKey(labels[0]): labels[1]}); got != want {
			t.Errorf("%v: %s, want %s", labels, got, want)
		}
	}
}
