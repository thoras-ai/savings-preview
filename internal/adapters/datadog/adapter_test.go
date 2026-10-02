package datadog

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
	validator "github.com/thoras-ai/savings-preview/internal/validate"
)

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

func peak(pts []contract.Point) float64 {
	m := 0.0
	for _, p := range pts {
		m = math.Max(m, p.V)
	}
	return m
}

func run(t *testing.T, f *fakeOrg, o Options) (*contract.File, *Report, error) {
	t.Helper()
	c, _ := testClient(f.server(t).URL)
	o.Client = c
	o.Now = time.Unix(testNow, 0)
	if o.Days == 0 {
		o.Days = 3
	}
	o.Concurrency = 1
	return Run(context.Background(), o)
}

func TestRunProducesValidContract(t *testing.T) {
	f := &fakeOrg{series: org("prod-us", 3)}
	file, rep, err := run(t, f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.Cluster != "prod-us" || rep.Containers != 3 || rep.Workloads != 2 || rep.Nodes != 2 || !rep.FullHistory {
		t.Errorf("report: %+v", rep)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("warnings: %v", rep.Warnings)
	}
	for _, r := range file.Resources {
		if r.Cluster != "prod-us" || r.Source != "datadog" || r.StepSeconds != 60 {
			t.Errorf("common attributes: %+v", r.Common)
		}
		if r.Namespace == "kube-system" {
			t.Errorf("a DaemonSet pod was reported: %+v", r)
		}
	}

	app := find(file, "shop", Deployment, "web", "app")
	if app == nil {
		t.Fatal("web/app is missing")
	}
	// Nanocores become cores: 2e8 nanocores is 0.2 cores, with 0.8 at the peaks.
	use, mx := series(app, contract.CPUUsage), series(app, contract.CPUUsageMax)
	if len(use) != 3*1440 || len(mx) != len(use) {
		t.Fatalf("points: usage %d, max %d", len(use), len(mx))
	}
	if math.Abs(peak(mx)-0.8) > 1e-9 {
		t.Errorf("CPU peak %v, want 0.8", peak(mx))
	}
	if v := use[10].V; math.Abs(v-0.2) > 1e-9 {
		t.Errorf("CPU usage %v, want 0.2", v)
	}
	// Within a step the peak lasts five minutes of the hour, so step means
	// and peaks agree at one-minute resolution; the max series is never
	// below the mean.
	for i := range use {
		if mx[i].T != use[i].T || mx[i].V < use[i].V {
			t.Fatalf("max point %v against usage %v", mx[i], use[i])
		}
	}
	if got := series(app, contract.CPURequest); len(got) == 0 || got[0].V != 1 {
		t.Errorf("CPU request %v", got[:1])
	}
	if got := series(app, contract.CPULimit); len(got) == 0 || got[0].V != 2 {
		t.Errorf("CPU limit missing")
	}
	if app.Tier != 3 {
		t.Errorf("3 days of history claims tier %d", app.Tier)
	}

	// A container that sets no request gets zero requests while the workload
	// has a replica count.
	envoy := find(file, "shop", Deployment, "web", "envoy")
	if envoy == nil || len(envoy.Missing) != 0 {
		t.Fatalf("envoy: %+v", envoy)
	}
	if got := series(envoy, contract.CPURequest); len(got) != len(series(envoy, contract.CPUUsage)) || peak(got) != 0 {
		t.Errorf("envoy CPU requests should be zero at every usage point")
	}
	if series(envoy, contract.CPULimit) != nil {
		t.Error("a limit that is not set must be left out")
	}

	if find(file, "shop", StatefulSet, "db", "postgres") == nil {
		t.Error("the StatefulSet container is missing")
	}
	var regions []string
	for _, r := range file.Resources {
		if r.Node != nil {
			regions = append(regions, r.Node.Name+"="+r.Node.Region)
		}
	}
	if strings.Join(regions, ",") != "ip-10-0-1-1=us-east-1,ip-10-0-1-2=unknown" {
		t.Errorf("node regions %v", regions)
	}
}

func TestChunksStayUnderThePointCap(t *testing.T) {
	f := &fakeOrg{series: org("prod-us", 14)}
	file, rep, err := run(t, f, Options{Days: 14})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	for _, a := range f.requests {
		if n := (a.To - a.From) / a.Interval; n > MaxPoints {
			t.Errorf("a request asks for %d points per series", n)
		}
	}
	app := find(file, "shop", Deployment, "web", "app")
	if n := len(series(app, contract.CPUUsage)); n != 14*1440 {
		t.Errorf("%d points, want %d", n, 14*1440)
	}
	if app.Tier != 1 || !rep.FullHistory {
		t.Errorf("14 days at 60 seconds should be tier 1, got %d", app.Tier)
	}
}

func TestCoarserIntervalSplitsTheChunk(t *testing.T) {
	// A cap below ChunkPoints makes the fake answer with a larger interval
	// until the adapter asks for less.
	f := &fakeOrg{series: org("prod-us", 1), maxPoints: 500}
	file, _, err := run(t, f, Options{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(series(find(file, "shop", Deployment, "web", "app"), contract.CPUUsage)); n != 1440 {
		t.Errorf("%d points, want 1440", n)
	}
}

func TestRateLimitWaitsForTheReset(t *testing.T) {
	f := &fakeOrg{series: org("prod-us", 1), rateLimited: 2}
	c, ns := testClient(f.server(t).URL)
	_, _, err := Run(context.Background(), Options{Client: c, Days: 1, Now: time.Unix(testNow, 0), Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns.waits) != 2 || ns.waits[0] != 4*time.Second {
		t.Errorf("waits %v, want two waits of the 3 s reset plus a second", ns.waits)
	}
}

func TestSeveralClustersStop(t *testing.T) {
	f := &fakeOrg{series: append(org("prod-us", 1), org("staging", 1)...)}
	_, _, err := run(t, f, Options{Days: 1})
	var ce *ClusterError
	if !errors.As(err, &ce) || strings.Join(ce.Values, ",") != "prod-us,staging" || ce.Selected {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "--selector kube_cluster_name:prod-us") {
		t.Errorf("error does not suggest a selector: %v", err)
	}

	f.requests = nil
	file, rep, err := run(t, f, Options{Days: 1, Filters: []string{"kube_cluster_name:staging"}})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.Cluster != "staging" || rep.Containers != 3 {
		t.Errorf("report %+v", rep)
	}
	for _, a := range f.requests {
		for _, q := range a.Queries {
			if !strings.Contains(q.Query, "{kube_cluster_name:staging,") {
				t.Fatalf("query without the selector: %s", q.Query)
			}
		}
	}

	_, _, err = run(t, f, Options{Days: 1, Filters: []string{"kube_cluster_name:*"}})
	if !errors.As(err, &ce) || !ce.Selected {
		t.Errorf("a selector matching both clusters: %v", err)
	}
}

func TestNoClusterTagNeedsAName(t *testing.T) {
	f := &fakeOrg{series: org("", 1)}
	_, _, err := run(t, f, Options{Days: 1})
	if err == nil || !strings.Contains(err.Error(), "--cluster-name") {
		t.Fatalf("got %v", err)
	}
	file, rep, err := run(t, f, Options{Days: 1, Cluster: "named"})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.Cluster != "named" {
		t.Errorf("cluster %q", rep.Cluster)
	}
}

func TestMissingRequestsAndReplicasAreDeclared(t *testing.T) {
	f := &fakeOrg{series: without(org("prod-us", 1), "kubernetes.cpu.requests", "kubernetes.memory.requests",
		"kubernetes_state.deployment.replicas_desired", "kubernetes_state.node.cpu_allocatable", "kubernetes_state.node.memory_allocatable")}
	file, rep, err := run(t, f, Options{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	app := find(file, "shop", Deployment, "web", "app")
	if strings.Join(app.Missing, ",") != contract.CPURequest+","+contract.MemoryRequest {
		t.Errorf("missing %v", app.Missing)
	}
	if series(app, contract.CPURequest) != nil {
		t.Error("requests must not be estimated")
	}
	wl := find(file, "shop", Deployment, "web", "")
	if len(wl.Missing) != 1 || wl.Missing[0] != contract.DeploymentPods {
		t.Errorf("workload missing %v", wl.Missing)
	}
	if sts := find(file, "shop", StatefulSet, "db", ""); len(sts.Missing) != 0 {
		t.Errorf("statefulset replicas are present: %v", sts.Missing)
	}
	if rep.Nodes != 0 || len(rep.Warnings) != 4 {
		t.Errorf("report %+v", rep)
	}
}

func TestKubeStateMetricsRequestsAreTheFallback(t *testing.T) {
	base := org("prod-us", 1)
	s := without(base, "kubernetes.cpu.requests", "kubernetes.memory.requests")
	s = append(s, renamed(base, "kubernetes.cpu.requests", "kubernetes_state.container.cpu_requested", 1)...)
	s = append(s, renamed(base, "kubernetes.memory.requests", "kubernetes_state.container.memory_requested", 1)...)
	file, rep, err := run(t, &fakeOrg{series: s}, Options{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if got := series(find(file, "shop", Deployment, "web", "app"), contract.MemoryRequest); len(got) == 0 || got[0].V != 1<<30 {
		t.Errorf("memory request %v", got)
	}
	if !strings.Contains(strings.Join(rep.Metrics, "\n"), "CPU requests from kubernetes_state.container.cpu_requested") {
		t.Errorf("metrics %v", rep.Metrics)
	}
}

func TestContainerCPUUsageUnitFromMagnitude(t *testing.T) {
	base := without(org("prod-us", 1), "kubernetes.cpu.usage.total")
	for _, c := range []struct {
		name  string
		scale float64 // from nanocores to the stored unit
	}{{"Agent nanocores", 1}, {"OpenTelemetry cores", 1e-9}} {
		s := append(base, renamed(org("prod-us", 1), "kubernetes.cpu.usage.total", "container.cpu.usage", c.scale)...)
		file, _, err := run(t, &fakeOrg{series: s}, Options{Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		if got := peak(series(find(file, "shop", Deployment, "web", "app"), contract.CPUUsageMax)); math.Abs(got-0.8) > 1e-9 {
			t.Errorf("%s: CPU peak %v, want 0.8 cores", c.name, got)
		}
	}
}

func TestNoUsageStops(t *testing.T) {
	f := &fakeOrg{series: without(org("prod-us", 1), "kubernetes.memory.working_set")}
	_, _, err := run(t, f, Options{Days: 1})
	if err == nil || !strings.Contains(err.Error(), "no container memory usage") || !strings.Contains(err.Error(), "container.memory.working_set") {
		t.Fatalf("got %v", err)
	}
}

func TestShortHistoryIsReported(t *testing.T) {
	f := &fakeOrg{series: org("prod-us", 2)}
	file, rep, err := run(t, f, Options{Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.FullHistory || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "less than the 7 days") {
		t.Errorf("report %+v", rep)
	}
}

func TestBadKeysStopBeforeQuerying(t *testing.T) {
	f := &fakeOrg{series: org("prod-us", 1), badKeys: true}
	_, _, err := run(t, f, Options{Days: 1})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("got %v", err)
	}
	if len(f.requests) != 0 {
		t.Error("queries were sent with keys that failed validation")
	}
	if strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), testAppKey) {
		t.Error("the error shows a key")
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{0: "0 nodes", 1: "1 node", 3: "3 nodes"} {
		if got := plural(n, "node"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}
