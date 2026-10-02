package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/kube"
	"k8s.io/client-go/rest"
)

var checkNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fakeCheck builds a checkEnv from canned answers and records what was read.
type fakeCheck struct {
	connErr    error
	denied     map[string]bool // Access.String() of denied checks
	metricsErr error
	metricPods int
	disc       *prometheus.Discovery
	discErr    error
	answerErr  map[string]error // by service
	probeErr   map[string]error
	history    time.Duration   // held by every store; 0 is the full --days
	noRequests map[string]bool // stores whose requests cannot be used, by service
	directErr  error
	ddErr      error
	reads      []string
}

func (f *fakeCheck) env() checkEnv {
	e := checkEnv{connErr: f.connErr}
	if f.connErr == nil {
		e.conn = &kube.Conn{Context: "test", ClusterName: "test-cluster", User: "admin", Config: &rest.Config{Host: "https://api.test"}}
	}
	note := func(s string) { f.reads = append(f.reads, s) }
	e.whoami = func(context.Context) (*kube.Identity, error) {
		note("whoami")
		return &kube.Identity{User: "jane", Groups: []string{"system:authenticated"}}, nil
	}
	e.canI = func(_ context.Context, a kube.Access) (bool, string, error) {
		note("can-i " + a.String())
		return !f.denied[a.String()], "", nil
	}
	e.metrics = func(context.Context) (int, error) {
		note("metrics")
		if f.metricsErr != nil {
			return 0, f.metricsErr
		}
		return f.metricPods, nil
	}
	e.discover = func(context.Context) (*prometheus.Discovery, error) {
		note("discover")
		if f.disc == nil {
			return &prometheus.Discovery{}, f.discErr
		}
		return f.disc, f.discErr
	}
	e.answer = func(_ context.Context, c prometheus.Candidate) error {
		note("answer " + c.Service)
		return f.answerErr[c.Service]
	}
	e.probe = func(_ context.Context, c prometheus.Candidate) (*prometheus.Report, error) {
		note("probe " + c.Service)
		rep := f.report()
		rep.NoRequests = f.noRequests[c.Service]
		return rep, f.probeErr[c.Service]
	}
	e.direct = func(context.Context) (string, func(context.Context) error, func(context.Context) (*prometheus.Report, error), error) {
		note("direct")
		if f.directErr != nil {
			return "", nil, nil, f.directErr
		}
		return "store at https://mimir.example", func(context.Context) error { return nil },
			func(context.Context) (*prometheus.Report, error) { return f.report(), nil }, nil
	}
	e.datadog = func(context.Context) (string, error) { note("datadog"); return "https://api.datadoghq.com", f.ddErr }
	return e
}

func (f *fakeCheck) report() *prometheus.Report {
	rep := &prometheus.Report{Version: "3.0.0", End: checkNow, Start: checkNow.Add(-14 * 24 * time.Hour), FullHistory: true}
	if f.history > 0 {
		rep.Start, rep.FullHistory = checkNow.Add(-f.history), false
		rep.Warnings = []string{"the store holds about 2.0 days of history, less than the 14 days asked for"}
	}
	return rep
}

func (f *fakeCheck) read(s string) bool {
	for _, r := range f.reads {
		if r == s || strings.HasPrefix(r, s+" ") {
			return true
		}
	}
	return false
}

func runCheckWith(t *testing.T, f *fakeCheck, args ...string) *checkReport {
	t.Helper()
	clearDatadogEnv(t)
	o, err := runFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	return check(context.Background(), o, f.env(), checkNow)
}

var found = &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA, storeB}}

func TestCheckReadyWithAStore(t *testing.T) {
	f := &fakeCheck{disc: found, metricPods: 1}
	r := runCheckWith(t, f)
	if r.ExitCode != checkReady || r.Source.Name != "prometheus" || r.Source.Tier != 1 {
		t.Fatalf("got %d %+v", r.ExitCode, r.Source)
	}
	if r.Kubernetes.Identity != "jane" || len(r.Stores) != 2 || r.MetricsServer == nil || !r.MetricsServer.Answers {
		t.Errorf("report %+v", r)
	}
	// Every store found is checked, not only the one run would use.
	if !f.read("probe vm") {
		t.Error("second store not probed")
	}
	var checks []string
	for _, a := range r.Access {
		checks = append(checks, a.Mode+" "+a.Check)
	}
	want := []string{
		"prometheus list services -A",
		"prometheus list prometheuses.monitoring.coreos.com -A",
		"prometheus list prometheusagents.monitoring.coreos.com -A",
		"prometheus get services/proxy prometheus:9090 -n monitoring",
		"prometheus get services/proxy vm:8428 -n obs",
		"poll list pods -A",
		"poll list replicasets.apps -A",
		"poll list deployments.apps -A",
		"poll list statefulsets.apps -A",
		"poll list nodes",
		"poll list pods.metrics.k8s.io -A",
	}
	if strings.Join(checks, "\n") != strings.Join(want, "\n") {
		t.Errorf("access checks:\n%s", strings.Join(checks, "\n"))
	}
}

