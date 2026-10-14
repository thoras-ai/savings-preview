package savings

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func same(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func sameSlice(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d", len(got), len(want))
	}
	for i := range got {
		if !same(got[i], want[i]) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRollingMax(t *testing.T) {
	nan := math.NaN()
	tm := []float64{0, 300, 600, 900, 1200, 1500, 1800}
	v := []float64{5, 1, nan, 2, 1, 7, 3}
	// The window is (t-900, t], so the 5 at 0 drops out at 900.
	sameSlice(t, rollingMax(tm, v, 900), []float64{5, 5, nan, 2, 2, 7, 7})
}

func TestResample(t *testing.T) {
	nan := math.NaN()
	s := series{t: []float64{100, 200, 400}, v: []float64{1, 2, 4}}
	grid := []float64{50, 100, 150, 250, 300, 350, 400, 500}
	sameSlice(t, resample(s, grid, 100, nil, false), []float64{nan, 1, 1, 2, nan, nan, 4, nan})
	// Without a step a point stays current until the next one.
	sameSlice(t, resample(s, grid, 0, nil, false), []float64{nan, 1, 1, 2, 2, 2, 4, 4})
	sameSlice(t, resample(series{}, grid, 100, nil, false), nans(len(grid)))
}

func TestResampleHeld(t *testing.T) {
	nan := math.NaN()
	// Points where the value changes; the last one lasts one step.
	s := series{t: []float64{100, 400, 500}, v: []float64{1, 4, 5}}
	grid := []float64{50, 100, 300, 399, 400, 450, 500, 599, 600}
	sameSlice(t, resample(s, grid, 100, nil, true), []float64{nan, 1, 1, 1, 4, 4, 5, 5, nan})
}

func TestResampleSpans(t *testing.T) {
	nan := math.NaN()
	// 300-second points before 1000, 60-second points from it.
	spans := parseSpans(strs("0/1000/300", "1000/2000/60"))
	s := series{t: []float64{600, 900, 1000, 1060}, v: []float64{1, 2, 3, 4}}
	grid := []float64{600, 899, 1150, 1000, 1059, 1060, 1119, 1120}
	sameSlice(t, resample(s, grid[:2], 60, spans, false), []float64{1, 1})
	sameSlice(t, resample(s, grid[3:], 60, spans, false), []float64{3, 3, 4, 4, nan})
}

func TestParseSpans(t *testing.T) {
	if parseSpans(nil) != nil || parseSpans([]*string{}) != nil {
		t.Fatal("no spans should be nil")
	}
	got := parseSpans(strs("10/20/5", "20/1000/60"))
	if got[0] != (span{10, 20, 5}) || got[1] != (span{20, 1000, 60}) {
		t.Fatalf("got %v", got)
	}
	if stepAt(got, 15, 1) != 5 || stepAt(got, 20, 1) != 60 || stepAt(got, 1000, 1) != 1 || stepAt(nil, 15, 1) != 1 {
		t.Error("stepAt")
	}
}

func TestWeightedMean(t *testing.T) {
	nan := math.NaN()
	if got := mean([]float64{1, 4, nan}, []float64{300, 60, 60}); !same(got, 1.5) {
		t.Errorf("got %v", got)
	}
	if got := mean([]float64{1, 2}, []float64{0, 0}); !math.IsNaN(got) {
		t.Errorf("zero weights: got %v", got)
	}
}

func TestFidelityNotes(t *testing.T) {
	note := func(vals ...string) map[string]any { return arrAttr("thoras.fidelity_notes", vals...) }
	doc := snapshotDoc()
	rms := doc["resourceMetrics"].([]any)
	for i, notes := range [][]string{{"usage_split_from_pod_by_requests", "b"}, {"a", "usage_split_from_pod_by_requests"}} {
		res := rms[i].(map[string]any)["resource"].(map[string]any)
		res["attributes"] = with(res["attributes"].([]map[string]any), note(notes...))
	}
	got := summarize(t, doc)
	if want := []string{"a", "b", "usage_split_from_pod_by_requests"}; !slices.Equal(got.FidelityNotes, want) {
		t.Errorf("got %q, want %q", got.FidelityNotes, want)
	}
	b, _ := json.Marshal(summarize(t, snapshotDoc()))
	if strings.Contains(string(b), "fidelity_notes") {
		t.Errorf("notes without any: %s", b)
	}
}

func summarize(t *testing.T, doc map[string]any) *Summary {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func strs(vals ...string) []*string {
	out := make([]*string, len(vals))
	for i := range vals {
		out[i] = &vals[i]
	}
	return out
}

// cloudNodes has EKS, GKE and AKS nodes: on-demand, spot, spot with no spot
// price, GCP custom types, an unknown type, a cloud with no table and a node
// whose cloud the file does not name. Prices are the platform table's rows.
var cloudNodes = []struct {
	n    node
	want nodePrice
}{
	{node{hostType: "m5.xlarge", provider: "aws", region: "us-east-1", capacity: "on_demand", cores: 3.92, gib: 14.8},
		nodePrice{cloud: "aws", cores: 4, gib: 16, hourly: 0.192}},
	{node{hostType: "m5.2xlarge", provider: "aws", region: "us-west-2", capacity: "spot", cores: 7.91, gib: 30.1},
		nodePrice{cloud: "aws", cores: 8, gib: 32, hourly: 0.384 * 0.36, spot: true, otherRegion: true}},
	{node{hostType: "c8ib.large", provider: "aws", region: "us-east-1", capacity: "spot", cores: 1.93, gib: 3.1},
		nodePrice{cloud: "aws", cores: 2, gib: 4, spot: true, reason: "spot"}},
	{node{hostType: "n2-standard-4", provider: "gcp", region: "us-central1", capacity: "on_demand", cores: 3.92, gib: 13},
		nodePrice{cloud: "gcp", cores: 4, gib: 16, hourly: 0.218756}},
	{node{hostType: "e2-standard-4", provider: "gcp", region: "us-central1", capacity: "spot", cores: 3.92, gib: 13},
		nodePrice{cloud: "gcp", cores: 4, gib: 16, hourly: 0.07547082, spot: true}},
	{node{hostType: "n2d-custom-16-32768", provider: "gcp", region: "us-central1", capacity: "on_demand", cores: 15.89, gib: 27.5},
		nodePrice{cloud: "gcp", cores: 16, gib: 32, hourly: 16*0.0325248 + 32*0.00435855, custom: true}},
	{node{hostType: "n2d-custom-4-8192", provider: "gcp", region: "us-central1", capacity: "spot", cores: 3.92, gib: 6.1},
		nodePrice{cloud: "gcp", cores: 4, gib: 8, hourly: 4*0.01015 + 8*0.001663, spot: true, custom: true}},
	{node{hostType: "Standard_D4s_v5", provider: "azure", region: "eastus", capacity: "on_demand", cores: 3.86, gib: 12.4},
		nodePrice{cloud: "azure", cores: 4, gib: 16, hourly: 0.192}},
	{node{hostType: "Standard_D4s_v5", provider: "azure", region: "eastus", capacity: "spot", cores: 3.86, gib: 12.4},
		nodePrice{cloud: "azure", cores: 4, gib: 16, hourly: 0.192, azureSpot: true}},
	{node{hostType: "x9.unknown", provider: "aws", region: "us-east-1", capacity: "on_demand", cores: 4, gib: 16},
		nodePrice{cloud: "aws", reason: "type"}},
	{node{hostType: "VM.Standard.E4.Flex", provider: "oci", region: "us-ashburn-1", capacity: "on_demand", cores: 4, gib: 16},
		nodePrice{cloud: "oci", reason: "cloud"}},
	{node{hostType: "c5.large", provider: "unknown", region: "unknown", capacity: "unknown", cores: 1.93, gib: 3.1},
		nodePrice{cloud: "aws", cores: 2, gib: 4, hourly: 0.085, inferred: true}},
	{node{hostType: "unknown", provider: "unknown", region: "unknown", capacity: "unknown", cores: 2, gib: 2},
		nodePrice{cloud: "unknown", reason: "type"}},
}

func TestPriceNode(t *testing.T) {
	for _, c := range cloudNodes {
		got := priceNode(c.n)
		if got.cloud != c.want.cloud || got.reason != c.want.reason || !same(got.cores, c.want.cores) || !same(got.gib, c.want.gib) ||
			!same(got.hourly, c.want.hourly) || got.spot != c.want.spot || got.custom != c.want.custom || got.inferred != c.want.inferred ||
			got.azureSpot != c.want.azureSpot || got.otherRegion != c.want.otherRegion {
			t.Errorf("%s %s %s: got %+v, want %+v", c.n.provider, c.n.hostType, c.n.capacity, got, c.want)
		}
	}
	// A custom type with no size in its name falls back to the allocatable.
	if got := priceNode(node{hostType: "e2-custom-small-2048", provider: "gcp", cores: 2, gib: 1.9}); !same(got.hourly, 2*0.02579582+1.9*0.00345659) || got.reason != "" {
		t.Errorf("unsized custom type: %+v", got)
	}
	if got := priceNode(node{hostType: "e2-custom-small-2048", provider: "gcp", cores: math.NaN()}); got.reason != "size" {
		t.Errorf("unsized custom type with no allocatable: %+v", got)
	}
}

func TestPricing(t *testing.T) {
	r := pricing([]node{{hostType: "x9.unknown", provider: "aws", cores: 8, gib: 32}})
	if r.basis != "default node (4 cores, 16 GiB, $0.192 per hour)" || !same(r.perCore, 0.048) || !same(r.perGiB, 0.012) {
		t.Fatalf("default: %+v", r)
	}
	r = pricing([]node{
		{hostType: "m5.xlarge", provider: "aws", region: "us-east-1", capacity: "on_demand", cores: 3.92, gib: 14.8},
		{hostType: "r5.large", provider: "aws", region: "us-east-1", capacity: "on_demand", cores: 1.93, gib: 15.1},
		{hostType: "x9.unknown", provider: "aws", cores: 8, gib: 32},
	})
	if want := "2 of 3 nodes at AWS us-east-1 on-demand list prices"; r.basis != want || !same(r.perCore, (0.192+0.126)/6) || !same(r.perGiB, (0.192+0.126)/32) {
		t.Fatalf("aws: %+v", r)
	}
	var nodes []node
	cost, cores, gib := 0.0, 0.0, 0.0
	for _, c := range cloudNodes {
		nodes = append(nodes, c.n)
		if c.want.reason == "" {
			cost, cores, gib = cost+c.want.hourly, cores+c.want.cores, gib+c.want.gib
		}
	}
	r = pricing(nodes)
	want := "9 of 13 nodes at AWS us-east-1, Google Cloud us-central1 and Azure eastus on-demand and spot list prices; " +
		"Azure spot nodes are priced at on-demand; nodes outside those regions are priced as if in them"
	if r.basis != want || !same(r.perCore, cost/cores) || !same(r.perGiB, cost/gib) {
		t.Fatalf("clouds: %+v, want %q %v %v", r, want, cost/cores, cost/gib)
	}
}

func TestParseErrors(t *testing.T) {
	for doc, want := range map[string]string{
		`{}`: "No resourceMetrics found. This is not a Savings Preview file.",
		`{"resourceMetrics":[{"resource":{"attributes":[]}}]}`: "The file has no container series, so there is nothing to analyze.",
	} {
		if _, err := Analyze([]byte(doc)); err == nil || err.Error() != want {
			t.Errorf("%s: got %v", doc, err)
		}
	}
}

// Builders for small contract documents.

func sAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}
func iAttr(k string, v int) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"intValue": strconv.Itoa(v)}}
}

