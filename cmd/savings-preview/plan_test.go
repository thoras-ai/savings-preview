package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
)

// fakePlanChecks answers the checks run makes before the plan: a store with
// 2.9 days of history for context "dev", polling for "staging", and a broken
// kubeconfig for any other context.
func fakePlanChecks(t *testing.T) {
	t.Helper()
	old, oldNow := newFleetCheckEnv, timeNow
	t.Cleanup(func() { newFleetCheckEnv, timeNow = old, oldNow })
	timeNow = func() time.Time { return checkNow }
	newFleetCheckEnv = func(o *runOptions) checkEnv {
		switch o.context {
		case "dev":
			f := &fakeCheck{metricPods: 3, history: 70 * time.Hour,
				disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{{Kind: prometheus.KindPrometheus, Namespace: "monitoring",
					Service: "prometheus-server", Port: "80", Scheme: "http", Reason: "service label app=prometheus"}}}}
			return f.env()
		case "staging":
			return (&fakeCheck{metricPods: 3}).env()
		}
		return checkEnv{connErr: errors.New(`context "` + o.context + `" does not exist`)}
	}
}

func planFor(t *testing.T, args ...string) string {
	t.Helper()
	fakePlanChecks(t)
	o, err := runFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := planChecks(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printPlan(&out, o, cs)
	return out.String()
}

func TestPlanText(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "savings-preview.otlp.json")
	text := planFor(t, "--context", "dev", "--context", "staging", "--context", "qa", "--duration", "2m", "-o", out)
	for _, s := range []string{
		"\ndev: Prometheus monitoring/prometheus-server:80, 2.9 days of history\n",
		"\nstaging: no store found, will poll metrics-server for 2m\n",
		"\nqa: cannot run: context \"qa\" does not exist\n",
		"This run only reads; it creates, changes and deletes nothing. It will:\n",
		"  - list Services, and Prometheus Operator resources if installed, to find a metrics store\n",
		"It writes savings-preview.otlp.json, savings-preview.compact.otlp.json, savings-preview.zip and each cluster's files in savings-preview.clusters, in " + dir + ". Nothing is uploaded.\n",
		"Then it opens the report in your browser.\n",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("plan lacks %q:\n%s", s, text)
		}
	}
	// Every cluster is listed before what the run reads.
	if strings.Index(text, "qa: cannot run") > strings.Index(text, "This run only reads") {
		t.Errorf("a cluster that cannot run is listed after the reads:\n%s", text)
	}

	text = planFor(t, "--context", "staging", "--source", "poll", "--no-open", "-o", out)
	for _, s := range []string{"\nstaging: will poll metrics-server until you press Ctrl-C\n",
		"It writes savings-preview.otlp.json, savings-preview.compact.otlp.json and savings-preview.zip, in " + dir + ".",
		"Then it serves the report on a local address and prints the address. It does not open a browser.\n"} {
		if !strings.Contains(text, s) {
			t.Errorf("plan lacks %q:\n%s", s, text)
		}
	}
	text = planFor(t, "--context", "dev", "--collect-only", "-o", out)
	if !strings.Contains(text, "Then it exits. It does not open a browser or serve the report.\n") || strings.Contains(text, "browser.\n\n") {
		t.Errorf("collect-only plan:\n%s", text)
	}
}

