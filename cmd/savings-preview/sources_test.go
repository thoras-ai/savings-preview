package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/datadog"
	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

func clearDatadogEnv(t *testing.T) {
	t.Helper()
	for _, e := range []string{datadog.EnvAPIKey, datadog.EnvAppKey, datadog.EnvAppKeyAlt, datadog.EnvSite} {
		t.Setenv(e, "")
	}
}

func TestRunFlagsSources(t *testing.T) {
	clearDatadogEnv(t)
	o, err := runFlags([]string{"--url", "https://mimir.example/prometheus", "--tenant", "team-a", "--max-query-range", "720h", "--selector", `cluster="prod"`})
	if err != nil {
		t.Fatal(err)
	}
	if o.direct.url != "https://mimir.example/prometheus" || o.direct.tenant != "team-a" || o.direct.maxRange != 720*time.Hour ||
		o.useDatadog || o.promSelector != `cluster="prod"` {
		t.Fatalf("parsed %+v", o)
	}
	// A URL in --target is the same as --url, as in the prometheus command.
	if o, err = runFlags([]string{"--target", "http://prom.example:9090"}); err != nil || o.direct.url != "http://prom.example:9090" || o.target != nil {
		t.Fatalf("--target URL: %+v, %v", o, err)
	}
	if o, err = runFlags([]string{"--source", "datadog", "--selector", "kube_cluster_name:prod"}); err != nil || !o.useDatadog ||
		len(o.ddFilters) != 1 || o.ddFilters[0] != "kube_cluster_name:prod" {
		t.Fatalf("--source datadog: %+v, %v", o, err)
	}
	// --project names Google Managed Prometheus, like a URL.
	if o, err = runFlags([]string{"--project", "shop-prod", "--gke-metrics", "system"}); err != nil ||
		o.direct.url != "https://monitoring.googleapis.com/v1/projects/shop-prod/location/global/prometheus" || o.direct.backend != prometheus.BackendGMP {
		t.Fatalf("--project: %+v, %v", o, err)
	}
	var access strings.Builder
	printAccess(&access, o)
	if !strings.Contains(access.String(), "Google Cloud Monitoring in project shop-prod") {
		t.Errorf("access: %s", access.String())
	}
	for _, bad := range [][]string{
		{"--project", "shop-prod", "--source", "poll"},
		{"--tenant", "t"},
		{"--backend", "mimir"},
		{"--url", "http://a.example", "--target", "http://b.example"},
		{"--url", "http://a.example", "--source", "poll"},
		{"--url", "http://a.example", "--source", "datadog"},
		{"--target", "monitoring/prometheus:9090", "--source", "datadog"},
		{"--url", "http://a.example", "--max-query-range", "-1h"},
		{"--selector", "kube_cluster_name:prod"},
		{"--source", "datadog", "--selector", `cluster="prod"`},
		{"--source", "datadog", "--step", "30s"},
	} {
		if _, err := runFlags(bad); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}

func TestRunFlagsPicksDatadogByKeys(t *testing.T) {
	clearDatadogEnv(t)
	cases := []struct {
		env  map[string]string
		args []string
		want bool
	}{
		{nil, nil, false},
		{map[string]string{datadog.EnvAPIKey: "a"}, nil, false},
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKey: "b"}, nil, true},
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKeyAlt: "b"}, nil, true},
		{nil, []string{"--api-key-file", "a", "--app-key-file", "b"}, true},
		{map[string]string{datadog.EnvAPIKey: "a"}, []string{"--app-key-file", "b"}, true},
		// A store named on the command line wins over keys in the environment.
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKey: "b"}, []string{"--url", "http://prom.example"}, false},
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKey: "b"}, []string{"--target", "monitoring/prometheus:9090"}, false},
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKey: "b"}, []string{"--source", "prometheus"}, false},
		{map[string]string{datadog.EnvAPIKey: "a", datadog.EnvAppKey: "b"}, []string{"--source", "poll"}, false},
	}
	for _, c := range cases {
		clearDatadogEnv(t)
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		o, err := runFlags(c.args)
		if err != nil {
			t.Fatalf("%v %v: %v", c.env, c.args, err)
		}
		if o.useDatadog != c.want {
			t.Errorf("%v %v: useDatadog %v, want %v", c.env, c.args, o.useDatadog, c.want)
		}
	}
}

