package savings

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// renamed reads a fixture and puts every resource in cluster name.
func renamed(t *testing.T, path, name string) []any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	rms := doc["resourceMetrics"].([]any)
	for _, rm := range rms {
		for _, a := range rm.(map[string]any)["resource"].(map[string]any)["attributes"].([]any) {
			if a := a.(map[string]any); a["key"] == "k8s.cluster.name" {
				a["value"] = map[string]any{"stringValue": name}
			}
		}
	}
	return rms
}

// onGKE moves a fixture's nodes to Google Cloud: each node takes the next of
// types as its instance type and gcp as its provider.
func onGKE(rms []any, types ...string) []any {
	i := 0
	for _, rm := range rms {
		attrs := rm.(map[string]any)["resource"].(map[string]any)["attributes"].([]any)
		node := false
		for _, a := range attrs {
			node = node || a.(map[string]any)["key"] == "k8s.node.name"
		}
		if !node {
			continue
		}
		for _, a := range attrs {
			a := a.(map[string]any)
			switch a["key"] {
			case "host.type":
				a["value"] = map[string]any{"stringValue": types[i%len(types)]}
			case "cloud.provider":
				a["value"] = map[string]any{"stringValue": "gcp"}
			case "cloud.region":
				a["value"] = map[string]any{"stringValue": "us-central1"}
			}
		}
		i++
	}
	return rms
}

func analyzeAll(t *testing.T, rms []any) *Summary {
	t.Helper()
	b, err := json.Marshal(map[string]any{"resourceMetrics": rms})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFleetTotalsAreTheSumOfTheClusters(t *testing.T) {
	a := renamed(t, "../../fixtures/minikube-poll.otlp.json", "alpha")
	b := renamed(t, "../../fixtures/intermittent.otlp.json", "beta")
	gone := map[string]any{"resource": map[string]any{"attributes": []any{
		map[string]any{"key": "k8s.cluster.name", "value": map[string]any{"stringValue": "gamma"}},
		map[string]any{"key": "thoras.cluster.error", "value": map[string]any{"stringValue": "list services: Unauthorized"}},
	}}, "scopeMetrics": []any{}}
	sa, sb := analyzeAll(t, a), analyzeAll(t, b)
	if sa.Clusters != nil || sb.Clusters != nil {
		t.Fatalf("a single-cluster summary lists clusters: %v %v", sa.Clusters, sb.Clusters)
	}
	f := analyzeAll(t, append(append(append([]any{}, a...), b...), gone))

	if len(f.Clusters) != 3 || f.Clusters[0].Cluster != "alpha" || f.Clusters[1].Cluster != "beta" || f.Clusters[2].Cluster != "gamma" {
		t.Fatalf("clusters: %+v", f.Clusters)
	}
	if f.Clusters[2].Error != "list services: Unauthorized" || f.Clusters[2].Summary != nil {
		t.Errorf("failed cluster: %+v", f.Clusters[2])
	}
	if f.Clusters[0].Totals != sa.Totals || f.Clusters[1].Totals != sb.Totals {
		t.Errorf("a cluster in a fleet differs from the cluster alone")
	}
	sum := func(x, y float64) float64 { return x + y }
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"spend", f.Totals.AnnualSpend, sum(sa.Totals.AnnualSpend, sb.Totals.AnnualSpend)},
		{"savings", f.Totals.AnnualSavings, sum(sa.Totals.AnnualSavings, sb.Totals.AnnualSavings)},
		{"waste cores", f.Totals.WasteCores, sum(sa.Totals.WasteCores, sb.Totals.WasteCores)},
		{"waste GiB", f.Totals.WasteGiB, sum(sa.Totals.WasteGiB, sb.Totals.WasteGiB)},
		{"requested cores", f.Totals.RequestedCores, sum(sa.Totals.RequestedCores, sb.Totals.RequestedCores)},
		{"requested GiB", f.Totals.RequestedGiB, sum(sa.Totals.RequestedGiB, sb.Totals.RequestedGiB)},
		{"workloads", float64(f.Totals.Workloads), float64(sa.Totals.Workloads + sb.Totals.Workloads)},
		{"counted", float64(f.Totals.Counted), float64(sa.Totals.Counted + sb.Totals.Counted)},
	} {
		if !same(c.got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if f.Cluster != "alpha, beta" || f.Tier != max(sa.Tier, sb.Tier) {
		t.Errorf("labels: %q tier %d", f.Cluster, f.Tier)
	}
	start, end := sa.Start, sa.End
	if sb.Start.Before(start) {
		start = sb.Start
	}
	if sb.End.After(end) {
		end = sb.End
	}
	if !f.Start.Equal(start) || !f.End.Equal(end) || f.SpanSeconds != int64(end.Sub(start).Seconds()) {
		t.Errorf("window: %v to %v, %d s", f.Start, f.End, f.SpanSeconds)
	}

	js, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Totals   map[string]any   `json:"totals"`
		Clusters []map[string]any `json:"clusters"`
	}
	if err := json.Unmarshal(js, &shape); err != nil {
		t.Fatal(err)
	}
	if shape.Clusters[0]["cluster"] != "alpha" || shape.Clusters[0]["totals"] == nil || shape.Clusters[2]["error"] == nil || shape.Totals["annual_savings_usd"] == nil {
		t.Errorf("summary.json shape: %s", js)
	}
}