func arrAttr(k string, vals ...string) map[string]any {
	var list []any
	for _, v := range vals {
		list = append(list, map[string]any{"stringValue": v})
	}
	return map[string]any{"key": k, "value": map[string]any{"arrayValue": map[string]any{"values": list}}}
}

const t0 = 1789516800

// metric builds a series sampled every step seconds; NaN values are left out.
func metric(name string, values []float64, step int, asInt bool) map[string]any {
	times := make([]int, len(values))
	for i := range times {
		times[i] = t0 + i*step
	}
	return metricAt(name, times, values, asInt)
}

// metricAt builds a series at the given times; NaN values are left out.
func metricAt(name string, times []int, values []float64, asInt bool) map[string]any {
	var pts []map[string]any
	for i, v := range values {
		if math.IsNaN(v) {
			continue
		}
		p := map[string]any{"timeUnixNano": strconv.FormatInt(int64(times[i])*1e9, 10)}
		if asInt {
			p["asInt"] = strconv.FormatInt(int64(v), 10)
		} else {
			p["asDouble"] = v
		}
		pts = append(pts, p)
	}
	return map[string]any{"name": name, "sum": map[string]any{"dataPoints": pts}}
}

func rm(attrs []map[string]any, metrics ...map[string]any) map[string]any {
	return map[string]any{"resource": map[string]any{"attributes": attrs}, "scopeMetrics": []any{map[string]any{"metrics": metrics}}}
}

