package savings

import (
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
	sameSlice(t, resample(s, grid, 100, nil), []float64{nan, 1, 1, 2, nan, nan, 4, nan})
	// Without a step a point stays current until the next one.
	sameSlice(t, resample(s, grid, 0, nil), []float64{nan, 1, 1, 2, 2, 2, 4, 4})
	sameSlice(t, resample(series{}, grid, 100, nil), nans(len(grid)))
}

func TestResampleSpans(t *testing.T) {
	nan := math.NaN()
	// 300-second points before 1000, 60-second points from it.
	spans := parseSpans(strs("0/1000/300", "1000/2000/60"))
	s := series{t: []float64{600, 900, 1000, 1060}, v: []float64{1, 2, 3, 4}}
	grid := []float64{600, 899, 1150, 1000, 1059, 1060, 1119, 1120}
	sameSlice(t, resample(s, grid[:2], 60, spans), []float64{1, 1})
	sameSlice(t, resample(s, grid[3:], 60, spans), []float64{3, 3, 4, 4, nan})
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

func TestPricing(t *testing.T) {
	r := pricing([]node{{hostType: "x9.unknown", cores: 8, gib: 32}})
	if r.basis != "default node (4 cores, 16 GiB, $0.192 per hour)" || !same(r.perCore, 0.048) || !same(r.perGiB, 0.012) {
		t.Fatalf("default: %+v", r)
	}
	r = pricing([]node{
		{hostType: "m5.xlarge", region: "us-east-1", capacity: "on_demand", cores: 4, gib: 16},
		{hostType: "r5.large", region: "eu-west-1", capacity: "spot", cores: 2, gib: 16},
		{hostType: "x9.unknown", cores: 8, gib: 32},
		{hostType: "m5.large", cores: math.NaN(), gib: 8},
	})
	want := "2 of 4 nodes at AWS us-east-1 on-demand list prices; spot nodes are priced at on-demand; nodes outside us-east-1 are priced as if in it"
	if r.basis != want || !same(r.perCore, (0.192+0.126)/6) || !same(r.perGiB, (0.192+0.126)/32) {
		t.Fatalf("table: %+v", r)
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

// snapshotDoc has a single sample.
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
		SpanSeconds float64 `json:"spanSeconds"`
		Snapshot    bool    `json:"snapshot"`
		Source      string  `json:"source"`
	} `json:"fidelity"`
	Basis  string             `json:"basis"`
	Totals map[string]float64 `json:"totals"`
}

const nodeScript = `const fs = require('fs');
const model = require(process.argv[2]);
const out = process.argv.slice(3).map(f => {
  const r = model.analyze(JSON.parse(fs.readFileSync(f, 'utf8')));
  return { cluster: r.cluster, fidelity: r.fidelity, basis: r.pricing.basis, totals: r.totals };
});
process.stdout.write(JSON.stringify(out));
`

func TestParityWithModelJS(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	modelJS, err := filepath.Abs("../../src/js/model.js")
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs("../../fixtures/savings-preview.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := []string{fixture}
	for name, doc := range map[string]map[string]any{"partial": partialDoc(), "nodes": nodesDoc(), "snapshot": snapshotDoc(), "spans": spansDoc()} {
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
	out, err := exec.Command(nodeBin, append([]string{script, modelJS}, files...)...).Output()
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
				{"step", float64(got.StepSeconds), w.Fidelity.StepSeconds},
				{"span", float64(got.SpanSeconds), w.Fidelity.SpanSeconds},
				{"end-start", got.End.Sub(got.Start).Seconds(), w.Fidelity.SpanSeconds},
				{"spend", got.Totals.AnnualSpend, w.Totals["spend"]},
				{"savings", got.Totals.AnnualSavings, w.Totals["savings"]},
				{"wasteCores", got.Totals.WasteCores, w.Totals["cores"]},
				{"wasteGiB", got.Totals.WasteGiB, w.Totals["gib"]},
				{"reqCores", got.Totals.RequestedCores, w.Totals["reqCores"]},
				{"reqGiB", got.Totals.RequestedGiB, w.Totals["reqGiB"]},
				{"workloads", float64(got.Totals.Workloads), w.Totals["workloads"]},
				{"counted", float64(got.Totals.Counted), w.Totals["counted"]},
			}
			for _, n := range nums {
				if !same(n.got, n.want) {
					t.Errorf("%s: got %v, want %v", n.name, n.got, n.want)
				}
			}
			js, _ := json.Marshal(got)
			t.Logf("%s", js)
		})
	}
}
