package savings

import (
	"encoding/json"
	"os"
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
