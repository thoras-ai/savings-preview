package prometheus

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

const gmpPrefix = "/v1/projects/shop-prod/location/global/prometheus"

// gkeOpts shape the fake Cloud Monitoring project.
type gkeOpts struct {
	prometheus bool     // Managed Prometheus also holds cAdvisor and kube-state-metrics series
	noRequests bool     // no container request metrics
	noReplicas bool     // managed kube-state-metrics is off
	noOwner    bool     // system metrics lack the top-level controller labels
	clusters   []string // cluster_name values; one cluster when empty
}

// gkeStore answers like the Cloud Monitoring PromQL API for a project holding
// three days of GKE system metrics for one deployment, one statefulset and
// one node. It does not serve buildinfo, as the real API does not.
func gkeStore(o gkeOpts) func(string, url.Values) (int, any) {
	first := int64(t0) - 3*86400
	prom := clusterStore(true, true, true)
	clusters := o.clusters
	if len(clusters) == 0 {
		clusters = []string{"shop-prod-1"}
	}
	return func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		if path == "/api/v1/status/buildinfo" {
			return 404, map[string]any{"status": "error", "errorType": "not_found", "error": "not found"}
		}
		if path == "/api/v1/query" {
			switch query {
			case "vector(1)":
				return 200, vector(1)
			case UsageProbe, PodUsageProbe, OwnerProbe:
				if o.prometheus {
					return 200, vector(1)
				}
				return 200, empty()
			case ExportedNamespaceProbe:
				return 200, empty()
			case GKERequestsProbe:
				if o.noRequests {
					return 200, empty()
				}
			case ReplicasProbe:
				if o.noReplicas {
					return 200, empty()
				}
			case GKEControllerProbe:
				if o.noOwner {
					return 200, empty()
				}
			}
			if strings.Contains(query, "prometheus_tsdb_lowest") {
				return 200, empty()
			}
			if strings.HasPrefix(query, "count by (cluster_name, location)") {
				var res []any
				for _, c := range clusters {
					res = append(res, map[string]any{"metric": map[string]string{"cluster_name": c, "location": "us-central1-a"}, "value": []any{1.0, "1"}})
				}
				return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}}
			}
			if o.prometheus && !strings.Contains(query, "kubernetes_io:") {
				return prom(path, q)
			}
			if atoi(q.Get("time")) < first {
				return 200, empty()
			}
			return 200, vector(1)
		}
		if o.prometheus && !strings.Contains(query, "kubernetes_io:") && !strings.Contains(query, "exported_namespace") {
			return prom(path, q)
		}
		s, e, step := atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step"))
		if s < first {
			s = first
		}
		wl := "web"
		if strings.Contains(query, `"StatefulSet"`) || strings.Contains(query, "kube_statefulset_replicas") {
			wl = "db"
		}
		c := map[string]string{"namespace_name": "shop", gkeController: wl, "container_name": "app"}
		series := func(l map[string]string, v float64) (int, any) {
			return 200, matrix(l, s, e, step, func(int64) float64 { return v })
		}
		switch {
		case strings.Contains(query, gkeNodeCPU):
			return series(map[string]string{"node_name": "gke-shop-pool-1-abcd", "location": "us-central1-a"}, 3.92)
		case strings.Contains(query, gkeNodeMem):
			return series(map[string]string{"node_name": "gke-shop-pool-1-abcd", "location": "us-central1-a"}, 13*1024*1024*1024)
		case strings.Contains(query, gkeCPU):
			return series(c, 0.25)
		case strings.Contains(query, gkeMemory):
			return series(c, 100*1024*1024)
		case strings.Contains(query, gkeCPUReq):
			return series(c, 0.5)
		case strings.Contains(query, gkeMemoryReq):
			return series(c, 256*1024*1024)
		case strings.Contains(query, gkeCPULim) && wl == "web":
			return series(c, 1)
		case strings.Contains(query, "_replicas{"):
			return series(map[string]string{"namespace": "shop", "workload": wl}, 3)
		}
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}}
	}
}

