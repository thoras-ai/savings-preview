package prometheus

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

func TestOwnerKey(t *testing.T) {
	for pod, want := range map[string]string{
		"web-7d4b9c8f6-x7k2p": "web-7d4b9c8f6",
		"db-0":                "db",
		"my-db-12":            "my-db",
		"nodash":              "",
		"-x":                  "",
	} {
		if got := ownerKey(pod); got != want {
			t.Errorf("ownerKey(%q) = %q, want %q", pod, got, want)
		}
	}
}

var (
	quotedString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	selectorName = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)?\{`)
)

// metricNames returns the metric names of the vector selectors in a query,
// with "" for a selector without one. Every selector of the adapter's
// queries has braces, except in the TSDB start probe of EarliestData.
func metricNames(q string) []string {
	if q == "min(prometheus_tsdb_lowest_timestamp_seconds)" {
		return []string{"prometheus_tsdb_lowest_timestamp_seconds"}
	}
	var out []string
	for _, m := range selectorName.FindAllStringSubmatch(quotedString.ReplaceAllString(q, `""`), -1) {
		out = append(out, m[1])
	}
	return out
}

// oneMetricStore rejects, like Azure Monitor managed Prometheus, a query that
// does not read exactly one metric.
func oneMetricStore(t *testing.T, h func(string, url.Values) (int, any)) func(string, url.Values) (int, any) {
	return func(path string, q url.Values) (int, any) {
		if path == "/api/v1/query" || path == "/api/v1/query_range" {
			names := map[string]bool{}
			for _, n := range metricNames(q.Get("query")) {
				names[n] = true
			}
			if len(names) != 1 || names[""] {
				t.Errorf("query reads %d metrics: %s", len(names), q.Get("query"))
				return 400, map[string]any{"status": "error", "errorType": "bad_data", "error": "query must be scoped to one metric"}
			}
		}
		return h(path, q)
	}
}

func TestMetricNames(t *testing.T) {
	for q, want := range map[string]string{
		`count(up{})`:                     "up",
		CPUUsage(Deployment, time.Minute): "container_cpu_usage_seconds_total kube_pod_owner kube_replicaset_owner",
		`vector(1)`:                       "",
		`sum({__name__="a"})`:             "",
		`max by (namespace) (kube_pod_owner{owner_name="x{y}"})`: "kube_pod_owner",
	} {
		names := metricNames(q)
		sort.Strings(names)
		if got := strings.Join(names, " "); got != want && !(want == "" && len(names) == 1 && names[0] == "") {
			t.Errorf("%s: got %q, want %q", q, got, want)
		}
	}
}

// splitPod is one pod of splitCluster.
type splitPod struct {
	name, ownerKind, ownerName string
	running                    bool
	// lingering is set for a deleted pod whose usage cAdvisor still reports.
	lingering   bool
	cpu, memory float64 // usage; zero for none
	containers  []string
	cpuRequest  float64
	// restarts of the first container at and after restartAt, whose last
	// termination is then an out-of-memory kill.
	restartAt int64
}

// splitCluster answers the queries of a store that reads one metric per
// query from a model of the pods in namespace shop: a Deployment web in the
// middle of a rollout from ReplicaSet web-7d4b9c to web-5f6c8d, with an
// evicted pod of the old ReplicaSet that kube-state-metrics still reports
// and a deleted pod of an older one that cAdvisor still reports; a
// StatefulSet db; a DaemonSet; and a StatefulSet named like a ReplicaSet of
// the Deployment api, so that their pods share an owner key.
func splitCluster(oomAt int64) (func(string, url.Values) (int, any), *sync.Map) {
	pods := []splitPod{
		{name: "web-7d4b9c-aaaaa", ownerKind: "ReplicaSet", ownerName: "web-7d4b9c", running: true, cpu: 0.4, memory: 200, containers: []string{"app"}, cpuRequest: 1},
		{name: "web-9a8b7c-eeeee", ownerKind: "ReplicaSet", ownerName: "web-9a8b7c", lingering: true, cpu: 5, memory: 5000, containers: []string{"app"}, cpuRequest: 9},
		{name: "web-7d4b9c-bbbbb", ownerKind: "ReplicaSet", ownerName: "web-7d4b9c", containers: []string{"app"}, cpuRequest: 1},
		{name: "web-5f6c8d-ccccc", ownerKind: "ReplicaSet", ownerName: "web-5f6c8d", running: true, cpu: 0.1, memory: 100, containers: []string{"app", "sidecar"}, cpuRequest: 0.5, restartAt: oomAt},
		{name: "web-5f6c8d-ddddd", ownerKind: "ReplicaSet", ownerName: "web-5f6c8d", running: true, cpu: 0.1, memory: 100, containers: []string{"app", "sidecar"}, cpuRequest: 0.5},
		{name: "db-0", ownerKind: "StatefulSet", ownerName: "db", running: true, cpu: 0.2, memory: 300, containers: []string{"db"}, cpuRequest: 0.05},
		{name: "db-1", ownerKind: "StatefulSet", ownerName: "db", running: true, cpu: 0.3, memory: 300, containers: []string{"db"}, cpuRequest: 0.05},
		{name: "fluentd-zzzzz", ownerKind: "DaemonSet", ownerName: "fluentd", running: true, cpu: 1, memory: 1000, containers: []string{"fluentd"}, cpuRequest: 1},
		{name: "api-6b7c-qqqqq", ownerKind: "ReplicaSet", ownerName: "api-6b7c", running: true, cpu: 1, memory: 1, containers: []string{"api"}, cpuRequest: 1},
		{name: "api-6b7c-0", ownerKind: "StatefulSet", ownerName: "api-6b7c", running: true, cpu: 1, memory: 1, containers: []string{"api"}, cpuRequest: 1},
	}
	replicaSets := map[string]string{"web-9a8b7c": "web", "web-7d4b9c": "web", "web-5f6c8d": "web", "api-6b7c": "api"}
	first := int64(t0) - 3*86400
	queries := &sync.Map{}

	vec := func(res []any) (int, any) {
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}}
	}
	mat := func(res []any) (int, any) {
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": res}}
	}
	// grouped aggregates per-pod values by namespace, owner key and,
	// unless podLevel, container, as byOwnerKey does on the store.
	grouped := func(q url.Values, agg string, podLevel bool, val func(p splitPod, c string, t int64) (float64, bool)) (int, any) {
		s, e, step := atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step"))
		if s < first {
			s = first
		}
		type g struct{ key, container string }
		vals := map[g]map[int64][]float64{}
		for _, p := range pods {
			cs := p.containers
			if podLevel {
				cs = []string{""}
			}
			for _, c := range cs {
				k := g{ownerKey(p.name), c}
				for t := s; t <= e; t += step {
					if v, ok := val(p, c, t); ok {
						if vals[k] == nil {
							vals[k] = map[int64][]float64{}
						}
						vals[k][t] = append(vals[k][t], v)
					}
				}
			}
		}
		var res []any
		for k, byT := range vals {
			var points []any
			for t := s; t <= e; t += step {
				vs, ok := byT[t]
				if !ok {
					continue
				}
				sum := 0.0
				for _, v := range vs {
					sum += v
				}
				out := float64(len(vs))
				if agg == "avg" {
					out = sum / float64(len(vs))
				}
				points = append(points, []any{float64(t), fmt.Sprint(out)})
			}
			labels := map[string]string{"namespace": "shop", ownerKeyLabel: k.key}
			if !podLevel {
				labels["container"] = k.container
			}
			res = append(res, map[string]any{"metric": labels, "values": points})
		}
		return mat(res)
	}
	usage := func(cpu bool) func(p splitPod, c string, t int64) (float64, bool) {
		return func(p splitPod, c string, t int64) (float64, bool) {
			if !p.running && !p.lingering {
				return 0, false
			}
			if cpu {
				return p.cpu, true
			}
			return p.memory, true
		}
	}

	return func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		queries.Store(path+" "+query, true)
		switch path {
		case "/api/v1/status/buildinfo":
			return 200, map[string]any{"status": "success", "data": map[string]any{"version": "2.53.0"}}
		case "/api/v1/query":
			switch {
			case strings.Contains(query, "prometheus_tsdb_lowest"), strings.HasPrefix(query, "last_over_time(kube_node"):
				return vec(nil)
			case strings.Contains(query, "last_over_time(kube_pod_owner"):
				var res []any
				for _, p := range pods {
					res = append(res, map[string]any{"metric": map[string]string{"namespace": "shop", "pod": p.name,
						"owner_kind": p.ownerKind, "owner_name": p.ownerName}, "value": []any{1.0, "1"}})
				}
				return vec(res)
			case strings.Contains(query, "last_over_time(kube_replicaset_owner"):
				var res []any
				for rs, d := range replicaSets {
					res = append(res, map[string]any{"metric": map[string]string{"namespace": "shop", "replicaset": rs, "owner_name": d}, "value": []any{1.0, "1"}})
				}
				return vec(res)
			}
			if atoi(q.Get("time")) < first {
				return vec(nil)
			}
			return 200, vector(1)
		}
		agg := "avg"
		if strings.HasPrefix(query, "count") {
			agg = "count"
		}
		resource := func(want float64, memory float64) func(p splitPod, c string, t int64) (float64, bool) {
			return func(p splitPod, c string, t int64) (float64, bool) {
				if strings.Contains(query, `"memory"`) {
					return memory, true
				}
				return want * p.cpuRequest, true
			}
		}
		s, e, step := atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step"))
		switch {
		case strings.Contains(query, "container_cpu_usage_seconds_total"):
			return grouped(q, agg, false, usage(true))
		case strings.Contains(query, "container_memory_working_set_bytes"):
			return grouped(q, agg, false, usage(false))
		case strings.Contains(query, "kube_pod_container_resource_requests"):
			return grouped(q, agg, false, resource(1, 64))
		case strings.Contains(query, "kube_pod_container_resource_limits"):
			return grouped(q, agg, false, resource(2, 128))
		case strings.Contains(query, "kube_pod_status_phase"):
			return grouped(q, "count", true, func(p splitPod, _ string, _ int64) (float64, bool) { return 1, p.running })
		case strings.Contains(query, "kube_pod_container_status_last_terminated_reason"):
			var res []any
			for _, p := range pods {
				if p.restartAt == 0 || e < p.restartAt {
					continue
				}
				res = append(res, map[string]any{"metric": map[string]string{"namespace": "shop", "pod": p.name, "container": p.containers[0]},
					"values": matrix(nil, max(s, p.restartAt), e, step, func(int64) float64 { return 1 })["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["values"]})
			}
			return mat(res)
		case strings.Contains(query, "kube_pod_container_status_restarts_total"):
			var res []any
			for _, p := range pods {
				if !strings.Contains(query, regexp.QuoteMeta(p.name)) {
					continue
				}
				for _, c := range p.containers {
					r := matrix(map[string]string{"namespace": "shop", "pod": p.name, "container": c}, max(s, first), e, step, func(t int64) float64 {
						if c == p.containers[0] && p.restartAt != 0 && t >= p.restartAt {
							return 3
						}
						return 2
					})
					res = append(res, r["data"].(map[string]any)["result"].([]any)...)
				}
			}
			return mat(res)
		case strings.Contains(query, "kube_deployment_spec_replicas"):
			return 200, matrix(map[string]string{"namespace": "shop", "workload": "web"}, max(s, first), e, step, func(int64) float64 { return 3 })
		case strings.Contains(query, "kube_statefulset_replicas"):
			return 200, matrix(map[string]string{"namespace": "shop", "workload": "db"}, max(s, first), e, step, func(int64) float64 { return 2 })
		case strings.Contains(query, "kube_node_status_allocatable"):
			v := 4.0
			if strings.Contains(query, `"memory"`) {
				v = 16 << 30
			}
			return 200, matrix(map[string]string{"node": "aks-pool-1"}, max(s, first), e, step, func(int64) float64 { return v })
		}
		return mat(nil)
	}, queries
}

func TestRunOneMetricPerQuery(t *testing.T) {
	oomAt := int64(t0) - 86400
	h, queries := splitCluster(oomAt)
	f := &fakeStore{handle: limitedStore(AzureMaxRange, oneMetricStore(t, h))}
	c := f.client(t)
	c.MaxRange, c.OneMetricPerQuery = AzureMaxRange, true
	step := 5 * time.Minute
	file, rep, err := Run(context.Background(), Options{Client: c, Cluster: "aks", Days: 40, Step: step, Now: time.Unix(t0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)

	at := func(r *contract.Resource, name string) float64 {
		t.Helper()
		pts := series(r, name)
		if len(pts) == 0 {
			t.Fatalf("%s/%s has no %s", r.Workload, r.Container, name)
		}
		return pts[len(pts)-1].V
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	app := find(file, "shop", Deployment, "web", "app")
	if app == nil {
		t.Fatalf("web/app is missing: %+v", file.Resources)
	}
	// Usage is the mean over the three running pods of both ReplicaSets.
	near("web/app cpu", at(app, contract.CPUUsage), (0.4+0.1+0.1)/3)
	near("web/app memory", at(app, contract.MemoryWorkingSet), (200+100+100)/3.0)
	// The evicted pod's request does not count.
	near("web/app cpu request", at(app, contract.CPURequest), (1+0.5+0.5)/3)
	near("web/app cpu limit", at(app, contract.CPULimit), 2*(1+0.5+0.5)/3)
	near("web/app memory request", at(app, contract.MemoryRequest), 64)
	sidecar := find(file, "shop", Deployment, "web", "sidecar")
	if sidecar == nil {
		t.Fatal("web/sidecar is missing")
	}
	near("web/sidecar cpu", at(sidecar, contract.CPUUsage), 0.1)
	db := find(file, "shop", StatefulSet, "db", "db")
	if db == nil {
		t.Fatal("db/db is missing")
	}
	near("db cpu", at(db, contract.CPUUsage), 0.25)
	near("db cpu request", at(db, contract.CPURequest), 0.05)

	oom := series(app, contract.OOMKills)
	if len(oom) != 1 || oom[0].T != oomAt || oom[0].V != 1 {
		t.Errorf("oom kills %+v, want one at %d", oom, oomAt)
	}
	if rep.OOMKills != 1 {
		t.Errorf("report counts %d oom kills", rep.OOMKills)
	}
	for _, r := range file.Resources {
		if r.Workload == "fluentd" || r.Workload == "api" || r.Workload == "api-6b7c" {
			t.Errorf("unexpected workload %s/%s", r.Kind, r.Workload)
		}
	}
	if rep.Workloads != 2 || rep.Containers != 3 || rep.Nodes != 1 {
		t.Errorf("report %+v", rep)
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || strings.Contains(w, "shop/api-6b7c")
	}
	if !warned {
		t.Errorf("no warning about the shared owner key: %q", rep.Warnings)
	}

	var restarts []string
	queries.Range(func(k, _ any) bool {
		if q := k.(string); strings.Contains(q, "restarts_total{namespace=") {
			restarts = append(restarts, q)
		}
		return true
	})
	for _, q := range restarts {
		if !strings.Contains(q, `pod=~"web-5f6c8d-ccccc"`) {
			t.Errorf("restarts read for other pods: %s", q)
		}
	}
	if len(restarts) == 0 {
		t.Error("restarts were not read")
	}
}

func TestOneMetricPerQueryNeedsContainerUsage(t *testing.T) {
	h, _ := splitCluster(0)
	f := &fakeStore{handle: oneMetricStore(t, func(path string, q url.Values) (int, any) {
		if path == "/api/v1/query" && q.Get("query") == UsageProbe {
			return 200, empty()
		}
		return h(path, q)
	})}
	c := f.client(t)
	c.OneMetricPerQuery = true
	_, _, err := Run(context.Background(), Options{Client: c, Cluster: "aks", Days: 1, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	if err == nil || !strings.Contains(err.Error(), "one metric per query") {
		t.Fatalf("got %v", err)
	}
}

func TestPodPatternBatches(t *testing.T) {
	h, queries := splitCluster(0)
	f := &fakeStore{handle: h}
	c := f.client(t)
	c.OneMetricPerQuery = true
	own := &owners{pods: map[nsName]wKey{}}
	var terminated []Series
	for i := range 300 {
		pod := fmt.Sprintf("web-5f6c8d-%05d", i)
		own.pods[nsName{"shop", pod}] = wKey{Deployment, "shop", "web"}
		terminated = append(terminated, Series{Labels: map[string]string{"namespace": "shop", "pod": pod, "container": "app"}})
	}
	o := Options{Client: c, Step: time.Minute, Now: time.Unix(t0, 0), Concurrency: 2, Log: func(string, ...any) {}}
	if err := pullOOMKills(context.Background(), o, time.Unix(t0-3600, 0), own, terminated, map[cKey][]contract.Point{}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	queries.Range(func(k, _ any) bool {
		q := k.(string)
		m := regexp.MustCompile(`pod=~"([^"]*)"`).FindStringSubmatch(q)
		if m == nil {
			return true
		}
		if len(m[1]) > maxPodsPattern {
			t.Errorf("pattern of %d characters", len(m[1]))
		}
		for _, p := range strings.Split(m[1], "|") {
			seen[p]++
		}
		return true
	})
	if len(seen) != 300 {
		t.Errorf("%d pods queried, want 300", len(seen))
	}
}

