package compact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/savings"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

type pt struct {
	t int64
	v float64
}

type res struct {
	attrs   map[string]any // strings, int64 or []string
	metrics map[string][]pt
}

// load reads a contract file into resources keyed by their identity.
func load(t *testing.T, b []byte) map[string]res {
	t.Helper()
	var doc struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value map[string]any
				}
			}
			ScopeMetrics []struct {
				Metrics []struct {
					Name       string
					Gauge, Sum *struct {
						DataPoints []struct {
							TimeUnixNano string
							AsDouble     *float64
							AsInt        string
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]res{}
	for _, rm := range doc.ResourceMetrics {
		r := res{attrs: map[string]any{}, metrics: map[string][]pt{}}
		for _, a := range rm.Resource.Attributes {
			switch {
			case a.Value["stringValue"] != nil:
				r.attrs[a.Key] = a.Value["stringValue"]
			case a.Value["intValue"] != nil:
				n, _ := strconv.ParseInt(a.Value["intValue"].(string), 10, 64)
				r.attrs[a.Key] = n
			case a.Value["arrayValue"] != nil:
				var l []string
				for _, x := range a.Value["arrayValue"].(map[string]any)["values"].([]any) {
					l = append(l, x.(map[string]any)["stringValue"].(string))
				}
				r.attrs[a.Key] = l
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				body := m.Gauge
				if body == nil {
					body = m.Sum
				}
				for _, p := range body.DataPoints {
					ns, _ := strconv.ParseInt(p.TimeUnixNano, 10, 64)
					v := 0.0
					if p.AsDouble != nil {
						v = *p.AsDouble
					} else {
						v, _ = strconv.ParseFloat(p.AsInt, 64)
					}
					r.metrics[m.Name] = append(r.metrics[m.Name], pt{ns / 1e9, v})
				}
			}
		}
		var id []string
		for _, k := range []string{"k8s.node.name", "k8s.namespace.name", "k8s.deployment.name", "k8s.statefulset.name", "k8s.container.name"} {
			if s, ok := r.attrs[k].(string); ok {
				id = append(id, s)
			}
		}
		out[strings.Join(id, "/")] = r
	}
	return out
}

func compactBytes(t *testing.T, in []byte) ([]byte, Stats) {
	t.Helper()
	var out bytes.Buffer
	st, err := Compact(bytes.NewReader(in), &out, DefaultStep)
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), st
}

func checkValid(t *testing.T, b []byte) {
	t.Helper()
	if errs := validate.Validate(b); len(errs) > 0 {
		t.Fatalf("compact file fails the Go checks:\n%s", strings.Join(errs[:min(len(errs), 10)], "\n"))
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		return
	}
	p := filepath.Join(t.TempDir(), "compact.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(py, "../../scripts/validate_contract.py", p).CombinedOutput(); err != nil {
		t.Fatalf("compact file fails validate_contract.py: %v\n%s", err, out)
	}
}

// checkPeaks recomputes each container's 15-minute peaks from the full file
// and requires the compact file to carry exactly those.
func checkPeaks(t *testing.T, full, small map[string]res, origin int64) {
	t.Helper()
	checked := 0
	for id, f := range full {
		if f.attrs["k8s.container.name"] == nil {
			continue
		}
		c := small[id]
		for use, peak := range map[string]string{contract.CPUUsage: contract.CPUUsageMax, contract.MemoryWorkingSet: contract.MemoryWorkingSetMax} {
			want := map[int64]float64{}
			for _, p := range slices.Concat(f.metrics[use], f.metrics[peak]) {
				b := origin + (p.t-origin)/DefaultStep*DefaultStep
				if v, ok := want[b]; !ok || p.v > v {
					want[b] = p.v
				}
			}
			got := c.metrics[peak]
			if len(got) != len(want) || len(c.metrics[use]) != len(want) {
				t.Fatalf("%s %s: %d peaks and %d means for %d steps", id, peak, len(got), len(c.metrics[use]), len(want))
			}
			for _, p := range got {
				if w, ok := want[p.t]; !ok || p.v != w {
					t.Fatalf("%s %s at %d: got %v, want %v", id, peak, p.t, p.v, w)
				}
			}
			checked += len(got)
		}
	}
	if checked == 0 {
		t.Fatal("no peaks checked")
	}
}

type totals map[string]float64

func goTotals(t *testing.T, b []byte) (totals, *savings.Summary) {
	t.Helper()
	s, err := savings.Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	x := s.Totals
	return totals{"spend": x.AnnualSpend, "savings": x.AnnualSavings, "cores": x.WasteCores, "gib": x.WasteGiB,
		"reqCores": x.RequestedCores, "reqGiB": x.RequestedGiB, "workloads": float64(x.Workloads), "counted": float64(x.Counted)}, s
}

const nodeScript = `const fs = require('fs');
const model = require(process.argv[2]);
const out = process.argv.slice(3).map(f => {
  const r = model.analyze(JSON.parse(fs.readFileSync(f, 'utf8')));
  return { totals: r.totals, fidelity: r.fidelity };
});
process.stdout.write(JSON.stringify(out));
`

type jsResult struct {
	Totals   totals
	Fidelity struct {
		Tier              int
		StepSeconds       int
		SourceStepSeconds int
		Label             string
	}
}

// jsTotals runs the page's model on each file, or returns nil without node.
func jsTotals(t *testing.T, files ...[]byte) []jsResult {
	t.Helper()
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Log("node not on PATH; skipping the page model")
		return nil
	}
	dir := t.TempDir()
	model, _ := filepath.Abs("../../src/js/model.js")
	script := filepath.Join(dir, "totals.js")
	os.WriteFile(script, []byte(nodeScript), 0o644)
	args := []string{script, model}
	for i, b := range files {
		p := filepath.Join(dir, fmt.Sprintf("%d.json", i))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(nodeBin, args...).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var res []jsResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func relDiff(a, b float64) float64 {
	if a == b {
		return 0
	}
	return math.Abs(a-b) / math.Max(math.Abs(a), math.Abs(b))
}

// checkTotals compares the full and compact totals from the Go math and, when
// node is available, from the page, which must agree with Go on each file.
// Counts match exactly. Requests and spend are within reqTol: a replica count
// that changes within a step is written as its mean, rounded to a whole pod.
// Savings and waste are within tol. The target follows the peak over the 15
// minutes before each point, which in the compact file is the point's own
// step, and a workload's peak is that of its containers' summed usage, which
// the compact file can only bound by the sum of each container's peak.
func checkTotals(t *testing.T, full, small []byte, reqTol, tol float64) {
	t.Helper()
	gf, sf := goTotals(t, full)
	gc, sc := goTotals(t, small)
	if sf.Tier != sc.Tier || sc.CompactStepSeconds != DefaultStep || sc.StepSeconds != sf.StepSeconds {
		t.Errorf("fidelity: full tier %d step %d, compact tier %d step %d at %d", sf.Tier, sf.StepSeconds, sc.Tier, sc.StepSeconds, sc.CompactStepSeconds)
	}
	for k, f := range gf {
		d := relDiff(f, gc[k])
		limit := tol
		switch k {
		case "workloads", "counted":
			limit = 0
		case "reqCores", "reqGiB", "spend":
			limit = reqTol
		}
		t.Logf("%-9s full %14.6f compact %14.6f  diff %.4f%%", k, f, gc[k], 100*d)
		if d > limit {
			t.Errorf("%s: full %v, compact %v (%.4f%% apart, limit %.4f%%)", k, f, gc[k], 100*d, 100*limit)
		}
	}
	js := jsTotals(t, full, small)
	if js == nil {
		return
	}
	for i, g := range []totals{gf, gc} {
		for k, v := range g {
			if relDiff(v, js[i].Totals[k]) > 1e-9 {
				t.Errorf("file %d %s: Go %v, page %v", i, k, v, js[i].Totals[k])
			}
		}
	}
	if js[1].Fidelity.SourceStepSeconds != sf.StepSeconds || !strings.Contains(js[1].Fidelity.Label, "kept as 15-minute peaks") {
		t.Errorf("page fidelity of the compact file: %+v", js[1].Fidelity)
	}
}

func TestCompactFixture(t *testing.T) {
	full, err := os.ReadFile("../../fixtures/savings-preview.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	small, st := compactBytes(t, full)
	if st.Resources != 10 || st.Compacted != 10 || st.SourceStep != 300 || st.FinestStep != 300 {
		t.Errorf("stats %+v", st)
	}
	t.Logf("%d bytes from %d", len(small), len(full))
	if len(small)*4 > len(full) {
		t.Errorf("compact file is %d bytes, more than a quarter of %d", len(small), len(full))
	}
	checkValid(t, small)
	checkPeaks(t, load(t, full), load(t, small), st.FirstBucket)
	checkTotals(t, full, small, 1e-12, 0.001)
}

func TestCompactSynthetic(t *testing.T) {
	var full bytes.Buffer
	if err := (synth{workloads: 10, days: 7, step: 60}).write(&full); err != nil {
		t.Fatal(err)
	}
	small, st := compactBytes(t, full.Bytes())
	t.Logf("%d bytes from %d; %d points from %d", len(small), full.Len(), st.PointsOut, st.PointsIn)
	if st.SourceStep != 60 || st.Compacted != st.Resources {
		t.Errorf("stats %+v", st)
	}
	checkValid(t, small)
	f, c := load(t, full.Bytes()), load(t, small)
	checkPeaks(t, f, c, st.FirstBucket)
	// Workload 1 halves its CPU request partway, inside a step that gets the
	// mean of both; workload 3 has an outage.
	if got := c["ns-1/app-1/app"].metrics[contract.CPURequest]; len(got) != 4 || got[0].v != 1 || got[1].v <= 0.5 || got[1].v >= 1 || got[2].v != 0.5 || got[3].v != 0.5 {
		t.Errorf("held CPU request of app-1: %v", got)
	}
	if got := c["ns-3/app-3/app"].attrs["thoras.held_metrics"]; !slices.Contains(got.([]string), contract.CPURequest) {
		t.Errorf("app-3 shares its outage with its usage, so its requests hold across it: %v", got)
	}
	if _, held := c["ns-3/app-3"].attrs["thoras.held_metrics"]; held || len(c["ns-3/app-3"].metrics[contract.DeploymentPods]) < 600 {
		t.Error("the replica count of app-3 has a gap, so it stays dense")
	}
	checkTotals(t, full.Bytes(), small, 0.002, 0.015)
}

func doc(resources ...contract.Resource) []byte {
	var b bytes.Buffer
	w := contract.NewWriter(&b)
	for _, r := range resources {
		if err := w.Resource(r); err != nil {
			panic(err)
		}
	}
	w.Close()
	return b.Bytes()
}

func series(name string, start int64, step int, vals ...float64) contract.Series {
	s := contract.Series{Name: name}
	for i, v := range vals {
		if !math.IsNaN(v) {
			s.Points = append(s.Points, contract.Point{T: start + int64(i*step), V: v})
		}
	}
	return s
}

const t0 = synthStart

func TestCompactReduces(t *testing.T) {
	common := contract.Common{Cluster: "c", Source: "poll", StepSeconds: 60, Tier: 3}
	// 300-second points until t0+1200, then 60-second points: the second
	// 15-minute step holds one 300-second point and ten 60-second points.
	spans := []contract.StepSpan{{Start: t0, End: t0 + 1200, Step: 300}, {Start: t0 + 1200, End: t0 + 1800, Step: 60}}
	times := []int64{t0, t0 + 300, t0 + 600, t0 + 900}
	for x := int64(t0 + 1200); x < t0+1800; x += 60 {
		times = append(times, x)
	}
	at := func(f func(i int) float64) []contract.Point {
		var out []contract.Point
		for i, x := range times {
			if v := f(i); !math.IsNaN(v) {
				out = append(out, contract.Point{T: x, V: v})
			}
		}
		return out
	}
	in := doc(contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", Container: "c", StepSpans: spans,
		Metrics: []contract.Series{
			// Step 1: 1, 2, 3. Step 2: 10 for 300 seconds, then 4 for 600.
			{Name: contract.CPUUsage, Points: at(func(i int) float64 { return []float64{1, 2, 3, 10, 4}[min(i, 4)] })},
			// A reading of 7 inside step 1 that the means hide.
			{Name: contract.CPUUsageMax, Points: at(func(i int) float64 { return []float64{1, 7, 3, 10, 4}[min(i, 4)] })},
			{Name: contract.MemoryWorkingSet, Points: at(func(i int) float64 { return []float64{10, 10, 11, 30, 20}[min(i, 4)] })},
			{Name: contract.CPURequest, Points: at(func(int) float64 { return 1 })},
			{Name: contract.MemoryRequest, Points: at(func(i int) float64 { return []float64{64, 64, 64, 64, 65}[min(i, 4)] })},
			{Name: contract.OOMKills, Points: at(func(i int) float64 { return map[int]float64{1: 1, 5: 2, 7: 1}[i] })},
		}},
		contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", StepSpans: spans,
			Metrics: []contract.Series{{Name: contract.DeploymentPods, Points: at(func(i int) float64 { return []float64{2, 2, 2, 3, 4}[min(i, 4)] })}}})
	out, st := compactBytes(t, in)
	if st.FinestStep != 60 || st.SourceStep != 300 || st.Compacted != 2 {
		t.Errorf("stats %+v", st)
	}
	checkValid(t, out)
	got := load(t, out)
	c := got["a/w/c"]
	want := map[string][]pt{
		contract.CPUUsage:            {{t0, 2}, {t0 + 900, 6}}, // (10*300 + 4*600) / 900
		contract.CPUUsageMax:         {{t0, 7}, {t0 + 900, 10}},
		contract.MemoryWorkingSet:    {{t0, 10}, {t0 + 900, 23}}, // 10.33 and 23.33, whole bytes
		contract.MemoryWorkingSetMax: {{t0, 11}, {t0 + 900, 30}},
		contract.CPURequest:          {{t0, 1}, {t0 + 900, 1}},
		contract.MemoryRequest:       {{t0, 64}, {t0 + 900, 65}}, // 64.67
		contract.OOMKills:            {{t0, 1}, {t0 + 900, 3}},
	}
	for name, w := range want {
		if !slices.Equal(c.metrics[name], w) {
			t.Errorf("%s: got %v, want %v", name, c.metrics[name], w)
		}
	}
	if c.attrs["thoras.step_seconds"] != int64(900) || c.attrs["thoras.source_step_seconds"] != int64(300) || c.attrs["thoras.step_spans"] != nil {
		t.Errorf("attributes %v", c.attrs)
	}
	if h, _ := c.attrs["thoras.held_metrics"].([]string); !slices.Equal(h, []string{contract.CPURequest, contract.MemoryRequest}) {
		t.Errorf("held %v", h)
	}
	// Replicas: 2, then (3*300 + 4*600) / 900 = 3.67, rounded to whole pods.
	w := got["a/w"]
	if !slices.Equal(w.metrics[contract.DeploymentPods], []pt{{t0, 2}, {t0 + 900, 4}}) {
		t.Errorf("workload: %v %v", w.metrics, w.attrs)
	}
}

func TestCompactHeldNeedsEveryPricedStep(t *testing.T) {
	nan := math.NaN()
	common := contract.Common{Cluster: "c", Source: "prometheus", StepSeconds: 900, Tier: 3}
	// 300-second data: six 15-minute steps. The memory request misses step 2
	// while usage has it, so it cannot hold; the CPU request misses step 4
	// along with the usage, so it can.
	use := series(contract.CPUUsage, t0, 300, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, nan, nan, nan, 1, 1, 1)
	mem := series(contract.MemoryWorkingSet, t0, 300, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, nan, nan, nan, 1, 1, 1)
	cpuReq := series(contract.CPURequest, t0, 300, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, nan, nan, nan, 1, 1, 1)
	memReq := series(contract.MemoryRequest, t0, 300, 8, 8, 8, 8, 8, 8, nan, nan, nan, 8, 8, 8, nan, nan, nan, 8, 8, 8)
	common.StepSeconds = 300
	in := doc(contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", Container: "c",
		Metrics: []contract.Series{use, mem, cpuReq, memReq}},
		contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w",
			Metrics: []contract.Series{series(contract.DeploymentPods, t0, 300, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, nan, nan, nan, 1, 1, 1)}},
		contract.Resource{Common: contract.Common{Cluster: "c", Source: "prometheus", StepSeconds: 3600, Tier: 3},
			Node:    &contract.Node{Name: "n", InstanceType: "m5.large", Provider: "aws", Region: "us-east-1", CapacityType: "spot"},
			Metrics: []contract.Series{series(contract.NodeCPU, t0, 3600, 2, 2), series(contract.NodeMemory, t0, 3600, 8, 8)}})
	out, st := compactBytes(t, in)
	if st.Unchanged != 1 || st.Compacted != 2 {
		t.Errorf("stats %+v", st)
	}
	checkValid(t, out)
	got := load(t, out)
	if h := got["a/w/c"].attrs["thoras.held_metrics"]; !slices.Equal(h.([]string), []string{contract.CPURequest}) {
		t.Errorf("held %v", h)
	}
	if n := len(got["a/w/c"].metrics[contract.MemoryRequest]); n != 4 {
		t.Errorf("memory request has %d points, want 4", n)
	}
	// A workload has no usage to compare with, so a gap keeps it dense.
	if h := got["a/w"].attrs["thoras.held_metrics"]; h != nil {
		t.Errorf("workload held %v", h)
	}
	n := got["n"]
	if n.attrs["thoras.step_seconds"] != int64(3600) || n.attrs["thoras.source_step_seconds"] != nil || len(n.metrics[contract.NodeCPU]) != 2 {
		t.Errorf("an hourly node is copied as it was: %v %v", n.attrs, n.metrics)
	}
	// Prices like the full file.
	a, _ := goTotals(t, in)
	b, _ := goTotals(t, out)
	for k := range a {
		if relDiff(a[k], b[k]) > 1e-9 {
			t.Errorf("%s: %v and %v", k, a[k], b[k])
		}
	}
}