func TestFleetWithNoClusterRead(t *testing.T) {
	doc := `{"resourceMetrics":[{"resource":{"attributes":[{"key":"k8s.cluster.name","value":{"stringValue":"gamma"}},{"key":"thoras.cluster.error","value":{"stringValue":"expired"}}]}}]}`
	if _, err := Analyze([]byte(doc)); err == nil || err.Error() != "no cluster in the file has data to analyze (gamma: expired)" {
		t.Errorf("got %v", err)
	}
}

func TestUnnamedResourcesJoinTheFirstNamedCluster(t *testing.T) {
	rms := renamed(t, "../../fixtures/savings-preview.otlp.json", "c")
	want := analyzeAll(t, rms)
	// Nodes first, without a cluster name.
	var nodes, rest []any
	for _, rm := range rms {
		attrs := rm.(map[string]any)["resource"].(map[string]any)
		var kept []any
		isNode := false
		for _, a := range attrs["attributes"].([]any) {
			switch a.(map[string]any)["key"] {
			case "k8s.node.name":
				isNode = true
			}
		}
		for _, a := range attrs["attributes"].([]any) {
			if !isNode || a.(map[string]any)["key"] != "k8s.cluster.name" {
				kept = append(kept, a)
			}
		}
		attrs["attributes"] = kept
		if isNode {
			nodes = append(nodes, rm)
		} else {
			rest = append(rest, rm)
		}
	}
	got := analyzeAll(t, append(nodes, rest...))
	if got.Clusters != nil || got.Cluster != "c" || got.Pricing != want.Pricing || got.Totals != want.Totals {
		t.Errorf("got %q %q %+v, want %q %+v", got.Cluster, got.Pricing, got.Totals, want.Pricing, want.Totals)
	}
}

const fleetParityScript = `
const model = require(process.argv[2]);
const doc = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const r = model.analyze(doc);
console.log(JSON.stringify({ totals: r.totals, clusters: r.clusters.map(c => ({ name: c.name, spend: c.spend, savings: c.savings, counted: c.counted, workloads: c.workloads })) }));
`

