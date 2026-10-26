package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/validate"
)

// named copies the fixture at path to dir with its cluster renamed to
// cluster, so one fixture can stand for several clusters.
func named(t *testing.T, path, from, cluster string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`"k8s.cluster.name","value":{"stringValue":"` + from + `"}`)
	if !bytes.Contains(b, old) {
		t.Fatalf("%s has no cluster %q", path, from)
	}
	b = bytes.ReplaceAll(b, old, []byte(`"k8s.cluster.name","value":{"stringValue":"`+cluster+`"}`))
	p := filepath.Join(t.TempDir(), cluster+".otlp.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const demoFixture = "../../fixtures/savings-preview.otlp.json"

func zipNames(t *testing.T, path string) []string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func TestReportCompactsAFullFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out", "savings-preview.otlp.json")
	var log bytes.Buffer
	b, err := buildFromFiles([]string{demoFixture}, out, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	text := log.String()
	for _, s := range []string{"10 at 15m steps from 5m data", "passes the contract checks", "demo-cluster: $", "a year in savings"} {
		if !strings.Contains(text, s) {
			t.Errorf("log lacks %q:\n%s", s, text)
		}
	}
	if errs := withFile(compactName(out), validate.ValidateReader); len(errs) > 0 {
		t.Errorf("compact file: %v", errs)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("report wrote a full-resolution file: %v", err)
	}
	if got := zipNames(t, b.zip); !slices.Equal(got, []string{"summary.json", "report.html", "savings-preview.compact.otlp.json"}) {
		t.Errorf("zip entries: %q", got)
	}
	var sum map[string]any
	if err := json.Unmarshal(readZipEntry(t, b.zip, "summary.json"), &sum); err != nil {
		t.Fatal(err)
	}
	if _, ok := sum["full_data"]; ok {
		t.Errorf("summary.json has full_data: %v", sum["full_data"])
	}
	if !bytes.Contains(readZipEntry(t, b.zip, "report.html"), []byte(dataStart)) {
		t.Error("report.html carries no data")
	}

	// run's path, on a copy of the same file, gives the same totals.
	runOut := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	in, _ := os.ReadFile(demoFixture)
	os.WriteFile(runOut, in, 0o644)
	rb, err := buildReport(runOut)
	if err != nil {
		t.Fatal(err)
	}
	if rb.summary.Totals != b.summary.Totals {
		t.Errorf("totals %+v, run's %+v", b.summary.Totals, rb.summary.Totals)
	}
	if dirEntries(t, filepath.Dir(out)) != "savings-preview.compact.otlp.json savings-preview.zip" {
		t.Errorf("left behind: %s", dirEntries(t, filepath.Dir(out)))
	}
}

func dirEntries(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, " ")
}

func TestReportPassesACompactFileThrough(t *testing.T) {
	dir := t.TempDir()
	compacted := filepath.Join(dir, "demo.compact.otlp.json")
	if _, err := compactFile(demoFixture, compacted); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "savings-preview.otlp.json")
	var log bytes.Buffer
	if _, err := buildFromFiles([]string{compacted}, out, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "10 already at 15m or coarser and copied as they were") {
		t.Errorf("log:\n%s", log.String())
	}
	want, _ := os.ReadFile(compacted)
	got, _ := os.ReadFile(compactName(out))
	if !bytes.Equal(got, want) {
		t.Error("the compact file was changed on the way through")
	}
}