func TestCompactErrors(t *testing.T) {
	for in, want := range map[string]string{
		`[]`:                        "expected",
		`{"other": 1}`:              "no resourceMetrics",
		`{"resourceMetrics": [{}]}`: "step_seconds",
		`{"resourceMetrics": [`:     "EOF",
	} {
		_, err := Compact(strings.NewReader(in), io.Discard, DefaultStep)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", in, err, want)
		}
	}
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// TestCompactLarge compacts 14 days of 1-minute data for 1,000
// workload-containers as a stream and checks the size of the result and that
// memory stays bounded.
func TestCompactLarge(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("takes about a minute")
	}
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
	pr, pw := io.Pipe()
	var full, small countingWriter
	go func() { pw.CloseWithError(synth{workloads: 500, days: 14, step: 60}.write(io.MultiWriter(pw, &full))) }()
	start := time.Now()
	st, err := Compact(pr, &small, DefaultStep)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d resources: %.0f MB of 1-minute data to %.0f MB at 15 minutes in %s, peak heap %.0f MB",
		st.Resources, float64(full.n)/1e6, float64(small.n)/1e6, time.Since(start).Round(time.Second), float64(peak.Load())/1e6)
	if small.n > 400e6 {
		t.Errorf("compact file is %d bytes", small.n)
	}
	// The heap includes the generator, which runs in the same process.
	if p := peak.Load(); p > 1<<30 {
		t.Errorf("peak heap %d bytes", p)
	}
}