func common(step, tier int, source string) []map[string]any {
	return []map[string]any{sAttr("k8s.cluster.name", "test-cluster"), sAttr("thoras.source", source), iAttr("thoras.step_seconds", step), iAttr("thoras.fidelity_tier", tier)}
}

func with(base []map[string]any, extra ...map[string]any) []map[string]any {
	return append(append([]map[string]any(nil), base...), extra...)
}

func fill(n int, f func(i int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

func containerMetrics(n, step int, cpu func(int) float64, memGiB func(int) float64, reqCores, reqGiB float64, skip string) []map[string]any {
	all := []map[string]any{
		metric(cpuUse, fill(n, cpu), step, false),
		metric(memUse, fill(n, func(i int) float64 { return math.Round(memGiB(i) * gib) }), step, true),
		metric(cpuReq, fill(n, func(int) float64 { return reqCores }), step, false),
		metric(memReq, fill(n, func(int) float64 { return reqGiB * gib }), step, true),
	}
	var out []map[string]any
	for _, m := range all {
		if m["name"] != skip {
			out = append(out, m)
		}
	}
	return out
}

// partialDoc has a partial workload, one with no replica series, one with no
// usable container, gaps in usage and a replica step coarser than the usage.
func partialDoc() map[string]any {
	n, step := 48, 300
	base := common(step, 2, "prometheus")
	api := with(base, sAttr("k8s.namespace.name", "shop"), sAttr("k8s.deployment.name", "api"))
	db := with(base, sAttr("k8s.namespace.name", "shop"), sAttr("k8s.statefulset.name", "db"))
	worker := with(base, sAttr("k8s.namespace.name", "jobs"), sAttr("k8s.deployment.name", "worker"))
	gappy := func(i int) float64 {
		if i%7 == 3 {
			return math.NaN()
		}
		return 0.3 + 0.2*math.Sin(float64(i)/3)
	}
	return map[string]any{"resourceMetrics": []any{
		rm(with(common(600, 2, "prometheus"), sAttr("k8s.namespace.name", "shop"), sAttr("k8s.deployment.name", "api")),
			metric("k8s.deployment.pod.desired", fill(n/2, func(i int) float64 { return float64(2 + i%3) }), 600, true)),
		rm(with(worker, sAttr("k8s.container.name", "run"))),
		rm(with(api, sAttr("k8s.container.name", "app")),
			containerMetrics(n, step, gappy, func(i int) float64 { return 1 + float64(i%5)/10 }, 1, 4, "")...),
		rm(with(api, sAttr("k8s.container.name", "sidecar")),
			containerMetrics(n, step, func(int) float64 { return 0.05 }, func(int) float64 { return 0.1 }, 0.1, 0.25, memReq)...),
		rm(with(db, iAttr("thoras.fidelity_tier", 3), sAttr("k8s.container.name", "pg")),
			containerMetrics(n, step, func(int) float64 { return 1 }, func(int) float64 { return 8 }, 2, 16, "")...),
		rm(with(worker, sAttr("k8s.container.name", "app")),
			containerMetrics(n, step, func(int) float64 { return 0.5 }, func(int) float64 { return 1 }, 1, 2, cpuUse)...),
		rm(with(api, sAttr("k8s.container.name", "init"), sAttr("thoras.source", "kubelet")),
			containerMetrics(n, step, func(int) float64 { return 0.01 }, func(int) float64 { return 0.01 }, 0.1, 0.1, cpuReq)...),
	}}
}

// nodesDoc prices from the node table, with a spot node outside us-east-1,
// a node type missing from the table and a statefulset at a 60-second step.
func nodesDoc() map[string]any {
	n, step := 120, 60
	base := common(step, 1, "metrics-server")
	nd := func(name, typ, region, capacity string, cores, memGiB float64) map[string]any {
		return rm(with(base, sAttr("k8s.node.name", name), sAttr("host.type", typ), sAttr("cloud.region", region), sAttr("thoras.node.capacity_type", capacity)),
			metric("k8s.node.cpu.allocatable", fill(4, func(int) float64 { return cores }), 3600, false),
			metric("k8s.node.memory.allocatable", fill(4, func(int) float64 { return memGiB * gib }), 3600, true))
	}
	web := with(base, sAttr("k8s.namespace.name", "web"), sAttr("k8s.deployment.name", "front"))
	cache := with(base, sAttr("k8s.namespace.name", "web"), sAttr("k8s.statefulset.name", "cache"))
	return map[string]any{"resourceMetrics": []any{
		nd("n1", "m5.xlarge", "us-east-1", "on_demand", 3.92, 14.5),
		nd("n2", "r5.large", "us-west-2", "spot", 1.93, 15.1),
		nd("n3", "z1d.large", "us-east-1", "on_demand", 2, 16),
		rm(with(web, sAttr("k8s.container.name", "nginx")),
			containerMetrics(n, step, func(i int) float64 { return 0.2 + float64(i%20)/40 }, func(i int) float64 { return 0.5 + float64(i%9)/30 }, 1.5, 2, "")...),
		rm(web, metric("k8s.deployment.pod.desired", fill(n, func(i int) float64 { return float64(3 + i/40) }), step, true)),
		rm(with(cache, sAttr("k8s.container.name", "redis")),
			containerMetrics(n, step, func(i int) float64 { return 0.1 }, func(i int) float64 { return 3 + float64(i%4)/4 }, 0.5, 6, "")...),
		rm(cache, metric("k8s.statefulset.pod.desired", fill(n, func(int) float64 { return 2 }), step, true)),
	}}
}

// cloudsDoc prices the cloudNodes.
func cloudsDoc() map[string]any {
	doc := nodesDoc()
	base := common(60, 1, "metrics-server")
	var rms []any
	for i, c := range cloudNodes {
		rms = append(rms, rm(with(base, sAttr("k8s.node.name", fmt.Sprintf("node-%d", i)), sAttr("host.type", c.n.hostType), sAttr("cloud.provider", c.n.provider),
			sAttr("cloud.region", c.n.region), sAttr("thoras.node.capacity_type", c.n.capacity)),
			metric("k8s.node.cpu.allocatable", fill(4, func(int) float64 { return c.n.cores }), 3600, false),
			metric("k8s.node.memory.allocatable", fill(4, func(int) float64 { return c.n.gib * gib }), 3600, true)))
	}
	for _, r := range doc["resourceMetrics"].([]any) {
		attrs := r.(map[string]any)["resource"].(map[string]any)["attributes"].([]map[string]any)
		if !slices.ContainsFunc(attrs, func(a map[string]any) bool { return a["key"] == "k8s.node.name" }) {
			rms = append(rms, r)
		}
	}
	return map[string]any{"resourceMetrics": rms}
}

// snapshotDoc has a single sample.
// heldDoc is a compact file: 15-minute points from 1-minute data, with
// requests, replicas and node allocatable written only where they change.
func heldDoc() map[string]any {
	n, step := 40, 900
	base := with(common(step, 1, "prometheus"), iAttr("thoras.source_step_seconds", 60))
	wl := with(base, sAttr("k8s.namespace.name", "shop"), sAttr("k8s.deployment.name", "api"))
	times := func(idx ...int) []int {
		out := make([]int, len(idx))
		for i, k := range idx {
			out[i] = t0 + k*step
		}
		return out
	}
	return map[string]any{"resourceMetrics": []any{
		rm(with(base, sAttr("k8s.node.name", "n1"), sAttr("host.type", "m5.xlarge"), sAttr("cloud.region", "us-east-1"), sAttr("thoras.node.capacity_type", "on_demand"),
			arrAttr("thoras.held_metrics", "k8s.node.cpu.allocatable", "k8s.node.memory.allocatable")),
			metricAt("k8s.node.cpu.allocatable", times(0, 10, 39), []float64{3.92, 7.9, 7.9}, false),
			metricAt("k8s.node.memory.allocatable", times(0, 39), []float64{14.5 * gib, 14.5 * gib}, true)),
		rm(with(wl, arrAttr("thoras.held_metrics", "k8s.deployment.pod.desired")),
			metricAt("k8s.deployment.pod.desired", times(0, 5, 25, 39), []float64{2, 3, 1, 1}, true)),
		rm(with(wl, sAttr("k8s.container.name", "app"), arrAttr("thoras.held_metrics", cpuReq, memReq)),
			metric(cpuUse, fill(n, func(i int) float64 { return 0.2 + float64(i%7)/20 }), step, false),
			metric(cpuPeak, fill(n, func(i int) float64 { return 0.3 + float64(i%7)/10 }), step, false),
			metric(memUse, fill(n, func(i int) float64 { return gib * (1 + float64(i%3)/4) }), step, true),
			metric(memPeak, fill(n, func(i int) float64 { return gib * (1.2 + float64(i%3)/4) }), step, true),
			metricAt(cpuReq, times(0, 20, 39), []float64{1, 0.5, 0.5}, false),
			// Ends before the usage, so the last steps have no request.
			metricAt(memReq, times(0, 30), []float64{2 * gib, 2 * gib}, true)),
	}}
}

func snapshotDoc() map[string]any {
	base := common(300, 3, "kube-api")
	wl := with(base, sAttr("k8s.namespace.name", "a"), sAttr("k8s.deployment.name", "b"))
	return map[string]any{"resourceMetrics": []any{
		rm(wl, metric("k8s.deployment.pod.desired", []float64{4}, 300, true)),
		rm(with(wl, sAttr("k8s.container.name", "c")),
			containerMetrics(1, 300, func(int) float64 { return 0.25 }, func(int) float64 { return 0.5 }, 1, 1, "")...),
	}}
}

// mixed is an hour rolled up to coarse points followed by fine ones until
// end, with the thoras.step_spans attribute that describes it.
func mixed(coarse, fine, end int) ([]int, map[string]any) {
	var times []int
	for t := t0; t < t0+3600; t += coarse {
		times = append(times, t)
	}
	for t := t0 + 3600; t < end; t += fine {
		times = append(times, t)
	}
	return times, arrAttr("thoras.step_spans", fmt.Sprintf("%d/%d/%d", t0, t0+3600, coarse), fmt.Sprintf("%d/%d/%d", t0+3600, end, fine))
}

func at(times []int, f func(i int, coarse bool) float64) []float64 {
	out := make([]float64, len(times))
	for i, t := range times {
		out[i] = f(i, t < t0+3600)
	}
	return out
}

// spansDoc mixes step spans on containers and replica series with containers
// at one step, and peak series with gaps that fall back to usage.
func spansDoc() map[string]any {
	base := common(60, 2, "poll")
	api := with(base, sAttr("k8s.namespace.name", "shop"), sAttr("k8s.deployment.name", "api"))
	db := with(base, sAttr("k8s.namespace.name", "shop"), sAttr("k8s.statefulset.name", "db"))
	batch := with(base, sAttr("k8s.namespace.name", "jobs"), sAttr("k8s.deployment.name", "batch"))
	ts, sp := mixed(300, 60, t0+7200)
	// Ends last, on a step coarser than thoras.step_seconds.
	coarseTs, coarseSp := mixed(900, 300, t0+9000)
	const gap = -1 // a point left out
	vals := func(times []int, f func(i int, coarse bool) float64) []float64 {
		return at(times, func(i int, c bool) float64 {
			if v := f(i, c); v != gap {
				return v
			}
			return math.NaN()
		})
	}
	cpu := func(i int, c bool) float64 {
		if c {
			return 0.2 + float64(i%4)/20
		}
		return 0.4 + 0.3*math.Sin(float64(i)/5)
	}
	memB := func(i int, c bool) float64 { return math.Round((1 + float64(i%6)/10) * gib) }
	return map[string]any{"resourceMetrics": []any{
		// Replicas rolled up like the containers.
		rm(with(api, sp), metricAt("k8s.deployment.pod.desired", ts, vals(ts, func(i int, c bool) float64 {
			if c {
				return 2
			}
			return float64(3 + i%2)
		}), true)),
		rm(with(api, sp, sAttr("k8s.container.name", "app")),
			metricAt(cpuUse, ts, at(ts, cpu), false),
			metricAt(memUse, ts, at(ts, memB), true),
			metricAt(cpuReq, ts, at(ts, func(i int, c bool) float64 {
				if c {
					return 2
				}
				return 1
			}), false),
			metricAt(memReq, ts, at(ts, func(int, bool) float64 { return 3 * gib }), true),
			// Peaks on most points, missing on some in both spans.
			metricAt(cpuPeak, ts, vals(ts, func(i int, c bool) float64 {
				if i%4 == 1 {
					return gap
				}
				if c {
					return 1.2 + float64(i%3)/10
				}
				return cpu(i, c) + 0.1
			}), false),
			metricAt(memPeak, ts, vals(ts, func(i int, c bool) float64 {
				if i%3 == 0 {
					return gap
				}
				return memB(i, c) + 0.5*gib
			}), true)),
		// A sidecar at one step for the whole two hours, with no peaks.
		rm(with(api, sAttr("k8s.container.name", "sidecar")),
			containerMetrics(120, 60, func(i int) float64 { return 0.05 + float64(i%3)/100 }, func(int) float64 { return 0.2 }, 0.1, 0.5, "")...),
		// Replicas at one step, a container with spans and a CPU peak only in
		// the rolled-up hour.
		rm(db, metric("k8s.statefulset.pod.desired", fill(24, func(int) float64 { return 2 }), 300, true)),
		rm(with(db, sp, iAttr("thoras.fidelity_tier", 3), sAttr("k8s.container.name", "pg")),
			metricAt(cpuUse, ts, at(ts, func(int, bool) float64 { return 0.5 }), false),
			metricAt(memUse, ts, at(ts, func(i int, c bool) float64 { return 4 * gib }), true),
			metricAt(cpuReq, ts, at(ts, func(int, bool) float64 { return 2 }), false),
			metricAt(memReq, ts, at(ts, func(int, bool) float64 { return 8 * gib }), true),
			metricAt(cpuPeak, ts, vals(ts, func(i int, c bool) float64 {
				if c {
					return 1.5
				}
				return gap
			}), false)),
		// Coarser spans on both, so the file's step is 900.
		rm(with(batch, coarseSp), metricAt("k8s.deployment.pod.desired", coarseTs, at(coarseTs, func(int, bool) float64 { return 1 }), true)),
		rm(with(batch, coarseSp, arrAttr("thoras.fidelity_notes", "usage_split_from_pod_by_requests"), sAttr("k8s.container.name", "run")),
			metricAt(cpuUse, coarseTs, at(coarseTs, func(i int, c bool) float64 { return 0.1 * float64(1+i%5) }), false),
			metricAt(memUse, coarseTs, at(coarseTs, memB), true),
			metricAt(cpuReq, coarseTs, at(coarseTs, func(int, bool) float64 { return 1 }), false),
			metricAt(memReq, coarseTs, at(coarseTs, func(int, bool) float64 { return 2 * gib }), true)),
	}}
}

type jsResult struct {
	Cluster  string `json:"cluster"`
	Fidelity struct {
		Tier        float64 `json:"tier"`
		StepSeconds float64 `json:"stepSeconds"`
		SourceStep  float64 `json:"sourceStepSeconds"`
		SpanSeconds float64 `json:"spanSeconds"`
		Snapshot    bool    `json:"snapshot"`
		Source      string  `json:"source"`
	} `json:"fidelity"`
	Basis  string             `json:"basis"`
	Totals map[string]float64 `json:"totals"`
	Rows   map[string]float64 `json:"rows"`
	// The money section's namespace figures, added up.
	Money map[string]float64 `json:"money"`
	// Section holds the figures the cluster and nodes section shows.
	Section struct {
		ReqCores, ReqGiB, WasteCores, WasteGiB float64
	} `json:"section"`
	// Nodes is each node as the page reads it and how it prices it.
	Nodes []struct {
		In struct {
			HostType, Provider, Region, Capacity string
			Cores, GiB                           *float64
		}
		Out struct {
			Cloud                                          string
			Cores, GiB, Hourly                             float64
			Spot, Custom, Inferred, AzureSpot, OtherRegion bool
			Reason                                         string
		}
	} `json:"nodes"`
}

// The page loads a file once and recomputes it whenever a lever changes, so
// the script computes with other levers first and then with the defaults.
// rows sums the workloads section's figures over the workloads it counts.
const nodeScript = `const fs = require('fs');
const path = require('path');
const model = require(process.argv[2]);
const money = require(process.argv[3]);
const section = require(path.join(path.dirname(process.argv[2]), 'section-cluster.js'));
const workloads = require(path.join(path.dirname(process.argv[2]), 'section-workloads.js'));
const out = process.argv.slice(4).map(f => {
  const data = model.prepare(JSON.parse(fs.readFileSync(f, 'utf8')));
  model.compute(data, { headroom: 1.5, peakWindow: 3600, rates: { perCore: 1, perGiB: 1 } });
  const r = model.compute(data, model.defaultLevers);
  const rows = { spend: 0, savings: 0, cores: 0, gib: 0 };
  workloads.rows(data, r).filter(w => w.status !== 'excluded').forEach(w => {
    rows.spend += w.annualSpend; rows.savings += w.annualSavings; rows.cores += w.wasteCores; rows.gib += w.wasteGiB;
  });
  const ns = { savings: 0, spend: 0 };
  money.summarize(r).namespaces.forEach(n => { ns.savings += n.savings; ns.spend += n.spend; });
  const s = section.summarize(data, r, { range: null, namespaces: null });
  const counted = r.totals.counted > 0;
  return {
    cluster: r.cluster, fidelity: r.fidelity, basis: r.pricing.basis, totals: r.totals, money: ns, rows,
    nodes: data.nodes.map(n => ({ in: { hostType: n.hostType, provider: n.provider, region: n.region, capacity: n.capacity, cores: n.cores, gib: n.gib }, out: model.priceNode(n) })),
    section: counted ? { ReqCores: s.cpu.req, ReqGiB: s.mem.req, WasteCores: s.cpu.req - s.cpu.target, WasteGiB: s.mem.req - s.mem.target } : {}
  };
});
process.stdout.write(JSON.stringify(out));
`

// A container without both usage values at a step is not running then and
// adds no request, so the check container's request counts at one step of
// 192. The monitoring workloads have no memory or replicas for their first 24
// steps, so they run 168 of the 192 steps the cluster has data for.
func TestIntermittentContainers(t *testing.T) {
	b, err := os.ReadFile("../../fixtures/intermittent.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Counted != 5 {
		t.Errorf("counted %d, want 5", got.Totals.Counted)
	}
	want := 3 + 1 + (0.45*191+0.55)/192 + (0.1+0.05)*168/192
	if math.Abs(got.Totals.RequestedCores-want) > 1e-9 {
		t.Errorf("requested cores %v, want %v", got.Totals.RequestedCores, want)
	}
}

func TestParityWithModelJS(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	modelJS, err := filepath.Abs("../../src/js/model.js")
	if err != nil {
		t.Fatal(err)
	}
	moneyJS, err := filepath.Abs("../../src/js/section-money.js")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("../../fixtures/savings-preview.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	intermittent, err := filepath.Abs("../../fixtures/intermittent.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := []string{fixture, intermittent}
	for name, doc := range map[string]map[string]any{"partial": partialDoc(), "nodes": nodesDoc(), "clouds": cloudsDoc(), "snapshot": snapshotDoc(), "spans": spansDoc(), "held": heldDoc(), "absent": absentDoc()} {
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	script := filepath.Join(dir, "parity.js")
	if err := os.WriteFile(script, []byte(nodeScript), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, append([]string{script, modelJS, moneyJS}, files...)...).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var want []jsResult
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}

	for i, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Analyze(b)
			if err != nil {
				t.Fatal(err)
			}
			w := want[i]
			if got.Cluster != w.Cluster || got.Source != w.Fidelity.Source || got.Pricing != w.Basis || got.Snapshot != w.Fidelity.Snapshot {
				t.Errorf("labels: got %q %q %q %v, want %q %q %q %v", got.Cluster, got.Source, got.Pricing, got.Snapshot,
					w.Cluster, w.Fidelity.Source, w.Basis, w.Fidelity.Snapshot)
			}
			nums := []struct {
				name      string
				got, want float64
			}{
				{"tier", float64(got.Tier), w.Fidelity.Tier},
				{"step", float64(cmp.Or(got.CompactStepSeconds, got.StepSeconds)), w.Fidelity.StepSeconds},
				{"sourceStep", float64(got.StepSeconds), cmp.Or(w.Fidelity.SourceStep, w.Fidelity.StepSeconds)},
				{"span", float64(got.SpanSeconds), w.Fidelity.SpanSeconds},
				{"end-start", got.End.Sub(got.Start).Seconds(), w.Fidelity.SpanSeconds},
				{"spend", got.Totals.AnnualSpend, w.Totals["spend"]},
				{"savings", got.Totals.AnnualSavings, w.Totals["savings"]},
				{"money section spend", got.Totals.AnnualSpend, w.Money["spend"]},
				{"money section savings", got.Totals.AnnualSavings, w.Money["savings"]},
				{"wasteCores", got.Totals.WasteCores, w.Totals["cores"]},
				{"wasteGiB", got.Totals.WasteGiB, w.Totals["gib"]},
				{"reqCores", got.Totals.RequestedCores, w.Totals["reqCores"]},
				{"reqGiB", got.Totals.RequestedGiB, w.Totals["reqGiB"]},
				{"workloads", float64(got.Totals.Workloads), w.Totals["workloads"]},
				{"counted", float64(got.Totals.Counted), w.Totals["counted"]},
				{"workload rows spend", got.Totals.AnnualSpend, w.Rows["spend"]},
				{"workload rows savings", got.Totals.AnnualSavings, w.Rows["savings"]},
				{"workload rows wasteCores", got.Totals.WasteCores, w.Rows["cores"]},
				{"workload rows wasteGiB", got.Totals.WasteGiB, w.Rows["gib"]},
				{"section reqCores", got.Totals.RequestedCores, w.Section.ReqCores},
				{"section reqGiB", got.Totals.RequestedGiB, w.Section.ReqGiB},
				{"section wasteCores", got.Totals.WasteCores, w.Section.WasteCores},
				{"section wasteGiB", got.Totals.WasteGiB, w.Section.WasteGiB},
			}
			for _, n := range nums {
				if !same(n.got, n.want) {
					t.Errorf("%s: got %v, want %v", n.name, n.got, n.want)
				}
			}
			for _, n := range w.Nodes {
				in := node{hostType: n.In.HostType, provider: n.In.Provider, region: n.In.Region, capacity: n.In.Capacity, cores: math.NaN(), gib: math.NaN()}
				if n.In.Cores != nil {
					in.cores = *n.In.Cores
				}
				if n.In.GiB != nil {
					in.gib = *n.In.GiB
				}
				p, o := priceNode(in), n.Out
				if p.cloud != o.Cloud || p.reason != o.Reason || !same(p.cores, o.Cores) || !same(p.gib, o.GiB) || !same(p.hourly, o.Hourly) ||
					p.spot != o.Spot || p.custom != o.Custom || p.inferred != o.Inferred || p.azureSpot != o.AzureSpot || p.otherRegion != o.OtherRegion {
					t.Errorf("node %+v: got %+v, want %+v", n.In, p, o)
				}
			}
			if name == "clouds" && len(w.Nodes) != len(cloudNodes) {
				t.Errorf("clouds: %d nodes, want %d", len(w.Nodes), len(cloudNodes))
			}
			js, _ := json.Marshal(got)
			t.Logf("%s", js)
		})
	}
}

