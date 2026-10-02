package prometheus

import (
	"context"
	"fmt"
	"regexp"
	"sort"
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
	Containers  int
	Workloads   int
	Nodes       int
	Warnings    []string
}

type cKey struct{ kind, ns, workload, container string }
type wKey struct{ kind, ns, workload string }

type pulled struct {
	cpuUse, memUse, cpuReq, memReq, cpuLim, memLim map[cKey][]contract.Point
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
	if ok, err := c.present(ctx, UsageProbe, o.Now); err != nil {
		return nil, rep, fmt.Errorf("probe container metrics: %w", err)
	} else if !ok {
		return nil, rep, fmt.Errorf("the store has no per-container usage metrics (container_memory_working_set_bytes with container and image labels); the kubelet cAdvisor job is probably not scraped, or the container runtime does not expose container labels")
	}
	hasRequests, _ := c.present(ctx, RequestsProbe, o.Now)
	hasOwners, _ := c.present(ctx, OwnerProbe, o.Now)
	if !hasOwners {
		return nil, rep, fmt.Errorf("kube-state-metrics is not in the store (no kube_pod_owner series): pods cannot be mapped to workloads, and requests and replica counts are unavailable, so no cost can be computed")
	}
	if !hasRequests {
		rep.Warnings = append(rep.Warnings, "kube-state-metrics exports no container requests (kube_pod_container_resource_requests): requests are unavailable and listed as missing")
	}

	maxBack := time.Duration(o.Days) * 24 * time.Hour
	earliest, full, err := c.EarliestData(ctx, UsageProbe, o.Now, maxBack)
	if err != nil {
		return nil, rep, fmt.Errorf("probe history: %w", err)
	}
	rep.Start, rep.FullHistory = earliest, full
	o.Log("history: data from %s (%s) of the %d days asked for", earliest.UTC().Format(time.RFC3339), spanText(o.Now.Sub(earliest)), o.Days)
	if !full {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the store holds about %s of history, less than the %d days asked for", spanText(o.Now.Sub(earliest)), o.Days))
	}

	p, err := pull(ctx, o, rep.Start, hasRequests)
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
	query string
	into  func([]Series)
}

func pull(ctx context.Context, o Options, start time.Time, requests bool) (*pulled, error) {
	p := &pulled{
		cpuUse: map[cKey][]contract.Point{}, memUse: map[cKey][]contract.Point{},
		cpuReq: map[cKey][]contract.Point{}, memReq: map[cKey][]contract.Point{},
		cpuLim: map[cKey][]contract.Point{}, memLim: map[cKey][]contract.Point{},
		replicas: map[wKey][]contract.Point{},
		nodeCPU:  map[string][]contract.Point{}, nodeMem: map[string][]contract.Point{},
	}
	var mu sync.Mutex
	var jobs []job
	add := func(name, q string, into func([]Series)) {
		jobs = append(jobs, job{query: name + "\x00" + q, into: into})
	}
	cs := func(kind string, dst map[cKey][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				dst[cKey{kind, s.Labels["namespace"], s.Labels["workload"], s.Labels["container"]}] = toPoints(s.Points)
			}
		}
	}
	for _, kind := range []string{Deployment, StatefulSet} {
		kind := kind
		add(kind+" cpu usage", CPUUsage(kind, o.Step), cs(kind, p.cpuUse))
		add(kind+" memory usage", MemoryUsage(kind), cs(kind, p.memUse))
		add(kind+" replicas", Replicas(kind), func(ss []Series) {
			for _, s := range ss {
				p.replicas[wKey{kind, s.Labels["namespace"], s.Labels["workload"]}] = toPoints(s.Points)
			}
		})
		if !requests {
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
			name, q, _ := strings.Cut(j.query, "\x00")
			t0 := time.Now()
			ss, err := o.Client.QueryRangeChunked(ctx, q, start, o.Now, o.Step, 24*time.Hour)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", name, err)
				return
			}
			mu.Lock()
			j.into(ss)
			mu.Unlock()
			o.Log("pulled %s: %d series in %s", name, len(ss), time.Since(t0).Round(time.Millisecond))
		}()
	}
	wg.Wait()
	close(errs)
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
		wk := wKey{k.kind, k.ns, k.workload}
		add := func(name string, pts []contract.Point, usage []contract.Point) {
			if len(pts) == 0 {
				if name == contract.CPURequest || name == contract.MemoryRequest {
					pts = zeroFill(usage, p.replicas[wk])
				}
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

// zeroFill returns zero points at the usage timestamps where the workload has
// a replica count.
func zeroFill(usage, replicas []contract.Point) []contract.Point {
	if len(usage) == 0 || len(replicas) == 0 {
		return nil
	}
	have := make(map[int64]bool, len(replicas))
	for _, r := range replicas {
		have[r.T] = true
	}
	var out []contract.Point
	for _, u := range usage {
		if have[u.T] {
			out = append(out, contract.Point{T: u.T})
		}
	}
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
