package datadog

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// Source is the thoras.source of every resource the adapter writes.
const Source = "datadog"

// ChunkPoints is the most points per series one request asks for, under
// Datadog's limit of 1,500.
const ChunkPoints = 1440

// minSplitPoints is the smallest chunk a request is split into when Datadog
// answers with a coarser interval than asked for.
const minSplitPoints = 60

// Options configure one pull.
type Options struct {
	Client      *Client
	Filters     []string // tag filters from --selector
	Cluster     string   // k8s.cluster.name; default: the kube_cluster_name of the series
	Days        int
	Step        time.Duration
	Now         time.Time
	Concurrency int
	Log         func(format string, args ...any)
}

// Report describes what a pull found and what it could not get.
type Report struct {
	Cluster     string
	Start, End  time.Time
	FullHistory bool // usage goes back the full number of days asked for
	Containers  int
	Workloads   int
	Nodes       int
	Metrics     []string // the Datadog metric used for each role
	Warnings    []string
}

// ClusterError reports series from more than one cluster.
type ClusterError struct {
	Values   []string // "" is series without the cluster tag
	Selected bool     // a selector was given and still matches several clusters
}

func (e *ClusterError) Error() string {
	shown := make([]string, len(e.Values))
	pick := ""
	for i, v := range e.Values {
		shown[i] = strconv.Quote(v)
		if pick == "" && v != "" {
			pick = v
		}
	}
	if e.Selected {
		return fmt.Sprintf("the selector matches %d clusters (%s: %s); narrow it to one, for example --selector %s:%s",
			len(e.Values), ClusterTag, strings.Join(shown, ", "), ClusterTag, pick)
	}
	return fmt.Sprintf("the organization reports %d clusters (%s: %s); their workloads would be averaged together. Choose one with --selector %s:%s",
		len(e.Values), ClusterTag, strings.Join(shown, ", "), ClusterTag, pick)
}

// found is a metric chosen for a role.
type found struct {
	Metric
	scale float64
}

// probed is what the probe found.
type probed struct {
	roles    map[Role]found
	clusters map[string]bool // kube_cluster_name values of the usage series
	tried    map[Role][]string
}

