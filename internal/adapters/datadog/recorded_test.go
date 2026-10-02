package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recording holds the API responses to one Run, keyed by method, path and
// request body, so the run can be replayed without the API. It never holds
// the keys: they travel in headers, which are not recorded.
type recording struct {
	Source    string              `json:"source"` // what answered: a site, or the fake organization of the tests
	Now       int64               `json:"now"`
	Days      int                 `json:"days"`
	StepS     int                 `json:"step_seconds"`
	Filters   []string            `json:"filters,omitempty"`
	Cluster   string              `json:"cluster,omitempty"`
	Responses map[string]response `json:"responses"`
}

type response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body"`
}

var recordedHeaders = []string{"X-RateLimit-Limit", "X-RateLimit-Period", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Name"}

func recordKey(r *http.Request) (string, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return "", err
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	return r.Method + " " + r.URL.Path + " " + string(body), nil
}

type recorder struct {
	hc Doer
	mu sync.Mutex
	r  *recording
}

func (rc *recorder) Do(req *http.Request) (*http.Response, error) {
	k, err := recordKey(req)
	if err != nil {
		return nil, err
	}
	resp, err := rc.hc.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	h := map[string]string{}
	for _, name := range recordedHeaders {
		if v := resp.Header.Get(name); v != "" {
			h[name] = v
		}
	}
	if !json.Valid(body) {
		body, _ = json.Marshal(string(body))
	}
	rc.mu.Lock()
	// A 429 is followed by the same request again; keep the answer that
	// let the run go on.
	if prev, ok := rc.r.Responses[k]; !ok || prev.Status == http.StatusTooManyRequests {
		rc.r.Responses[k] = response{Status: resp.StatusCode, Headers: h, Body: body}
	}
	rc.mu.Unlock()
	return resp, nil
}

type replayer struct {
	r      *recording
	mu     sync.Mutex
	missed []string
}

func (p *replayer) Do(req *http.Request) (*http.Response, error) {
	k, err := recordKey(req)
	if err != nil {
		return nil, err
	}
	resp, ok := p.r.Responses[k]
	if !ok {
		p.mu.Lock()
		p.missed = append(p.missed, k)
		p.mu.Unlock()
		return nil, fmt.Errorf("no recorded response for %s", k)
	}
	h := http.Header{}
	for k, v := range resp.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: resp.Status, Header: h, Body: io.NopCloser(bytes.NewReader(resp.Body))}, nil
}

func record(t *testing.T, path string, rec *recording, c *Client) {
	t.Helper()
	rc := &recorder{hc: c.HTTP, r: rec}
	if rc.hc == nil {
		rc.hc = http.DefaultClient
	}
	c.HTTP = rc
	_, rep, err := Run(context.Background(), Options{Client: c, Filters: rec.Filters, Cluster: rec.Cluster, Days: rec.Days,
		Step: time.Duration(rec.StepS) * time.Second, Now: time.Unix(rec.Now, 0), Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d responses: %+v", len(rec.Responses), rep)
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRecordDatadog records a run against a live organization into
// testdata/live-SITE.json. Set RECORD_DATADOG=1 with DD_API_KEY, DD_APP_KEY
// and DD_SITE, and RECORD_DATADOG_SELECTOR when the organization has several
// clusters. The recording holds the organization's metric data for one day:
// cluster, namespace, workload, container and node names, and their usage.
func TestRecordDatadog(t *testing.T) {
	if os.Getenv("RECORD_DATADOG") == "" {
		t.Skip("RECORD_DATADOG is not set")
	}
	site := os.Getenv(EnvSite)
	base, err := ResolveSite(site)
	if err != nil {
		t.Fatal(err)
	}
	filters, err := ParseSelector(os.Getenv("RECORD_DATADOG_SELECTOR"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Base: base, APIKey: os.Getenv(EnvAPIKey), AppKey: os.Getenv(EnvAppKey), Log: t.Logf}
	rec := &recording{Source: base, Now: time.Now().Truncate(time.Hour).Unix(), Days: 1, StepS: 60, Filters: filters,
		Responses: map[string]response{}}
	name := strings.NewReplacer("https://", "", "http://", "", "/", "_", ":", "_").Replace(base)
	record(t, filepath.Join("testdata", "live-"+name+".json"), rec, c)
}

// TestRecordFakeOrg rewrites testdata/fake-org.json from the fake
// organization of these tests. Set UPDATE_RECORDINGS=1.
func TestRecordFakeOrg(t *testing.T) {
	if os.Getenv("UPDATE_RECORDINGS") == "" {
		t.Skip("UPDATE_RECORDINGS is not set")
	}
	f := &fakeOrg{series: append(org("prod-us", 1), org("staging", 1)...), rateLimited: 1}
	c, _ := testClient(f.server(t).URL)
	rec := &recording{Source: "the fake organization in fake_test.go", Now: testNow, Days: 1, StepS: 300,
		Filters: []string{"kube_cluster_name:prod-us"}, Responses: map[string]response{}}
	record(t, filepath.Join("testdata", "fake-org.json"), rec, c)
}

func TestReplayRecordings(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no recordings in testdata")
	}
	for _, name := range files {
		t.Run(filepath.Base(name), func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var rec recording
			if err := json.Unmarshal(b, &rec); err != nil {
				t.Fatal(err)
			}
			p := &replayer{r: &rec}
			c, _ := testClient("https://api.datadoghq.invalid")
			c.HTTP = p
			if strings.HasPrefix(rec.Source, "http") {
				c.Base = rec.Source
			}
			file, rep, err := Run(context.Background(), Options{Client: c, Filters: rec.Filters, Cluster: rec.Cluster, Days: rec.Days,
				Step: time.Duration(rec.StepS) * time.Second, Now: time.Unix(rec.Now, 0), Concurrency: 1})
			if err != nil {
				t.Fatalf("%v (requests not in the recording: %v)", err, p.missed)
			}
			validate(t, file)
			if rep.Containers == 0 || rep.Workloads == 0 {
				t.Errorf("report: %+v", rep)
			}
		})
	}
}