func TestCompactKeepsCoarseSpans(t *testing.T) {
	common := contract.Common{Cluster: "c", Source: "poll", StepSeconds: 60, Tier: 3}
	// Two hours rolled up to 1 hour, then 30 minutes of 1-minute readings.
	var times []int64
	times = append(times, t0, t0+3600)
	for x := int64(t0 + 7200); x < t0+9000; x += 60 {
		times = append(times, x)
	}
	at := func(v float64) []contract.Point {
		out := make([]contract.Point, len(times))
		for i, x := range times {
			out[i] = contract.Point{T: x, V: v + float64(i%3)}
		}
		return out
	}
	spans := []contract.StepSpan{{Start: t0, End: t0 + 7200, Step: 3600}, {Start: t0 + 7200, End: t0 + 9000, Step: 60}}
	res := func(sp []contract.StepSpan) contract.Resource {
		return contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", Container: "c", StepSpans: sp,
			Metrics: []contract.Series{{Name: contract.CPUUsage, Points: at(1)}, {Name: contract.MemoryWorkingSet, Points: at(100)},
				{Name: contract.CPURequest, Points: at(5)}, {Name: contract.MemoryRequest, Points: at(500)}}}
	}
	wl := contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", StepSpans: spans,
		Metrics: []contract.Series{{Name: contract.DeploymentPods, Points: at(1)}}}
	out, st := compactBytes(t, doc(wl, res(spans)))
	if st.Compacted != 2 || st.Unchanged != 0 {
		t.Errorf("stats %+v", st)
	}
	checkValid(t, out)
	c := load(t, out)["a/w/c"]
	if got := c.attrs["thoras.step_spans"]; !slices.Equal(got.([]string), []string{
		fmt.Sprintf("%d/%d/3600", t0, t0+7200), fmt.Sprintf("%d/%d/900", t0+7200, t0+9000)}) {
		t.Errorf("spans %v", got)
	}
	if got := c.metrics[contract.CPUUsageMax]; len(got) != 4 || got[0].t != t0 || got[1].t != t0+3600 || got[2].t != t0+7200 || got[3].t != t0+8100 {
		t.Errorf("points %v", got)
	}
	if c.attrs["thoras.step_seconds"] != int64(900) || c.attrs["thoras.source_step_seconds"] != int64(3600) {
		t.Errorf("attributes %v", c.attrs)
	}

	// Spans off the 15-minute grid are copied as they are.
	off := []contract.StepSpan{{Start: t0, End: t0 + 7200, Step: 3600}, {Start: t0 + 7200, End: t0 + 9000, Step: 60}}
	shifted := res(off)
	for i := range shifted.Metrics {
		for j := range shifted.Metrics[i].Points {
			shifted.Metrics[i].Points[j].T += 300
		}
	}
	for i := range shifted.StepSpans {
		shifted.StepSpans[i].Start += 300
		shifted.StepSpans[i].End += 300
	}
	if _, st := compactBytes(t, doc(wl, shifted)); st.Unchanged != 1 {
		t.Errorf("off-grid spans: %+v", st)
	}
}

func TestCompactKeepsPeaksWithoutUsage(t *testing.T) {
	common := contract.Common{Cluster: "c", Source: "poll", StepSeconds: 300, Tier: 3}
	in := doc(contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w"},
		contract.Resource{Common: common, Namespace: "a", Kind: "deployment", Workload: "w", Container: "c",
			Missing: []string{contract.CPUUsage},
			Metrics: []contract.Series{series(contract.CPUUsageMax, t0, 300, 1, 3, 2, 5), series(contract.MemoryWorkingSet, t0, 300, 1, 1, 1, 1),
				series(contract.CPURequest, t0, 300, 1, 1, 1, 1), series(contract.MemoryRequest, t0, 300, 1, 1, 1, 1)}})
	out, _ := compactBytes(t, in)
	if got := load(t, out)["a/w/c"].metrics[contract.CPUUsageMax]; !slices.Equal(got, []pt{{t0, 3}, {t0 + 900, 5}}) {
		t.Errorf("cpu peaks %v", got)
	}
}