func TestOneMetricPerQueryStatefulSetStatusReplicas(t *testing.T) {
	h, _ := splitCluster(0)
	f := &fakeStore{handle: oneMetricStore(t, func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		switch {
		case path == "/api/v1/query" && query == "count(kube_statefulset_replicas{})":
			return 200, empty()
		case strings.Contains(query, "kube_statefulset_replicas"):
			t.Errorf("read the missing desired replicas: %s", query)
		case strings.Contains(query, "kube_statefulset_status_replicas"):
			return 200, matrix(map[string]string{"namespace": "shop", "workload": "db"}, atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step")),
				func(int64) float64 { return 2 })
		}
		return h(path, q)
	})}
	c := f.client(t)
	c.OneMetricPerQuery = true
	file, rep, err := Run(context.Background(), Options{Client: c, Cluster: "aks", Days: 1, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	var replicas []contract.Point
	for _, r := range file.Resources {
		if r.Kind == StatefulSet && r.Workload == "db" && r.Container == "" {
			replicas = series(&r, contract.StatefulSetPods)
		}
	}
	if len(replicas) == 0 || replicas[0].V != 2 {
		t.Errorf("db replicas %+v", replicas)
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || strings.Contains(w, "kube_statefulset_status_replicas")
	}
	if !warned {
		t.Errorf("warnings %q", rep.Warnings)
	}
}
