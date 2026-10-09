package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
	"github.com/thoras-ai/savings-preview/internal/savings"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

func TestCompactName(t *testing.T) {
	for in, want := range map[string]string{
		"savings-preview.otlp.json": "savings-preview.compact.otlp.json",
		"out/prod.json":             "out/prod.compact.otlp.json",
		"noext":                     "noext.compact.otlp.json",
	} {
		if got := compactName(in); got != want {
			t.Errorf("compactName(%q) = %q, want %q", in, got, want)
		}
	}
}

func copyFixture(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile("../../fixtures/savings-preview.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "savings-preview.otlp.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExportCompactFile(t *testing.T) {
	dir := t.TempDir()
	in := copyFixture(t, dir)
	if err := runExport([]string{"--compact", in}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "savings-preview.compact.otlp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if errs := validate.Validate(b); len(errs) > 0 {
		t.Fatal(errs)
	}
	other := filepath.Join(dir, "x.json")
	if err := runExport([]string{"--compact", "-o", other, in}); err != nil {
		t.Fatal(err)
	}
	if c, _ := os.ReadFile(other); !bytes.Equal(c, b) {
		t.Error("-o wrote different content")
	}
}

func TestExportRefusals(t *testing.T) {
	dir := t.TempDir()
	in := copyFixture(t, dir)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{in}, "already a contract file; add --compact"},
		{[]string{"--compact", "-o", in, in}, "would overwrite the input"},
		{[]string{"--compact"}, "one INPUT"},
		{[]string{"--compact", filepath.Join(dir, "nope.json")}, "no such file"},
	} {
		err := runExport(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }
func (c *stepClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

// oneApp is a cluster of one Deployment with one pod and one container, whose
// usage climbs with each read.
type oneApp struct{ reads int }

func (r *oneApp) Read(context.Context) (*polling.Cluster, error) {
	r.reads++
	one := int32(1)
	q := resource.MustParse
	c := &polling.Cluster{
		Deployments: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "a"}, Spec: appsv1.DeploymentSpec{Replicas: &one}}},
		ReplicaSets: []appsv1.ReplicaSet{{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "a",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}}}},
		Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "api-1-x", Namespace: "a",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-1"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: q("1"), corev1.ResourceMemory: q("1Gi")}}}}}}},
		Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: q("4"), corev1.ResourceMemory: q("16Gi")}}}},
	}
	var pm polling.PodMetrics
	pm.Metadata.Name, pm.Metadata.Namespace = "api-1-x", "a"
	pm.Containers = []polling.ContainerUsage{{Name: "app", Usage: corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(100+r.reads%17*10), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(300+r.reads%5)<<20, resource.BinarySI)}}}
	c.PodMetrics = []polling.PodMetrics{pm}
	return c, nil
}

