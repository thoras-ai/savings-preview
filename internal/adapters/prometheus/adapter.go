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
}

// Report describes what a pull found and what it could not get.
type Report struct {
	Version     string
	Start, End  time.Time
	FullHistory bool // the store holds at least Days of data
	// UsageFromPods is set when the store has no per-container usage and
	// container usage was split from the pod cgroup series by requests.
	UsageFromPods bool
	Containers    int
	Workloads     int
	Nodes         int
	OOMKills      int
	Warnings      []string
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
			len(e.Values), e.Label, strings.Join(shown, ", "), e.Label, strconv.Quote(pick))
	}
	return fmt.Sprintf("the store holds %d clusters (label %s: %s); their workloads would be averaged together. Choose one with --selector '%s=%s'",
		len(e.Values), e.Label, strings.Join(shown, ", "), e.Label, strconv.Quote(pick))
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
}

// Run pulls the history from the store and assembles the contract file.
func Run(ctx context.Context, o Options) (*contract.File, *Report, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 2
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	c := o.Client
	rep := &Report{Version: c.Build(ctx), End: o.Now}

	if err := c.Check(ctx); err != nil {
		return nil, rep, err
	}
	maxBack := time.Duration(o.Days) * 24 * time.Hour
	if err := oneCluster(ctx, c, o.Now, maxBack); err != nil {
		return nil, rep, err
	}
	usageProbe := UsageProbe
	if ok, err := c.present(ctx, UsageProbe, o.Now); err != nil {
		return nil, rep, fmt.Errorf("probe container metrics: %w", err)
	} else if !ok {
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
	} else if s, err := c.Query(ctx, UnlabeledPods, o.Now); err == nil && len(s) > 0 && len(s[0].Points) > 0 && s[0].Points[0].V > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%.0f workload pods have pod-level usage but no per-container usage series (their nodes do not label cAdvisor series) and are left out of the usage averages", s[0].Points[0].V))
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

	p, err := pull(ctx, o, rep.Start, sources{requests: hasRequests, podUsage: rep.UsageFromPods, oom: hasRestarts}, kinds)
	if err != nil {
		return nil, rep, err
	}
	f, err := assemble(ctx, o, p, hasRequests, rep)
	if err != nil {
		return nil, rep, err
	}
	return f, rep, nil
}

func spanText(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%.0f h", d.Hours())
	}
	return fmt.Sprintf("%.1f days", d.Hours()/24)
}

type job struct {
	name  string
	query string
	into  func([]Series)
}

// sources says which series the store can provide.
type sources struct {
	requests bool // requests and limits, limited to running pods
	podUsage bool // usage only from the pod cgroup series
	oom      bool // container restarts and termination reasons
}

func pull(ctx context.Context, o Options, start time.Time, src sources, kinds []string) (*pulled, error) {
	p := &pulled{
		cpuUse: map[cKey][]contract.Point{}, memUse: map[cKey][]contract.Point{}, oom: map[cKey][]contract.Point{},
		cpuReq: map[cKey][]contract.Point{}, memReq: map[cKey][]contract.Point{},
		cpuLim: map[cKey][]contract.Point{}, memLim: map[cKey][]contract.Point{},
		replicas: map[wKey][]contract.Point{},
		nodeCPU:  map[string][]contract.Point{}, nodeMem: map[string][]contract.Point{},
	}
	var mu sync.Mutex
	var jobs []job
	add := func(name, q string, into func([]Series)) {
		jobs = append(jobs, job{name: name, query: q, into: into})
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
			add(kind+" cpu usage from pods", PodCPUUsage(kind, o.Step), cs(kind, p.cpuUse))
			add(kind+" memory usage from pods", PodMemoryUsage(kind), cs(kind, p.memUse))
		} else {
			add(kind+" cpu usage", CPUUsage(kind, o.Step), cs(kind, p.cpuUse))
			add(kind+" memory usage", MemoryUsage(kind), cs(kind, p.memUse))
		}
		if src.oom {
			add(kind+" oom kills", OOMKills(kind, o.Step), cs(kind, p.oom))
		}
		add(kind+" replicas", Replicas(kind), func(ss []Series) {
			for _, s := range ss {
				p.replicas[wKey{kind, s.Labels["namespace"], s.Labels["workload"]}] = toPoints(s.Points)
			}
		})
		if !src.requests {
			continue
		}
		add(kind+" cpu requests", Requests(kind, "cpu"), cs(kind, p.cpuReq))
		add(kind+" memory requests", Requests(kind, "memory"), cs(kind, p.memReq))
		add(kind+" cpu limits", Limits(kind, "cpu"), cs(kind, p.cpuLim))
		add(kind+" memory limits", Limits(kind, "memory"), cs(kind, p.memLim))
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
			ss, err := o.Client.QueryRangeChunked(ctx, j.query, start, o.Now, o.Step, 24*time.Hour)
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
	if err := <-errs; err != nil {
		return nil, err
	}
	return p, nil
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
	base := contract.Common{Cluster: o.Cluster, Source: "prometheus", StepSeconds: step}

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
		rep.Warnings = append(rep.Warnings, "no node allocatable series (kube_node_status_allocatable): node resources are left out and workloads are priced with the default node")
	}
	if rep.Containers == 0 {
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
	window := time.Duration(o.Days) * 24 * time.Hour
	qi, ql := NodeInfoAt(window)
	info, err := o.Client.Query(ctx, qi, o.Now)
	if err != nil {
		return nil, err
	}
	labels, err := o.Client.Query(ctx, ql, o.Now)
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
	names := map[string]bool{}
	for n := range p.nodeCPU {
		names[n] = true
	}
	for n := range p.nodeMem {
		names[n] = true
	}
	var out []contract.Resource
	for n := range names {
		r := contract.Resource{Common: base, Node: nodeAttrs(n, infoBy[n], labelsBy[n])}
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
	return out, nil
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
	switch strings.ToLower(firstLabel(l, "eks.amazonaws.com/capacityType", "karpenter.sh/capacity-type", "node.kubernetes.io/capacity-type")) {
	case "spot":
		return "spot"
	case "on_demand", "on-demand", "ondemand":
		return "on_demand"
	}
	if firstLabel(l, "cloud.google.com/gke-spot") == "true" || firstLabel(l, "cloud.google.com/gke-provisioning") == "spot" ||
		strings.EqualFold(firstLabel(l, "kubernetes.azure.com/scalesetpriority"), "spot") {
		return "spot"
	}
	return "unknown"
}

// oneCluster fails when a cluster label has more than one value in any of the
// ClusterValues queries.
func oneCluster(ctx context.Context, c *Client, now time.Time, window time.Duration) error {
	for _, q := range ClusterValues(window) {
		ss, err := c.Query(ctx, q, now)
		if err != nil {
			return fmt.Errorf("probe cluster labels: %w", err)
		}
		for _, l := range ClusterLabels {
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