func runGKEStore(t *testing.T, f *fakeStore, mode, selector string) (*contract.File, *Report, error) {
	t.Helper()
	c := f.client(t)
	c.Selector = selector
	return Run(context.Background(), Options{Client: c, Cluster: "shop-prod-1", Days: 7, Step: 5 * time.Minute,
		Now: time.Unix(t0, 0), GKE: mode})
}

func TestGKESystemMetrics(t *testing.T) {
	f := &fakeStore{handle: gkeStore(gkeOpts{})}
	file, rep, err := runGKEStore(t, f, GKEAuto, "")
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if !rep.GKESystemMetrics || rep.Containers != 2 || rep.Workloads != 2 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	if want := time.Unix(t0, 0).Add(-GKERequestLag); !rep.End.Equal(want) {
		t.Errorf("the pull must end %s before now: end %s", GKERequestLag, rep.End)
	}
	web := find(file, "shop", Deployment, "web", "app")
	if web == nil {
		t.Fatal("no web/app resource")
	}
	if web.Source != "gke" {
		t.Errorf("source %q", web.Source)
	}
	for name, want := range map[string]float64{contract.CPUUsage: 0.25, contract.MemoryWorkingSet: 100 * 1024 * 1024,
		contract.CPURequest: 0.5, contract.MemoryRequest: 256 * 1024 * 1024, contract.CPULimit: 1} {
		if got := series(web, name); len(got) == 0 || got[0].V != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if got := series(web, contract.MemoryLimit); len(got) != 0 {
		t.Error("a limit that is not set must be left out")
	}
	if wl := find(file, "shop", StatefulSet, "db", ""); wl == nil || len(series(wl, contract.StatefulSetPods)) == 0 {
		t.Errorf("statefulset replicas: %+v", wl)
	}
	for _, r := range file.Resources {
		if r.Node != nil && (r.Node.Name != "gke-shop-pool-1-abcd" || r.Node.Provider != "gcp" || r.Node.Region != "us-central1" || r.Node.InstanceType != "unknown") {
			t.Errorf("node attributes: %+v", r.Node)
		}
	}
	for _, q := range f.requests {
		query := q.Get("query")
		if strings.Contains(query, "kubernetes_io:container_memory_used_bytes{") && !strings.Contains(query, gkeWorkingSet) {
			t.Errorf("memory usage must read the non-evictable series: %s", query)
		}
		if strings.Contains(query, "_replicas{") && !strings.Contains(query, "exported_namespace") && !strings.HasPrefix(query, "count(") {
			t.Errorf("replicas must take exported_namespace when present: %s", query)
		}
	}
}

func TestGKESelectorUsesEachMetricsClusterLabel(t *testing.T) {
	for _, sel := range []string{`cluster="shop-prod-1"`, `cluster_name="shop-prod-1"`} {
		f := &fakeStore{handle: gkeStore(gkeOpts{})}
		if _, _, err := runGKEStore(t, f, GKESystem, sel); err != nil {
			t.Fatal(err)
		}
		for _, q := range f.requests {
			query := q.Get("query")
			switch {
			case query == "vector(1)":
			case strings.Contains(query, "kubernetes_io:"):
				if !strings.Contains(query, `cluster_name="shop-prod-1"`) || strings.Contains(query, `{cluster=`) {
					t.Errorf("%s: system metrics query: %s", sel, query)
				}
			case strings.Contains(query, "kube_"):
				if !strings.Contains(query, `{cluster="shop-prod-1"`) {
					t.Errorf("%s: kube-state-metrics query: %s", sel, query)
				}
			}
		}
	}
}

func TestGKEWithoutReplicasOrRequests(t *testing.T) {
	f := &fakeStore{handle: gkeStore(gkeOpts{noReplicas: true, noRequests: true})}
	file, rep, err := runGKEStore(t, f, GKEAuto, "")
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	wl := find(file, "shop", Deployment, "web", "")
	if wl == nil || len(wl.Missing) != 1 || wl.Missing[0] != contract.DeploymentPods {
		t.Errorf("replicas must be declared missing: %+v", wl)
	}
	web := find(file, "shop", Deployment, "web", "app")
	if web == nil || len(web.Missing) != 2 {
		t.Errorf("requests must be declared missing: %+v", web)
	}
	joined := strings.Join(rep.Warnings, "\n")
	if !strings.Contains(joined, "managed kube-state-metrics") || !strings.Contains(joined, gkeCPUReq) {
		t.Errorf("warnings: %v", rep.Warnings)
	}
	for _, q := range f.requests {
		if strings.Contains(q.Get("query"), "_replicas{") && q.Get("start") != "" {
			t.Errorf("replicas pulled though absent: %s", q.Get("query"))
		}
	}
}

func TestGKEAutoPrefersCompleteManagedPrometheus(t *testing.T) {
	f := &fakeStore{handle: gkeStore(gkeOpts{prometheus: true})}
	file, rep, err := runGKEStore(t, f, GKEAuto, "")
	if err != nil {
		t.Fatal(err)
	}
	validate(t, file)
	if rep.GKESystemMetrics || file.Resources[0].Source != "prometheus" {
		t.Errorf("auto must read Managed Prometheus series when they are complete: %+v", rep)
	}
	f = &fakeStore{handle: gkeStore(gkeOpts{prometheus: true})}
	if _, rep, err = runGKEStore(t, f, GKESystem, ""); err != nil || !rep.GKESystemMetrics {
		t.Errorf("--gke-metrics system: %+v, %v", rep, err)
	}
}

func TestGKEManagedPrometheusExportedNamespace(t *testing.T) {
	h := gkeStore(gkeOpts{prometheus: true})
	f := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if q.Get("query") == ExportedNamespaceProbe {
			return 200, vector(12)
		}
		return h(path, q)
	}}
	_, _, err := runGKEStore(t, f, GKEPrometheus, "")
	if err == nil || !strings.Contains(err.Error(), "exported_namespace") || !strings.Contains(err.Error(), "--gke-metrics system") {
		t.Errorf("got %v", err)
	}
}