func TestReportReadsTheZipRunWrote(t *testing.T) {
	first := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	a, err := buildFromFiles([]string{demoFixture}, first, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	b, err := buildFromFiles([]string{a.zip}, second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.summary.Totals != b.summary.Totals {
		t.Errorf("from the zip %+v, from the file %+v", b.summary.Totals, a.summary.Totals)
	}

	// A zip made by hand around the full file is read too.
	plain := filepath.Join(t.TempDir(), "cluster.zip")
	zf, _ := os.Create(plain)
	zw := zip.NewWriter(zf)
	ew, _ := zw.Create("savings-preview.otlp.json")
	full, _ := os.ReadFile(demoFixture)
	ew.Write(full)
	zw.Close()
	zf.Close()
	c, err := buildFromFiles([]string{plain}, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.summary.Totals != a.summary.Totals {
		t.Errorf("from a zip of the full file %+v, from the file %+v", c.summary.Totals, a.summary.Totals)
	}
}

func TestReportJoinsClustersIntoAFleet(t *testing.T) {
	east := named(t, demoFixture, "demo-cluster", "east")
	west := named(t, "../../fixtures/intermittent.otlp.json", "intermittent", "west")
	var each []float64
	for _, in := range []string{east, west} {
		b, err := buildFromFiles([]string{in}, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		each = append(each, b.summary.Totals.AnnualSpend, b.summary.Totals.AnnualSavings)
	}
	out := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	var log bytes.Buffer
	b, err := buildFromFiles([]string{east, west}, out, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	sum := b.summary
	if len(sum.Clusters) != 2 || sum.Clusters[0].Cluster != "east" || sum.Clusters[1].Cluster != "west" {
		t.Fatalf("clusters: %+v", sum.Clusters)
	}
	if !same(sum.Totals.AnnualSpend, each[0]+each[2]) || !same(sum.Totals.AnnualSavings, each[1]+each[3]) {
		t.Errorf("fleet %v / %v, clusters %v", sum.Totals.AnnualSpend, sum.Totals.AnnualSavings, each)
	}
	if errs := withFile(compactName(out), validate.ValidateReader); len(errs) > 0 {
		t.Errorf("joined file: %v", errs)
	}
	got := zipNames(t, b.zip)
	want := []string{"summary.json", "report.html", "savings-preview.clusters/east.compact.otlp.json", "savings-preview.clusters/west.compact.otlp.json"}
	if !slices.Equal(got, want) {
		t.Errorf("zip entries: %q", got)
	}
	for _, s := range []string{"Fleet, 2 of 2 clusters:", "each cluster's compact file is in"} {
		if !strings.Contains(log.String(), s) {
			t.Errorf("log lacks %q:\n%s", s, log.String())
		}
	}

	// The joined file read again splits into the same clusters.
	again, err := buildFromFiles([]string{compactName(out)}, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if again.summary.Totals != sum.Totals || len(again.summary.Clusters) != 2 {
		t.Errorf("one file of two clusters: %+v, want %+v", again.summary.Totals, sum.Totals)
	}
}

func TestReportMatchesRunsFleet(t *testing.T) {
	fakeFleet(t, map[string]string{"a": "prod", "b": "staging"}, map[string]error{})
	runOut := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	o, err := runFlags([]string{"--context", "a", "--context", "b", "-o", runOut})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := runFleet(context.Background(), o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	dir := clustersDir(runOut)
	inputs := []string{filepath.Join(dir, "prod.otlp.json"), filepath.Join(dir, "staging.otlp.json")}
	b, err := buildFromFiles(inputs, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if b.summary.Totals != rb.summary.Totals {
		t.Errorf("report %+v, run %+v", b.summary.Totals, rb.summary.Totals)
	}
	got, _ := os.ReadFile(b.compact)
	want, _ := os.ReadFile(rb.compact)
	if !bytes.Equal(got, want) {
		t.Error("the joined compact file differs from run's")
	}
}

func TestReportRefusesTheSameClusterTwice(t *testing.T) {
	again := named(t, demoFixture, "demo-cluster", "demo-cluster")
	_, err := buildFromFiles([]string{demoFixture, again}, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `cluster "demo-cluster" is in both `) {
		t.Errorf("got %v", err)
	}
}

func TestReportRefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"notes.txt":  "hello\n",
		"empty.json": "",
		"other.json": `{"data":[1,2,3]}`,
		"plain.html": "<html><body>hi</body></html>",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		_, err := buildFromFiles([]string{p}, filepath.Join(t.TempDir(), "savings-preview.otlp.json"), io.Discard)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: got %v", name, err)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "not a contract file") && !strings.Contains(msg, "not a Savings Preview file") &&
			!strings.Contains(msg, "not a report page") {
			t.Errorf("%s: unclear error %q", name, msg)
		}
	}
}

func TestReportPlan(t *testing.T) {
	dir := t.TempDir()
	var w bytes.Buffer
	if err := reportWith([]string{"-o", dir, demoFixture}, strings.NewReader("q\n"), &w, true); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"savings-preview report, 1 file", "This command reads only local files", "read " + demoFixture + " (5.7 MB)",
		"It writes savings-preview.compact.otlp.json and savings-preview.zip", "Press Enter", "Stopped; nothing was read."} {
		if !strings.Contains(w.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, w.String())
		}
	}
	if got := dirEntries(t, dir); got != "" {
		t.Errorf("quitting wrote %s", got)
	}
	w.Reset()
	if err := reportWith([]string{"--no-open", "--yes", "-o", dir, demoFixture}, strings.NewReader(""), &w, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.String(), "Press Enter") || !strings.Contains(w.String(), "wrote ") {
		t.Errorf("output:\n%s", w.String())
	}
	if err := reportWith(nil, strings.NewReader(""), io.Discard, false); err == nil {
		t.Error("no FILE was accepted")
	}
}