func (o *Options) defaults() {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Step <= 0 {
		o.Step = time.Minute
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	o.Now = o.Now.Truncate(o.Step)
}

func (o *Options) window() time.Duration { return time.Duration(o.Days) * 24 * time.Hour }

// probe finds, for each role, the first candidate metric that has series with
// the tags the role needs in the window.
func probe(ctx context.Context, o Options) (*probed, error) {
	if err := o.Client.Validate(ctx); err != nil {
		return nil, fmt.Errorf("check the keys: %w", err)
	}
	p := &probed{roles: map[Role]found{}, clusters: map[string]bool{}, tried: map[Role][]string{}}
	from := o.Now.Add(-o.window())
	for _, r := range Roles {
		for _, m := range Candidates[r] {
			p.tried[r] = append(p.tried[r], m.String())
			ss, _, err := o.Client.Timeseries(ctx, from, o.Now, o.window(), []Query{{Name: "a", Query: ProbeQuery(r, m, o.Filters, o.window())}})
			if err != nil {
				return nil, fmt.Errorf("look for %s (%s): %w", r, m.Name, err)
			}
			peak, any := 0.0, false
			for _, s := range ss {
				for _, pt := range s.Points {
					any = true
					peak = math.Max(peak, pt.V)
				}
			}
			if !any {
				continue
			}
			f := found{Metric: m, scale: 1}
			switch m.Unit {
			case Nanocores:
				f.scale = 1e-9
			case CoresOrNanocores:
				if peak > NanocoreFloor {
					f.scale = 1e-9
				}
			}
			p.roles[r] = f
			if r == CPUUsage || r == MemoryUsage {
				for _, s := range ss {
					if len(s.Points) > 0 {
						p.clusters[clusterValue(s.Tags[ClusterTag])] = true
					}
				}
			}
			o.Log("%s: %s", r, m)
			break
		}
	}
	return p, nil
}

// clusterValue maps the group value of series without the tag to "".
func clusterValue(v string) string {
	if strings.EqualFold(v, "n/a") {
		return ""
	}
	return v
}

// List prints the metric found for each role and the clusters of the usage
// series.
func List(ctx context.Context, o Options, w io.Writer) error {
	o.defaults()
	p, err := probe(ctx, o)
	if err != nil {
		return err
	}
	for _, r := range Roles {
		if f, ok := p.roles[r]; ok {
			fmt.Fprintf(w, "%s: %s\n", r, f.Metric)
		} else {
			fmt.Fprintf(w, "%s: not found (looked for %s)\n", r, strings.Join(p.tried[r], ", "))
		}
	}
	for _, c := range sortedKeys(p.clusters) {
		if c == "" {
			fmt.Fprintf(w, "cluster: (series without %s)\n", ClusterTag)
		} else {
			fmt.Fprintf(w, "cluster: %s:%s\n", ClusterTag, c)
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Run pulls the history and assembles the contract file.
func Run(ctx context.Context, o Options) (*contract.File, *Report, error) {
	o.defaults()
	rep := &Report{End: o.Now}
	p, err := probe(ctx, o)
	if err != nil {
		return nil, rep, err
	}
	for _, r := range []Role{CPUUsage, MemoryUsage} {
		if _, ok := p.roles[r]; !ok {
			msg := fmt.Sprintf("no container %s in the last %d days: looked for %s with the %s and %s tags", r, o.Days,
				strings.Join(p.tried[r], ", "), KubeTags.Namespace, KubeTags.Container)
			if len(o.Filters) > 0 {
				msg += " matching the selector"
			} else {
				msg += "; is the Datadog Agent running on the cluster?"
			}
			return nil, rep, fmt.Errorf("%s", msg)
		}
	}
	clusters := sortedKeys(p.clusters)
	if len(clusters) > 1 {
		return nil, rep, &ClusterError{Values: clusters, Selected: len(o.Filters) > 0}
	}
	rep.Cluster = o.Cluster
	if rep.Cluster == "" {
		if len(clusters) == 1 && clusters[0] != "" {
			rep.Cluster = clusters[0]
		} else {
			return nil, rep, fmt.Errorf("the usage series have no %s tag; pass --cluster-name", ClusterTag)
		}
	}
	for _, r := range Roles {
		if f, ok := p.roles[r]; ok {
			rep.Metrics = append(rep.Metrics, fmt.Sprintf("%s from %s", r, f.Metric))
		}
	}
	if _, ok := p.roles[CPURequest]; !ok {
		rep.Warnings = append(rep.Warnings, "no container CPU request metric found (looked for "+strings.Join(p.tried[CPURequest], ", ")+"): CPU requests are listed as missing")
	}
	if _, ok := p.roles[MemoryRequest]; !ok {
		rep.Warnings = append(rep.Warnings, "no container memory request metric found (looked for "+strings.Join(p.tried[MemoryRequest], ", ")+"): memory requests are listed as missing")
	}
	for _, r := range []Role{DeploymentReplicas, StatefulSetReplicas} {
		if _, ok := p.roles[r]; !ok {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("no %s metric found (looked for %s; the kube-state-metrics core check provides it): replica counts are listed as missing", r, strings.Join(p.tried[r], ", ")))
		}
	}
	if _, ok := p.roles[NodeCPU]; !ok {
		rep.Warnings = append(rep.Warnings, "no node allocatable metric found (kubernetes_state.node.cpu_allocatable): node resources are left out and workloads are priced with the default node")
	}

	start := o.Now.Add(-o.window())
	pl, err := pull(ctx, o, p, start)
	if err != nil {
		return nil, rep, err
	}
	f := assemble(o, p, pl, rep)
	if rep.Containers == 0 {
		return nil, rep, fmt.Errorf("no workload containers found: usage series exist, but none carry a %s or %s tag (OpenTelemetry data gets them from the collector's k8sattributes processor)", KubeTags.Deployment, KubeTags.StatefulSet)
	}
	return f, rep, nil
}

type cKey struct{ kind, ns, workload, container string }
type wKey struct{ kind, ns, workload string }

type nodeInfo struct{ region string }

type pulled struct {
	mu                                                     sync.Mutex
	cpuUse, cpuMax, memUse, memMax, cpuReq, memReq, cpuLim map[cKey][]contract.Point
	memLim                                                 map[cKey][]contract.Point
	replicas                                               map[wKey][]contract.Point
	nodeCPU, nodeMem                                       map[string][]contract.Point
	nodes                                                  map[string]nodeInfo
}

// job is one request shape, pulled over the whole window in chunks.
type job struct {
	name    string
	queries []Query
	into    []func(Series, []contract.Point)
}

func pull(ctx context.Context, o Options, p *probed, start time.Time) (*pulled, error) {
	pl := &pulled{
		cpuUse: map[cKey][]contract.Point{}, cpuMax: map[cKey][]contract.Point{},
		memUse: map[cKey][]contract.Point{}, memMax: map[cKey][]contract.Point{},
		cpuReq: map[cKey][]contract.Point{}, memReq: map[cKey][]contract.Point{},
		cpuLim: map[cKey][]contract.Point{}, memLim: map[cKey][]contract.Point{},
		replicas: map[wKey][]contract.Point{},
		nodeCPU:  map[string][]contract.Point{}, nodeMem: map[string][]contract.Point{},
		nodes: map[string]nodeInfo{},
	}
	var jobs []job
	for _, kind := range []string{Deployment, StatefulSet} {
		kind := kind
		container := func(dst map[cKey][]contract.Point, t TagSet) func(Series, []contract.Point) {
			return func(s Series, pts []contract.Point) {
				dst[cKey{kind, s.Tags[t.Namespace], s.Tags[t.workload(kind)], s.Tags[t.Container]}] = pts
			}
		}
		// Means and peaks go in separate requests to keep each response to
		// two values per container and step.
		var use, peak job
		use.name = kind + " usage"
		peak.name = kind + " usage peaks"
		var res job
		res.name = kind + " requests and limits"
		for _, x := range []struct {
			role Role
			fn   string
			dst  map[cKey][]contract.Point
			j    *job
		}{
			{CPUUsage, "avg", pl.cpuUse, &use}, {CPUUsage, "max", pl.cpuMax, &peak},
			{MemoryUsage, "avg", pl.memUse, &use}, {MemoryUsage, "max", pl.memMax, &peak},
			{CPURequest, "avg", pl.cpuReq, &res}, {MemoryRequest, "avg", pl.memReq, &res},
			{CPULimit, "avg", pl.cpuLim, &res}, {MemoryLimit, "avg", pl.memLim, &res},
		} {
			f, ok := p.roles[x.role]
			if !ok {
				continue
			}
			x.j.queries = append(x.j.queries, Query{Name: fmt.Sprintf("q%d", len(x.j.queries)), Query: ContainerQuery(f.Metric, x.role, kind, o.Filters, x.fn, o.Step)})
			x.j.into = append(x.j.into, scaled(f.scale, container(x.dst, f.Tags)))
		}
		jobs = append(jobs, use, peak)
		if len(res.queries) > 0 {
			jobs = append(jobs, res)
		}
		role := DeploymentReplicas
		if kind == StatefulSet {
			role = StatefulSetReplicas
		}
		if f, ok := p.roles[role]; ok {
			t := f.Tags
			jobs = append(jobs, job{name: kind + " replicas",
				queries: []Query{{Name: "q0", Query: ReplicasQuery(f.Metric, role, o.Filters, o.Step)}},
				into: []func(Series, []contract.Point){func(s Series, pts []contract.Point) {
					pl.replicas[wKey{kind, s.Tags[t.Namespace], s.Tags[t.workload(kind)]}] = pts
				}}})
		}
	}
	var nodes job
	nodes.name = "nodes"
	for _, x := range []struct {
		role Role
		dst  map[string][]contract.Point
	}{{NodeCPU, pl.nodeCPU}, {NodeMemory, pl.nodeMem}} {
		f, ok := p.roles[x.role]
		if !ok {
			continue
		}
		dst, t := x.dst, f.Tags
		nodes.queries = append(nodes.queries, Query{Name: fmt.Sprintf("q%d", len(nodes.queries)), Query: NodeQuery(f.Metric, x.role, o.Filters, o.Step)})
		nodes.into = append(nodes.into, func(s Series, pts []contract.Point) {
			n := s.Tags[t.Node]
			dst[n] = mergePoints(dst[n], pts)
			if r := clusterValue(s.Tags[RegionTag]); r != "" {
				pl.nodes[n] = nodeInfo{region: r}
			}
		})
	}
	if len(nodes.queries) > 0 {
		jobs = append(jobs, nodes)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, o.Concurrency)
	errs := make(chan error, len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			got, err := pullChunked(ctx, o.Client, j.queries, start, o.Now, o.Step)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", j.name, err)
				cancel()
				return
			}
			pl.mu.Lock()
			n := 0
			for qi, ss := range got {
				for _, s := range ss {
					j.into[qi](s, toPoints(s.Points))
					n++
				}
			}
			pl.mu.Unlock()
			o.Log("pulled %s: %d series in %s", j.name, n, time.Since(t0).Round(time.Millisecond))
		}()
	}
	wg.Wait()
	close(errs)
	// The first error is the cause; the rest are cancellations that followed it.
	if err := <-errs; err != nil {
		return nil, err
	}
	return pl, nil
}

func scaled(f float64, into func(Series, []contract.Point)) func(Series, []contract.Point) {
	if f == 1 {
		return into
	}
	return func(s Series, pts []contract.Point) {
		for i := range pts {
			pts[i].V *= f
		}
		into(s, pts)
	}
}

// mergePoints joins two time-ordered series, keeping the larger value where
// both have a point. A node can appear under two region groups when its
// label changed.
func mergePoints(a, b []contract.Point) []contract.Point {
	if len(a) == 0 {
		return b
	}
	m := map[int64]float64{}
	for _, p := range a {
		m[p.T] = p.V
	}
	for _, p := range b {
		if v, ok := m[p.T]; !ok || p.V > v {
			m[p.T] = p.V
		}
	}
	out := make([]contract.Point, 0, len(m))
	for t, v := range m {
		out = append(out, contract.Point{T: t, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func toPoints(in []Point) []contract.Point {
	out := make([]contract.Point, len(in))
	for i, p := range in {
		out[i] = contract.Point{T: p.T, V: math.Max(p.V, 0)}
	}
	return out
}

func seriesKey(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(tags[k])
		b.WriteByte(0)
	}
	return b.String()
}

// pullChunked runs the queries over [from, to) in chunks of at most
// ChunkPoints steps, one chunk after another, and returns the merged series
// of each query. Every point is on the step grid. When Datadog answers a
// chunk with a coarser interval than the step, the chunk is split in half
// until it answers at the step.
func pullChunked(ctx context.Context, c *Client, qs []Query, from, to time.Time, step time.Duration) ([][]Series, error) {
	stepS := int64(step / time.Second)
	merged := make([]map[string]*Series, len(qs))
	order := make([][]string, len(qs))
	for i := range merged {
		merged[i] = map[string]*Series{}
	}
	var run func(a, b time.Time) error
	run = func(a, b time.Time) error {
		ss, spacing, err := c.Timeseries(ctx, a, b, step, qs)
		if err != nil {
			return fmt.Errorf("range %s to %s: %w", a.UTC().Format(time.RFC3339), b.UTC().Format(time.RFC3339), err)
		}
		if spacing > step {
			n := int64(b.Sub(a) / step)
			if n <= minSplitPoints {
				return fmt.Errorf("range %s to %s: Datadog returned points %s apart, not every %s; pass a larger --step",
					a.UTC().Format(time.RFC3339), b.UTC().Format(time.RFC3339), spacing, step)
			}
			mid := a.Add(time.Duration(n/2) * step)
			if err := run(a, mid); err != nil {
				return err
			}
			return run(mid, b)
		}
		for _, s := range ss {
			if s.Query < 0 || s.Query >= len(qs) {
				continue
			}
			k := seriesKey(s.Tags)
			m, ok := merged[s.Query][k]
			if !ok {
				m = &Series{Query: s.Query, Tags: s.Tags}
				merged[s.Query][k] = m
				order[s.Query] = append(order[s.Query], k)
			}
			for _, p := range s.Points {
				p.T = p.T / stepS * stepS
				if p.T >= a.Unix() && p.T < b.Unix() {
					m.Points = append(m.Points, p)
				}
			}
		}
		return nil
	}
	span := step * ChunkPoints
	for a := from; a.Before(to); a = a.Add(span) {
		b := a.Add(span)
		if b.After(to) {
			b = to
		}
		if err := run(a, b); err != nil {
			return nil, err
		}
	}
	out := make([][]Series, len(qs))
	for qi := range qs {
		for _, k := range order[qi] {
			m := merged[qi][k]
			sort.SliceStable(m.Points, func(i, j int) bool { return m.Points[i].T < m.Points[j].T })
			m.Points = dedupe(m.Points)
			if len(m.Points) > 0 {
				out[qi] = append(out[qi], *m)
			}
		}
	}
	return out, nil
}

func dedupe(p []Point) []Point {
	out := p[:0]
	for i, x := range p {
		if i > 0 && x.T == out[len(out)-1].T {
			continue
		}
		out = append(out, x)
	}
	return out
}

func assemble(o Options, p *probed, pl *pulled, rep *Report) *contract.File {
	step := int(o.Step / time.Second)
	base := contract.Common{Cluster: rep.Cluster, Source: Source, StepSeconds: step}
	_, hasCPUReq := p.roles[CPURequest]
	_, hasMemReq := p.roles[MemoryRequest]

	keys := map[cKey]bool{}
	for k := range pl.cpuUse {
		keys[k] = true
	}
	for k := range pl.memUse {
		keys[k] = true
	}
	ordered := make([]cKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool { return fmt.Sprint(ordered[i]) < fmt.Sprint(ordered[j]) })

	f := &contract.File{}
	haveWorkload := map[wKey]bool{}
	earliest := int64(0)
	for _, k := range ordered {
		r := contract.Resource{Common: base, Namespace: k.ns, Kind: k.kind, Workload: k.workload, Container: k.container}
		wk := wKey{k.kind, k.ns, k.workload}
		add := func(name string, pts []contract.Point) {
			if len(pts) == 0 {
				if spec, _ := contract.SpecOf(name); spec.Required {
					r.Missing = append(r.Missing, name)
				}
				return
			}
			r.Metrics = append(r.Metrics, contract.Series{Name: name, Points: pts})
		}
		add(contract.CPUUsage, pl.cpuUse[k])
		add(contract.MemoryWorkingSet, pl.memUse[k])
		add(contract.CPUUsageMax, peaks(pl.cpuMax[k], pl.cpuUse[k]))
		add(contract.MemoryWorkingSetMax, peaks(pl.memMax[k], pl.memUse[k]))
		if hasCPUReq {
			add(contract.CPURequest, fillRequests(pl.cpuReq[k], pl.cpuUse[k], pl.replicas[wk]))
		} else {
			r.Missing = append(r.Missing, contract.CPURequest)
		}
		if hasMemReq {
			add(contract.MemoryRequest, fillRequests(pl.memReq[k], pl.memUse[k], pl.replicas[wk]))
		} else {
			r.Missing = append(r.Missing, contract.MemoryRequest)
		}
		add(contract.CPULimit, pl.cpuLim[k])
		add(contract.MemoryLimit, pl.memLim[k])
		for _, s := range r.Metrics {
			if t := s.Points[0].T; earliest == 0 || t < earliest {
				earliest = t
			}
		}
		r.Tier = tierOf(step, r.Metrics)
		f.Resources = append(f.Resources, r)
		haveWorkload[wk] = true
		rep.Containers++
	}

	wls := make([]wKey, 0, len(haveWorkload))
	for k := range haveWorkload {
		wls = append(wls, k)
	}
	sort.Slice(wls, func(i, j int) bool { return fmt.Sprint(wls[i]) < fmt.Sprint(wls[j]) })
	for _, k := range wls {
		name := contract.DeploymentPods
		if k.kind == StatefulSet {
			name = contract.StatefulSetPods
		}
		r := contract.Resource{Common: base, Namespace: k.ns, Kind: k.kind, Workload: k.workload}
		if pts := pl.replicas[k]; len(pts) > 0 {
			r.Metrics = []contract.Series{{Name: name, Points: pts}}
		} else {
			r.Missing = []string{name}
		}
		r.Tier = tierOf(step, r.Metrics)
		f.Resources = append(f.Resources, r)
		rep.Workloads++
	}

	names := map[string]bool{}
	for n := range pl.nodeCPU {
		names[n] = true
	}
	for n := range pl.nodeMem {
		names[n] = true
	}
	for _, n := range sortedKeys(names) {
		node := &contract.Node{Name: n, InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown"}
		if i, ok := pl.nodes[n]; ok {
			node.Region = i.region
		}
		r := contract.Resource{Common: base, Node: node}
		for _, m := range []struct {
			name string
			pts  []contract.Point
		}{{contract.NodeCPU, pl.nodeCPU[n]}, {contract.NodeMemory, pl.nodeMem[n]}} {
			if len(m.pts) == 0 {
				r.Missing = append(r.Missing, m.name)
				continue
			}
			r.Metrics = append(r.Metrics, contract.Series{Name: m.name, Points: m.pts})
		}
		r.Tier = tierOf(step, r.Metrics)
		f.Resources = append(f.Resources, r)
		rep.Nodes++
	}

	if earliest > 0 {
		rep.Start = time.Unix(earliest, 0)
	}
	rep.FullHistory = earliest > 0 && rep.Start.Sub(o.Now.Add(-o.window())) < time.Hour
	if earliest > 0 && !rep.FullHistory {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("Datadog holds container usage from %s, less than the %d days asked for",
			rep.Start.UTC().Format(time.RFC3339), o.Days))
	}
	return f
}

// peaks keeps the peak points at the times the mean has a point, never below
// the mean.
func peaks(max, mean []contract.Point) []contract.Point {
	if len(max) == 0 || len(mean) == 0 {
		return nil
	}
	at := make(map[int64]float64, len(max))
	for _, p := range max {
		at[p.T] = p.V
	}
	var out []contract.Point
	for _, p := range mean {
		if v, ok := at[p.T]; ok {
			out = append(out, contract.Point{T: p.T, V: math.Max(v, p.V)})
		}
	}
	return out
}

// fillRequests adds a zero request at every usage time where the workload
// has a replica count but no request series has a point. That is a container
// with no request set: the kubelet check reports requests only for
// containers that set one.
func fillRequests(req, usage, replicas []contract.Point) []contract.Point {
	if len(usage) == 0 || len(replicas) == 0 {
		return req
	}
	have := make(map[int64]bool, len(req))
	for _, r := range req {
		have[r.T] = true
	}
	up := make(map[int64]bool, len(replicas))
	for _, r := range replicas {
		up[r.T] = true
	}
	out := append([]contract.Point(nil), req...)
	for _, u := range usage {
		if up[u.T] && !have[u.T] {
			out = append(out, contract.Point{T: u.T})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// tierOf claims the best tier the required series in this resource support.
func tierOf(step int, ms []contract.Series) int {
	var first, last int64
	seen := false
	for _, m := range ms {
		sp, _ := contract.SpecOf(m.Name)
		if !sp.Required || len(m.Points) == 0 {
			continue
		}
		f, l := m.Points[0].T, m.Points[len(m.Points)-1].T
		if !seen || f < first {
			first = f
		}
		if !seen || l > last {
			last = l
		}
		seen = true
	}
	if !seen {
		return 3
	}
	return contract.TierFor(step, last-first+int64(step))
}
