package polling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		c.now = c.now.Add(d)
	}
	return nil
}

// fakeReader returns testCluster, takes readTime per read, fails the reads
// listed in fail, and cancels the run after stopAfter reads. With vary, the
// first web pod's CPU usage is 100m plus 10m times the read number mod 10.
type fakeReader struct {
	clock     *fakeClock
	metrics   bool
	vary      bool
	readTime  time.Duration
	fail      map[int]bool
	stopAfter int
	cancel    context.CancelFunc
	reads     int
}

func (r *fakeReader) Read(ctx context.Context) (*Cluster, error) {
	r.reads++
	r.clock.now = r.clock.now.Add(r.readTime)
	if r.stopAfter > 0 && r.reads > r.stopAfter {
		r.cancel()
		return nil, context.Canceled
	}
	if r.fail[r.reads] {
		return nil, errors.New("connection refused")
	}
	c := testCluster(r.metrics)
	if r.vary && r.metrics {
		c.PodMetrics[0].Containers[0].Usage = rl("cpu", fmt.Sprintf("%dm", 100+10*(r.reads%10)), "memory", "100Mi")
	}
	return c, nil
}

type pt struct {
	T int64
	V float64
}

type res struct {
	attrs   map[string]any
	metrics map[string][]pt
}

func (r res) str(k string) string { s, _ := r.attrs[k].(string); return s }

