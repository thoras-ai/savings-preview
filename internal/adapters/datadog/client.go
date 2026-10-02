// Package datadog reads a cluster's usage history from the Datadog metrics
// query API and assembles the Savings Preview contract file.
package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Doer sends one HTTP request. *http.Client implements it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client calls the Datadog API of one site.
type Client struct {
	Base   string // API base URL, from ResolveSite
	APIKey string
	AppKey string
	HTTP   Doer
	// Retries is how many times a request is retried after a 429 or a 5xx
	// response or a network error.
	Retries int
	// Log, when set, reports waits for the rate limit.
	Log func(format string, args ...any)

	sleep func(context.Context, time.Duration) error
	now   func() time.Time

	mu         sync.Mutex
	pauseUntil time.Time // no request starts before this time
}

// APIError is an error response from Datadog.
type APIError struct {
	Status int
	Errors []string
}

func (e *APIError) Error() string {
	if len(e.Errors) == 0 {
		return fmt.Sprintf("Datadog returned %d", e.Status)
	}
	return fmt.Sprintf("Datadog returned %d: %s", e.Status, strings.Join(e.Errors, "; "))
}

// Query is one metrics query of a timeseries request, such as
// "avg:kubernetes.memory.working_set{kube_cluster_name:prod} by {kube_namespace}.rollup(avg, 60)".
type Query struct {
	Name  string
	Query string
}

// Series is one group of one query's result.
type Series struct {
	Query  int               // index into the request's queries
	Tags   map[string]string // group tags; a tag without a value maps to ""
	Unit   string            // the metric's unit name, such as "nanocore", or ""
	Points []Point
}

// Point is one value at a time in unix seconds.
type Point struct {
	T int64
	V float64
}

type tsRequest struct {
	Data tsRequestData `json:"data"`
}

type tsRequestData struct {
	Type       string       `json:"type"`
	Attributes tsAttributes `json:"attributes"`
}

type tsAttributes struct {
	From     int64     `json:"from"`
	To       int64     `json:"to"`
	Interval int64     `json:"interval"`
	Queries  []tsQuery `json:"queries"`
}

type tsQuery struct {
	DataSource string `json:"data_source"`
	Name       string `json:"name"`
	Query      string `json:"query"`
}

type tsResponse struct {
	Data struct {
		Attributes struct {
			Series []struct {
				GroupTags  []string `json:"group_tags"`
				QueryIndex int      `json:"query_index"`
				Unit       []*struct {
					Name string `json:"name"`
				} `json:"unit"`
			} `json:"series"`
			Times  []int64      `json:"times"`
			Values [][]*float64 `json:"values"`
		} `json:"attributes"`
	} `json:"data"`
	Errors json.RawMessage `json:"errors"`
}

// TimeseriesPath is the endpoint of the timeseries query API.
const TimeseriesPath = "/api/v2/query/timeseries"

// MaxPoints is the most points Datadog returns for one series of one query.
// A request for more gets a larger interval than asked for.
const MaxPoints = 1500

// Timeseries runs the queries over [from, to) at interval and returns one
// entry per group per query, plus the spacing of the returned times.
func (c *Client) Timeseries(ctx context.Context, from, to time.Time, interval time.Duration, queries []Query) ([]Series, time.Duration, error) {
	req := tsRequest{Data: tsRequestData{Type: "timeseries_request", Attributes: tsAttributes{
		From: from.UnixMilli(), To: to.UnixMilli(), Interval: interval.Milliseconds(),
	}}}
	for _, q := range queries {
		req.Data.Attributes.Queries = append(req.Data.Attributes.Queries, tsQuery{DataSource: "metrics", Name: q.Name, Query: q.Query})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}
	raw, err := c.do(ctx, http.MethodPost, TimeseriesPath, body)
	if err != nil {
		return nil, 0, err
	}
	var resp tsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, 0, fmt.Errorf("decode timeseries response: %w", err)
	}
	a := resp.Data.Attributes
	if msg := errorText(resp.Errors); msg != "" {
		if len(a.Series) == 0 {
			return nil, 0, &APIError{Status: http.StatusOK, Errors: []string{msg}}
		}
		if c.Log != nil {
			c.Log("Datadog reported, with partial results: %s", msg)
		}
	}
	// The API documents times as seconds but its example, and the API, use
	// milliseconds. A value past 1e11 can only be milliseconds.
	times := make([]int64, len(a.Times))
	for i, t := range a.Times {
		if t > 1e11 {
			t /= 1000
		}
		times[i] = t
	}
	var spacing time.Duration
	for i := 1; i < len(times); i++ {
		if d := time.Duration(times[i]-times[i-1]) * time.Second; spacing == 0 || d < spacing {
			spacing = d
		}
	}
	out := make([]Series, 0, len(a.Series))
	for i, s := range a.Series {
		ser := Series{Query: s.QueryIndex, Tags: parseTags(s.GroupTags)}
		if len(s.Unit) > 0 && s.Unit[0] != nil {
			ser.Unit = s.Unit[0].Name
		}
		if i < len(a.Values) {
			for j, v := range a.Values[i] {
				if v == nil || j >= len(times) || math.IsNaN(*v) || math.IsInf(*v, 0) {
					continue
				}
				ser.Points = append(ser.Points, Point{T: times[j], V: *v})
			}
		}
		out = append(out, ser)
	}
	return out, spacing, nil
}

