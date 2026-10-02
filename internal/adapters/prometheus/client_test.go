package prometheus

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestChunkSpanRespectsPointCap(t *testing.T) {
	if got := ChunkSpan(time.Second, 24*time.Hour); got != 10000*time.Second {
		t.Errorf("1s step: got %v", got)
	}
	if got := ChunkSpan(time.Minute, 24*time.Hour); got != 24*time.Hour {
		t.Errorf("1m step: got %v", got)
	}
}

func TestQueryRangeChunkedSplitsAndMerges(t *testing.T) {
	const step = 60
	start := int64(1_790_000_040) // not aligned to the step
	end := start + 3*86400
	f := &fakeStore{}
	f.handle = func(path string, q url.Values) (int, any) {
		s, e := atoi(q["start"][0]), atoi(q["end"][0])
		if (e-s)/step+1 > MaxPointsPerSeries {
			t.Errorf("chunk of %d points exceeds the cap", (e-s)/step+1)
		}
		if s%step != 0 || e%step != 0 {
			t.Errorf("chunk %d..%d is not aligned to the step", s, e)
		}
		return http.StatusOK, matrix(map[string]string{"a": "b"}, s, e, step, func(t int64) float64 { return float64(t % 7) })
	}
	c := f.client(t)
	got, err := c.QueryRangeChunked(context.Background(), "up", time.Unix(start, 0), time.Unix(end, 0), time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 4 {
		t.Errorf("want 4 chunks for 3 days plus a partial, got %d", len(f.requests))
	}
	if len(got) != 1 {
		t.Fatalf("want one merged series, got %d", len(got))
	}
	pts := got[0].Points
	for i := 1; i < len(pts); i++ {
		if pts[i].T-pts[i-1].T != step {
			t.Fatalf("points %d and %d are %ds apart", i-1, i, pts[i].T-pts[i-1].T)
		}
	}
	if want := (end/step*step-start/step*step)/step + 1; int64(len(pts)) != want {
		t.Errorf("got %d points, want %d", len(pts), want)
	}
}

func TestQueryDropsNaN(t *testing.T) {
	f := &fakeStore{handle: func(string, url.Values) (int, any) {
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{
			map[string]any{"metric": map[string]string{}, "values": []any{[]any{60.0, "1"}, []any{120.0, "NaN"}, []any{180.0, "+Inf"}, []any{240.0, "2"}}}}}}
	}}
	got, err := f.client(t).QueryRange(context.Background(), "x", time.Unix(60, 0), time.Unix(240, 0), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Points) != 2 {
		t.Errorf("NaN and Inf must be dropped, got %v", got[0].Points)
	}
}

func TestErrorsCarryTheStoreMessage(t *testing.T) {
	f := &fakeStore{handle: func(string, url.Values) (int, any) {
		return 422, map[string]any{"status": "error", "errorType": "execution", "error": "query processing would load too many samples"}
	}}
	_, err := f.client(t).Query(context.Background(), "x", time.Time{})
	if err == nil || err.Error() != "store returned 422 execution: query processing would load too many samples" {
		t.Errorf("unexpected error: %v", err)
	}
	if len(f.requests) != 1 {
		t.Errorf("a 4xx must not be retried, got %d requests", len(f.requests))
	}
}

func TestRetriesServerErrors(t *testing.T) {
	n := 0
	f := &fakeStore{}
	f.handle = func(string, url.Values) (int, any) {
		n++
		if n == 1 {
			return 503, map[string]any{"status": "error", "error": "busy"}
		}
		return 200, vector(1)
	}
	c := f.client(t)
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if _, err := c.Query(context.Background(), "x", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(waits) != 1 || waits[0] != 500*time.Millisecond {
		t.Errorf("want one retry after 500ms, got %d requests and waits %v", n, waits)
	}
}

func TestBearerAuth(t *testing.T) {
	var got string
	f := &fakeStore{}
	srv := f.client(t)
	g := srv.G.(*HTTPGetter)
	g.Auth = Bearer{Token: "tok"}
	f.handle = func(string, url.Values) (int, any) { return 200, vector(1) }
	// Capture the header through a transport wrapper.
	g.HC = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header.Get("Authorization")
		return http.DefaultTransport.RoundTrip(r)
	})}
	if _, err := srv.Query(context.Background(), "x", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestUnsupportedAuthFails(t *testing.T) {
	g := &HTTPGetter{Base: "http://127.0.0.1:1", Auth: Unsupported{Scheme: "sigv4"}}
	if _, _, err := g.Get(context.Background(), "/x", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAPIServerStatusErrorIsReadable(t *testing.T) {
	f := &fakeStore{handle: func(string, url.Values) (int, any) {
		return 403, map[string]any{"kind": "Status", "status": "Failure", "reason": "Forbidden", "code": 403,
			"message": `services "prom:9090" is forbidden: User "u" cannot get resource "services/proxy" in API group "" in the namespace "mon"`}
	}}
	_, err := f.client(t).Query(context.Background(), "x", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "Kubernetes API server") || !strings.Contains(err.Error(), "services/proxy") {
		t.Errorf("unexpected error: %v", err)
	}
}
