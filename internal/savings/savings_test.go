package savings

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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
	sameSlice(t, resample(s, grid, 100), []float64{nan, 1, 1, 2, nan, nan, 4, nan})
	// Without a step a point stays current until the next one.
	sameSlice(t, resample(s, grid, 0), []float64{nan, 1, 1, 2, 2, 2, 4, 4})
	sameSlice(t, resample(series{}, grid, 100), nans(len(grid)))
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

const t0 = 1789516800

// metric builds a series sampled every step seconds; NaN values are left out.
func metric(name string, values []float64, step int, asInt bool) map[string]any {
	var pts []map[string]any
	for i, v := range values {
		if math.IsNaN(v) {
			continue
		}
		p := map[string]any{"timeUnixNano": strconv.FormatInt(int64(t0+i*step)*1e9, 10)}
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
	for name, doc := range map[string]map[string]any{"partial": partialDoc(), "nodes": nodesDoc(), "snapshot": snapshotDoc()} {
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
