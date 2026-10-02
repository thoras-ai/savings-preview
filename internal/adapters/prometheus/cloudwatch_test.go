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
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
)

// cloudWatchFake answers like the CloudWatch PromQL endpoint in front of
// handle: it wants requests signed for the monitoring service, rejects a
// query whose range and range selectors span more than seven days, and
// returns at most 500 series with a warning, as the endpoint documents.
type cloudWatchFake struct {
	handle func(path string, q url.Values) (int, any)

	mu        sync.Mutex
	queries   []url.Values
	unsigned  int
	truncated int
}

var rangeSel = regexp.MustCompile(`\[(\d+)s\]`)

func (f *cloudWatchFake) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.queries = append(f.queries, q)
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/monitoring/aws4_request") {
			f.unsigned++
		}
		f.mu.Unlock()
		reply := func(status int, body any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		}
		var span int64
		for _, m := range rangeSel.FindAllStringSubmatch(q.Get("query"), -1) {
			span = max(span, atoi(m[1]))
		}
		span += 300 // lookback
		if r.URL.Path == "/api/v1/query_range" {
			span += atoi(q.Get("end")) - atoi(q.Get("start"))
		}
		if span > 7*86400 {
			reply(422, map[string]any{"status": "error", "errorType": "execution", "error": "query time range exceeds the limit of 7 days"})
			return
		}
		status, body := f.handle(r.URL.Path, q)
		if m, ok := body.(map[string]any); ok {
			if d, ok := m["data"].(map[string]any); ok {
				if res, ok := d["result"].([]any); ok && len(res) > CloudWatchMaxSeries {
					d["result"] = res[:CloudWatchMaxSeries]
					m["warnings"] = []string{"query result truncated to 500 series"}
					f.mu.Lock()
					f.truncated++
					f.mu.Unlock()
				}
			}
		}
		reply(status, body)
	}))
	t.Cleanup(srv.Close)
	sig := &SigV4{Region: "us-east-1", Service: SigV4CloudWatch,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""), Now: func() time.Time { return time.Unix(t0, 0) }}
	b := Backend{Name: BackendCloudWatch, MaxRange: CloudWatchMaxRange, MaxSeries: CloudWatchMaxSeries}
	c := b.NewClient(&HTTPGetter{Base: srv.URL, Auth: sig}, "")
	c.Retries, c.sleep = 1, noSleep
	return c
}

func runCloudWatch(t *testing.T, c *Client) (*Report, error) {
	t.Helper()
	f, rep, err := Run(context.Background(), Options{Client: c, Cluster: "prod", Days: 14, Step: 5 * time.Minute,
		Now: time.Unix(t0, 0), CloudWatch: "us-east-1"})
	if err == nil {
		validate(t, f)
	}
	return rep, err
}

func TestCloudWatchRun(t *testing.T) {
	cw := &cloudWatchFake{handle: clusterStore(true, true, true)}
	rep, err := runCloudWatch(t, cw.client(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Containers != 2 || rep.Workloads != 2 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	if cw.unsigned > 0 {
		t.Errorf("%d requests were not signed for the monitoring service", cw.unsigned)
	}
	clusterQuery := false
	for _, q := range cw.queries {
		if strings.Contains(q.Get("query"), `"@resource.k8s.cluster.name", "@aws.account")`) {
			clusterQuery = true
		}
	}
	if !clusterQuery {
		t.Error("the cluster check does not count series by the CloudWatch cluster labels")
	}
}

func TestCloudWatchWithoutContainerInsights(t *testing.T) {
	h := clusterStore(true, true, true)
	without := func(path string, q url.Values) (int, any) {
		if strings.Contains(q.Get("query"), "container_memory_working_set_bytes") {
			return 200, empty()
		}
		return h(path, q)
	}
	_, err := runCloudWatch(t, (&cloudWatchFake{handle: without}).client(t))
	var ae *AddOnMissingError
	if !errors.As(err, &ae) || len(ae.Clusters) != 0 {
		t.Fatalf("want an AddOnMissingError, got %v", err)
	}
	for _, want := range []string{"us-east-1", "amazon-cloudwatch-observability", "6.2.0", "otelContainerInsights.enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %s: %v", want, err)
		}
	}
}

