package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeStore answers the query API from a handler that gets the parsed request.
type fakeStore struct {
	mu       sync.Mutex
	requests []url.Values
	paths    []string
	handle   func(path string, q url.Values) (status int, body any)
}

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (f *fakeStore) client(t *testing.T) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.requests = append(f.requests, q)
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		status, body := f.handle(r.URL.Path, q)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return &Client{G: &HTTPGetter{Base: srv.URL}, Retries: 1, sleep: noSleep}
}

func vector(val float64) map[string]any {
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector",
		"result": []any{map[string]any{"metric": map[string]string{}, "value": []any{1.0, strconv.FormatFloat(val, 'f', -1, 64)}}}}}
}

func empty() map[string]any {
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{}}}
}

func matrix(labels map[string]string, start, end, step int64, val func(t int64) float64) map[string]any {
	var vals []any
	for t := start; t <= end; t += step {
		vals = append(vals, []any{float64(t), fmt.Sprint(val(t))})
	}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix",
		"result": []any{map[string]any{"metric": labels, "values": vals}}}}
}

func atoi(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

var _ = context.Background