func attrValue(raw map[string]json.RawMessage) any {
	if v, ok := raw["stringValue"]; ok {
		var s string
		json.Unmarshal(v, &s)
		return s
	}
	if v, ok := raw["intValue"]; ok {
		var s string
		json.Unmarshal(v, &s)
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	}
	if v, ok := raw["arrayValue"]; ok {
		var arr struct {
			Values []map[string]json.RawMessage `json:"values"`
		}
		json.Unmarshal(v, &arr)
		var out []string
		for _, x := range arr.Values {
			s, _ := attrValue(x).(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

func readFile(t *testing.T, path string) []res {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type dps struct {
		DataPoints []struct {
			Time     string   `json:"timeUnixNano"`
			AsInt    string   `json:"asInt"`
			AsDouble *float64 `json:"asDouble"`
		} `json:"dataPoints"`
	}
	var doc struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes []struct {
					Key   string                     `json:"key"`
					Value map[string]json.RawMessage `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeMetrics []struct {
				Metrics []struct {
					Name  string `json:"name"`
					Gauge *dps   `json:"gauge"`
					Sum   *dps   `json:"sum"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []res
	for _, rm := range doc.ResourceMetrics {
		r := res{attrs: map[string]any{}, metrics: map[string][]pt{}}
		for _, kv := range rm.Resource.Attributes {
			r.attrs[kv.Key] = attrValue(kv.Value)
		}
		for _, m := range rm.ScopeMetrics[0].Metrics {
			d := m.Gauge
			if d == nil {
				d = m.Sum
			}
			for _, p := range d.DataPoints {
				ns, _ := strconv.ParseInt(p.Time, 10, 64)
				v := 0.0
				if p.AsDouble != nil {
					v = *p.AsDouble
				} else {
					v, _ = strconv.ParseFloat(p.AsInt, 64)
				}
				r.metrics[m.Name] = append(r.metrics[m.Name], pt{ns / 1e9, v})
			}
		}
		out = append(out, r)
	}
	return out
}

func find(t *testing.T, rs []res, workload, container string) res {
	t.Helper()
	for _, r := range rs {
		if (r.str("k8s.deployment.name") == workload || r.str("k8s.statefulset.name") == workload) && r.str("k8s.container.name") == container {
			return r
		}
	}
	t.Fatalf("no resource %s/%s", workload, container)
	return res{}
}

func setup(t *testing.T) (string, *fakeClock, *bytes.Buffer) {
	return filepath.Join(t.TempDir(), "out.json"), &fakeClock{now: time.Unix(1_790_000_000, 0)}, &bytes.Buffer{}
}

func opts(out string) Options {
	return Options{Cluster: "test", Output: out, Interval: time.Minute, WriteEvery: 10 * time.Minute,
		KeepRaw: 48 * time.Hour, Rollup: 5 * time.Minute, noSync: true}
}

func TestOnce(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Once = true
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	if _, err := os.Stat(ReadingsDir(out)); !os.IsNotExist(err) {
		t.Errorf("--once saved readings")
	}
	rs := readFile(t, out)
	for _, r := range rs {
		if r.attrs["thoras.fidelity_tier"] != int64(3) || r.str("thoras.source") != "kubectl-snapshot" {
			t.Errorf("resource attributes %v", r.attrs)
		}
	}
	w := find(t, rs, "web", "app")
	if got := w.metrics["container.cpu.usage"]; len(got) != 1 || got[0].V != 0.2 {
		t.Errorf("web cpu usage = %v", got)
	}
	if got := w.metrics["k8s.container.memory.request.desired"]; got[0].V != 256<<20 {
		t.Errorf("web memory request = %v", got)
	}
	if _, ok := w.metrics["thoras.container.cpu.usage.max"]; ok {
		t.Errorf("a single reading has no peaks to report")
	}
	if !strings.Contains(log.String(), "understates peaks") {
		t.Errorf("log does not warn about peaks:\n%s", log)
	}
}

func TestOnceWithoutMetrics(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Once = true
	if err := Run(context.Background(), o, &fakeReader{clock: clk}, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	missing := 0
	for _, r := range readFile(t, out) {
		if m, ok := r.attrs["thoras.missing_metrics"].([]string); ok {
			missing++
			if strings.Join(m, ",") != "container.cpu.usage,container.memory.working_set" {
				t.Errorf("missing_metrics = %v", m)
			}
		}
	}
	if missing != 4 {
		t.Errorf("%d resources list missing metrics, want the 4 containers", missing)
	}
	if !strings.Contains(log.String(), "could not find the requested resource") {
		t.Errorf("log does not say why usage is missing:\n%s", log)
	}
}

func TestPollForDuration(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 10 * time.Minute
	rd := &fakeReader{clock: clk, metrics: true, readTime: 7 * time.Second}
	if err := Run(context.Background(), o, rd, clk, log); err != nil {
		t.Fatal(err)
	}
	if rd.reads != 11 {
		t.Errorf("%d reads over 10 minutes at 1 minute, want 11", rd.reads)
	}
	validate(t, out)
	for _, want := range []string{"11 readings covering 11m", "Tier 2 needs 7 days: 6d23h49m more", "set --keep-raw to 336h"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("progress lacks %q:\n%s", want, log)
		}
	}
	o.KeepRaw = 0
	log.Reset()
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "Tier 1 (the forecaster's target) needs 14 days") {
		t.Errorf("progress without rollup:\n%s", log)
	}
}

func TestInterruptKeepsReadingsAndRerunExtends(t *testing.T) {
	out, clk, log := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	rd := &fakeReader{clock: clk, metrics: true, stopAfter: 3, cancel: cancel}
	if err := Run(ctx, opts(out), rd, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	if got := len(find(t, readFile(t, out), "web", "app").metrics["container.cpu.usage"]); got != 3 {
		t.Fatalf("after interrupt: %d usage points, want 3", got)
	}

	// A crash tore the last frame; resume 30 minutes later.
	segs, _ := segments(ReadingsDir(out))
	f, _ := os.OpenFile(filepath.Join(ReadingsDir(out), rawName(segs[len(segs)-1].start)), os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0x90, 0x03, 'R', 1, 2})
	f.Close()
	clk.now = clk.now.Add(30*time.Minute + 13*time.Second)
	log.Reset()
	o := opts(out)
	o.Duration = 2 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "continuing") {
		t.Errorf("log:\n%s", log)
	}
	validate(t, out)
	usage := find(t, readFile(t, out), "web", "app").metrics["container.cpu.usage"]
	if len(usage) != 5 || usage[3].T-usage[2].T != 32*60 {
		t.Errorf("after resume: %v, want 3 points, a gap, then 2", usage)
	}
}

func TestResumeRejectsOtherInterval(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	o.Interval = 30 * time.Second
	err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log)
	if err == nil || !strings.Contains(err.Error(), "same cluster, --interval and --rollup") {
		t.Errorf("err = %v", err)
	}
}

func TestSecondPollerRefused(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	st, err := OpenStore(ReadingsDir(out), Meta{Cluster: "test", Step: 60, RollupStep: 300, Origin: clk.now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	err = Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log)
	if err == nil || !strings.Contains(err.Error(), "another poller is writing") {
		t.Errorf("second poller: err = %v", err)
	}
	o.Once = true
	err = Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log)
	if err == nil || !strings.Contains(err.Error(), "a poller is writing") {
		t.Errorf("--once beside a poller: err = %v", err)
	}
	st.Close()
	o.Once = false
	o.Duration = time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Errorf("after the first poller stopped: %v", err)
	}
}

func TestReadErrors(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 5 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, fail: map[int]bool{1: true}}, clk, log); err == nil {
		t.Errorf("a failing first read should stop the run")
	}
	out, clk, log = setup(t)
	o = opts(out)
	o.Duration = 5 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true, fail: map[int]bool{3: true}}, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	if !strings.Contains(log.String(), "leaving a gap") || !strings.Contains(log.String(), "5 readings") {
		t.Errorf("log:\n%s", log)
	}
}