func TestCheckLimitedByShortHistory(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{disc: found, metricPods: 1, history: 48 * time.Hour})
	if r.ExitCode != checkLimited || r.Source.Tier != 3 || r.Stores[0].HistorySecs != 48*3600 {
		t.Fatalf("got %d %+v %+v", r.ExitCode, r.Source, r.Stores[0])
	}
	if !strings.Contains(r.Summary, "2.0 days of history") {
		t.Errorf("summary %q", r.Summary)
	}
}

func TestCheckSkipsAStoreThatCannotBeUsed(t *testing.T) {
	f := &fakeCheck{disc: found, metricPods: 1, answerErr: map[string]error{"prometheus": errors.New("503")},
		probeErr: map[string]error{"vm": errors.New("kube-state-metrics is not in the store")}}
	r := runCheckWith(t, f)
	if r.Source.Name != "poll" || r.ExitCode != checkLimited || r.Source.Tier != 3 {
		t.Fatalf("got %d %+v", r.ExitCode, r.Source)
	}
	if r.Stores[0].Answers || !r.Stores[1].Answers || r.Stores[1].Usable {
		t.Errorf("stores %+v", r.Stores)
	}
	if len(r.Order) != 4 || r.Order[3].Source != "polling metrics-server" || !r.Order[3].Usable {
		t.Errorf("order %+v", r.Order)
	}
}

func TestCheckPollTierFollowsDuration(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{metricPods: 1}, "--duration", "336h", "--keep-raw", "0")
	if r.Source.Name != "poll" || r.Source.Tier != 1 || r.ExitCode != checkReady {
		t.Fatalf("got %d %+v", r.ExitCode, r.Source)
	}
}

func TestCheckCannotRun(t *testing.T) {
	cases := []struct {
		name string
		f    *fakeCheck
		args []string
		want string
	}{
		{"no metrics-server", &fakeCheck{metricsErr: errors.New("metrics.k8s.io is not served")}, nil, "metrics.k8s.io is not served"},
		{"metrics-server has no pods yet", &fakeCheck{}, nil, "no pod usage yet"},
		{"poll denied", &fakeCheck{metricPods: 1, denied: map[string]bool{"list nodes": true}}, nil, "may not list nodes"},
		{"no kubeconfig", &fakeCheck{connErr: errors.New("no configuration")}, nil, "cannot read the kubeconfig"},
		{"failing target does not fall back", &fakeCheck{metricPods: 1, answerErr: map[string]error{"prometheus": errors.New("403")}},
			[]string{"--target", "monitoring/prometheus:9090"}, "does not answer queries: 403"},
		{"--source prometheus does not fall back", &fakeCheck{metricPods: 1}, []string{"--source", "prometheus"}, "no Prometheus-compatible store"},
		{"bad keys with --source datadog", &fakeCheck{ddErr: errors.New("Datadog returned 403")}, []string{"--source", "datadog"}, "403"},
		{"failing --url", &fakeCheck{directErr: errors.New("no credentials")}, []string{"--url", "https://mimir.example"}, "no credentials"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "bad keys with --source datadog" {
				t.Setenv("DD_API_KEY", "a")
				t.Setenv("DD_APP_KEY", "b")
			}
			o, err := runFlags(c.args)
			if err != nil {
				t.Fatal(err)
			}
			r := check(context.Background(), o, c.f.env(), checkNow)
			if r.ExitCode != checkBlocked || r.Status != "cannot run" || r.Source.Name != "" {
				t.Fatalf("got %d %+v", r.ExitCode, r.Source)
			}
			if !strings.Contains(r.Summary, c.want) {
				t.Errorf("summary %q lacks %q", r.Summary, c.want)
			}
		})
	}
}

func TestCheckDatadog(t *testing.T) {
	clearDatadogEnv(t)
	t.Setenv("DD_API_KEY", "a")
	t.Setenv("DD_APP_KEY", "b")
	o, _ := runFlags(nil)
	f := &fakeCheck{disc: found, metricPods: 1}
	r := check(context.Background(), o, f.env(), checkNow)
	if r.Source.Name != "datadog" || r.ExitCode != checkReady || !r.Datadog.Valid {
		t.Fatalf("got %d %+v", r.ExitCode, r.Source)
	}
	// The fallbacks are still checked, since run falls back when Datadog has no data.
	if !f.read("discover") || !f.read("metrics") {
		t.Errorf("reads %v", f.reads)
	}

	f = &fakeCheck{disc: found, metricPods: 1, ddErr: errors.New("Datadog returned 403")}
	r = check(context.Background(), o, f.env(), checkNow)
	if r.Source.Name != "prometheus" || r.Order[0].Usable || r.Order[0].Note != "Datadog returned 403" {
		t.Fatalf("invalid keys: %+v %+v", r.Source, r.Order)
	}

	o, _ = runFlags([]string{"--source", "datadog", "--days", "3"})
	f = &fakeCheck{}
	r = check(context.Background(), o, f.env(), checkNow)
	if r.Source.Name != "datadog" || r.ExitCode != checkLimited || r.Source.Tier != 3 {
		t.Fatalf("--days 3: %d %+v", r.ExitCode, r.Source)
	}
	if strings.Join(f.reads, " ") != "datadog" {
		t.Errorf("--source datadog read %v", f.reads)
	}
}

