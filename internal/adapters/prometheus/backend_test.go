package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestResolveBackend(t *testing.T) {
	for _, c := range []struct {
		url, name string
		tenant    bool
		want      Backend
		err       string
	}{
		{url: "http://prometheus.example:9090", want: Backend{Name: BackendPrometheus, Base: "http://prometheus.example:9090"}},
		{url: "http://mimir.example:8080", tenant: true, want: Backend{Name: BackendMimir, Base: "http://mimir.example:8080/prometheus"}},
		{url: "http://mimir.example:8080/", name: BackendMimir, want: Backend{Name: BackendMimir, Base: "http://mimir.example:8080/prometheus"}},
		{url: "https://gw.example/api/v1/push", name: BackendMimir, want: Backend{Name: BackendMimir, Base: "https://gw.example/prometheus"}},
		{url: "https://gw.example/api/prom", name: BackendMimir, want: Backend{Name: BackendMimir, Base: "https://gw.example/api/prom"}},
		{url: "https://mimir.example/prometheus/api/v1/query", tenant: true, want: Backend{Name: BackendMimir, Base: "https://mimir.example/prometheus"}},
		{url: "https://prometheus-prod-13-prod-us-east-0.grafana.net", want: Backend{Name: BackendGrafanaCloud,
			Base: "https://prometheus-prod-13-prod-us-east-0.grafana.net/api/prom", MaxRange: GrafanaCloudMaxRange}},
		{url: "https://prometheus-prod-13-prod-us-east-0.grafana.net/api/prom/push", want: Backend{Name: BackendGrafanaCloud,
			Base: "https://prometheus-prod-13-prod-us-east-0.grafana.net/api/prom", MaxRange: GrafanaCloudMaxRange}},
		{url: "https://aps-workspaces.eu-west-1.amazonaws.com/workspaces/ws-0a1b2c3d/api/v1/query", want: Backend{Name: BackendAMP,
			Base: "https://aps-workspaces.eu-west-1.amazonaws.com/workspaces/ws-0a1b2c3d", Region: "eu-west-1", MaxRange: AMPMaxRange}},
		{url: "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1/api/v1/remote_write", want: Backend{Name: BackendAMP,
			Base: "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1", Region: "us-east-1", MaxRange: AMPMaxRange}},
		{url: "https://vpce-0abc-1def.aps-workspaces.us-east-2.vpce.amazonaws.com/workspaces/ws-1", want: Backend{Name: BackendAMP,
			Base: "https://vpce-0abc-1def.aps-workspaces.us-east-2.vpce.amazonaws.com/workspaces/ws-1", Region: "us-east-2", MaxRange: AMPMaxRange}},
		{url: "https://amp-proxy.internal/workspaces/ws-1", name: BackendAMP, want: Backend{Name: BackendAMP,
			Base: "https://amp-proxy.internal/workspaces/ws-1", MaxRange: AMPMaxRange}},
		{url: "https://aps-workspaces.us-east-1.amazonaws.com/", err: "not an Amazon Managed Prometheus workspace URL"},
		{url: "https://user:secret@prom.example/?token=abc", want: Backend{Name: BackendPrometheus, Base: "https://user:secret@prom.example"}},
		{url: "prom.example:9090", err: "not an http or https URL"},
		{url: "http://prom.example", name: "cortex", err: `unknown backend "cortex"`},
	} {
		got, err := ResolveBackend(c.url, c.name, c.tenant)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %v, want error %q", c.url, err, c.err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("%s: error shows a credential: %v", c.url, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s %q: got %+v, %v; want %+v", c.url, c.name, got, err, c.want)
		}
	}
}

var rangeSelector = regexp.MustCompile(`\[(\d+)s\]`)