func TestSlowReadsStayOnGrid(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Interval = 15 * time.Second
	o.Duration = 3 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true, readTime: 20 * time.Second}, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	m, _ := readMeta(ReadingsDir(out))
	n := 0
	scan(ReadingsDir(out), math.MaxInt64, nil, func(r *record) error {
		n++
		if (r.T-m.Origin)%15 != 0 {
			t.Errorf("reading at %d is off the grid from %d", r.T, m.Origin)
		}
		return nil
	})
	if n < 5 {
		t.Errorf("%d readings", n)
	}
}

func TestExport(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 3 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	os.Remove(out)
	o.Export = true
	if err := Run(context.Background(), o, nil, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
}

// Three hours at 1 minute, keeping the last hour raw: the first two hours
// become 5-minute steps with the mean and the peak.
func TestRollup(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 3*time.Hour - time.Minute
	o.KeepRaw = time.Hour
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true, vary: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	validate(t, out)
	segs, _ := segments(ReadingsDir(out))
	var raw, rolled int
	for _, s := range segs {
		if s.raw {
			raw++
		} else {
			rolled++
		}
	}
	if rolled < 1 || raw < 1 {
		t.Errorf("segments %+v: want rolled-up and raw ones", segs)
	}

	rs := readFile(t, out)
	w := find(t, rs, "web", "app")
	spans, _ := w.attrs["thoras.step_spans"].([]string)
	if len(spans) != 2 || !strings.HasSuffix(spans[0], "/300") || !strings.HasSuffix(spans[1], "/60") || w.attrs["thoras.step_seconds"] != int64(60) {
		t.Fatalf("step spans %v, step %v", spans, w.attrs["thoras.step_seconds"])
	}
	usage, peak := w.metrics["container.cpu.usage"], w.metrics["thoras.container.cpu.usage.max"]
	if len(usage) != len(peak) {
		t.Fatalf("%d usage points but %d peaks", len(usage), len(peak))
	}
	// Read n (from 1) averages pod 1 at 100m+10m*(n%10) and pod 2 at 300m, so
	// a bucket of reads 1-5 has mean 215m and peak 225m, and reads 6-10 have
	// mean 230m and peak 245m.
	if d := usage[1].T - usage[0].T; d != 300 {
		t.Errorf("rolled points %ds apart", d)
	}
	for i := 0; i < 12; i++ {
		u, p := usage[i].V, peak[i].V
		if !(u == 0.215 && p == 0.225) && !(u == 0.23 && p == 0.245) {
			t.Errorf("bucket %d: mean %v, peak %v", i, u, p)
		}
	}
	last := len(usage) - 1
	if usage[last].T-usage[last-1].T != 60 || usage[last].V != peak[last].V {
		t.Errorf("raw tail: %v / %v", usage[last-1:], peak[last-1:])
	}
	if w.metrics["k8s.container.cpu.request.desired"][0].V != 0.25 {
		t.Errorf("request after rollup = %v", w.metrics["k8s.container.cpu.request.desired"][0])
	}
	for _, r := range rs {
		if r.attrs["thoras.fidelity_tier"] != int64(3) {
			t.Errorf("tier %v", r.attrs["thoras.fidelity_tier"])
		}
	}
	if !strings.Contains(log.String(), "older than 1h0m kept at 5m0s") {
		t.Errorf("progress does not mention the rollup:\n%s", log)
	}
}

// An export says when the poll rolled readings up, not what its own
// --keep-raw default would have done.
func TestExportReportsThePollsRollupAge(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 3*time.Hour - time.Minute
	o.KeepRaw = time.Hour
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	export := opts(out)
	export.Export = true
	log.Reset()
	if err := Run(context.Background(), export, nil, clk, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "older than 1h0m kept at 5m0s") || strings.Contains(log.String(), "2d0h0m") {
		t.Errorf("export:\n%s", log)
	}

	// Directories written before the age was recorded leave it out.
	dir := ReadingsDir(out)
	m, err := readMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.KeepRaw = nil
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := Run(context.Background(), export, nil, clk, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "(older readings kept at 5m0s)") {
		t.Errorf("export without the age:\n%s", log)
	}

	// A later poll with another --keep-raw records it.
	o.Duration = time.Minute
	o.KeepRaw = 2 * time.Hour
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true}, clk, io.Discard); err != nil {
		t.Fatal(err)
	}
	if m, err = readMeta(dir); err != nil || m.KeepRaw == nil || *m.KeepRaw != 7200 {
		t.Fatalf("meta after a second poll: %+v, %v", m, err)
	}
}