func TestGKEErrors(t *testing.T) {
	_, _, err := runGKEStore(t, &fakeStore{handle: gkeStore(gkeOpts{clusters: []string{"a", "b"}})}, GKEAuto, "")
	var ce *ClusterError
	if !errors.As(err, &ce) || ce.Label != "cluster_name" || !strings.Contains(err.Error(), `--selector 'cluster_name="a"'`) {
		t.Errorf("two clusters: %v", err)
	}
	_, _, err = runGKEStore(t, &fakeStore{handle: gkeStore(gkeOpts{noOwner: true})}, GKEAuto, "")
	if err == nil || !strings.Contains(err.Error(), gkeController) {
		t.Errorf("no controller labels: %v", err)
	}
	none := &fakeStore{handle: func(path string, q url.Values) (int, any) {
		if q.Get("query") == "vector(1)" {
			return 200, vector(1)
		}
		return 200, empty()
	}}
	_, _, err = runGKEStore(t, none, GKEAuto, "")
	if err == nil || !strings.Contains(err.Error(), "neither GKE system metrics") {
		t.Errorf("empty project: %v", err)
	}
	_, _, err = runGKEStore(t, none, GKESystem, "")
	if err == nil || !strings.Contains(err.Error(), "no GKE system metrics") {
		t.Errorf("empty project, system: %v", err)
	}
}

func TestDownsampledWarning(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		span, step time.Duration
		warn       bool
	}{
		{14 * day, time.Minute, false},
		{42 * day, time.Minute, false},
		{60 * day, time.Minute, true},
		{60 * day, 10 * time.Minute, false},
	} {
		w := downsampledWarning(c.span, c.step)
		if (w != "") != c.warn {
			t.Errorf("%s at %s: %q", c.span, c.step, w)
		}
		if w != "" && !strings.Contains(w, "--step 10m") {
			t.Errorf("warning: %q", w)
		}
	}
}

