package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawSeries is one source series in the fake organization, sampled every 15
// seconds as the Agent does.
type rawSeries struct {
	metric string
	tags   map[string]string
	value  func(t int64) (float64, bool)
}

// fakeOrg answers the timeseries API from raw series. It evaluates the subset
// of the query language the adapter writes: AGG:METRIC{FILTERS} by {TAGS}.rollup(FN, SECONDS).
type fakeOrg struct {
	series []rawSeries
	units  map[string]string
	// maxPoints is the point cap per series; a request over it gets a larger
	// interval, as Datadog does.
	maxPoints int
	// rateLimited makes the first n timeseries requests answer 429.
	rateLimited int
	badKeys     bool

	mu       sync.Mutex
	requests []tsAttributes
	served   int
}

const (
	testAPIKey = "api-key-0123"
	testAppKey = "app-key-4567"
)

func (f *fakeOrg) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(f)
	t.Cleanup(s.Close)
	return s
}

func (f *fakeOrg) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if f.badKeys || r.Header.Get("DD-API-KEY") != testAPIKey || r.Header.Get("DD-APPLICATION-KEY") != testAppKey {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"errors":["Forbidden"]}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == ValidateKeysPath:
		io.WriteString(w, `{"status":"ok"}`)
	case r.Method == http.MethodPost && r.URL.Path == TimeseriesPath:
		var req tsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Data.Type != "timeseries_request" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"errors":["Bad Request"]}`)
			return
		}
		f.mu.Lock()
		f.served++
		limited := f.served <= f.rateLimited
		f.requests = append(f.requests, req.Data.Attributes)
		f.mu.Unlock()
		w.Header().Set("X-RateLimit-Name", "query_timeseries")
		w.Header().Set("X-RateLimit-Period", "10")
		w.Header().Set("X-RateLimit-Reset", "3")
		if limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"errors":["Too many requests"]}`)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "100")
		resp, err := f.answer(req.Data.Attributes)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"errors": []string{err.Error()}})
			return
		}
		json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"errors":["Not found"]}`)
	}
}

var queryRe = regexp.MustCompile(`^(avg|max|min|sum):([a-z0-9_.]+)\{([^}]*)\}(?: by \{([^}]*)\})?\.rollup\((avg|max|min|sum), (\d+)\)$`)

func (f *fakeOrg) answer(a tsAttributes) (map[string]any, error) {
	interval := a.Interval / 1000
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be set")
	}
	from, to := a.From/1000, a.To/1000
	cap := f.maxPoints
	if cap == 0 {
		cap = MaxPoints
	}
	for (to-from)/interval > int64(cap) {
		interval *= 2
	}
	type group struct {
		q    int
		tags []string
		pts  map[int64][]float64
	}
	var groups []*group
	for qi, q := range a.Queries {
		m := queryRe.FindStringSubmatch(q.Query)
		if m == nil {
			return nil, fmt.Errorf("fake: cannot parse %q", q.Query)
		}
		space, metric, filters, by, fn := m[1], m[2], m[3], m[4], m[5]
		roll, _ := strconv.ParseInt(m[6], 10, 64)
		if roll < interval {
			roll = interval
		}
		var byTags []string
		if by != "" {
			byTags = strings.Split(by, ",")
		}
		byKey := map[string]*group{}
		for _, s := range f.series {
			if s.metric != metric || !matches(s.tags, filters) {
				continue
			}
			var gt []string
			for _, t := range byTags {
				v, ok := s.tags[t]
				if !ok {
					v = "N/A"
				}
				gt = append(gt, t+":"+v)
			}
			k := strings.Join(gt, ",")
			g := byKey[k]
			if g == nil {
				g = &group{q: qi, tags: gt, pts: map[int64][]float64{}}
				byKey[k] = g
				groups = append(groups, g)
			}
			// Roll up the source series, then aggregate across series.
			for b := from / roll * roll; b < to; b += roll {
				var vals []float64
				for t := b; t < b+roll; t += 15 {
					if t < from || t >= to {
						continue
					}
					if v, ok := s.value(t); ok {
						vals = append(vals, v)
					}
				}
				if len(vals) > 0 {
					g.pts[b] = append(g.pts[b], agg(fn, vals))
				}
			}
		}
		for _, g := range byKey {
			for b, vs := range g.pts {
				g.pts[b] = []float64{agg(space, vs)}
			}
		}
	}
	var times []int64
	for b := from / interval * interval; b < to; b += interval {
		times = append(times, b*1000)
	}
	series := []map[string]any{}
	values := [][]*float64{}
	for _, g := range groups {
		row := make([]*float64, len(times))
		for i, t := range times {
			if v, ok := g.pts[t/1000]; ok {
				x := v[0]
				row[i] = &x
			}
		}
		metric := queryRe.FindStringSubmatch(a.Queries[g.q].Query)[2]
		var unit any
		if u := f.units[metric]; u != "" {
			unit = []any{map[string]any{"family": "cpu", "name": u, "plural": u + "s", "scale_factor": 1.0, "short_name": u}, nil}
		}
		series = append(series, map[string]any{"group_tags": g.tags, "query_index": g.q, "unit": unit})
		values = append(values, row)
	}
	return map[string]any{"data": map[string]any{"type": "timeseries_response", "id": "0",
		"attributes": map[string]any{"series": series, "times": times, "values": values}}}, nil
}

func agg(fn string, vs []float64) float64 {
	out := vs[0]
	sum := 0.0
	for _, v := range vs {
		sum += v
		switch fn {
		case "max":
			out = math.Max(out, v)
		case "min":
			out = math.Min(out, v)
		}
	}
	switch fn {
	case "avg":
		return sum / float64(len(vs))
	case "sum":
		return sum
	}
	return out
}

// matches applies a scope: comma-separated tag:value filters, "*" anywhere in
// the value, "!" to negate.
func matches(tags map[string]string, scope string) bool {
	if scope == "*" || scope == "" {
		return true
	}
	for _, f := range strings.Split(scope, ",") {
		neg := strings.HasPrefix(f, "!")
		f = strings.TrimPrefix(f, "!")
		k, pat, _ := strings.Cut(f, ":")
		v, ok := tags[k]
		hit := ok && globMatch(pat, v)
		if hit == neg {
			return false
		}
	}
	return true
}

func globMatch(pat, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pat), `\*`, ".*") + "$"
	return regexp.MustCompile(re).MatchString(s)
}

// steady is a value that holds from start on.
func steady(v float64, start int64) func(int64) (float64, bool) {
	return func(t int64) (float64, bool) { return v, t >= start }
}

// wave rises to peak once an hour for a few minutes over a base.
func wave(base, peak float64, start int64) func(int64) (float64, bool) {
	return func(t int64) (float64, bool) {
		if t < start {
			return 0, false
		}
		if t%3600 < 300 {
			return peak, true
		}
		return base, true
	}
}

const testNow = 1_790_006_400 // a multiple of 3600

// org builds a cluster's worth of Agent metrics: a Deployment with two pods
// of two containers, a StatefulSet with one pod, a DaemonSet pod and two
// nodes, all with data for days of history.
func org(cluster string, days int) []rawSeries {
	start := int64(testNow - days*86400)
	pod := func(ns, kindTag, wl, pod, ctr string) map[string]string {
		m := map[string]string{"kube_namespace": ns, "pod_name": pod, "kube_container_name": ctr}
		if kindTag != "" {
			m[kindTag] = wl
		}
		if cluster != "" {
			m["kube_cluster_name"] = cluster
		}
		return m
	}
	var out []rawSeries
	add := func(tags map[string]string, cpuNano, cpuPeakNano, mem, cpuReq, memReq, cpuLim float64) {
		out = append(out,
			rawSeries{"kubernetes.cpu.usage.total", tags, wave(cpuNano, cpuPeakNano, start)},
			rawSeries{"kubernetes.memory.working_set", tags, wave(mem, mem*1.5, start)},
		)
		if cpuReq > 0 {
			out = append(out, rawSeries{"kubernetes.cpu.requests", tags, steady(cpuReq, start)})
		}
		if memReq > 0 {
			out = append(out, rawSeries{"kubernetes.memory.requests", tags, steady(memReq, start)})
		}
		if cpuLim > 0 {
			out = append(out, rawSeries{"kubernetes.cpu.limits", tags, steady(cpuLim, start)})
		}
	}
	for _, p := range []string{"web-5d9c-a", "web-5d9c-b"} {
		add(pod("shop", "kube_deployment", "web", p, "app"), 2e8, 8e8, 400<<20, 1, 1<<30, 2)
		add(pod("shop", "kube_deployment", "web", p, "envoy"), 5e7, 1e8, 64<<20, 0, 0, 0)
	}
	add(pod("shop", "kube_stateful_set", "db", "db-0", "postgres"), 5e8, 5e8, 2<<30, 2, 4<<30, 0)
	add(pod("kube-system", "", "", "node-agent-x", "agent"), 1e8, 1e8, 100<<20, 0.1, 128<<20, 0)

	ksm := func(extra map[string]string) map[string]string {
		if cluster != "" {
			extra["kube_cluster_name"] = cluster
		}
		return extra
	}
	out = append(out,
		rawSeries{"kubernetes_state.deployment.replicas_desired", ksm(map[string]string{"kube_namespace": "shop", "kube_deployment": "web"}), steady(2, start)},
		rawSeries{"kubernetes_state.statefulset.replicas_desired", ksm(map[string]string{"kube_namespace": "shop", "kube_stateful_set": "db"}), steady(1, start)},
		rawSeries{"kubernetes_state.node.cpu_allocatable", ksm(map[string]string{"node": "ip-10-0-1-1", "kube_region": "us-east-1"}), steady(3.92, start)},
		rawSeries{"kubernetes_state.node.memory_allocatable", ksm(map[string]string{"node": "ip-10-0-1-1", "kube_region": "us-east-1"}), steady(15<<30, start)},
		rawSeries{"kubernetes_state.node.cpu_allocatable", ksm(map[string]string{"node": "ip-10-0-1-2"}), steady(3.92, start)},
		rawSeries{"kubernetes_state.node.memory_allocatable", ksm(map[string]string{"node": "ip-10-0-1-2"}), steady(15<<30, start)},
	)
	return out
}

func without(ss []rawSeries, metrics ...string) []rawSeries {
	drop := map[string]bool{}
	for _, m := range metrics {
		drop[m] = true
	}
	var out []rawSeries
	for _, s := range ss {
		if !drop[s.metric] {
			out = append(out, s)
		}
	}
	return out
}

// renamed copies the series of one metric under another name, scaling values.
func renamed(ss []rawSeries, from, to string, scale float64) []rawSeries {
	var out []rawSeries
	for _, s := range ss {
		if s.metric == from {
			v := s.value
			out = append(out, rawSeries{to, s.tags, func(t int64) (float64, bool) { x, ok := v(t); return x * scale, ok }})
		}
	}
	return out
}

// noSleep records waits instead of sleeping.
type noSleep struct {
	mu    sync.Mutex
	waits []time.Duration
	now   time.Time
}

func testClient(base string) (*Client, *noSleep) {
	ns := &noSleep{now: time.Unix(testNow, 0)}
	c := &Client{Base: base, APIKey: testAPIKey, AppKey: testAppKey, Retries: 3}
	c.now = func() time.Time { ns.mu.Lock(); defer ns.mu.Unlock(); return ns.now }
	c.sleep = func(_ context.Context, d time.Duration) error {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		ns.waits = append(ns.waits, d)
		ns.now = ns.now.Add(d)
		return nil
	}
	return c, ns
}

func sortedTags(m map[string]string) string {
	var ks []string
	for k, v := range m {
		ks = append(ks, k+":"+v)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}