// absentDoc has 20 steps of 300 seconds. whole runs every step with data,
// half the last 9 of them, and job the first 4 and the last 4. No workload
// has data at steps 8 and 9, an outage of the metrics source.
func absentDoc() map[string]any {
	n, step := 20, 300
	base := common(step, 2, "prometheus")
	outage := func(i int) bool { return i == 8 || i == 9 }
	var rms []any
	for _, w := range []struct {
		name    string
		runs    func(int) bool
		reqCPU  float64
		memGiB  float64
		usedCPU float64
	}{
		{"whole", func(int) bool { return true }, 2, 4, 0.5},
		{"half", func(i int) bool { return i >= 11 }, 2, 4, 0.5},
		{"job", func(i int) bool { return i < 4 || i >= 16 }, 3, 2, 1},
	} {
		at := func(v float64) func(int) float64 {
			return func(i int) float64 {
				if !w.runs(i) || outage(i) {
					return math.NaN()
				}
				return v
			}
		}
		wl := with(base, sAttr("k8s.namespace.name", "ns"), sAttr("k8s.deployment.name", w.name))
		rms = append(rms,
			rm(wl, metric("k8s.deployment.pod.desired", fill(n, func(int) float64 { return 1 }), step, true)),
			rm(with(wl, sAttr("k8s.container.name", "app")),
				metric(cpuUse, fill(n, at(w.usedCPU)), step, false),
				metric(memUse, fill(n, at(gib)), step, true),
				metric(cpuReq, fill(n, at(w.reqCPU)), step, false),
				metric(memReq, fill(n, at(w.memGiB*gib)), step, true)))
	}
	return map[string]any{"resourceMetrics": rms}
}

