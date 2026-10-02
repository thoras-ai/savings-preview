package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// scrubbedLabels maps the labels whose values name a cluster, its namespaces,
// workloads, pods, containers and nodes to the kind of name that replaces
// them. Values of one kind are numbered together, so a Deployment keeps its
// name whether it is read from workload or deployment.
var scrubbedLabels = map[string]string{
	"namespace": "namespace", "workload": "workload", "deployment": "workload", "statefulset": "workload",
	"replicaset": "replicaset", "owner_name": "owner", "pod": "pod", "container": "container", "node": "node",
}

func init() {
	for _, l := range ClusterLabels {
		scrubbedLabels[l] = "cluster"
	}
}

// keptLabels are the other labels the adapter reads from responses. Every
// label not listed here or in scrubbedLabels is dropped, which removes
// addresses, instance names, host details and the stack's own labels.
var keptLabels = map[string]bool{"__name__": true, "resource": true, "phase": true, "owner_kind": true, "reason": true, "unit": true}

// keptNodeLabels are the node labels nodeAttrs reads, as kube_node_labels
// names them.
var keptNodeLabels = map[string]bool{}

func init() {
	for _, k := range []string{"node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type",
		"topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region",
		"eks.amazonaws.com/capacityType", "karpenter.sh/capacity-type", "node.kubernetes.io/capacity-type",
		"cloud.google.com/gke-spot", "cloud.google.com/gke-provisioning", "kubernetes.azure.com/scalesetpriority"} {
		keptNodeLabels[labelKey(k)] = true
	}
}

var matcher = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)(=~|!~|!=|=)"((?:[^"\\]|\\.)*)"`)

type scrubber struct{ names map[string]map[string]string }

func (s *scrubber) name(kind, v string) string {
	if v == "" {
		return ""
	}
	if n, ok := s.names[kind][v]; ok {
		return n
	}
	return v
}

func (s *scrubber) collect(kind, v string) {
	if v == "" {
		return
	}
	if s.names[kind] == nil {
		s.names[kind] = map[string]string{}
	}
	s.names[kind][v] = ""
}

// scrubRecording replaces cluster, namespace, workload, pod, container and node
// names in the recorded queries, selector and responses with numbered ones
// such as namespace-2, drops every other label the adapter does not read, and
// keeps only the scheme of provider IDs. The same value gets the same name
// everywhere, so the run replays as recorded. Usage values are kept.
func scrubRecording(rec *recording) error {
	s := &scrubber{names: map[string]map[string]string{}}
	type parsed struct {
		path   string
		params url.Values
		body   map[string]any
		resp   response
	}
	var all []parsed
	for k, resp := range rec.Responses {
		path, raw, _ := strings.Cut(k, "?")
		params, err := url.ParseQuery(raw)
		if err != nil {
			return fmt.Errorf("recorded key %q: %w", k, err)
		}
		var body map[string]any
		if json.Unmarshal(resp.Body, &body) == nil {
			eachMetric(body, func(m map[string]any) {
				for l, v := range m {
					if kind, ok := scrubbedLabels[l]; ok {
						if sv, ok := v.(string); ok {
							s.collect(kind, sv)
						}
					}
				}
			})
		}
		all = append(all, parsed{path, params, body, resp})
	}
	for _, m := range matcher.FindAllStringSubmatch(rec.Selector, -1) {
		if kind, ok := scrubbedLabels[m[1]]; ok && (m[2] == "=" || m[2] == "!=") {
			s.collect(kind, m[3])
		}
	}
	s.collect("cluster", rec.Cluster)
	for kind, vs := range s.names {
		sorted := make([]string, 0, len(vs))
		for v := range vs {
			sorted = append(sorted, v)
		}
		sort.Strings(sorted)
		for i, v := range sorted {
			vs[v] = fmt.Sprintf("%s-%d", kind, i+1)
		}
	}

	query := func(q string) string {
		return matcher.ReplaceAllStringFunc(q, func(m string) string {
			sm := matcher.FindStringSubmatch(m)
			kind, ok := scrubbedLabels[sm[1]]
			if !ok || (sm[2] != "=" && sm[2] != "!=") {
				return m
			}
			return sm[1] + sm[2] + `"` + s.name(kind, sm[3]) + `"`
		})
	}
	out := map[string]response{}
	for _, p := range all {
		if q, ok := p.params["query"]; ok {
			p.params.Set("query", query(q[0]))
		}
		resp := p.resp
		if p.body != nil {
			eachMetric(p.body, func(m map[string]any) {
				for l, v := range m {
					sv, _ := v.(string)
					switch kind, scrub := scrubbedLabels[l]; {
					case scrub:
						m[l] = s.name(kind, sv)
					case l == "provider_id":
						scheme, _, _ := strings.Cut(sv, ":")
						m[l] = scheme + "://"
					case !keptLabels[l] && !keptNodeLabels[l]:
						delete(m, l)
					}
				}
			})
			b, err := json.Marshal(p.body)
			if err != nil {
				return err
			}
			resp.Body = b
		}
		out[recordKey(p.path, p.params)] = resp
	}
	rec.Responses = out
	rec.Selector = query(rec.Selector)
	rec.Cluster = s.name("cluster", rec.Cluster)
	return nil
}

