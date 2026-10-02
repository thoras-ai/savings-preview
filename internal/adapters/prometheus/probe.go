package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// ErrAgentMode is returned for a Prometheus running in agent mode.
type ErrAgentMode struct{ RemoteWrite []string }

func (e *ErrAgentMode) Error() string {
	if len(e.RemoteWrite) == 0 {
		return "Prometheus is running in agent mode and has no query API; its remote_write targets could not be read"
	}
	return "Prometheus is running in agent mode and has no query API; its data is sent to: " + strings.Join(e.RemoteWrite, ", ")
}

// Check confirms the store answers queries. A Prometheus in agent mode answers
// 404 on the query API, so the remote_write targets are read from its config
// and returned in an ErrAgentMode instead of a bare failure.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.Query(ctx, "vector(1)", time.Time{})
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == 404 || strings.Contains(strings.ToLower(ae.Message), "agent")) {
		if rw, rerr := c.RemoteWriteTargets(ctx); rerr == nil {
			return &ErrAgentMode{RemoteWrite: rw}
		}
		if strings.Contains(strings.ToLower(ae.Message), "agent") {
			return &ErrAgentMode{}
		}
	}
	return err
}

// RemoteWriteTargets reads the remote_write URLs from the running config.
func (c *Client) RemoteWriteTargets(ctx context.Context) ([]string, error) {
	raw, err := c.Raw(ctx, "/api/v1/status/config", nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var cfg struct {
		RemoteWrite []struct {
			URL string `json:"url"`
		} `json:"remote_write"`
	}
	if err := yaml.Unmarshal([]byte(d.YAML), &cfg); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range cfg.RemoteWrite {
		out = append(out, RedactURL(r.URL))
	}
	sort.Strings(out)
	return out, nil
}

func (c *Client) present(ctx context.Context, q string, at time.Time) (bool, error) {
	s, err := c.Query(ctx, q, at)
	if err != nil {
		return false, err
	}
	return len(s) > 0 && len(s[0].Points) > 0 && s[0].Points[0].V > 0, nil
}

// EarliestData finds how far back query has data, no further back than
// maxBack before now. It returns the earliest time found, rounded to the hour.
// When data exists at the far end, that end is returned and full is true.
func (c *Client) EarliestData(ctx context.Context, query string, now time.Time, maxBack time.Duration) (earliest time.Time, full bool, err error) {
	ok, err := c.present(ctx, query, now)
	if err != nil {
		return time.Time{}, false, err
	}
	if !ok {
		return time.Time{}, false, fmt.Errorf("no samples for the query in the last few minutes")
	}
	far := now.Add(-maxBack)
	lo := far
	// Nothing exists before the oldest sample in a Prometheus TSDB, which
	// narrows the search. Other stores do not have the metric; that is fine.
	if s, qerr := c.Query(ctx, "min(prometheus_tsdb_lowest_timestamp_seconds)", now); qerr == nil && len(s) > 0 && len(s[0].Points) > 0 {
		if t := time.Unix(int64(s[0].Points[0].V), 0).Add(-time.Hour); t.After(lo) {
			lo = t
		}
	}
	if lo.Equal(far) {
		if ok, err := c.present(ctx, query, far); err != nil {
			return time.Time{}, false, err
		} else if ok {
			return far, true, nil
		}
	}
	hi := now
	for hi.Sub(lo) > time.Hour {
		mid := lo.Add(hi.Sub(lo) / 2)
		ok, err := c.present(ctx, query, mid)
		if err != nil {
			return time.Time{}, false, err
		}
		if ok {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi.Truncate(time.Hour), false, nil
}

// Build returns the store's version string, empty if unknown.
func (c *Client) Build(ctx context.Context) string {
	raw, err := c.Raw(ctx, "/api/v1/status/buildinfo", url.Values{})
	if err != nil {
		return ""
	}
	var d struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(raw, &d)
	return d.Version
}