// A tiny export budget forces one resource per pass; the output must not change.
func TestExportBatchesMatch(t *testing.T) {
	out, clk, log := setup(t)
	o := opts(out)
	o.Duration = 90 * time.Minute
	o.KeepRaw = 30 * time.Minute
	if err := Run(context.Background(), o, &fakeReader{clock: clk, metrics: true, vary: true}, clk, log); err != nil {
		t.Fatal(err)
	}
	var whole, batched bytes.Buffer
	if _, err := Export(dirSource(ReadingsDir(out)), "test", &whole, DefaultExportBudget); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(dirSource(ReadingsDir(out)), "test", &batched, 1); err != nil {
		t.Fatal(err)
	}
	if whole.String() != batched.String() {
		t.Errorf("batched export differs")
	}
}

func TestStoreKeepsLevelsAcrossSegments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	st, err := OpenStore(dir, Meta{Cluster: "c", Step: 60, RollupStep: 300, Origin: 1_790_000_000})
	if err != nil {
		t.Fatal(err)
	}
	st.sync = false
	c := testCluster(true)
	for i := int64(0); i < 130; i++ {
		if i == 70 {
			c.Pods[0].Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = rl("cpu", "300m")[corev1.ResourceCPU]
			c.Pods[1].Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = rl("cpu", "300m")[corev1.ResourceCPU]
		}
		if err := st.Append(fromReading(1_790_000_000+i*60, 60, Reduce(c))); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	var reqs []int64
	scan(dir, math.MaxInt64, nil, func(r *record) error {
		for _, ce := range r.Containers {
			if ce.Key == web {
				reqs = append(reqs, ce.CPURequest)
			}
		}
		return nil
	})
	if len(reqs) != 130 || reqs[69] != 250e6 || reqs[70] != 300e6 || reqs[129] != 300e6 {
		t.Errorf("requests: %d readings, [69]=%d [70]=%d [129]=%d", len(reqs), reqs[69], reqs[70], reqs[129])
	}
}
