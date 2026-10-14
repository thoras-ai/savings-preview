package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thoras-ai/savings-preview/internal/compact"
	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/savings"
	contractcheck "github.com/thoras-ai/savings-preview/internal/validate"
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

func record(t *testing.T, path string, rec *recording, c *Client, scrubNames bool) {
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
	if scrubNames {
		b = scrub(b)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scrubbedTags are the tags whose values name the organization's clusters,
// namespaces, workloads, containers, nodes and regions, under the names the
// Agent and the legacy kube-state-metrics check give them.
var scrubbedTags = map[string]string{
	"kube_cluster_name": "cluster", "kube_namespace": "namespace", "namespace": "namespace",
	"kube_deployment": "deployment", "deployment": "deployment",
	"kube_stateful_set": "statefulset", "statefulset": "statefulset",
	"kube_container_name": "container", "container": "container",
	"node": "node", "kube_region": "region",
}

var tagValue = regexp.MustCompile(`\b([a-z_]+):([A-Za-z0-9_.\-/]+)`)

// scrub replaces every value of the scrubbed tags, in queries, group tags and
// filters alike, with a numbered name such as namespace-2. The same value
// gets the same name everywhere, so the run replays as recorded.
func scrub(b []byte) []byte {
	values := map[string]map[string]bool{}
	for _, m := range tagValue.FindAllSubmatch(b, -1) {
		kind, ok := scrubbedTags[string(m[1])]
		if !ok || string(m[2]) == "N/A" {
			continue
		}
		if values[kind] == nil {
			values[kind] = map[string]bool{}
		}
		values[kind][string(m[2])] = true
	}
	names := map[string]map[string]string{}
	for kind, vs := range values {
		sorted := make([]string, 0, len(vs))
		for v := range vs {
			sorted = append(sorted, v)
		}
		sort.Strings(sorted)
		names[kind] = map[string]string{}
		for i, v := range sorted {
			names[kind][v] = fmt.Sprintf("%s-%d", kind, i+1)
		}
	}
	return tagValue.ReplaceAllFunc(b, func(m []byte) []byte {
		sm := tagValue.FindSubmatch(m)
		kind, ok := scrubbedTags[string(sm[1])]
		if !ok {
			return m
		}
		if n, ok := names[kind][string(sm[2])]; ok {
			return []byte(string(sm[1]) + ":" + n)
		}
		return m
	})
}

// TestRecordDatadog records a run against a live organization into
// testdata/live-SITE.json. Set RECORD_DATADOG=1 with DD_API_KEY, DD_APP_KEY
// and DD_SITE, and RECORD_DATADOG_SELECTOR when the organization has several
// clusters. The recording holds one day of the organization's usage, with
// cluster, namespace, workload, container, node and region names replaced by
// numbered ones. Values of other tags, such as a selector on env or team,
// are kept, so review the file before committing it.
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
	rec := &recording{Source: base, Now: time.Now().Truncate(time.Minute).Unix(), Days: 1, StepS: 60, Filters: filters,
		Responses: map[string]response{}}
	name := strings.NewReplacer("https://", "", "http://", "", "/", "_", ":", "_").Replace(base)
	record(t, filepath.Join("testdata", "live-"+name+".json"), rec, c, true)
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
	record(t, filepath.Join("testdata", "fake-org.json"), rec, c, false)
}

func replay(t *testing.T, rec *recording) (*contract.File, *Report) {
	t.Helper()
	p := &replayer{r: rec}
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
	return file, rep
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
			file, rep := replay(t, &rec)
			validate(t, file)
			if rep.Containers == 0 || rep.Workloads == 0 {
				t.Errorf("report: %+v", rep)
			}
			checkCompacts(t, file)
		})
	}
}

func TestScrubbedRecordingReplays(t *testing.T) {
	f := &fakeOrg{series: append(org("prod-us", 1), org("staging", 1)...)}
	c, _ := testClient(f.server(t).URL)
	rec := &recording{Source: "the fake organization in fake_test.go", Now: testNow, Days: 1, StepS: 300,
		Filters: []string{"kube_cluster_name:prod-us"}, Responses: map[string]response{}}
	path := filepath.Join(t.TempDir(), "scrubbed.json")
	record(t, path, rec, c, true)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.series {
		for k, v := range s.tags {
			if _, ok := scrubbedTags[k]; ok && bytes.Contains(b, []byte(k+":"+v)) {
				t.Errorf("recording still holds %s:%s", k, v)
			}
		}
	}
	var scrubbed recording
	if err := json.Unmarshal(b, &scrubbed); err != nil {
		t.Fatal(err)
	}
	if want := []string{"kube_cluster_name:cluster-1"}; fmt.Sprint(scrubbed.Filters) != fmt.Sprint(want) {
		t.Errorf("filters = %v, want %v", scrubbed.Filters, want)
	}
	file, rep := replay(t, &scrubbed)
	validate(t, file)
	if rep.Cluster != "cluster-1" || rep.Containers == 0 {
		t.Errorf("report: %+v", rep)
	}
	for _, r := range file.Resources {
		if r.Container != "" && !strings.HasPrefix(r.Container, "container-") {
			t.Errorf("container %q was not renamed", r.Container)
		}
	}
}

// checkCompacts writes the compact copy of f, which must follow the contract
// and count the same workloads and requests.
func checkCompacts(t *testing.T, f *contract.File) {
	t.Helper()
	var full, small bytes.Buffer
	if err := f.Write(&full); err != nil {
		t.Fatal(err)
	}
	st, err := compact.Compact(bytes.NewReader(full.Bytes()), &small, compact.DefaultStep)
	if err != nil {
		t.Fatal(err)
	}
	if st.Compacted == 0 {
		t.Fatalf("nothing compacted: %+v", st)
	}
	if errs := contractcheck.Validate(small.Bytes()); len(errs) > 0 {
		t.Fatalf("compact file: %v", errs)
	}
	a, err := savings.Analyze(full.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	b, err := savings.Analyze(small.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// A workload counts only while it runs, and the compact file keeps when it
	// runs to the 15-minute step: one that runs for part of a step counts for
	// the whole step, and so does the cluster's first and last step. In the
	// 35-minute live recording two workloads run for 3 minutes, which the
	// compact file counts as one of its three steps, so its requests come out
	// 3.6% higher. Workloads that run the whole time match exactly.
	if a.Totals.Counted != b.Totals.Counted || math.Abs(a.Totals.RequestedCores-b.Totals.RequestedCores) > 0.05*a.Totals.RequestedCores {
		t.Errorf("full %+v, compact %+v", a.Totals, b.Totals)
	}
	t.Logf("%d to %d bytes; savings %.2f full, %.2f compact", full.Len(), small.Len(), a.Totals.AnnualSavings, b.Totals.AnnualSavings)
}
