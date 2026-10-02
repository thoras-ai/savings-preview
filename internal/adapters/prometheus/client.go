// Package prometheus is a small client for the Prometheus HTTP query API. The same
// client works against Prometheus, Thanos Query, VictoriaMetrics and other
// stores that implement the API.
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Getter performs a GET against the store's HTTP API. path is relative to the
// store root, for example "/api/v1/query_range".
type Getter interface {
	Get(ctx context.Context, path string, params url.Values) (body []byte, status int, err error)
}

// Point is one sample.
type Point struct {
	T int64 // unix seconds
	V float64
}

// Series is one labeled time series.
type Series struct {
	Labels map[string]string
	Points []Point
}

// Client queries a Prometheus-compatible store.
type Client struct {
	G       Getter
	Retries int
	// Selector holds label matchers, as returned by ParseSelector, that are
	// added to every vector selector of every query.
	Selector string
	// MaxRange is the longest time range the store accepts in one query,
	// counting range selectors; zero means no limit.
	MaxRange time.Duration
	// OneMetricPerQuery is set for stores that reject a query that reads
	// more than one metric, or none. Joins between metrics are then made on
	// the client.
	OneMetricPerQuery bool

	// sleep waits out the backoff between attempts; tests replace it.
	sleep func(context.Context, time.Duration) error
}

// APIError is an error reported by the store.
type APIError struct {
	Status    int
	ErrorType string
	Message   string
}

func (e *APIError) Error() string {
	if e.ErrorType != "" {
		return fmt.Sprintf("store returned %d %s: %s", e.Status, e.ErrorType, e.Message)
	}
	return fmt.Sprintf("store returned %d: %s", e.Status, e.Message)
}

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	// A response from the Kubernetes API server instead of the store.
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type resultData struct {
	ResultType string `json:"resultType"`
	Result     []struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
		Values [][2]any          `json:"values"`
	} `json:"result"`
}

func (c *Client) backoff(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	retries := c.Retries
	if retries == 0 {
		retries = 3
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if err := c.backoff(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
				return nil, err
			}
		}
		body, status, err := c.G.Get(ctx, path, params)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		var resp apiResponse
		jerr := json.Unmarshal(body, &resp)
		if status == http.StatusOK && jerr == nil && resp.Status == "success" {
			return resp.Data, nil
		}
		ae := &APIError{Status: status, ErrorType: resp.ErrorType, Message: resp.Error}
		if jerr == nil && resp.Kind == "Status" && resp.Message != "" {
			ae.Message = "Kubernetes API server: " + resp.Message
		}
		if jerr != nil || ae.Message == "" {
			ae.Message = truncate(string(body), 200)
		}
		lastErr = ae
		if status != http.StatusTooManyRequests && status < 500 {
			return nil, ae
		}
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func parseSample(p [2]any) (Point, bool) {
	ts, ok := p[0].(float64)
	if !ok {
		return Point{}, false
	}
	s, ok := p[1].(string)
	if !ok {
		return Point{}, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Point{}, false
	}
	return Point{T: int64(math.Round(ts)), V: v}, true
}

func decode(raw json.RawMessage) ([]Series, error) {
	var d resultData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	out := make([]Series, 0, len(d.Result))
	for _, r := range d.Result {
		s := Series{Labels: r.Metric}
		if r.Values != nil {
			for _, v := range r.Values {
				if p, ok := parseSample(v); ok {
					s.Points = append(s.Points, p)
				}
			}
		} else if p, ok := parseSample(r.Value); ok {
			s.Points = append(s.Points, p)
		}
		out = append(out, s)
	}
	return out, nil
}

// Query runs an instant query.
func (c *Client) Query(ctx context.Context, q string, at time.Time) ([]Series, error) {
	p := url.Values{"query": {withSelector(q, c.Selector)}}
	if !at.IsZero() {
		p.Set("time", strconv.FormatInt(at.Unix(), 10))
	}
	raw, err := c.get(ctx, "/api/v1/query", p)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// QueryRange runs one range query. The caller keeps (end-start)/step under the
// store's point limit; see QueryRangeChunked.
func (c *Client) QueryRange(ctx context.Context, q string, start, end time.Time, step time.Duration) ([]Series, error) {
	p := url.Values{
		"query": {withSelector(q, c.Selector)},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step/time.Second), 10)},
	}
	raw, err := c.get(ctx, "/api/v1/query_range", p)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// MaxPointsPerSeries is the hard cap Prometheus applies to one range query.
const MaxPointsPerSeries = 11000