// limitedStore rejects queries longer than limit the way Mimir and Grafana
// Cloud do, counting range selectors and the range of a range query.
func limitedStore(limit time.Duration, h func(string, url.Values) (int, any)) func(string, url.Values) (int, any) {
	return func(path string, q url.Values) (int, any) {
		length := time.Duration(0)
		for _, m := range rangeSelector.FindAllStringSubmatch(q.Get("query"), -1) {
			n, _ := strconv.Atoi(m[1])
			length = max(length, time.Duration(n)*time.Second)
		}
		if path == "/api/v1/query_range" {
			length += time.Duration(atoi(q.Get("end"))-atoi(q.Get("start"))) * time.Second
		}
		if length > limit {
			return 422, map[string]any{"status": "error", "errorType": "execution",
				"error": fmt.Sprintf("the query time range exceeds the limit (query length: %s, limit: %s) (err-mimir-max-query-length)", length, limit)}
		}
		return h(path, q)
	}
}

func TestMaxRangeSplitsLongWindows(t *testing.T) {
	f := &fakeStore{handle: limitedStore(GrafanaCloudMaxRange, clusterStore(true, true, true))}
	run := func(c *Client) (*Report, error) {
		_, rep, err := Run(context.Background(), Options{Client: c, Cluster: "test", Days: 90, Step: 5 * time.Minute, Now: time.Unix(t0, 0)})
		return rep, err
	}
	if _, err := run(f.client(t)); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("without MaxRange: got %v", err)
	}
	f.requests = nil
	c := f.client(t)
	c.MaxRange = GrafanaCloudMaxRange
	rep, err := run(c)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Nodes != 1 {
		t.Errorf("node info from the split window: %+v", rep)
	}
	var nodeInfo int
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), "last_over_time(kube_node_info") {
			nodeInfo++
		}
	}
	// 90 days in pieces of 31 days 23 hours, for the cluster check and the node attributes.
	if nodeInfo != 6 {
		t.Errorf("want 3 pieces twice, got %d node info queries", nodeInfo)
	}
}

func TestQueryWindowKeepsNewestSample(t *testing.T) {
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		v := strconv.FormatInt(atoi(q.Get("time")), 10)
		res := []any{map[string]any{"metric": map[string]string{"node": "n1"}, "value": []any{1.0, v}}}
		if atoi(q.Get("time")) < t0 {
			res = append(res, map[string]any{"metric": map[string]string{"node": "old"}, "value": []any{1.0, v}})
		}
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}}
	}}
	c := f.client(t)
	c.MaxRange = 10 * 24 * time.Hour
	ss, err := c.queryWindow(context.Background(), func(w time.Duration) string { return fmt.Sprintf("last_over_time(kube_node_info[%ds])", seconds(w)) },
		time.Unix(t0, 0), 25*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 3 || len(ss) != 2 || ss[0].Labels["node"] != "n1" || ss[0].Points[0].V != t0 {
		t.Fatalf("%d requests, series %+v", len(f.requests), ss)
	}
	last := f.requests[2]
	if w := rangeSelector.FindStringSubmatch(last.Get("query"))[1]; w != strconv.Itoa(seconds(25*24*time.Hour-2*(10*24*time.Hour-time.Hour))) {
		t.Errorf("last piece covers the rest of the window, got %ss", w)
	}
}

func TestMissingLabels(t *testing.T) {
	// kube_pod_owner aggregated without owner_kind.
	h := func(path string, q url.Values) (int, any) {
		switch q.Get("query") {
		case `count(kube_pod_owner{namespace!="",pod!="",owner_kind!="",owner_name!=""})`, `count(kube_pod_owner{owner_kind!=""})`:
			return 200, empty()
		}
		return clusterStore(true, true, true)(path, q)
	}
	_, _, err := runAgainst(t, &fakeStore{handle: h})
	var le *LabelsMissingError
	if !errors.As(err, &le) || le.Metric != "kube_pod_owner" || strings.Join(le.Labels, ",") != "owner_kind" || le.Aggregations {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "relabeling rule") {
		t.Errorf("message: %v", err)
	}
}

// grafanaCloudStore answers like a Grafana Cloud stack whose Adaptive Metrics
// rule aggregated the cAdvisor metrics without the container and image
// labels: every series has namespace and pod, none has container.
func grafanaCloudStore(h func(string, url.Values) (int, any)) func(string, url.Values) (int, any) {
	return func(path string, q url.Values) (int, any) {
		switch q.Get("query") {
		case UsageProbe, `count(container_memory_working_set_bytes{container!="",image!=""})`,
			`count(container_memory_working_set_bytes{container!=""})`, `count(container_memory_working_set_bytes{image!=""})`:
			return 200, empty()
		}
		return h(path, q)
	}
}