// Each cluster of a fleet is priced from its own nodes, from its own cloud's table.
func TestFleetClustersArePricedFromTheirOwnCloud(t *testing.T) {
	eks := renamed(t, "../../fixtures/savings-preview.otlp.json", "eks")
	// The fixture's first node is on-demand, its second spot.
	gke := onGKE(renamed(t, "../../fixtures/savings-preview.otlp.json", "gke"), "n2-standard-4", "n2d-custom-8-32768")
	f := analyzeAll(t, append(append([]any{}, eks...), gke...))
	if len(f.Clusters) != 2 {
		t.Fatalf("clusters: %+v", f.Clusters)
	}
	want := map[string]string{
		"eks": "2 of 2 nodes at AWS us-east-1 on-demand and spot list prices",
		"gke": "2 of 2 nodes at Google Cloud us-central1 on-demand and spot list prices",
	}
	for _, c := range f.Clusters {
		if c.Summary == nil || c.Pricing != want[c.Cluster] {
			t.Errorf("%s: %+v", c.Cluster, c.Summary)
		}
	}
	// gke: n2-standard-4 on-demand and n2d-custom-8-32768 at its spot per-unit prices.
	hourly := 0.218756 + 8*0.01015 + 32*0.001663
	r := pricing([]node{{hostType: "n2-standard-4", provider: "gcp", capacity: "on_demand"}, {hostType: "n2d-custom-8-32768", provider: "gcp", capacity: "spot"}})
	if !same(r.perCore, hourly/12) || !same(r.perGiB, hourly/48) {
		t.Errorf("gke rates: %+v", r)
	}
	alone := analyzeAll(t, onGKE(renamed(t, "../../fixtures/savings-preview.otlp.json", "gke"), "n2-standard-4", "n2d-custom-8-32768"))
	if f.Clusters[1].Totals != alone.Totals {
		t.Errorf("gke in the fleet %+v, alone %+v", f.Clusters[1].Totals, alone.Totals)
	}
}

func TestFleetParityWithModelJS(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	modelJS, err := filepath.Abs("../../src/js/model.js")
	if err != nil {
		t.Fatal(err)
	}
	rms := append(renamed(t, "../../fixtures/savings-preview.otlp.json", "alpha"), renamed(t, "../../fixtures/intermittent.otlp.json", "beta")...)
	rms = append(rms, onGKE(renamed(t, "../../fixtures/savings-preview.otlp.json", "gamma"), "e2-standard-4", "n2d-custom-8-32768")...)
	b, err := json.Marshal(map[string]any{"resourceMetrics": rms})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	docPath, script := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "fleet.js")
	if err := os.WriteFile(docPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(fleetParityScript), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, script, modelJS, docPath).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var js struct {
		Totals   map[string]float64
		Clusters []struct {
			Name                               string
			Spend, Savings, Counted, Workloads float64
		}
	}
	if err := json.Unmarshal(out, &js); err != nil {
		t.Fatal(err)
	}
	got, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"spend", got.Totals.AnnualSpend, js.Totals["spend"]},
		{"savings", got.Totals.AnnualSavings, js.Totals["savings"]},
		{"waste cores", got.Totals.WasteCores, js.Totals["cores"]},
		{"waste GiB", got.Totals.WasteGiB, js.Totals["gib"]},
		{"requested cores", got.Totals.RequestedCores, js.Totals["reqCores"]},
		{"requested GiB", got.Totals.RequestedGiB, js.Totals["reqGiB"]},
		{"counted", float64(got.Totals.Counted), js.Totals["counted"]},
		{"workloads", float64(got.Totals.Workloads), js.Totals["workloads"]},
	} {
		if !same(c.got, c.want) {
			t.Errorf("%s: go %v, js %v", c.name, c.got, c.want)
		}
	}
	byName := map[string]ClusterSummary{}
	for _, c := range got.Clusters {
		byName[c.Cluster] = c
	}
	if len(js.Clusters) != 3 {
		t.Fatalf("js clusters: %+v", js.Clusters)
	}
	for _, c := range js.Clusters {
		g := byName[c.Name]
		if g.Summary == nil || !same(g.Totals.AnnualSpend, c.Spend) || !same(g.Totals.AnnualSavings, c.Savings) ||
			float64(g.Totals.Counted) != c.Counted || float64(g.Totals.Workloads) != c.Workloads {
			t.Errorf("%s: go %+v, js %+v", c.Name, g.Summary, c)
		}
	}
}