func TestCloudWatchSelectorMatchingNoCluster(t *testing.T) {
	h := clusterStore(true, true, true)
	cw := &cloudWatchFake{handle: func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		switch {
		case strings.HasPrefix(query, `count by ("@resource.k8s.cluster.name") (container_memory_working_set_bytes{})`):
			return 200, groups(map[string]string{CloudWatchClusterLabel: "prod-b"}, map[string]string{CloudWatchClusterLabel: "prod-a"})
		case strings.Contains(query, `"@resource.k8s.cluster.name"="prod"`):
			return 200, empty()
		}
		return h(path, q)
	}}
	c := cw.client(t)
	sel, err := ParseSelector(`@resource.k8s.cluster.name=prod`)
	if err != nil {
		t.Fatal(err)
	}
	c.Selector = sel
	_, err = runCloudWatch(t, c)
	var ae *AddOnMissingError
	if !errors.As(err, &ae) || strings.Join(ae.Clusters, ",") != "prod-a,prod-b" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), `--selector '"@resource.k8s.cluster.name"="prod-a"'`) {
		t.Errorf("the suggestion must name a cluster: %v", err)
	}
}

func TestCloudWatchSeveralClustersStop(t *testing.T) {
	h := clusterStore(true, true, true)
	cw := &cloudWatchFake{handle: func(path string, q url.Values) (int, any) {
		if strings.HasPrefix(q.Get("query"), "count by (cluster, ") {
			return 200, groups(map[string]string{CloudWatchClusterLabel: "prod", "@aws.account": "111122223333"},
				map[string]string{CloudWatchClusterLabel: "staging", "@aws.account": "111122223333"})
		}
		return h(path, q)
	}}
	_, err := runCloudWatch(t, cw.client(t))
	var ce *ClusterError
	if !errors.As(err, &ce) || ce.Label != CloudWatchClusterLabel {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), `--selector '"@resource.k8s.cluster.name"="prod"'`) {
		t.Errorf("the suggestion must quote the label: %v", err)
	}
}

var nsMatcher = regexp.MustCompile(`namespace(=~|!~)("(?:[^"\\]|\\.)*")`)

// bigCluster has 12 namespaces of 40 single-container Deployments each, which
// is more than one query may return, plus a namespace deleted during the
// window that only the history holds.
func bigCluster(t *testing.T, oneNamespace bool) func(string, url.Values) (int, any) {
	h := clusterStore(true, true, true)
	var names []string
	perNS := 40
	if oneNamespace {
		names, perNS = []string{"big"}, 600
	} else {
		for i := range 12 {
			names = append(names, fmt.Sprintf("team-%02d", i))
		}
	}
	return func(path string, q url.Values) (int, any) {
		query := q.Get("query")
		if path == "/api/v1/query" && strings.HasPrefix(query, "count by (namespace) (kube_pod_container_info{") {
			var sets []map[string]string
			for _, n := range names {
				sets = append(sets, map[string]string{"namespace": n})
			}
			body := groups(sets...)
			for _, r := range body["data"].(map[string]any)["result"].([]any) {
				r.(map[string]any)["value"] = []any{1.0, strconv.Itoa(perNS)}
			}
			return 200, body
		}
		if path != "/api/v1/query_range" || strings.Contains(query, "kube_node_status_allocatable") {
			return h(path, q)
		}
		in := func(ns string) bool {
			for _, m := range nsMatcher.FindAllStringSubmatch(query, -1) {
				re, _ := strconv.Unquote(m[2])
				ok := regexp.MustCompile("^(?:" + re + ")$").MatchString(ns)
				if (m[1] == "=~") != ok {
					return false
				}
			}
			return true
		}
		replicas := strings.Contains(query, "kube_deployment_spec_replicas")
		if !strings.Contains(query, "kube_replicaset_owner") && !strings.Contains(query, "kube_deployment_spec_replicas") {
			return 200, empty() // no StatefulSets
		}
		s, e, step := atoi(q.Get("start")), atoi(q.Get("end")), atoi(q.Get("step"))
		var res []any
		for _, ns := range append(append([]string(nil), names...), "gone") {
			if !in(ns) {
				continue
			}
			n := perNS
			if ns == "gone" {
				n = 1
			}
			for w := range n {
				l := map[string]string{"namespace": ns, "workload": fmt.Sprintf("w%03d", w)}
				if !replicas {
					l["container"] = "app"
				}
				m := matrix(l, s, e, step, func(int64) float64 { return 1 })
				res = append(res, m["data"].(map[string]any)["result"].([]any)...)
			}
		}
		return 200, map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": res}}
	}
}