func TestStoreTitle(t *testing.T) {
	for in, want := range map[string]string{
		"prometheus monitoring/prom:9090 (service label app=prometheus)": "Prometheus monitoring/prom:9090",
		"victoriametrics obs/vm:8428/select/0/prometheus (port 8428)":    "VictoriaMetrics obs/vm:8428/select/0/prometheus",
		"store at https://mimir.example":                                 "store at https://mimir.example",
	} {
		if got := storeTitle(in); got != want {
			t.Errorf("storeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreflightPrompt(t *testing.T) {
	fakePlanChecks(t)
	const prompt = "Press Enter to continue, or q to quit."
	cases := []struct {
		name, input string
		ask, yes    bool
		goOn        bool
		prompts     int
	}{
		{"enter", "\n", true, false, true, 1},
		{"q", "q\n", true, false, false, 1},
		{"Q and spaces", "  Q \n", true, false, false, 1},
		{"end of input", "", true, false, false, 1},
		{"other text asks again", "what\n\n", true, false, true, 2},
		{"no terminal", "q\n", false, false, true, 0},
		{"--yes", "q\n", true, true, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"--context", "dev", "-o", filepath.Join(t.TempDir(), "o.otlp.json")}
			if c.yes {
				args = append(args, "--yes")
			}
			o, err := runFlags(args)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			goOn, err := preflight(context.Background(), o, strings.NewReader(c.input), &out, c.ask)
			if err != nil || goOn != c.goOn {
				t.Errorf("got %v, %v; want %v", goOn, err, c.goOn)
			}
			if n := strings.Count(out.String(), prompt); n != c.prompts {
				t.Errorf("%d prompts, want %d:\n%s", n, c.prompts, out.String())
			}
			if !o.planned || !strings.Contains(out.String(), "dev: Prometheus") {
				t.Errorf("no plan:\n%s", out.String())
			}
			if !c.goOn && !strings.Contains(out.String(), "Stopped; no history was read.") {
				t.Errorf("quit without saying so:\n%s", out.String())
			}
		})
	}
}

func TestPreflightCtrlC(t *testing.T) {
	fakePlanChecks(t)
	o, err := runFlags([]string{"--context", "dev"})
	if err != nil {
		t.Fatal(err)
	}
	// The input never ends, as a terminal's does not; Ctrl-C cancels ctx.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	goOn, err := preflight(ctx, o, r, io.Discard, true)
	if goOn || err != nil {
		t.Errorf("got %v, %v", goOn, err)
	}
}

func TestInteractiveNeedsTerminals(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	// /dev/null is a character device but not a terminal.
	for _, f := range [][]*os.File{{r, w, w}, {null, null, null}, {r, null, w}} {
		if interactive(f...) {
			t.Errorf("interactive(%s, %s, %s)", f[0].Name(), f[1].Name(), f[2].Name())
		}
	}
}

// TestRunCollectOnly runs a fleet with --collect-only from flags to exit: it
// prints the plan without waiting, writes the zip with each cluster's compact
// file and returns instead of serving the report.
func TestRunCollectOnly(t *testing.T) {
	fakePlanChecks(t)
	fakeFleet(t, map[string]string{"dev": "dev", "staging": "staging"}, nil)
	dir := t.TempDir()
	out := filepath.Join(dir, "savings-preview.otlp.json")
	var log bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runWith([]string{"--context", "dev", "--context", "staging", "--context", "qa", "--collect-only", "-o", out},
			strings.NewReader(""), &log, false)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run --collect-only did not exit")
	}
	text := log.String()
	if strings.Contains(text, "Press Enter") || strings.Contains(text, "Report: http") {
		t.Errorf("prompted or served:\n%s", text)
	}
	// The reads are listed once, in the plan.
	if n := strings.Count(text, "This run only reads"); n != 1 {
		t.Errorf("reads listed %d times:\n%s", n, text)
	}
	zipPath := filepath.Join(dir, "savings-preview.zip")
	if !strings.HasSuffix(text, "\nSend "+zipPath+" to your Thoras contact. It holds the report and the data, and nothing else from your environment.\n") {
		t.Errorf("last lines:\n%s", text)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	want := []string{"summary.json", "report.html", "savings-preview.clusters/dev.compact.otlp.json", "savings-preview.clusters/staging.compact.otlp.json"}
	if !slices.Equal(names, want) {
		t.Errorf("zip holds %q, want %q", names, want)
	}
	for _, n := range want[2:] {
		disk, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(readZipEntry(t, zipPath, n), disk) {
			t.Errorf("%s in the zip differs from the file on disk", n)
		}
	}
}