// fakeSources records which sources collect tried.
type fakeSources struct {
	tried     []string
	directErr error
	ddErr     error
	store     fakeStore
}

func (f *fakeSources) sources(direct, dd bool) sources {
	s := sources{store: f.store.steps()}
	result := func(from string) *pulled { return &pulled{from: from, file: priced()} }
	if direct {
		s.direct = func(context.Context) (*pulled, error) {
			f.tried = append(f.tried, "url")
			return result("url"), f.directErr
		}
	}
	if dd {
		s.datadog = func(context.Context) (*pulled, error) {
			f.tried = append(f.tried, "datadog")
			if f.ddErr != nil {
				return nil, f.ddErr
			}
			return result("Datadog"), nil
		}
	}
	discover := s.store.discover
	s.store.discover = func(ctx context.Context) (*prometheus.Discovery, error) {
		f.tried = append(f.tried, "discover")
		return discover(ctx)
	}
	s.poll = func(context.Context) error { f.tried = append(f.tried, "poll"); return nil }
	return s
}

func TestCollectOrder(t *testing.T) {
	none := &prometheus.Discovery{}
	found := &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA}}
	cases := []struct {
		name        string
		source      string
		direct, dd  bool
		directErr   error
		ddErr       error
		disc        *prometheus.Discovery
		wantTried   string
		wantErr     bool
		wantMessage string
	}{
		{name: "url", source: "auto", direct: true, wantTried: "url"},
		{name: "failing url is an error", source: "auto", direct: true, directErr: errors.New("401"), wantTried: "url", wantErr: true},
		{name: "failing url with --source prometheus", source: "prometheus", direct: true, directErr: errors.New("401"), wantTried: "url", wantErr: true},
		{name: "datadog first", source: "auto", dd: true, disc: found, wantTried: "datadog"},
		{name: "datadog falls back to a store", source: "auto", dd: true, ddErr: errors.New("no container usage"), disc: found,
			wantTried: "datadog discover", wantMessage: "Not using Datadog: no container usage."},
		{name: "datadog falls back to polling", source: "auto", dd: true, ddErr: errors.New("403"), disc: none, wantTried: "datadog discover poll"},
		{name: "datadog chosen with --source", source: "datadog", dd: true, ddErr: errors.New("403"), wantTried: "datadog", wantErr: true},
		{name: "several clusters in datadog", source: "auto", dd: true, ddErr: &datadog.ClusterError{Values: []string{"a", "b"}}, wantTried: "datadog", wantErr: true},
		{name: "no datadog keys", source: "auto", disc: none, wantTried: "discover poll"},
		{name: "poll", source: "poll", wantTried: "poll"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSources{directErr: c.directErr, ddErr: c.ddErr, store: fakeStore{disc: c.disc, file: priced()}}
			o := &runOptions{out: filepath.Join(t.TempDir(), "out.json"), source: c.source}
			var log bytes.Buffer
			err := collect(context.Background(), f.sources(c.direct, c.dd), o, &log)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(f.tried, " "); got != c.wantTried {
				t.Errorf("tried %q, want %q", got, c.wantTried)
			}
			if c.wantMessage != "" && !strings.Contains(log.String(), c.wantMessage) {
				t.Errorf("log %q lacks %q", log.String(), c.wantMessage)
			}
		})
	}
}

func TestCollectStoppedDuringDatadog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeSources{ddErr: context.Canceled, store: fakeStore{disc: &prometheus.Discovery{}}}
	s := f.sources(false, true)
	inner := s.datadog
	s.datadog = func(ctx context.Context) (*pulled, error) { cancel(); return inner(ctx) }
	err := collect(ctx, s, &runOptions{out: filepath.Join(t.TempDir(), "o.json"), source: "auto"}, io.Discard)
	if !errors.Is(err, context.Canceled) || strings.Join(f.tried, " ") != "datadog" {
		t.Fatalf("err %v, tried %v", err, f.tried)
	}
}

