package prometheus

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// Options configure one pull.
type Options struct {
	Client      *Client
	Cluster     string
	Days        int
	Step        time.Duration
	Now         time.Time
	Concurrency int
	Log         func(format string, args ...any)
	// AdaptiveMetrics is set for Grafana Cloud, where Adaptive Metrics can
	// aggregate series and drop labels. Pod-level usage is not split there,
	// because a series without a container label may be the sum of a pod's
	// containers rather than the pod cgroup.
	AdaptiveMetrics bool
	// GKE is set for Google Managed Prometheus to one of GKEModes: which
	// metrics to read from the project.
	GKE string
	// CloudWatch is set for the CloudWatch PromQL endpoint to its AWS region.
	CloudWatch string
}

// Report describes what a pull found and what it could not get.
type Report struct {
	Version     string
	Start, End  time.Time
	FullHistory bool // the store holds at least Days of data
	// UsageFromPods is set when the store has no per-container usage and
	// container usage was split from the pod cgroup series by requests.
	UsageFromPods bool
	// GKESystemMetrics is set when the data came from GKE system metrics.
	GKESystemMetrics bool
	// NoRequests is set when the store's requests cannot be used, so every
	// container lacks them and no savings can be computed from the file.
	NoRequests bool
	Containers int
	Workloads  int
	Nodes      int
	OOMKills   int
	Warnings   []string
}

// ClusterError reports a store that holds more than one cluster.
type ClusterError struct {
	Label    string
	Values   []string // "" is a series without the label
	Selected bool     // a selector was given and still matches several clusters
}

func (e *ClusterError) Error() string {
	shown := make([]string, len(e.Values))
	for i, v := range e.Values {
		shown[i] = strconv.Quote(v)
	}
	pick := e.Values[0]
	for _, v := range e.Values {
		if v != "" {
			pick = v
			break
		}
	}
	if e.Selected {
		return fmt.Sprintf("the selector matches %d clusters (label %s: %s); narrow it to one, for example --selector '%s=%s'",
			len(e.Values), e.Label, strings.Join(shown, ", "), QuoteLabel(e.Label), strconv.Quote(pick))
	}
	return fmt.Sprintf("the store holds %d clusters (label %s: %s); their workloads would be averaged together. Choose one with --selector '%s=%s'",
		len(e.Values), e.Label, strings.Join(shown, ", "), QuoteLabel(e.Label), strconv.Quote(pick))
}

// UsageFromPodsNote is the fidelity note on containers whose usage was split
// from the pod cgroup series.
const UsageFromPodsNote = "usage_split_from_pod_by_requests"

type cKey struct{ kind, ns, workload, container string }
type wKey struct{ kind, ns, workload string }

type pulled struct {
	cpuUse, memUse, cpuReq, memReq, cpuLim, memLim map[cKey][]contract.Point
	oom                                            map[cKey][]contract.Point
	replicas                                       map[wKey][]contract.Point
	nodeCPU, nodeMem                               map[string][]contract.Point
	// gke is set when the series are GKE system metrics. Node attributes
	// then come from the node series' labels, in gkeNodes.
	gke      bool
	gkeNodes map[string]*contract.Node
	warnings []string
}

// Run pulls the history from the store and assembles the contract file.
func Run(ctx context.Context, o Options) (*contract.File, *Report, error) {
	o.defaults()
	pl, rep, err := prepare(ctx, o)
	if err != nil {
		return nil, rep, err
	}
	if pl.gke != nil {
		return pullGKE(ctx, o, pl.gke, rep)
	}
	o.Client = pl.client
	pullFn := pull
	if o.Client.OneMetricPerQuery {
		pullFn = pullSplit
	}
	p, err := pullFn(ctx, o, rep.Start, pl.src, pl.kinds)
	if err != nil {
		return nil, rep, err
	}
	rep.Warnings = append(rep.Warnings, p.warnings...)
	f, err := assemble(ctx, o, p, pl.src.requests, rep)
	if err != nil {
		return nil, rep, err
	}
	return f, rep, nil
}

// Probe makes the checks Run makes before it pulls the history: the store
// answers, holds one cluster and the series the savings math needs, and how
// far back its data goes. The report has no counts.
func Probe(ctx context.Context, o Options) (*Report, error) {
	o.defaults()
	_, rep, err := prepare(ctx, o)
	return rep, err
}

func (o *Options) defaults() {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
}

