package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/contract"
)

var (
	storeA = prometheus.Candidate{Kind: prometheus.KindPrometheus, Namespace: "monitoring", Service: "prometheus", Port: "9090", Scheme: "http"}
	storeB = prometheus.Candidate{Kind: prometheus.KindPrometheus, Namespace: "obs", Service: "vm", Port: "8428", Scheme: "http"}
)

func priced() *contract.File {
	return &contract.File{Resources: []contract.Resource{
		{Namespace: "a", Kind: "deployment", Workload: "api"},
		{Namespace: "a", Kind: "deployment", Workload: "api", Container: "app"},
	}}
}

func unpriced() *contract.File {
	return &contract.File{Resources: []contract.Resource{
		{Namespace: "a", Kind: "deployment", Workload: "api"},
		{Namespace: "a", Kind: "deployment", Workload: "api", Container: "app", Missing: []string{contract.CPURequest, contract.MemoryRequest}},
	}}
}

type fakeStore struct {
	disc      *prometheus.Discovery
	discErr   error
	checkErr  map[string]error
	file      *contract.File
	pullErr   error
	pulled    []string
	discovers int
}

func (f *fakeStore) steps() storeSteps {
	return storeSteps{
		discover: func(context.Context) (*prometheus.Discovery, error) { f.discovers++; return f.disc, f.discErr },
		check:    func(_ context.Context, c prometheus.Candidate) error { return f.checkErr[c.Service] },
		pull: func(_ context.Context, c prometheus.Candidate) (*contract.File, *prometheus.Report, error) {
			f.pulled = append(f.pulled, c.Service)
			return f.file, &prometheus.Report{}, f.pullErr
		},
	}
}

func TestTryStoreFallbacks(t *testing.T) {
	cases := []struct {
		name   string
		store  fakeStore
		target *prometheus.Candidate
		want   string // fallback reason substring; empty means the store is used
		used   string
	}{
		{name: "no store", store: fakeStore{disc: &prometheus.Discovery{}},
			want: "no Prometheus-compatible store found"},
		{name: "discovery forbidden", store: fakeStore{discErr: errors.New("list services: forbidden")},
			want: "could not look for a store: list services: forbidden"},
		{name: "agent mode only", store: fakeStore{disc: &prometheus.Discovery{Agents: []prometheus.AgentTarget{
			{Namespace: "monitoring", Name: "agent", RemoteWrite: []string{"https://remote.example/write"}}}}},
			want: "agent mode"},
		{name: "no store answers", store: fakeStore{disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA}},
			checkErr: map[string]error{"prometheus": errors.New("connection refused")}},
			want: "no store answered queries"},
		{name: "required series missing", store: fakeStore{disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA}},
			pullErr: errors.New("the store has no per-container usage metrics")},
			want: "cannot be used: the store has no per-container usage metrics"},
		{name: "no container can be priced", store: fakeStore{disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA}},
			file: unpriced()},
			want: "no container with all of"},
		{name: "second store used when first fails", store: fakeStore{disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA, storeB}},
			checkErr: map[string]error{"prometheus": errors.New("502")}, file: priced()},
			used: "vm"},
		{name: "target skips discovery", store: fakeStore{file: priced()}, target: &storeB, used: "vm"},
		{name: "unreachable target falls back", store: fakeStore{checkErr: map[string]error{"vm": errors.New("404")}}, target: &storeB,
			want: "no store answered queries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tryStore(context.Background(), tc.store.steps(), tc.target, func(string) {})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if res.store.Service != tc.used {
					t.Fatalf("used %s, want %s", res.store.Service, tc.used)
				}
				return
			}
			if !isFallback(err) {
				t.Fatalf("want a fallback, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reason %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTryStoreTargetDoesNotDiscover(t *testing.T) {
	f := fakeStore{file: priced()}
	if _, err := tryStore(context.Background(), f.steps(), &storeA, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if f.discovers != 0 {
		t.Fatalf("discovered %d times with a target", f.discovers)
	}
}

func TestTryStoreStoppedIsNotAFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := fakeStore{disc: &prometheus.Discovery{Candidates: []prometheus.Candidate{storeA}}}
	steps := f.steps()
	steps.pull = func(context.Context, prometheus.Candidate) (*contract.File, *prometheus.Report, error) {
		cancel()
		return nil, nil, context.Canceled
	}
	_, err := tryStore(ctx, steps, nil, func(string) {})
	if isFallback(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunFlags(t *testing.T) {
	o, err := runFlags([]string{"--context", "minikube", "--duration", "3m", "--no-open"})
	if err != nil {
		t.Fatal(err)
	}
	if o.context != "minikube" || o.duration.Minutes() != 3 || !o.noOpen || o.source != "auto" || o.out != "savings-preview.otlp.json" {
		t.Fatalf("parsed %+v", o)
	}
	for _, bad := range [][]string{
		{"--source", "datadog"}, {"--interval", "1500ms"}, {"--duration", "-1m"}, {"--target", "nonsense"}, {"extra"},
	} {
		if _, err := runFlags(bad); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}

func TestZipName(t *testing.T) {
	for in, want := range map[string]string{
		"savings-preview.otlp.json": "savings-preview.zip",
		"out/prod.json":             "out/prod.zip",
		"/tmp/cluster-a.otlp.json":  "/tmp/cluster-a.zip",
		"noext":                     "noext.zip",
	} {
		if got := zipName(in); got != want {
			t.Errorf("zipName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Minute: "1m", 3*time.Minute + 30*time.Second: "3m30s", 2 * time.Hour: "2h", 45 * time.Second: "45s",
	} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%s) = %q, want %q", d, got, want)
		}
	}
}