func TestAdaptiveMetricsDroppedContainerLabel(t *testing.T) {
	f := &fakeStore{handle: grafanaCloudStore(clusterStore(true, true, true))}
	_, _, err := Run(context.Background(), Options{Client: f.client(t), Cluster: "test", Days: 7, Step: 5 * time.Minute,
		Now: time.Unix(t0, 0), AdaptiveMetrics: true})
	var le *LabelsMissingError
	if !errors.As(err, &le) || le.Metric != "container_memory_working_set_bytes" || strings.Join(le.Labels, ",") != "container,image" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "Adaptive Metrics") {
		t.Errorf("message: %v", err)
	}
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), podCgroup) && q.Get("start") != "" {
			t.Fatal("usage must not be split from pod series on Grafana Cloud")
		}
	}
}

// backendServer serves a fake store under prefix, after check accepts the request.
func backendServer(t *testing.T, prefix string, check func(*http.Request) (int, string), h func(string, url.Values) (int, any)) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, body := check(r); status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix+"/api/v1/") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "404 page not found")
			return
		}
		status, body := h(strings.TrimPrefix(r.URL.Path, prefix), r.URL.Query())
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runBackend(t *testing.T, b Backend, auth Auth) (*Report, error) {
	_, rep, err := Run(context.Background(), Options{Client: &Client{G: &HTTPGetter{Base: b.Base, Auth: auth}, Retries: 1, MaxRange: b.MaxRange},
		Cluster: "test", Days: 40, Step: 5 * time.Minute, Now: time.Unix(t0, 0), AdaptiveMetrics: b.Name == BackendGrafanaCloud})
	return rep, err
}

func TestGrafanaCloud(t *testing.T) {
	check := func(r *http.Request) (int, string) {
		if u, p, ok := r.BasicAuth(); !ok || u != "1234567" || p != "glc_secret" {
			return http.StatusUnauthorized, `{"status":"error","error":"authentication error: invalid token"}`
		}
		return 0, ""
	}
	srv := backendServer(t, "/api/prom", check, limitedStore(GrafanaCloudMaxRange, clusterStore(true, true, true)))
	b, err := ResolveBackend(srv.URL, BackendGrafanaCloud, false)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := runBackend(t, b, Basic{User: "1234567", Password: "glc_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Containers != 2 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	_, err = runBackend(t, b, Basic{User: "1234567", Password: "wrong"})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(ae.Message, "invalid token") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad token: %v", err)
	}
}

func TestAmazonManagedPrometheus(t *testing.T) {
	const akid, secret = "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	var mu sync.Mutex
	var signed int
	check := func(r *http.Request) (int, string) {
		if err := verifySigV4(r, akid, secret, "us-east-1", "aps"); err != nil {
			return http.StatusForbidden, `{"message":"The request signature we calculated does not match the signature you provided."}`
		}
		mu.Lock()
		signed++
		mu.Unlock()
		return 0, ""
	}
	srv := backendServer(t, "/workspaces/ws-0a1b2c3d", check, limitedStore(AMPMaxRange, clusterStore(true, true, true)))
	b, err := ResolveBackend(srv.URL+"/workspaces/ws-0a1b2c3d/api/v1/query", BackendAMP, false)
	if err != nil {
		t.Fatal(err)
	}
	b.Region = "us-east-1"
	rep, err := runBackend(t, b, &SigV4{Region: b.Region, Credentials: credentials.NewStaticCredentialsProvider(akid, secret, "")})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Containers != 2 || signed == 0 {
		t.Errorf("report %+v, %d signed requests", rep, signed)
	}
	_, err = runBackend(t, b, &SigV4{Region: b.Region, Credentials: credentials.NewStaticCredentialsProvider(akid, "other", "")})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 || !strings.Contains(ae.Message, "signature") {
		t.Errorf("bad signature: %v", err)
	}
}