// eachMetric calls f with the label set of every series in a query response.
func eachMetric(body map[string]any, f func(map[string]any)) {
	data, _ := body["data"].(map[string]any)
	results, _ := data["result"].([]any)
	for _, r := range results {
		if r, ok := r.(map[string]any); ok {
			if m, ok := r["metric"].(map[string]any); ok {
				f(m)
			}
		}
	}
}

func recordRun(t *testing.T, rec *recording, g Getter, path string) {
	t.Helper()
	r := &recorder{g: g, r: rec}
	if _, rep, err := Run(context.Background(), recordedOptions(rec, r)); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("recorded %d responses: %+v", len(rec.Responses), rep)
	}
	if err := scrubRecording(rec); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func recordedOptions(rec *recording, g Getter) Options {
	c := &Client{G: g, Retries: 1, Selector: rec.Selector}
	if rec.Backend == BackendGrafanaCloud {
		c.MaxRange = GrafanaCloudMaxRange
	}
	cluster := rec.Cluster
	if cluster == "" {
		cluster = "minikube"
	}
	return Options{Client: c, Cluster: cluster, Days: rec.Days, Step: time.Duration(rec.StepS) * time.Second,
		Now: time.Unix(rec.Now, 0), Concurrency: 1, AdaptiveMetrics: rec.Backend == BackendGrafanaCloud}
}