func TestCloudWatchQueriesByNamespaceGroups(t *testing.T) {
	cw := &cloudWatchFake{handle: bigCluster(t, false)}
	f, rep, err := Run(context.Background(), Options{Client: cw.client(t), Cluster: "prod", Days: 2, Step: time.Hour,
		Now: time.Unix(t0, 0), CloudWatch: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	validate(t, f)
	if rep.Containers != 12*40+1 || rep.Workloads != 12*40+1 {
		t.Errorf("got %d containers in %d workloads; want 481 of each", rep.Containers, rep.Workloads)
	}
	if find(f, "gone", Deployment, "w000", "app") == nil {
		t.Error("the namespace without pods now is missing")
	}
	if cw.truncated > 0 {
		t.Errorf("%d results were truncated", cw.truncated)
	}
	groupsSeen := map[string]bool{}
	for _, q := range cw.queries {
		if q.Get("start") != "" && strings.Contains(q.Get("query"), "container_cpu_usage_seconds_total") {
			for _, m := range nsMatcher.FindAllStringSubmatch(q.Get("query"), 1) {
				groupsSeen[m[1]+m[2]] = true
			}
		}
	}
	var got []string
	for g := range groupsSeen {
		got = append(got, g)
	}
	sort.Strings(got)
	// 250 series per group: six namespaces of 40, then the rest.
	want := []string{
		`!~"team-00|team-01|team-02|team-03|team-04|team-05|team-06|team-07|team-08|team-09|team-10|team-11"`,
		`=~"team-00|team-01|team-02|team-03|team-04|team-05"`,
		`=~"team-06|team-07|team-08|team-09|team-10|team-11"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("namespace groups:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCloudWatchTruncatedResultFails(t *testing.T) {
	cw := &cloudWatchFake{handle: bigCluster(t, true)}
	_, err := runCloudWatch(t, cw.client(t))
	var te *TruncatedError
	if !errors.As(err, &te) || te.Limit != CloudWatchMaxSeries {
		t.Fatalf("want a TruncatedError, got %v", err)
	}
}

func TestStoresWithoutASeriesLimitAreNotSplit(t *testing.T) {
	f := &fakeStore{handle: bigCluster(t, false)}
	c := f.client(t)
	shards, warning, err := namespaceShards(context.Background(), c, time.Unix(t0, 0))
	if err != nil || len(shards) != 1 || shards[0] != c || warning != "" {
		t.Fatalf("got %d shards, %q, %v", len(shards), warning, err)
	}
	if len(f.requests) != 0 {
		t.Error("a store without a series limit must not be asked for namespaces")
	}
}

func TestNamespaceShardsKeepTheSelector(t *testing.T) {
	f := &fakeStore{handle: bigCluster(t, false)}
	c := f.client(t)
	c.MaxSeries, c.Selector = CloudWatchMaxSeries, `"@resource.k8s.cluster.name"="prod"`
	shards, _, err := namespaceShards(context.Background(), c, time.Unix(t0, 0))
	if err != nil || len(shards) != 3 {
		t.Fatalf("got %d shards, %v", len(shards), err)
	}
	for _, s := range shards {
		if !strings.HasPrefix(s.Selector, `"@resource.k8s.cluster.name"="prod",namespace`) {
			t.Errorf("selector %s", s.Selector)
		}
	}
	if c.Selector != `"@resource.k8s.cluster.name"="prod"` {
		t.Error("the shards changed the client they came from")
	}
}

func TestNamespaceShardsWithTooManyNamespacesWarn(t *testing.T) {
	var sets []map[string]string
	for i := range 400 {
		sets = append(sets, map[string]string{"namespace": fmt.Sprintf("namespace-number-%03d", i)})
	}
	f := &fakeStore{handle: func(string, url.Values) (int, any) { return 200, groups(sets...) }}
	c := f.client(t)
	c.MaxSeries = CloudWatchMaxSeries
	shards, warning, err := namespaceShards(context.Background(), c, time.Unix(t0, 0))
	if err != nil || len(shards) != 2 || !strings.Contains(warning, "deleted during the window") {
		t.Fatalf("got %d shards, %q, %v", len(shards), warning, err)
	}
	for _, s := range shards {
		if strings.Contains(s.Selector, "!~") {
			t.Error("the matcher for the other namespaces must be left out")
		}
	}
}

func TestClientTruncation(t *testing.T) {
	sets := make([]map[string]string, 3)
	for i := range sets {
		sets[i] = map[string]string{"pod": strconv.Itoa(i)}
	}
	f := &fakeStore{handle: func(string, url.Values) (int, any) { return 200, groups(sets...) }}
	c := f.client(t)
	if ss, err := c.Query(context.Background(), "up{}", time.Time{}); err != nil || len(ss) != 3 {
		t.Fatalf("no limit: %d, %v", len(ss), err)
	}
	c.MaxSeries = 4
	if _, err := c.Query(context.Background(), "up{}", time.Time{}); err != nil {
		t.Fatalf("under the limit: %v", err)
	}
	c.MaxSeries = 3
	var te *TruncatedError
	if _, err := c.Query(context.Background(), "up{}", time.Time{}); !errors.As(err, &te) {
		t.Fatalf("at the limit: %v", err)
	}
}