// TestCompactReadings polls three hours, rolling up all but the last 30
// minutes, and checks that compacting the readings directly gives the same
// file as compacting the poll's output.
func TestCompactReadings(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "savings-preview.otlp.json")
	clk := &stepClock{now: time.Unix(1789516800, 0)}
	err := polling.Run(context.Background(), polling.Options{Cluster: "c", Output: out, Interval: time.Minute, Duration: 3 * time.Hour,
		WriteEvery: time.Hour, KeepRaw: 30 * time.Minute, Rollup: 5 * time.Minute}, &oneApp{}, clk, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	fromFile := filepath.Join(dir, "a.json")
	fromReadings := filepath.Join(dir, "b.json")
	st, err := compactFile(out, fromFile)
	if err != nil {
		t.Fatal(err)
	}
	if st.FinestStep != 60 || st.SourceStep != 300 || st.Compacted != 3 {
		t.Errorf("stats %+v", st)
	}
	if err := runExport([]string{"--compact", "-o", fromReadings, polling.ReadingsDir(out)}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(fromFile)
	b, _ := os.ReadFile(fromReadings)
	if !bytes.Equal(a, b) {
		t.Fatal("compacting the readings and the file differ")
	}
	if errs := validate.Validate(a); len(errs) > 0 {
		t.Fatal(errs)
	}
	// Without --compact a readings directory is written at full resolution.
	full := filepath.Join(dir, "full.json")
	if err := runExport([]string{"-o", full, polling.ReadingsDir(out)}); err != nil {
		t.Fatal(err)
	}
	if want, _ := os.ReadFile(out); !bytes.Equal(must(os.ReadFile(full)), want) {
		t.Error("export of the readings differs from the poll's file")
	}
	// Default name next to the input.
	if err := runExport([]string{"--compact", polling.ReadingsDir(out)}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(must(os.ReadFile(filepath.Join(dir, "savings-preview.compact.otlp.json"))), a) {
		t.Error("default output differs")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// TestBuildReport writes the compact file, the summary and the zip from a
// contract file, and checks that the data in report.html comes back out
// unchanged with export.
func TestBuildReport(t *testing.T) {
	dir := t.TempDir()
	out := copyFixture(t, dir)
	b, err := buildReport(out)
	if err != nil {
		t.Fatal(err)
	}
	data := must(os.ReadFile(b.compact))
	zr, err := zip.OpenReader(b.zip)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	files := map[string][]byte{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		files[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	if strings.Join(names, " ") != "summary.json report.html savings-preview.compact.otlp.json" {
		t.Errorf("zip holds %v", names)
	}
	if !bytes.Equal(files["savings-preview.compact.otlp.json"], data) {
		t.Error("the compact file in the zip differs from the one on disk")
	}
	var s map[string]any
	if err := json.Unmarshal(files["summary.json"], &s); err != nil {
		t.Fatal(err)
	}
	fd, _ := s["full_data"].(map[string]any)
	if fd["file"] != "savings-preview.otlp.json" || fd["bytes"] != float64(5689124) || s["step_seconds"] != float64(300) || s["compact_step_seconds"] != float64(900) {
		t.Errorf("summary %s", files["summary.json"])
	}
	if !strings.Contains(zr.Comment, "savings-preview export -o savings-preview.otlp.json report.html") ||
		!strings.Contains(zr.Comment, "The full-resolution file, savings-preview.otlp.json (5.7 MB), is not in the zip") || strings.Contains(zr.Comment, dir) {
		t.Errorf("comment %q", zr.Comment)
	}

	// The page carries the data once, and export takes it back out.
	unzipped := filepath.Join(dir, "unzipped")
	os.Mkdir(unzipped, 0o755)
	page := filepath.Join(unzipped, "report.html")
	if err := os.WriteFile(page, files["report.html"], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runExport([]string{page}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(must(os.ReadFile(filepath.Join(unzipped, "report.otlp.json"))), data) {
		t.Error("data from report.html differs from the compact file")
	}
	// Compacting a page's compact data copies it as it is.
	if err := runExport([]string{"--compact", page}); err != nil {
		t.Fatal(err)
	}
	if errs := validate.Validate(must(os.ReadFile(filepath.Join(unzipped, "report.compact.otlp.json")))); len(errs) > 0 {
		t.Error(errs)
	}
	totals, err := savings.Analyze(data)
	if err != nil || totals.Totals != b.summary.Totals {
		t.Errorf("totals %+v, summary %+v (%v)", totals.Totals, b.summary.Totals, err)
	}
}

func TestPageData(t *testing.T) {
	for page, want := range map[string]string{
		`<html><script type="application/json" id="report-data">{"a":"\u003c/script>"}</script><script>x</script>`: `{"a":"\u003c/script>"}`,
		`<<script type="application/json" id="report-data">{"b":"</scrip"}</script>`:                               `{"b":"</scrip"}`,
		`<script type="application/json" id="report-data">{}<</script>`:                                            `{}<`,
	} {
		var out bytes.Buffer
		if err := pageData(bufio.NewReader(strings.NewReader(page)), &out); err != nil || out.String() != want {
			t.Errorf("%s: got %q (%v), want %q", page, out.String(), err, want)
		}
	}
	for _, page := range []string{`<html></html>`, `<script type="application/json" id="report-data">{"a":1}`} {
		if err := pageData(bufio.NewReader(strings.NewReader(page)), io.Discard); err == nil {
			t.Errorf("%s: want an error", page)
		}
	}
}