// TestRecordGrafanaCloud records one day of a live Grafana Cloud stack into
// testdata/grafana-cloud.json. Set RECORD_GRAFANA_CLOUD_URL to the stack's
// Prometheus endpoint, RECORD_GRAFANA_CLOUD_AUTH_FILE to a file holding
// INSTANCE_ID:TOKEN, and RECORD_SELECTOR to pick the cluster. Names are
// replaced as scrubRecording describes; review the file before committing it.
func TestRecordGrafanaCloud(t *testing.T) {
	raw, authFile := os.Getenv("RECORD_GRAFANA_CLOUD_URL"), os.Getenv("RECORD_GRAFANA_CLOUD_AUTH_FILE")
	if raw == "" || authFile == "" {
		t.Skip("RECORD_GRAFANA_CLOUD_URL or RECORD_GRAFANA_CLOUD_AUTH_FILE is not set")
	}
	store, err := ResolveBackend(raw, BackendGrafanaCloud, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	user, pass, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	if !ok {
		t.Fatal("the auth file must hold INSTANCE_ID:TOKEN")
	}
	sel, err := ParseSelector(os.Getenv("RECORD_SELECTOR"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recording{Store: "grafana-cloud", Backend: BackendGrafanaCloud, Cluster: "minikube", Selector: sel,
		Now: time.Now().Truncate(time.Minute).Unix(), Days: 1, StepS: 60, Responses: map[string]response{}}
	recordRun(t, rec, &HTTPGetter{Base: store.Base, Auth: Basic{User: user, Password: pass}}, "testdata/grafana-cloud.json")
}

func TestReplayGrafanaCloud(t *testing.T) {
	rec := loadRecording(t, "testdata/grafana-cloud.json")
	p := &replayer{r: rec}
	file, rep, err := Run(context.Background(), recordedOptions(rec, p))
	if err != nil {
		t.Fatalf("%v (requests not in the recording: %v)", err, p.missed)
	}
	validate(t, file)
	if rep.UsageFromPods {
		t.Error("usage was split from pods on Grafana Cloud")
	}
	if rep.Containers == 0 || rep.Workloads == 0 || rep.Nodes != 1 {
		t.Errorf("report: %+v", rep)
	}
	for _, r := range file.Resources {
		if r.Node != nil {
			if !strings.HasPrefix(r.Node.Name, "node-") {
				t.Errorf("node %q was not renamed", r.Node.Name)
			}
			continue
		}
		for _, v := range []string{r.Namespace, r.Workload, r.Container} {
			if v != "" && !regexp.MustCompile(`^(namespace|workload|container)-\d+$`).MatchString(v) {
				t.Errorf("%q was not renamed", v)
			}
		}
	}
}

func TestScrubRecording(t *testing.T) {
	sel := `cluster="prod-us"`
	usage := MemoryUsage(Deployment)
	rec := &recording{Cluster: "prod-us", Selector: sel, Responses: map[string]response{}}
	add := func(q string, body string) {
		rec.Responses[recordKey("/api/v1/query", url.Values{"query": {q}, "time": {"100"}})] = response{Status: 200, Body: json.RawMessage(body)}
	}
	add(`count by (cluster) (kube_node_info{cluster="prod-us"})`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"cluster":"prod-us"},"value":[100,"1"]}]}}`)
	add(usage, `{"status":"success","data":{"resultType":"vector","result":[`+
		`{"metric":{"namespace":"payments","workload":"checkout","container":"app"},"value":[100,"5"]},`+
		`{"metric":{"namespace":"payments","workload":"ledger","container":"app"},"value":[100,"6"]}]}}`)
	add(`kube_node_info{cluster="prod-us"}`, `{"status":"success","data":{"resultType":"vector","result":[`+
		`{"metric":{"__name__":"kube_node_info","node":"ip-10-1-2-3.ec2.internal","internal_ip":"10.1.2.3","instance":"10.0.0.9:8080",`+
		`"provider_id":"aws:///us-east-1a/i-0123456789abcdef0","label_node_kubernetes_io_instance_type":"m5.large","label_team":"payments-core"},"value":[100,"1"]}]}}`)
	if err := scrubRecording(rec); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"prod-us", "payments", "checkout", "ledger", "ip-10-1-2-3", "10.1.2.3", "10.0.0.9", "i-0123456789abcdef0", "label_team"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("scrubbed recording still holds %q", leak)
		}
	}
	if rec.Cluster != "cluster-1" || rec.Selector != `cluster="cluster-1"` {
		t.Errorf("cluster %q, selector %q", rec.Cluster, rec.Selector)
	}
	if _, ok := rec.Responses[recordKey("/api/v1/query", url.Values{"query": {`count by (cluster) (kube_node_info{cluster="cluster-1"})`}, "time": {"100"}})]; !ok {
		t.Error("the query key was not rewritten with the scrubbed selector")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Responses[recordKey("/api/v1/query", url.Values{"query": {usage}, "time": {"100"}})].Body, &body)
	var got []string
	eachMetric(body, func(m map[string]any) {
		got = append(got, fmt.Sprint(m["namespace"], "/", m["workload"], "/", m["container"]))
	})
	sort.Strings(got)
	if want := "[namespace-1/workload-1/container-1 namespace-1/workload-2/container-1]"; fmt.Sprint(got) != want {
		t.Errorf("usage labels = %v, want %s", got, want)
	}
	var node map[string]any
	_ = json.Unmarshal(rec.Responses[recordKey("/api/v1/query", url.Values{"query": {`kube_node_info{cluster="cluster-1"}`}, "time": {"100"}})].Body, &node)
	eachMetric(node, func(m map[string]any) {
		want := map[string]any{"__name__": "kube_node_info", "node": "node-1", "provider_id": "aws://", "label_node_kubernetes_io_instance_type": "m5.large"}
		if fmt.Sprint(m) != fmt.Sprint(want) {
			t.Errorf("node labels = %v, want %v", m, want)
		}
	})
}