func TestGKEClientRenamesLabels(t *testing.T) {
	c := &Client{Selector: `cluster="a",namespace_name="shop",pod=~"web-.*",container_name="app",location="us-central1"`}
	if got, want := gkeClient(c, "cluster_name").Selector,
		`cluster_name="a",namespace_name="shop",pod_name=~"web-.*",container_name="app",location="us-central1"`; got != want {
		t.Errorf("system: %s", got)
	}
	if got, want := gkeClient(c, "cluster").Selector,
		`cluster="a",namespace="shop",pod=~"web-.*",container="app",location="us-central1"`; got != want {
		t.Errorf("prometheus: %s", got)
	}
}

func TestRenameMatchers(t *testing.T) {
	sel, err := ParseSelector(`cluster="a,b", location=~"us-.*", cluster_name!="c"`)
	if err != nil {
		t.Fatal(err)
	}
	got := renameMatchers(sel, map[string]string{"cluster": "cluster_name", "cluster_name": "cluster_name"})
	if want := `cluster_name="a,b",location=~"us-.*",cluster_name!="c"`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestResolveBackendGMP(t *testing.T) {
	base := "https://monitoring.googleapis.com" + gmpPrefix
	for _, u := range []string{base, base + "/", base + "/api/v1/query_range", base + "/api/v1/query?query=up"} {
		b, err := ResolveBackend(u, "", false)
		if err != nil || b != (Backend{Name: BackendGMP, Base: base, Project: "shop-prod"}) {
			t.Errorf("%s: %+v, %v", u, b, err)
		}
	}
	for _, u := range []string{"https://monitoring.googleapis.com/", "https://monitoring.googleapis.com/v1/projects/p/location/us/prometheus"} {
		if _, err := ResolveBackend(u, "", false); err == nil || !strings.Contains(err.Error(), "--project") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if u, err := GMPURL("shop-prod"); err != nil || u != base {
		t.Errorf("GMPURL: %s, %v", u, err)
	}
	for _, p := range []string{"", "Shop", "shop/prod", "shop-"} {
		if _, err := GMPURL(p); err == nil {
			t.Errorf("project %q accepted", p)
		}
	}
}

func TestGoogleAuth(t *testing.T) {
	var mu sync.Mutex
	var authorized int
	check := func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") != "Bearer ya29.test-token" {
			return http.StatusUnauthorized, `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`
		}
		mu.Lock()
		authorized++
		mu.Unlock()
		return 0, ""
	}
	srv := backendServer(t, gmpPrefix, check, gkeStore(gkeOpts{}))
	b, err := ResolveBackend(srv.URL+gmpPrefix, BackendGMP, false)
	if err != nil {
		t.Fatal(err)
	}
	run := func(token string) (*Report, error) {
		g := &Google{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}
		_, rep, err := Run(context.Background(), Options{Client: &Client{G: &HTTPGetter{Base: b.Base, Auth: g}, Retries: 1, sleep: noSleep},
			Cluster: "shop-prod-1", Days: 7, Step: 5 * time.Minute, Now: time.Unix(t0, 0), GKE: GKEAuto})
		return rep, err
	}
	rep, err := run("ya29.test-token")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Containers != 2 || authorized == 0 {
		t.Errorf("report %+v, %d authorized requests", rep, authorized)
	}
	_, err = run("expired")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || strings.Contains(err.Error(), "expired") {
		t.Errorf("bad token: %v", err)
	}
}

func TestNewGoogleKeyFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	write := func(v map[string]string) string {
		b, _ := json.Marshal(v)
		p := filepath.Join(t.TempDir(), "key.json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sa := write(map[string]string{"type": "service_account", "project_id": "shop-prod", "private_key_id": "k1",
		"private_key": string(pemKey), "client_email": "reader@shop-prod.iam.gserviceaccount.com", "token_uri": "https://oauth2.googleapis.com/token"})
	if g, err := NewGoogle(context.Background(), sa); err != nil || g.Source == nil {
		t.Errorf("service account key: %v", err)
	}
	user := write(map[string]string{"type": "authorized_user", "client_id": "x", "client_secret": "y", "refresh_token": "z"})
	if _, err := NewGoogle(context.Background(), user); err == nil || !strings.Contains(err.Error(), "service_account") {
		t.Errorf("a key file must be a service account key: %v", err)
	}
}