// ChunkSpan returns the span of one chunk: at most maxSpan, and never more
// than the store's point cap at this step (with headroom).
func ChunkSpan(step, maxSpan time.Duration) time.Duration {
	limit := step * 10000
	if maxSpan <= 0 || maxSpan > limit {
		return limit
	}
	return maxSpan
}

func seriesKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(l[k])
		b.WriteByte(0)
	}
	return b.String()
}

// QueryRangeChunked runs q over [start, end] in consecutive chunks and merges
// the series. start is aligned down to a multiple of step, so every returned
// timestamp is a multiple of step. Chunks run one after another: the caller
// parallelizes across queries, not within one.
func (c *Client) QueryRangeChunked(ctx context.Context, q string, start, end time.Time, step, maxSpan time.Duration) ([]Series, error) {
	stepS := int64(step / time.Second)
	if stepS <= 0 {
		return nil, fmt.Errorf("step must be at least one second")
	}
	if lim := c.rangeLimit(); lim > 0 && (maxSpan <= 0 || maxSpan > lim) {
		maxSpan = lim
	}
	span := int64(ChunkSpan(step, maxSpan) / time.Second)
	if span < stepS {
		span = stepS
	}
	s := start.Unix() / stepS * stepS
	e := end.Unix() / stepS * stepS
	merged := map[string]*Series{}
	var order []string
	for cs := s; cs <= e; cs += span {
		ce := cs + span - stepS
		if ce > e {
			ce = e
		}
		got, err := c.QueryRange(ctx, q, time.Unix(cs, 0), time.Unix(ce, 0), step)
		if err != nil {
			return nil, fmt.Errorf("range %s to %s: %w", time.Unix(cs, 0).UTC().Format(time.RFC3339), time.Unix(ce, 0).UTC().Format(time.RFC3339), err)
		}
		for _, ser := range got {
			k := seriesKey(ser.Labels)
			m, ok := merged[k]
			if !ok {
				m = &Series{Labels: ser.Labels}
				merged[k] = m
				order = append(order, k)
			}
			m.Points = append(m.Points, ser.Points...)
		}
	}
	out := make([]Series, 0, len(order))
	for _, k := range order {
		m := merged[k]
		sort.Slice(m.Points, func(i, j int) bool { return m.Points[i].T < m.Points[j].T })
		m.Points = dedupe(m.Points)
		out = append(out, *m)
	}
	return out, nil
}

// rangeLimit is MaxRange with an hour of headroom, so that the store's own
// lookback or rounding does not tip a query over its limit.
func (c *Client) rangeLimit() time.Duration {
	if c.MaxRange <= 0 {
		return 0
	}
	if c.MaxRange > 2*time.Hour {
		return c.MaxRange - time.Hour
	}
	return c.MaxRange
}

// queryWindow runs an instant query whose range selectors cover window,
// ending at now. When window is longer than the store allows, it runs the
// query in pieces, as queryPieces does.
func (c *Client) queryWindow(ctx context.Context, q func(window time.Duration) string, now time.Time, window time.Duration) ([]Series, error) {
	return c.queryPieces(ctx, q, now, window, c.rangeLimit())
}

// queryPieces runs an instant query whose range selectors cover window,
// ending at now, once per consecutive piece of at most piece, newest first,
// and keeps the newest sample of each series.
func (c *Client) queryPieces(ctx context.Context, q func(window time.Duration) string, now time.Time, window, piece time.Duration) ([]Series, error) {
	if piece <= 0 || window <= piece {
		return c.Query(ctx, q(window), now)
	}
	seen := map[string]bool{}
	var out []Series
	for at, left := now, window; left > 0; at, left = at.Add(-piece), left-piece {
		w := piece
		if left < w {
			w = left
		}
		got, err := c.Query(ctx, q(w), at)
		if err != nil {
			return nil, err
		}
		for _, s := range got {
			if k := seriesKey(s.Labels); !seen[k] {
				seen[k] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func dedupe(p []Point) []Point {
	out := p[:0]
	for i, x := range p {
		if i > 0 && x.T == out[len(out)-1].T {
			continue
		}
		out = append(out, x)
	}
	return out
}

// Raw fetches an API path and returns the data payload as JSON.
func (c *Client) Raw(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	return c.get(ctx, path, params)
}

// HTTPGetter reaches a store directly over HTTP with an Auth applied.
type HTTPGetter struct {
	Base string
	HC   *http.Client
	Auth Auth
}

func (h *HTTPGetter) Get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	u := strings.TrimRight(h.Base, "/") + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	hc := h.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	if h.Auth != nil {
		if hc, err = h.Auth.Wrap(hc); err != nil {
			return nil, 0, err
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return body, resp.StatusCode, err
}