// plan is what prepare found that the pull needs.
type plan struct {
	src   sources
	kinds []string
	// client is the client the pull uses: o.Client, or for Managed
	// Prometheus a copy that maps the cluster label.
	client *Client
	// gke is set when the pull reads GKE system metrics instead.
	gke *gkePlan
}

func prepare(ctx context.Context, o Options) (*plan, *Report, error) {
	c := o.Client
	rep := &Report{Version: c.Build(ctx), End: o.Now}

	if err := c.Check(ctx); err != nil {
		return nil, rep, err
	}
	if o.CloudWatch != "" {
		if err := checkContainerInsights(ctx, c, o.CloudWatch, o.Now); err != nil {
			return nil, rep, err
		}
	}
	if o.GKE != "" {
		mode, err := gkeMode(ctx, c, o.GKE, o.Now)
		if err != nil {
			return nil, rep, err
		}
		if mode == GKESystem {
			return prepareGKE(ctx, o, rep)
		}
		c = gkeClient(c, "cluster")
		o.Client = c
		if ok, err := c.present(ctx, ExportedNamespaceProbe, o.Now); err != nil {
			return nil, rep, fmt.Errorf("probe kube-state-metrics: %w", err)
		} else if ok {
			return nil, rep, fmt.Errorf("kube_pod_owner has an exported_namespace label: Managed Prometheus replaced the namespace of the pods with the namespace of kube-state-metrics, so pods cannot be mapped to workloads. Scrape kube-state-metrics with a ClusterPodMonitoring, which keeps the namespace label, or read GKE system metrics with --gke-metrics system")
		}
	}
	maxBack := time.Duration(o.Days) * 24 * time.Hour
	if err := oneCluster(ctx, c, o.Now, maxBack); err != nil {
		return nil, rep, err
	}
	if err := checkLabels(ctx, c, o.Now, o.AdaptiveMetrics); err != nil {
		return nil, rep, err
	}
	usageProbe := UsageProbe
	if ok, err := c.present(ctx, UsageProbe, o.Now); err != nil {
		return nil, rep, fmt.Errorf("probe container metrics: %w", err)
	} else if !ok {
		if o.AdaptiveMetrics {
			if missing, err := c.missingLabels(ctx, "container_memory_working_set_bytes", containerLabels, o.Now); err != nil {
				return nil, rep, fmt.Errorf("probe container metrics: %w", err)
			} else if len(missing) > 0 {
				return nil, rep, &LabelsMissingError{Metric: "container_memory_working_set_bytes", Labels: missing, Aggregations: true, Runtime: true}
			}
			if ok, err := c.present(ctx, CAdvisorProbe, o.Now); err != nil {
				return nil, rep, fmt.Errorf("probe container metrics: %w", err)
			} else if ok {
				return nil, rep, fmt.Errorf("the store has no per-container usage metrics (container_memory_working_set_bytes with container and image labels), but cAdvisor is collected (machine_memory_bytes has series): the collector probably dropped container series without a container label, as the Grafana k8s-monitoring chart does by default, because the cluster's container runtime does not label them (older containerd setups, minikube). Collect per-container usage another way, such as container_cpu_usage_seconds_total and container_memory_working_set_bytes from the kubelet's /metrics/resource endpoint; see docs/prometheus-adapter.md, Grafana Cloud")
			}
			return nil, rep, fmt.Errorf("the store has no per-container usage metrics (container_memory_working_set_bytes with container and image labels); the kubelet cAdvisor job is probably not collected")
		}
		if c.OneMetricPerQuery {
			return nil, rep, fmt.Errorf("the store has no per-container usage metrics (container_memory_working_set_bytes with container and image labels); usage from pod-level series cannot be split between containers on a store that reads one metric per query")
		}
		if ok, err := c.present(ctx, PodUsageProbe, o.Now); err != nil {
			return nil, rep, fmt.Errorf("probe pod metrics: %w", err)
		} else if !ok {
			if c.Selector != "" {
				return nil, rep, fmt.Errorf("no container usage metrics (container_memory_working_set_bytes) match the selector; it must match the cAdvisor series as well as the kube-state-metrics series")
			}
			return nil, rep, fmt.Errorf("the store has no container usage metrics (container_memory_working_set_bytes with container and image labels, or with pod labels for the pod cgroup); the kubelet cAdvisor job is probably not scraped")
		}
		usageProbe, rep.UsageFromPods = PodUsageProbe, true
	}
	hasRequests, _ := c.present(ctx, RequestsProbe, o.Now)
	hasOwners, _ := c.present(ctx, OwnerProbe, o.Now)
	if !hasOwners {
		return nil, rep, fmt.Errorf("kube-state-metrics is not in the store (no kube_pod_owner series): pods cannot be mapped to workloads, and requests and replica counts are unavailable, so no cost can be computed")
	}
	if rep.UsageFromPods {
		if ok, _ := c.present(ctx, ContainerProbe, o.Now); !ok {
			return nil, rep, fmt.Errorf("the store has no per-container usage metrics (container_memory_working_set_bytes with container and image labels), and kube_pod_container_info is not in the store to split the pod cgroup series between containers")
		}
		if missing, err := c.missingLabels(ctx, "kube_pod_container_info", []string{"namespace", "pod", "container"}, o.Now); err != nil {
			return nil, rep, fmt.Errorf("probe labels of kube_pod_container_info: %w", err)
		} else if len(missing) > 0 {
			return nil, rep, &LabelsMissingError{Metric: "kube_pod_container_info", Labels: missing}
		}
	} else if !c.OneMetricPerQuery { // UnlabeledPods joins three metrics
		if s, err := c.Query(ctx, UnlabeledPods, o.Now); err == nil && len(s) > 0 && len(s[0].Points) > 0 && s[0].Points[0].V > 0 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%.0f workload pods have pod-level usage but no per-container usage series (their nodes do not label cAdvisor series) and are left out of the usage averages", s[0].Points[0].V))
		}
	}
	hasRestarts, _ := c.present(ctx, RestartsProbe, o.Now)
	hasPhase, _ := c.present(ctx, PhaseProbe, o.Now)
	hasReplicaSets, _ := c.present(ctx, ReplicaSetProbe, o.Now)
	kinds := []string{StatefulSet}
	if hasReplicaSets {
		kinds = []string{Deployment, StatefulSet}
	} else {
		rep.Warnings = append(rep.Warnings, "kube_replicaset_owner is not in the store: Deployments cannot be mapped from their pods and are left out")
	}
	if !hasRequests {
		rep.Warnings = append(rep.Warnings, "kube-state-metrics exports no container requests (kube_pod_container_resource_requests): requests are unavailable and listed as missing")
	} else if !hasPhase {
		hasRequests = false
		rep.Warnings = append(rep.Warnings, "kube_pod_status_phase is not in the store, so requests cannot be limited to running pods: requests are unavailable and listed as missing")
	}

	earliest, full, err := c.EarliestData(ctx, usageProbe, o.Now, maxBack)
	if err != nil {
		return nil, rep, fmt.Errorf("probe history: %w", err)
	}
	rep.Start, rep.FullHistory = earliest, full
	o.Log("history: data from %s (%s) of the %d days asked for", earliest.UTC().Format(time.RFC3339), spanText(o.Now.Sub(earliest)), o.Days)
	if !full {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the store holds about %s of history, less than the %d days asked for", spanText(o.Now.Sub(earliest)), o.Days))
	}
	if o.GKE != "" {
		if w := downsampledWarning(o.Now.Sub(earliest), o.Step); w != "" {
			rep.Warnings = append(rep.Warnings, w)
		}
	}

	rep.NoRequests = !hasRequests
	return &plan{src: sources{requests: hasRequests, podUsage: rep.UsageFromPods, oom: hasRestarts, phase: hasPhase}, kinds: kinds, client: c}, rep, nil
}

