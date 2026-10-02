package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// recording holds the responses of a live store to one Run, keyed by path and
// encoded parameters, so the run can be replayed without the store.
type recording struct {
	Store     string              `json:"store"`
	Backend   string              `json:"backend,omitempty"`  // as ResolveBackend names it; empty for Mimir
	Cluster   string              `json:"cluster,omitempty"`  // empty means "minikube"
	Selector  string              `json:"selector,omitempty"` // as ParseSelector returns it
	Now       int64               `json:"now"`
	Days      int                 `json:"days"`
	StepS     int                 `json:"step_seconds"`
	Headers   map[string]string   `json:"headers"` // non-secret headers the store required
	Responses map[string]response `json:"responses"`
}

type response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func recordKey(path string, params url.Values) string { return path + "?" + params.Encode() }

type recorder struct {
	g  Getter
	mu sync.Mutex
	r  *recording
}

func (r *recorder) Get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	body, status, err := r.g.Get(ctx, path, params)
	if err == nil {
		r.mu.Lock()
		if !json.Valid(body) {
			body, _ = json.Marshal(string(body))
		}
		r.r.Responses[recordKey(path, params)] = response{Status: status, Body: body}
		r.mu.Unlock()
	}
	return body, status, err
}

type replayer struct {
	r      *recording
	mu     sync.Mutex
	missed []string
}

func (p *replayer) Get(_ context.Context, path string, params url.Values) ([]byte, int, error) {
	k := recordKey(path, params)
	resp, ok := p.r.Responses[k]
	if !ok {
		p.mu.Lock()
		p.missed = append(p.missed, k)
		p.mu.Unlock()
		return nil, 0, fmt.Errorf("no recorded response for %s", k)
	}
	return resp.Body, resp.Status, nil
}

// TestRecordMimir records a run against a live Mimir. Set
// RECORD_MIMIR_URL (the base of the query API) and RECORD_MIMIR_TENANT.
func TestRecordMimir(t *testing.T) {
	base, tenant := os.Getenv("RECORD_MIMIR_URL"), os.Getenv("RECORD_MIMIR_TENANT")
	if base == "" {
		t.Skip("RECORD_MIMIR_URL is not set")
	}
	rec := &recording{Store: "mimir", Now: time.Now().Truncate(time.Minute).Unix(), Days: 1, StepS: 60,
		Headers: map[string]string{TenantHeader: tenant}, Responses: map[string]response{}}
	r := &recorder{g: &HTTPGetter{Base: base, Auth: Header{Name: TenantHeader, Value: tenant}}, r: rec}
	_, _, err := Run(context.Background(), Options{Client: &Client{G: r, Retries: 1}, Cluster: "minikube",
		Days: rec.Days, Step: time.Duration(rec.StepS) * time.Second, Now: time.Unix(rec.Now, 0)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/mimir.json", append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadRecording(t *testing.T, name string) *recording {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var r recording
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestReplayMimir(t *testing.T) {
	rec := loadRecording(t, "testdata/mimir.json")
	p := &replayer{r: rec}
	file, rep, err := Run(context.Background(), Options{Client: &Client{G: p, Retries: 1}, Cluster: "minikube",
		Days: rec.Days, Step: time.Duration(rec.StepS) * time.Second, Now: time.Unix(rec.Now, 0), Concurrency: 1})
	if err != nil {
		t.Fatalf("%v (requests not in the recording: %v)", err, p.missed)
	}
	validate(t, file)
	if !rep.UsageFromPods {
		t.Error("the recorded minikube store has no per-container usage labels")
	}
	if rep.Containers == 0 || rep.Workloads == 0 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	if find(file, "sp-mimir-test", Deployment, "demo-deploy", "sleeper") == nil {
		t.Error("the demo deployment is missing")
	}
}