// replay serves the API responses of a recorded Datadog run.
func replay(t *testing.T, path string) (*httptest.Server, int64) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Now       int64 `json:"now"`
		Responses map[string]struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, ok := rec.Responses[r.Method+" "+r.URL.Path+" "+string(body)]
		if !ok {
			http.Error(w, `{"errors":["not recorded"]}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(resp.Status)
		w.Write(resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec.Now
}

func TestRunReadsDatadog(t *testing.T) {
	clearDatadogEnv(t)
	srv, now := replay(t, "../../internal/adapters/datadog/testdata/fake-org.json")
	t.Setenv(datadog.EnvAPIKey, "api")
	t.Setenv(datadog.EnvAppKey, "app")
	out := filepath.Join(t.TempDir(), "savings-preview.otlp.json")
	o, err := runFlags([]string{"--site", srv.URL, "--selector", "kube_cluster_name:prod-us", "--days", "1", "--step", "5m", "-o", out,
		"--kubeconfig", filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	o.now = time.Unix(now, 0)
	var log bytes.Buffer
	s, err := newSources(o, &log)
	if err != nil {
		t.Fatal(err)
	}
	s.poll = func(context.Context) error { t.Fatal("polled although Datadog answered"); return nil }
	if err := collect(context.Background(), s, o, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	// Without a kubeconfig the run still reads Datadog.
	if !strings.Contains(log.String(), "Cannot read the kubeconfig") || !strings.Contains(log.String(), "query the Datadog metrics API") ||
		!strings.Contains(log.String(), "from Datadog:") {
		t.Errorf("log:\n%s", log.String())
	}
	if errs := withFile(out, validate.ValidateReader); len(errs) > 0 {
		t.Fatalf("contract problems: %v", errs)
	}
}

func TestRunDatadogFailureFallsBackOnlyInAuto(t *testing.T) {
	clearDatadogEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":["Forbidden"]}`, http.StatusForbidden)
	}))
	defer srv.Close()
	t.Setenv(datadog.EnvAPIKey, "api")
	t.Setenv(datadog.EnvAppKey, "app")
	for _, source := range []string{"auto", "datadog"} {
		o, err := runFlags([]string{"--site", srv.URL, "--source", source, "-o", filepath.Join(t.TempDir(), "o.json")})
		if err != nil {
			t.Fatal(err)
		}
		polled := false
		s := sources{
			datadog: func(ctx context.Context) (*pulled, error) { return pullDatadog(ctx, o, io.Discard) },
			store:   (&fakeStore{disc: &prometheus.Discovery{}}).steps(),
			poll:    func(context.Context) error { polled = true; return nil },
		}
		var log bytes.Buffer
		err = collect(context.Background(), s, o, &log)
		switch source {
		case "auto":
			if err != nil || !polled || !strings.Contains(log.String(), "Not using Datadog") {
				t.Errorf("auto: err %v, polled %v, log %q", err, polled, log.String())
			}
		case "datadog":
			if err == nil || polled || !strings.Contains(err.Error(), "timeseries_query scope") {
				t.Errorf("--source datadog: err %v, polled %v", err, polled)
			}
		}
	}
}

func TestRunURLIsNeverAFallback(t *testing.T) {
	clearDatadogEnv(t)
	t.Setenv(envBearer, "")
	t.Setenv(envBasic, "")
	t.Setenv(envHeaders, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","errorType":"unauthorized","error":"no org id"}`))
	}))
	defer srv.Close()
	o, err := runFlags([]string{"--url", srv.URL, "--cluster-name", "c", "-o", filepath.Join(t.TempDir(), "o.json")})
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	s, err := newSources(o, &log)
	if err != nil {
		t.Fatal(err)
	}
	if s.poll != nil || s.datadog != nil || s.store.discover != nil {
		t.Fatal("a run with --url has other sources to fall back to")
	}
	err = collect(context.Background(), s, o, &log)
	if err == nil || !strings.Contains(err.Error(), "--tenant") {
		t.Fatalf("want the tenant hint, got %v", err)
	}
	if strings.Contains(log.String(), "metrics-server") {
		t.Errorf("a run with --url mentions polling:\n%s", log.String())
	}
}

func TestRunURLNeedsAClusterName(t *testing.T) {
	clearDatadogEnv(t)
	o, err := runFlags([]string{"--url", "http://prom.example", "--kubeconfig", filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSources(o, io.Discard); err == nil || !strings.Contains(err.Error(), "--cluster-name is required") {
		t.Fatalf("got %v", err)
	}
}