func spanText(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%.0f h", d.Hours())
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

type job struct {
	client *Client // o.Client when nil
	name   string
	query  string
	into   func([]Series)
}

// sources says which series the store can provide.
type sources struct {
	requests bool // requests and limits, limited to running pods
	podUsage bool // usage only from the pod cgroup series
	oom      bool // container restarts and termination reasons
	phase    bool // pod phases
}

func pull(ctx context.Context, o Options, start time.Time, src sources, kinds []string) (*pulled, error) {
	p := newPulled()
	shards, warning, err := namespaceShards(ctx, o.Client, o.Now)
	if err != nil {
		return nil, err
	}
	if warning != "" {
		p.warnings = append(p.warnings, warning)
	}
	var jobs []job
	add := func(name, q string, into func([]Series)) {
		jobs = append(jobs, job{name: name, query: q, into: into})
	}
	// Workload queries run once per group of namespaces on a store that
	// truncates large results.
	addNS := func(name, q string, into func([]Series)) {
		if len(shards) == 1 {
			add(name, q, into)
			return
		}
		for i, c := range shards {
			jobs = append(jobs, job{client: c, name: fmt.Sprintf("%s (namespaces %d of %d)", name, i+1, len(shards)), query: q, into: into})
		}
	}
	cs := func(kind string, dst map[cKey][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				dst[cKey{kind, s.Labels["namespace"], s.Labels["workload"], s.Labels["container"]}] = toPoints(s.Points)
			}
		}
	}
	for _, kind := range kinds {
		kind := kind
		if src.podUsage {
			addNS(kind+" cpu usage from pods", PodCPUUsage(kind, o.Step), cs(kind, p.cpuUse))
			addNS(kind+" memory usage from pods", PodMemoryUsage(kind), cs(kind, p.memUse))
		} else {
			addNS(kind+" cpu usage", CPUUsage(kind, o.Step), cs(kind, p.cpuUse))
			addNS(kind+" memory usage", MemoryUsage(kind), cs(kind, p.memUse))
		}
		if src.oom {
			addNS(kind+" oom kills", OOMKills(kind, o.Step), cs(kind, p.oom))
		}
		addNS(kind+" replicas", Replicas(kind), func(ss []Series) {
			for _, s := range ss {
				p.replicas[wKey{kind, s.Labels["namespace"], s.Labels["workload"]}] = toPoints(s.Points)
			}
		})
		if !src.requests {
			continue
		}
		addNS(kind+" cpu requests", Requests(kind, "cpu"), cs(kind, p.cpuReq))
		addNS(kind+" memory requests", Requests(kind, "memory"), cs(kind, p.memReq))
		addNS(kind+" cpu limits", Limits(kind, "cpu"), cs(kind, p.cpuLim))
		addNS(kind+" memory limits", Limits(kind, "memory"), cs(kind, p.memLim))
	}
	nodes := func(dst map[string][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				dst[s.Labels["node"]] = toPoints(s.Points)
			}
		}
	}
	add("node cpu", NodeAllocatable("cpu"), nodes(p.nodeCPU))
	add("node memory", NodeAllocatable("memory"), nodes(p.nodeMem))
	if err := runJobs(ctx, o, start, jobs); err != nil {
		return nil, err
	}
	return p, nil
}

