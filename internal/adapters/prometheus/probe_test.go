package prometheus

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEarliestDataBinarySearch(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	first := now.Add(-100 * time.Hour)
	f := &fakeStore{}
	f.handle = func(path string, q url.Values) (int, any) {
		if strings.Contains(q.Get("query"), "prometheus_tsdb_lowest_timestamp_seconds") {
			return 200, empty() // a store without the metric, such as Thanos
		}
		if atoi(q.Get("time")) >= first.Unix() {
			return 200, vector(3)
		}
		return 200, empty()
	}
	got, full, err := f.client(t).EarliestData(context.Background(), "count(x)", now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if full {
		t.Error("history is shorter than asked for")
	}
	if d := first.Sub(got); d < 0 || d > time.Hour {
		t.Errorf("earliest %v is not within an hour before the first sample %v", got, first)
	}
	if n := len(f.requests); n > 14 {
		t.Errorf("binary search used %d queries", n)
	}
}

func TestEarliestDataUsesTSDBLowestTimestamp(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	first := now.Add(-5 * time.Hour)
	f := &fakeStore{}
	f.handle = func(path string, q url.Values) (int, any) {
		if strings.Contains(q.Get("query"), "prometheus_tsdb_lowest_timestamp_seconds") {
			return 200, vector(float64(first.Unix()))
		}
		if atoi(q.Get("time")) >= first.Unix() {
			return 200, vector(1)
		}
		return 200, empty()
	}
	got, _, err := f.client(t).EarliestData(context.Background(), "count(x)", now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if d := first.Sub(got); d < 0 || d > time.Hour {
		t.Errorf("earliest %v, first sample %v", got, first)
	}
	if n := len(f.requests); n > 8 {
		t.Errorf("the lowest timestamp should narrow the search, used %d queries", n)
	}
}

func TestEarliestDataFullHistory(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	f := &fakeStore{handle: func(string, url.Values) (int, any) { return 200, vector(1) }}
	got, full, err := f.client(t).EarliestData(context.Background(), "count(x)", now, 14*24*time.Hour)
	if err != nil || !full || !got.Equal(now.Add(-14*24*time.Hour)) {
		t.Errorf("got %v full=%v err=%v", got, full, err)
	}
}

func TestEarliestDataNoCurrentData(t *testing.T) {
	f := &fakeStore{handle: func(string, url.Values) (int, any) { return 200, empty() }}
	if _, _, err := f.client(t).EarliestData(context.Background(), "count(x)", time.Unix(1_790_000_000, 0), time.Hour); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCheckReportsAgentModeTargets(t *testing.T) {
	f := &fakeStore{}
	f.handle = func(path string, q url.Values) (int, any) {
		switch path {
		case "/api/v1/query":
			return 404, map[string]any{"status": "error", "errorType": "not_found", "error": "unavailable with Prometheus Agent"}
		case "/api/v1/status/config":
			return 200, map[string]any{"status": "success", "data": map[string]any{
				"yaml": "global: {}\nremote_write:\n- url: https://user:secret@prom.example.com/api/v1/write?token=abc\n"}}
		}
		return 404, empty()
	}
	err := f.client(t).Check(context.Background())
	am, ok := err.(*ErrAgentMode)
	if !ok {
		t.Fatalf("want ErrAgentMode, got %v", err)
	}
	if len(am.RemoteWrite) != 1 || am.RemoteWrite[0] != "https://prom.example.com/api/v1/write" {
		t.Errorf("remote_write targets %v must be listed without credentials", am.RemoteWrite)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "abc") {
		t.Errorf("credentials leaked in %q", err)
	}
}

func TestRedactURL(t *testing.T) {
	if got := RedactURL("https://u:p@h.example/x?y=1#z"); got != "https://h.example/x" {
		t.Errorf("got %q", got)
	}
}

// Behind Thanos Query the lowest timestamp describes a sidecar's local TSDB,
// while the store answers for much longer.
func TestEarliestDataIgnoresTSDBHintWhenDataExistsBeforeIt(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	first := now.Add(-10 * 24 * time.Hour)
	f := &fakeStore{}
	f.handle = func(path string, q url.Values) (int, any) {
		if strings.Contains(q.Get("query"), "prometheus_tsdb_lowest_timestamp_seconds") {
			return 200, vector(float64(now.Add(-2 * time.Hour).Unix()))
		}
		if atoi(q.Get("time")) >= first.Unix() {
			return 200, vector(1)
		}
		return 200, empty()
	}
	got, _, err := f.client(t).EarliestData(context.Background(), "count(x)", now, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if d := first.Sub(got); d < 0 || d > time.Hour {
		t.Errorf("earliest %v, first sample %v: the hint must not cut the history", got, first)
	}
}

func TestEarliestDataNeverStartsAfterTheFirstSample(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	for _, offset := range []time.Duration{31 * time.Minute, 95 * time.Minute, 5*time.Hour + 17*time.Minute, 49*time.Hour + 59*time.Minute} {
		first := now.Add(-offset)
		f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
			if strings.Contains(q.Get("query"), "prometheus_tsdb_lowest") {
				return 200, empty()
			}
			if atoi(q.Get("time")) >= first.Unix() {
				return 200, vector(1)
			}
			return 200, empty()
		}}
		got, _, err := f.client(t).EarliestData(context.Background(), "count(x)", now, 14*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got.After(first) || first.Sub(got) > time.Hour {
			t.Errorf("first sample %v ago: start is %v ago", offset, now.Sub(got))
		}
	}
}

func TestProbeStopsBeforeThePull(t *testing.T) {
	f := &fakeStore{handle: clusterStore(true, true, true)}
	rep, err := Probe(context.Background(), Options{Client: f.client(t), Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range f.paths {
		if p == "/api/v1/query_range" {
			t.Fatal("Probe pulled a range")
		}
	}
	// The fake holds 3 days; the history search ends within an hour of it.
	first := time.Unix(t0-3*86400, 0)
	if rep.FullHistory || rep.Start.After(first) || first.Sub(rep.Start) > time.Hour {
		t.Errorf("start %s, full %v; want within an hour before %s", rep.Start, rep.FullHistory, first)
	}
	if rep.Version != "3.0.0" || len(rep.Warnings) != 1 {
		t.Errorf("report %+v", rep)
	}
}

func TestProbeReportsWhatRunWouldFailOn(t *testing.T) {
	f := &fakeStore{handle: clusterStore(false, false, false)}
	if _, err := Probe(context.Background(), Options{Client: f.client(t), Days: 7, Step: time.Minute, Now: time.Unix(t0, 0)}); err == nil {
		t.Fatal("want the kube-state-metrics error")
	}
}

func TestProbeFlagsStoresWithoutUsableRequests(t *testing.T) {
	for name, handle := range map[string]func(string, url.Values) (int, any){
		"no requests": clusterStore(true, false, false),
		"no phase":    clusterStore(true, true, true, PhaseProbe),
	} {
		f := &fakeStore{handle: handle}
		rep, err := Probe(context.Background(), Options{Client: f.client(t), Days: 7, Step: time.Minute, Now: time.Unix(t0, 0)})
		if err != nil || !rep.NoRequests {
			t.Errorf("%s: err %v, NoRequests %v", name, err, rep != nil && rep.NoRequests)
		}
	}
	f := &fakeStore{handle: clusterStore(true, true, true)}
	if rep, err := Probe(context.Background(), Options{Client: f.client(t), Days: 7, Step: time.Minute, Now: time.Unix(t0, 0)}); err != nil || rep.NoRequests {
		t.Errorf("with requests: err %v, NoRequests %v", err, rep.NoRequests)
	}
}