// errorText reads the errors member of a response, which is a string in the
// timeseries API and a list of strings elsewhere.
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var l []string
	if json.Unmarshal(raw, &l) == nil {
		return strings.Join(l, "; ")
	}
	return string(raw)
}

func parseTags(tags []string) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v, _ := strings.Cut(t, ":")
		m[k] = v
	}
	return m
}

// MaxRateLimitWaits is how many 429 responses with rate-limit headers one
// request waits out before it gives up.
const MaxRateLimitWaits = 100

// ValidateKeysPath checks the API key and the application key together and
// needs no scope.
const ValidateKeysPath = "/api/v2/validate_keys"

// Validate checks both keys before any query, so that a wrong key or site is
// reported as such.
func (c *Client) Validate(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, ValidateKeysPath, nil)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	retries := c.Retries
	if retries == 0 {
		retries = 6
	}
	var lastErr error
	attempt, limited := 0, 0
	for {
		if err := c.waitTurn(ctx); err != nil {
			return nil, err
		}
		raw, status, h, err := c.send(ctx, method, path, body)
		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
		case status >= 200 && status < 300:
			c.noteLimit(h, status)
			return raw, nil
		case status == http.StatusTooManyRequests || status >= 500:
			lastErr = &APIError{Status: status, Errors: responseErrors(raw)}
			if status == http.StatusTooManyRequests && c.noteLimit(h, status) {
				// waitTurn holds the next attempt until the window resets.
				// Such waits follow Datadog's own schedule, so they have a
				// budget of their own and do not use up the retries.
				if limited++; limited > MaxRateLimitWaits {
					return nil, lastErr
				}
				continue
			}
			if c.noteLimit(h, status) {
				wait = -1
			}
		default:
			c.noteLimit(h, status)
			return nil, &APIError{Status: status, Errors: responseErrors(raw)}
		}
		if attempt >= retries {
			return nil, lastErr
		}
		attempt++
		if wait == 0 {
			wait = time.Duration(1<<min(attempt-1, 5)) * time.Second
		}
		if wait > 0 {
			if err := c.sleepFor(ctx, wait); err != nil {
				return nil, err
			}
		}
	}
}

func (c *Client) send(ctx context.Context, method, path string, body []byte) ([]byte, int, http.Header, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rd)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("DD-API-KEY", c.APIKey)
	if c.AppKey != "" {
		req.Header.Set("DD-APPLICATION-KEY", c.AppKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return raw, resp.StatusCode, resp.Header, err
}

// noteLimit reads the rate-limit headers. When the window is used up, or the
// request was refused with 429, no request starts until the window resets
// (X-RateLimit-Reset is the number of seconds until then). It reports
// whether it set such a pause.
func (c *Client) noteLimit(h http.Header, status int) bool {
	remaining, errR := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	resetS, errS := strconv.Atoi(h.Get("X-RateLimit-Reset"))
	if errS != nil || resetS < 0 {
		return false
	}
	if status != http.StatusTooManyRequests && (errR != nil || remaining > 0) {
		return false
	}
	// A second past the reset, so the next request lands in the new window.
	reset := time.Duration(resetS)*time.Second + time.Second
	until := c.clock().Add(reset)
	c.mu.Lock()
	if until.After(c.pauseUntil) {
		c.pauseUntil = until
		if c.Log != nil {
			c.Log("Datadog rate limit %q reached; waiting %s", h.Get("X-RateLimit-Name"), reset)
		}
	}
	c.mu.Unlock()
	return true
}

// waitTurn returns once no pause is in force. Another request can extend
// the pause while this one sleeps, so it checks again after each sleep.
func (c *Client) waitTurn(ctx context.Context) error {
	for {
		c.mu.Lock()
		d := c.pauseUntil.Sub(c.clock())
		c.mu.Unlock()
		if d <= 0 {
			return nil
		}
		if err := c.sleepFor(ctx, d); err != nil {
			return err
		}
	}
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) sleepFor(ctx context.Context, d time.Duration) error {
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

// responseErrors reads {"errors": [...]} from an error body. Errors name the
// status and Datadog's message, never the request headers.
func responseErrors(raw []byte) []string {
	var e struct {
		Errors json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if msg := errorText(e.Errors); msg != "" {
			return []string{msg}
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	if s == "" {
		return nil
	}
	return []string{s}
}