func newPulled() *pulled {
	return &pulled{
		cpuUse: map[cKey][]contract.Point{}, memUse: map[cKey][]contract.Point{}, oom: map[cKey][]contract.Point{},
		cpuReq: map[cKey][]contract.Point{}, memReq: map[cKey][]contract.Point{},
		cpuLim: map[cKey][]contract.Point{}, memLim: map[cKey][]contract.Point{},
		replicas: map[wKey][]contract.Point{},
		nodeCPU:  map[string][]contract.Point{}, nodeMem: map[string][]contract.Point{},
	}
}

// runJobs runs the range queries o.Concurrency at a time. Each job's into
// runs under one lock, so jobs may write to shared maps.
func runJobs(ctx context.Context, o Options, start time.Time, jobs []job) error {
	var mu sync.Mutex
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
			c := j.client
			if c == nil {
				c = o.Client
			}
			ss, err := c.QueryRangeChunked(ctx, j.query, start, o.Now, o.Step, 24*time.Hour)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", j.name, err)
				cancel()
				return
			}
			mu.Lock()
			j.into(ss)
			mu.Unlock()
			o.Log("pulled %s: %d series in %s", j.name, len(ss), time.Since(t0).Round(time.Millisecond))
		}()
	}
	wg.Wait()
	close(errs)
	// The first error is the cause; the rest are cancellations that followed it.
	return <-errs
}

func toPoints(in []Point) []contract.Point {
	out := make([]contract.Point, len(in))
	for i, p := range in {
		v := p.V
		if v < 0 {
			v = 0
		}
		out[i] = contract.Point{T: p.T, V: v}
	}
	return out
}