func TestCheckURLReadsNothingFromTheCluster(t *testing.T) {
	f := &fakeCheck{}
	r := runCheckWith(t, f, "--url", "https://mimir.example/prometheus", "--cluster-name", "prod")
	if r.ExitCode != checkReady || r.Source.From != "store at https://mimir.example" {
		t.Fatalf("got %d %+v", r.ExitCode, r.Source)
	}
	if strings.Join(f.reads, " ") != "direct" || r.Kubernetes.Needed || len(r.Access) != 0 {
		t.Errorf("reads %v, report %+v", f.reads, r.Kubernetes)
	}
}

func TestCheckTargetAccessNamesTheService(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{metricPods: 1}, "--target", "https:monitoring/thanos-query:9091")
	var proxy []string
	for _, a := range r.Access {
		if a.Resource == "services/proxy" {
			proxy = append(proxy, a.Check)
		}
		if a.Resource == "services" {
			t.Errorf("--target skips discovery, but checks %s", a.Check)
		}
	}
	if strings.Join(proxy, ",") != "get services/proxy https:thanos-query:9091 -n monitoring" {
		t.Errorf("proxy checks %v", proxy)
	}
}

func TestCheckOutput(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{disc: found, metricPods: 1, history: 48 * time.Hour})
	var plain, colored bytes.Buffer
	printCheck(&plain, r, false)
	printCheck(&colored, r, true)
	out := plain.String()
	if strings.Contains(out, "\x1b[") || !strings.Contains(colored.String(), "\x1b[") {
		t.Error("color is not optional")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "limited: run reads prometheus monitoring/prometheus:9090") {
		t.Errorf("summary line %q", last)
	}
	for _, s := range []string{"identity: jane", "list services -A", "metrics-server: answers", "2.0 days", "Datadog: no keys", "expected tier: 3 (limited history)"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}

	js, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatal(err)
	}
	if back["status"] != "limited" || back["exit_code"] != 1.0 || back["source"].(map[string]any)["tier"] != 3.0 {
		t.Errorf("json %s", js)
	}
}

func TestCheckFlags(t *testing.T) {
	clearDatadogEnv(t)
	for _, bad := range [][]string{{"--color", "pink"}, {"--timeout", "0s"}, {"--source", "nowhere"}, {"extra"}} {
		if code := runCheck(bad); code != checkBlocked {
			t.Errorf("%v: exit %d, want %d", bad, code, checkBlocked)
		}
	}
	if code := runCheck([]string{"-h"}); code != 0 {
		t.Errorf("-h: exit %d", code)
	}
}

func TestCheckURLNeedsAClusterName(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{connErr: errors.New("no configuration")}, "--url", "https://mimir.example/prometheus")
	if r.ExitCode != checkBlocked || !strings.Contains(r.Summary, "--cluster-name is required") {
		t.Fatalf("got %d %q", r.ExitCode, r.Summary)
	}
	r = runCheckWith(t, &fakeCheck{connErr: errors.New("no configuration")}, "--url", "https://mimir.example/prometheus", "--cluster-name", "prod")
	if r.ExitCode != checkReady {
		t.Fatalf("with --cluster-name: %d %q", r.ExitCode, r.Summary)
	}
}

func TestCheckStoreWithoutRequestsIsSkipped(t *testing.T) {
	f := &fakeCheck{disc: found, metricPods: 1, noRequests: map[string]bool{"prometheus": true}}
	r := runCheckWith(t, f)
	if r.Stores[0].Usable || !strings.Contains(r.Stores[0].Error, "no container with all of") || r.Source.From != storeB.String() {
		t.Fatalf("stores %+v, source %+v", r.Stores, r.Source)
	}
	r = runCheckWith(t, f, "--target", "monitoring/prometheus:9090")
	if r.ExitCode != checkBlocked {
		t.Fatalf("--target: %d %q", r.ExitCode, r.Summary)
	}
}

func TestCheckOrderWithTargetLeavesDatadogOut(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{metricPods: 1}, "--target", "monitoring/prometheus:9090")
	for _, s := range r.Order {
		if s.Source == "Datadog" {
			t.Errorf("order %+v", r.Order)
		}
	}
}

func TestCheckOutputSaysACommonErrorOnce(t *testing.T) {
	r := runCheckWith(t, &fakeCheck{metricPods: 1})
	for i := range r.Access {
		r.Access[i].Error = "x509: certificate signed by unknown authority"
	}
	var b bytes.Buffer
	printCheck(&b, r, false)
	if n := strings.Count(b.String(), "x509"); n != 1 {
		t.Errorf("error printed %d times:\n%s", n, b.String())
	}
}
