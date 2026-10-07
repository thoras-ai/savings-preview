package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/kube"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

func TestParseContexts(t *testing.T) {
	got := parseContexts("prod-us # main\n\n  # staging later\nstaging\r\n arn:aws:eks:us-east-1:1:cluster/x \n")
	want := []string{"prod-us", "staging", "arn:aws:eks:us-east-1:1:cluster/x"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFleetFlags(t *testing.T) {
	o, err := runFlags([]string{"--context", "one"})
	if err != nil || o.fleet || o.context != "one" {
		t.Fatalf("one context: %v %+v", err, o)
	}
	o, err = runFlags(nil)
	if err != nil || o.fleet || o.context != "" {
		t.Fatalf("no context: %v", err)
	}
	t.Setenv("DD_API_KEY", "k")
	t.Setenv("DD_APP_KEY", "k")
	o, err = runFlags([]string{"--context", "a", "--context", "b"})
	if err != nil || !o.fleet || o.useDatadog {
		t.Fatalf("two contexts: %v fleet=%v datadog=%v", err, o.fleet, o.useDatadog)
	}
	for _, args := range [][]string{
		{"--all-contexts", "--url", "https://prom.example.com"},
		{"--context", "a", "--context", "b", "--target", "https://prom.example.com"},
		{"--contexts-file", "f", "--cluster-name", "x"},
		{"--all-contexts", "--source", "datadog"},
	} {
		if _, err := runFlags(args); err == nil {
			t.Errorf("%q: accepted", args)
		}
	}
}

func TestFleetContexts(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	if err := os.WriteFile(kc, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {}}]
contexts:
- {name: zeta, context: {cluster: c, user: u}}
- {name: alpha, context: {cluster: c, user: u}}
current-context: alpha
`), 0o600); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "contexts")
	if err := os.WriteFile(list, []byte("beta\nalpha # again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &runOptions{kubeconfig: kc, contexts: contextList{"alpha"}, contextsFile: list, allContexts: true}
	got, err := fleetContexts(o)
	if err != nil || !slices.Equal(got, []string{"alpha", "beta", "zeta"}) {
		t.Errorf("got %q, %v", got, err)
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("# none\n"), 0o644)
	if _, err := fleetContexts(&runOptions{contextsFile: empty}); err == nil {
		t.Error("an empty contexts file was accepted")
	}
}

// fleetFile is one cluster's data: a workload with one container, two hours
// at 1-minute steps.
func fleetFile(cluster string, cpuReq float64) *contract.File {
	common := contract.Common{Cluster: cluster, Source: "prometheus", StepSeconds: 60, Tier: 3}
	var use, req, mem, memReq, pods []contract.Point
	for i := range 120 {
		ts := int64(1789516800 + 60*i)
		use = append(use, contract.Point{T: ts, V: 0.2})
		req = append(req, contract.Point{T: ts, V: cpuReq})
		mem = append(mem, contract.Point{T: ts, V: 1 << 28})
		memReq = append(memReq, contract.Point{T: ts, V: 1 << 30})
		pods = append(pods, contract.Point{T: ts, V: 2})
	}
	return &contract.File{Resources: []contract.Resource{
		{Common: common, Namespace: "web", Kind: "deployment", Workload: "api", Metrics: []contract.Series{{Name: contract.DeploymentPods, Points: pods}}},
		{Common: common, Namespace: "web", Kind: "deployment", Workload: "api", Container: "app", Metrics: []contract.Series{
			{Name: contract.CPUUsage, Points: use}, {Name: contract.MemoryWorkingSet, Points: mem},
			{Name: contract.CPURequest, Points: req}, {Name: contract.MemoryRequest, Points: memReq}}},
	}}
}

func fakeFleet(t *testing.T, conns map[string]string, broken map[string]error) {
	t.Helper()
	oldConnect, oldSources, oldPoll := connectFleet, memberSources, memberPoll
	t.Cleanup(func() { connectFleet, memberSources, memberPoll = oldConnect, oldSources, oldPoll })
	connectFleet = func(_, ctx string) (*kube.Conn, error) {
		name, ok := conns[ctx]
		if !ok {
			return nil, errors.New(`context "` + ctx + `" does not exist`)
		}
		return &kube.Conn{ClusterName: name, Context: ctx}, nil
	}
	memberSources = func(o *runOptions, w io.Writer) (sources, error) {
		return sources{direct: func(context.Context) (*pulled, error) {
			if err := broken[o.context]; err != nil {
				return nil, err
			}
			return &pulled{from: "the store", file: fleetFile(o.cluster, 1), start: time.Unix(1789516800, 0), end: time.Unix(1789524000, 0)}, nil
		}}, nil
	}
	memberPoll = func(context.Context, *kube.Conn, string, *runOptions) error {
		return errors.New("polling is not faked")
	}
}

func TestRunFleet(t *testing.T) {
	fakeFleet(t, map[string]string{"a": "prod", "b": "prod", "c": "staging", "d": "dev"}, map[string]error{"d": errors.New("Unauthorized")})
	dir := t.TempDir()
	out := filepath.Join(dir, "savings-preview.otlp.json")
	o, err := runFlags([]string{"--context", "a", "--context", "b", "--context", "c", "--context", "d", "--context", "gone", "-o", out})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	b, err := runFleet(context.Background(), o, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	for _, p := range []string{out, compactName(out), b.zip} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
	if errs := withFile(compactName(out), validate.ValidateReader); len(errs) > 0 {
		t.Errorf("merged compact file: %v", errs)
	}
	if errs := withFile(out, validate.ValidateReader); len(errs) > 0 {
		t.Errorf("merged full file: %v", errs)
	}
	var names []string
	for _, c := range b.summary.Clusters {
		names = append(names, c.Cluster+"="+c.Error)
	}
	// Two contexts on one cluster are named after their contexts.
	want := []string{"a=", "b=", "staging=", "dev=Unauthorized", `gone=context "gone" does not exist`}
	if !slices.Equal(names, want) {
		t.Errorf("clusters: got %q, want %q", names, want)
	}
	one := b.summary.Clusters[0].Totals.AnnualSpend
	if !same(b.summary.Totals.AnnualSpend, 3*one) || b.summary.Totals.Counted != 3 {
		t.Errorf("fleet spend %v, want 3 × %v", b.summary.Totals.AnnualSpend, one)
	}
	text := log.String()
	for _, s := range []string{"Reading 5 clusters: a, b, c, d, gone", "Contexts a, b all point at cluster prod", "[d] not read: Unauthorized",
		"Fleet, 3 of 5 clusters:", "not read: gone: context \"gone\" does not exist"} {
		if !strings.Contains(text, s) {
			t.Errorf("log lacks %q:\n%s", s, text)
		}
	}
	var sum map[string]any
	if err := json.Unmarshal(readZipEntry(t, b.zip, "summary.json"), &sum); err != nil {
		t.Fatal(err)
	}
	if cl, _ := sum["clusters"].([]any); len(cl) != 5 {
		t.Errorf("summary.json clusters: %v", sum["clusters"])
	}
}

func TestRunFleetAllFail(t *testing.T) {
	fakeFleet(t, map[string]string{"a": "x"}, map[string]error{"a": errors.New("Unauthorized")})
	o, err := runFlags([]string{"--context", "a", "--context", "b", "-o", filepath.Join(t.TempDir(), "o.otlp.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runFleet(context.Background(), o, io.Discard); err == nil || !strings.Contains(err.Error(), "none of the 2 clusters could be read") {
		t.Errorf("got %v", err)
	}
}

func TestCheckFleet(t *testing.T) {
	old := newFleetCheckEnv
	t.Cleanup(func() { newFleetCheckEnv = old })
	newFleetCheckEnv = func(o *runOptions) checkEnv {
		return checkEnv{connErr: errors.New("context " + o.context + " is broken")}
	}
	o, err := runFlags([]string{"--context", "a", "--context", "b", "--source", "poll"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := checkFleet(context.Background(), o, false, false, &out); code != checkBlocked {
		t.Errorf("exit %d", code)
	}
	for _, s := range []string{"context  cluster  source", "a                 none", "context a is broken", "Fleet: 0 of 2 clusters can run"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}
	out.Reset()
	if code := checkFleet(context.Background(), o, true, false, &out); code != checkBlocked {
		t.Errorf("exit %d", code)
	}
	var fc fleetCheck
	if err := json.Unmarshal(out.Bytes(), &fc); err != nil || len(fc.Clusters) != 2 || fc.Clusters[1].Context != "b" || fc.Status != "cannot run" {
		t.Errorf("json: %v %s", err, out.String())
	}
}

func readZipEntry(t *testing.T, path, name string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			b, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("%s has no %s", path, name)
	return nil
}

func same(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