func assemble(ctx context.Context, o Options, p *pulled, requests bool, rep *Report) (*contract.File, error) {
	step := int(o.Step / time.Second)
	source := "prometheus"
	if p.gke {
		source = "gke"
	}
	base := contract.Common{Cluster: o.Cluster, Source: source, StepSeconds: step}

	// A container is reported when it has usage. Requests the store does not
	// carry for a container with usage are zero requests, but only at times
	// when kube-state-metrics was answering (the workload has a replica count).
	keys := map[cKey]bool{}
	for k := range p.cpuUse {
		keys[k] = true
	}
	for k := range p.memUse {
		keys[k] = true
	}
	ordered := make([]cKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool { return fmt.Sprint(ordered[i]) < fmt.Sprint(ordered[j]) })

	f := &contract.File{}
	haveWorkload := map[wKey]bool{}
	for _, k := range ordered {
		r := contract.Resource{Common: base, Namespace: k.ns, Kind: k.kind, Workload: k.workload, Container: k.container}
		if rep.UsageFromPods {
			r.Notes = []string{UsageFromPodsNote}
		}
		wk := wKey{k.kind, k.ns, k.workload}
		add := func(name string, pts []contract.Point, usage []contract.Point) {
			if name == contract.CPURequest || name == contract.MemoryRequest {
				pts = fillRequests(pts, usage, p.replicas[wk])
			}
			if len(pts) == 0 {
				if spec, _ := contract.SpecOf(name); spec.Required {
					r.Missing = append(r.Missing, name)
				}
				return
			}
			r.Metrics = append(r.Metrics, contract.Series{Name: name, Points: pts})
		}
		add(contract.CPUUsage, p.cpuUse[k], nil)
		add(contract.MemoryWorkingSet, p.memUse[k], nil)
		if requests {
			add(contract.CPURequest, p.cpuReq[k], p.cpuUse[k])
			add(contract.MemoryRequest, p.memReq[k], p.memUse[k])
			optional := func(name string, pts []contract.Point) {
				if len(pts) > 0 {
					r.Metrics = append(r.Metrics, contract.Series{Name: name, Points: pts})
				}
			}
			optional(contract.CPULimit, p.cpuLim[k])
			optional(contract.MemoryLimit, p.memLim[k])
		} else {
			r.Missing = append(r.Missing, contract.CPURequest, contract.MemoryRequest)
		}
		if pts := p.oom[k]; len(pts) > 0 {
			r.Metrics = append(r.Metrics, contract.Series{Name: contract.OOMKills, Points: pts})
			for _, pt := range pts {
				rep.OOMKills += int(math.Round(pt.V))
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
		if pts := p.replicas[k]; len(pts) > 0 {
			r.Metrics = []contract.Series{{Name: name, Points: pts}}
		} else {
			r.Missing = []string{name}
		}
		r.Tier = tierOf(step, r.Metrics)
		f.Resources = append(f.Resources, r)
		rep.Workloads++
	}

	nodes, err := nodeResources(ctx, o, p, base)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "node details unavailable: "+err.Error())
	}
	f.Resources = append(f.Resources, nodes...)
	rep.Nodes = len(nodes)
	if len(p.nodeCPU) == 0 && len(p.nodeMem) == 0 {
		metric := "kube_node_status_allocatable"
		if p.gke {
			metric = gkeNodeCPU
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("no node allocatable series (%s): node resources are left out and workloads are priced with the default node", metric))
	}
	if rep.Containers == 0 {
		if p.gke {
			return nil, fmt.Errorf("no workload containers found: GKE system metrics exist but none belong to a Deployment or StatefulSet")
		}
		return nil, fmt.Errorf("no workload containers found: usage series exist but none map to a Deployment or StatefulSet through kube-state-metrics owner metrics")
	}
	return f, nil
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

// fillRequests adds a zero request at every usage timestamp where the
// workload has a replica count but the store has no request series. That is a
// container with no request set, or the time before one was set.
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

var nonLabel = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func labelKey(k string) string { return "label_" + nonLabel.ReplaceAllString(k, "_") }

func firstLabel(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[labelKey(k)]; v != "" {
			return v
		}
	}
	return ""
}

func nodeResources(ctx context.Context, o Options, p *pulled, base contract.Common) ([]contract.Resource, error) {
	if p.gke {
		return nodesFrom(o, p, base, func(n string) *contract.Node {
			if a := p.gkeNodes[n]; a != nil {
				return a
			}
			return nodeAttrs(n, nil, nil)
		}), nil
	}
	window := time.Duration(o.Days) * 24 * time.Hour
	info, err := o.Client.queryWindow(ctx, func(w time.Duration) string { q, _ := NodeInfoAt(w); return q }, o.Now, window)
	if err != nil {
		return nil, err
	}
	labels, err := o.Client.queryWindow(ctx, func(w time.Duration) string { _, q := NodeInfoAt(w); return q }, o.Now, window)
	if err != nil {
		return nil, err
	}
	infoBy := map[string]map[string]string{}
	for _, s := range info {
		infoBy[s.Labels["node"]] = s.Labels
	}
	labelsBy := map[string]map[string]string{}
	for _, s := range labels {
		labelsBy[s.Labels["node"]] = s.Labels
	}
	return nodesFrom(o, p, base, func(n string) *contract.Node { return nodeAttrs(n, infoBy[n], labelsBy[n]) }), nil
}