// A workload counts only while it runs: its averages are over the time the
// cluster has data, and an outage with no data from any workload is not part
// of that time.
func TestWorkloadsCountOnlyWhileTheyRun(t *testing.T) {
	b, err := json.Marshal(absentDoc())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	// 18 steps have data. whole runs all 18, half 9 and job 8.
	wantCores := 2 + 2*9.0/18 + 3*8.0/18
	wantGiB := 4 + 4*9.0/18 + 2*8.0/18
	if !same(got.Totals.RequestedCores, wantCores) || !same(got.Totals.RequestedGiB, wantGiB) {
		t.Errorf("requested %v cores, %v GiB; want %v, %v", got.Totals.RequestedCores, got.Totals.RequestedGiB, wantCores, wantGiB)
	}
	// Default node pricing: CPU costs more than memory for every workload,
	// at 0.048 per core-hour.
	wantSpend := wantCores * 0.048 * hoursPerYear
	if !same(got.Totals.AnnualSpend, wantSpend) {
		t.Errorf("spend %v, want %v", got.Totals.AnnualSpend, wantSpend)
	}
}

// A workload present for half the covered time with the same requests costs
// half of one present the whole time.
func TestHalfTheTimeCostsHalf(t *testing.T) {
	doc := func(names ...string) *Summary {
		t.Helper()
		var rms []any
		for _, r := range absentDoc()["resourceMetrics"].([]any) {
			for _, a := range r.(map[string]any)["resource"].(map[string]any)["attributes"].([]map[string]any) {
				if a["key"] == "k8s.deployment.name" && slices.Contains(names, a["value"].(map[string]any)["stringValue"].(string)) {
					rms = append(rms, r)
				}
			}
		}
		b, err := json.Marshal(map[string]any{"resourceMetrics": rms})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Analyze(b)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	whole, both := doc("whole"), doc("whole", "half")
	half := Totals{
		AnnualSpend: both.Totals.AnnualSpend - whole.Totals.AnnualSpend, AnnualSavings: both.Totals.AnnualSavings - whole.Totals.AnnualSavings,
		WasteCores: both.Totals.WasteCores - whole.Totals.WasteCores, WasteGiB: both.Totals.WasteGiB - whole.Totals.WasteGiB,
		RequestedCores: both.Totals.RequestedCores - whole.Totals.RequestedCores, RequestedGiB: both.Totals.RequestedGiB - whole.Totals.RequestedGiB,
	}
	// whole runs all 18 steps with data, half 9 of them.
	f := 0.5
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"spend", half.AnnualSpend, whole.Totals.AnnualSpend * f},
		{"savings", half.AnnualSavings, whole.Totals.AnnualSavings * f},
		{"waste cores", half.WasteCores, whole.Totals.WasteCores * f},
		{"waste GiB", half.WasteGiB, whole.Totals.WasteGiB * f},
		{"requested cores", half.RequestedCores, whole.Totals.RequestedCores * f},
		{"requested GiB", half.RequestedGiB, whole.Totals.RequestedGiB * f},
	} {
		if !same(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}