// nodesFrom makes a node resource for every node with an allocatable series.
func nodesFrom(o Options, p *pulled, base contract.Common, attrs func(name string) *contract.Node) []contract.Resource {
	names := map[string]bool{}
	for n := range p.nodeCPU {
		names[n] = true
	}
	for n := range p.nodeMem {
		names[n] = true
	}
	var out []contract.Resource
	for n := range names {
		r := contract.Resource{Common: base, Node: attrs(n)}
		for _, m := range []struct {
			name string
			pts  []contract.Point
		}{{contract.NodeCPU, p.nodeCPU[n]}, {contract.NodeMemory, p.nodeMem[n]}} {
			if len(m.pts) == 0 {
				r.Missing = append(r.Missing, m.name)
				continue
			}
			r.Metrics = append(r.Metrics, contract.Series{Name: m.name, Points: m.pts})
		}
		r.Tier = tierOf(int(o.Step/time.Second), r.Metrics)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node.Name < out[j].Node.Name })
	return out
}

func nodeAttrs(name string, info, labels map[string]string) *contract.Node {
	n := &contract.Node{Name: name, InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown"}
	if v := firstLabel(labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"); v != "" {
		n.InstanceType = v
	}
	if v := firstLabel(labels, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"); v != "" {
		n.Region = v
	}
	pid := info["provider_id"]
	switch {
	case strings.HasPrefix(pid, "aws:"):
		n.Provider = "aws"
	case strings.HasPrefix(pid, "gce:"):
		n.Provider = "gcp"
	case strings.HasPrefix(pid, "azure:"):
		n.Provider = "azure"
	}
	n.CapacityType = capacityType(labels)
	return n
}

func capacityType(l map[string]string) string {
	if k := strings.ToLower(firstLabel(l, "karpenter.sh/capacity-type")); strings.Contains(k, "spot") || strings.Contains(k, "preemptible") {
		return "spot"
	}
	switch strings.ToLower(firstLabel(l, "eks.amazonaws.com/capacityType", "karpenter.sh/capacity-type", "node.kubernetes.io/capacity-type")) {
	case "spot":
		return "spot"
	case "on_demand", "on-demand", "ondemand":
		return "on_demand"
	}
	if strings.EqualFold(firstLabel(l, "cloud.google.com/gke-spot"), "true") || strings.EqualFold(firstLabel(l, "cloud.google.com/gke-preemptible"), "true") ||
		firstLabel(l, "cloud.google.com/gke-provisioning") == "spot" ||
		strings.EqualFold(firstLabel(l, "kubernetes.azure.com/scalesetpriority"), "spot") {
		return "spot"
	}
	return "unknown"
}

// oneCluster fails when a cluster label has more than one value in any of the
// ClusterValues queries.
func oneCluster(ctx context.Context, c *Client, now time.Time, window time.Duration) error {
	for i, q := range ClusterValues(window, c.ClusterLabels...) {
		var ss []Series
		var err error
		if i == 0 {
			// Only the node info query has a range selector.
			ss, err = c.queryWindow(ctx, func(w time.Duration) string { return ClusterValues(w, c.ClusterLabels...)[0] }, now, window)
		} else {
			ss, err = c.Query(ctx, q, now)
		}
		if err != nil {
			return fmt.Errorf("probe cluster labels: %w", err)
		}
		for _, l := range append(append([]string(nil), ClusterLabels...), c.ClusterLabels...) {
			seen := map[string]bool{}
			for _, s := range ss {
				seen[s.Labels[l]] = true
			}
			if len(seen) > 1 {
				vals := make([]string, 0, len(seen))
				for v := range seen {
					vals = append(vals, v)
				}
				sort.Strings(vals)
				return &ClusterError{Label: l, Values: vals, Selected: c.Selector != ""}
			}
		}
	}
	return nil
}
