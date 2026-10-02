package prometheus

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// Metrics read from a Google Managed Prometheus project.
const (
	GKEAuto       = "auto"       // Managed Prometheus series when they are complete, else GKE system metrics
	GKEPrometheus = "prometheus" // cAdvisor and kube-state-metrics series, as from any store
	GKESystem     = "system"     // GKE system metrics
)

// GKEModes lists the values Options.GKE accepts.
var GKEModes = []string{GKEAuto, GKEPrometheus, GKESystem}

// GKE system metrics under their PromQL names. Cloud Monitoring writes
// kubernetes.io/container/cpu/core_usage_time as
// kubernetes_io:container_cpu_core_usage_time.
const (
	gkeCPU       = "kubernetes_io:container_cpu_core_usage_time"
	gkeMemory    = "kubernetes_io:container_memory_used_bytes"
	gkeCPUReq    = "kubernetes_io:container_cpu_request_cores"
	gkeMemoryReq = "kubernetes_io:container_memory_request_bytes"
	gkeCPULim    = "kubernetes_io:container_cpu_limit_cores"
	gkeMemoryLim = "kubernetes_io:container_memory_limit_bytes"
	gkeNodeCPU   = "kubernetes_io:node_cpu_allocatable_cores"
	gkeNodeMem   = "kubernetes_io:node_memory_allocatable_bytes"
)

// Labels of GKE system metrics: the k8s_container and k8s_node resource
// labels, and the system metadata that names a pod's top-level controller (a
// Deployment rather than its ReplicaSet).
const (
	gkeController     = "metadata_system_top_level_controller_name"
	gkeControllerKind = "metadata_system_top_level_controller_type"
	gkeContainerRes   = `monitored_resource="k8s_container"`
	gkeNodeRes        = `monitored_resource="k8s_node"`
	// The non-evictable part of used memory is what the kubelet reports as
	// the working set.
	gkeWorkingSet = `memory_type="non-evictable"`
)

// GKE probes.
const (
	GKEUsageProbe      = `count(` + gkeMemory + `{` + gkeContainerRes + `,` + gkeWorkingSet + `})`
	GKEControllerProbe = `count(` + gkeMemory + `{` + gkeContainerRes + `,` + gkeWorkingSet + `,` + gkeController + `!=""})`
	GKERequestsProbe   = `count(` + gkeCPUReq + `{` + gkeContainerRes + `})`
	ReplicasProbe      = `count(kube_deployment_spec_replicas{}) or count(kube_statefulset_replicas{})`
	// ExportedNamespaceProbe finds kube-state-metrics series whose namespace
	// label Managed Prometheus replaced with the scrape target's.
	ExportedNamespaceProbe = `count(kube_pod_owner{exported_namespace!=""})`
)

// GKEFullResolution is how long Cloud Monitoring keeps GKE system metrics and
// Managed Prometheus samples at full resolution. Older data is kept at
// GKEDownsampledStep.
const (
	GKEFullResolution  = 6 * 7 * 24 * time.Hour
	GKEDownsampledStep = 10 * time.Minute
)

// GKERequestLag is how long GKE request metrics can take to become
// queryable. The pull ends this long before now so the last steps are not
// left without requests.
const GKERequestLag = 3 * time.Minute

func gkeKind(kind string) string {
	if kind == StatefulSet {
		return "StatefulSet"
	}
	return "Deployment"
}

// gkeContainer selects a workload kind's application containers.
func gkeContainer(kind string) string {
	return fmt.Sprintf(`%s,%s=%q,container_name!=""`, gkeContainerRes, gkeControllerKind, gkeKind(kind))
}

// gkePerPod averages a per-container expression over a workload's pods. The
// inner max keeps one series per container of a pod.
func gkePerPod(expr string) string {
	return fmt.Sprintf(`avg by (namespace_name, %[1]s, container_name) (
  max by (namespace_name, pod_name, container_name, %[1]s) (%[2]s)
)`, gkeController, expr)
}

// GKECPUUsage is the mean CPU use per pod, in cores, for each workload container.
func GKECPUUsage(kind string, step time.Duration) string {
	return gkePerPod(fmt.Sprintf(`rate(%s{%s}[%ds])`, gkeCPU, gkeContainer(kind), seconds(rateWindow(step))))
}

// GKEMemoryUsage is the mean non-evictable memory per pod, in bytes.
func GKEMemoryUsage(kind string) string {
	return gkePerPod(fmt.Sprintf(`%s{%s,%s}`, gkeMemory, gkeContainer(kind), gkeWorkingSet))
}

// GKERequests is the mean per-pod request for a resource ("cpu" or "memory").
func GKERequests(kind, resource string) string {
	m := gkeCPUReq
	if resource == "memory" {
		m = gkeMemoryReq
	}
	return gkePerPod(fmt.Sprintf(`%s{%s}`, m, gkeContainer(kind)))
}

// GKELimits is the mean per-pod limit for a resource.
func GKELimits(kind, resource string) string {
	m := gkeCPULim
	if resource == "memory" {
		m = gkeMemoryLim
	}
	return gkePerPod(fmt.Sprintf(`%s{%s}`, m, gkeContainer(kind)))
}

// GKEReplicas is the desired replica count of each workload, from GKE's
// managed kube-state-metrics. Managed Prometheus renames a metric's namespace
// label to exported_namespace when it differs from the namespace of the
// scrape target, so that label wins when present.
func GKEReplicas(kind string) string {
	metric, label := "kube_deployment_spec_replicas", "deployment"
	if kind == StatefulSet {
		metric, label = "kube_statefulset_replicas", "statefulset"
	}
	return fmt.Sprintf(`max by (namespace, workload) (label_replace(label_replace(%s{}, "workload", "$1", %q, "(.+)"), "namespace", "$1", "exported_namespace", "(.+)"))`,
		metric, label)
}

// GKENodeAllocatable is the allocatable amount of a resource per node.
func GKENodeAllocatable(resource string) string {
	m := gkeNodeCPU
	if resource == "memory" {
		m = gkeNodeMem
	}
	return fmt.Sprintf(`max by (node_name, location) (%s{%s})`, m, gkeNodeRes)
}

// GKEClusterValues returns queries that count series by cluster and location:
// node allocatable over the window, and container memory at the query time.
func GKEClusterValues(window time.Duration) []string {
	return []string{
		fmt.Sprintf(`count by (cluster_name, location) (last_over_time(%s{%s}[%ds]))`, gkeNodeCPU, gkeNodeRes, seconds(window)),
		fmt.Sprintf(`count by (cluster_name, location) (%s{%s,%s})`, gkeMemory, gkeContainerRes, gkeWorkingSet),
	}
}

// gkeMode works out which metrics to read from a Managed Prometheus project.
func gkeMode(ctx context.Context, c *Client, mode string, now time.Time) (string, error) {
	if mode != GKEAuto {
		return mode, nil
	}
	prom := gkeClient(c, "cluster")
	usage, err := prom.present(ctx, UsageProbe, now)
	if err != nil {
		return "", fmt.Errorf("probe container metrics: %w", err)
	}
	if !usage {
		if usage, err = prom.present(ctx, PodUsageProbe, now); err != nil {
			return "", fmt.Errorf("probe container metrics: %w", err)
		}
	}
	owners, err := prom.present(ctx, OwnerProbe, now)
	if err != nil {
		return "", fmt.Errorf("probe kube-state-metrics: %w", err)
	}
	if usage && owners {
		return GKEPrometheus, nil
	}
	sys, err := gkeClient(c, "cluster_name").present(ctx, GKEUsageProbe, now)
	if err != nil {
		return "", fmt.Errorf("probe GKE system metrics: %w", err)
	}
	if sys {
		return GKESystem, nil
	}
	return "", fmt.Errorf("the project has neither GKE system metrics (%s) nor cAdvisor and kube-state-metrics series (container_memory_working_set_bytes and kube_pod_owner) from Managed Prometheus; check the project, that system metrics are enabled on the cluster, and that the selector matches it", gkeMemory)
}

// gkeClient is c with the cluster matchers of its selector renamed to the
// label the metrics use: cluster_name on GKE system metrics, cluster on
// Managed Prometheus series. Either name can then be given in --selector.
func gkeClient(c *Client, clusterLabel string) *Client {
	cc := *c
	cc.Selector = renameMatchers(c.Selector, map[string]string{"cluster": clusterLabel, "cluster_name": clusterLabel})
	return &cc
}

// renameMatchers renames the labels of canonical matchers, as ParseSelector
// returns them.
func renameMatchers(sel string, names map[string]string) string {
	if sel == "" {
		return ""
	}
	var out []string
	for rest := sel; rest != ""; {
		m, r, err := parseMatcher(rest)
		if err != nil {
			return sel
		}
		i := strings.IndexAny(m, "=!")
		if to, ok := names[m[:i]]; ok {
			m = to + m[i:]
		}
		out = append(out, m)
		rest = strings.TrimPrefix(strings.TrimSpace(r), ",")
	}
	return strings.Join(out, ",")
}

// runGKE pulls GKE system metrics from the Cloud Monitoring PromQL API.
func runGKE(ctx context.Context, o Options, rep *Report) (*contract.File, *Report, error) {
	sys := gkeClient(o.Client, "cluster_name")
	ksm := gkeClient(o.Client, "cluster")
	o.Now = o.Now.Add(-GKERequestLag)
	rep.End, rep.GKESystemMetrics = o.Now, true
	maxBack := time.Duration(o.Days) * 24 * time.Hour

	if ok, err := sys.present(ctx, GKEUsageProbe, o.Now); err != nil {
		return nil, rep, fmt.Errorf("probe GKE system metrics: %w", err)
	} else if !ok {
		if sys.Selector != "" {
			return nil, rep, fmt.Errorf("no GKE system metrics (%s) match the selector", gkeMemory)
		}
		return nil, rep, fmt.Errorf("the project has no GKE system metrics (%s): system metrics may be turned off on the cluster", gkeMemory)
	}
	if err := gkeOneCluster(ctx, sys, o.Now, maxBack); err != nil {
		return nil, rep, err
	}
	if ok, err := sys.present(ctx, GKEControllerProbe, o.Now); err != nil {
		return nil, rep, fmt.Errorf("probe GKE system metrics: %w", err)
	} else if !ok {
		return nil, rep, fmt.Errorf("GKE system metrics carry no %s label, so containers cannot be mapped to their Deployment or StatefulSet", gkeController)
	}
	hasRequests, err := sys.present(ctx, GKERequestsProbe, o.Now)
	if err != nil {
		return nil, rep, fmt.Errorf("probe GKE request metrics: %w", err)
	}
	if !hasRequests {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("no container requests in the GKE system metrics (%s): requests are unavailable and listed as missing", gkeCPUReq))
	}
	hasReplicas, err := ksm.present(ctx, ReplicasProbe, o.Now)
	if err != nil {
		return nil, rep, fmt.Errorf("probe kube-state-metrics: %w", err)
	}
	if !hasReplicas {
		rep.Warnings = append(rep.Warnings, "no replica counts (kube_deployment_spec_replicas, kube_statefulset_replicas): GKE's managed kube-state-metrics is not enabled or not collected, so replica counts are listed as missing")
	}

	earliest, full, err := sys.EarliestData(ctx, GKEUsageProbe, o.Now, maxBack)
	if err != nil {
		return nil, rep, fmt.Errorf("probe history: %w", err)
	}
	rep.Start, rep.FullHistory = earliest, full
	o.Log("history: data from %s (%s) of the %d days asked for", earliest.UTC().Format(time.RFC3339), spanText(o.Now.Sub(earliest)), o.Days)
	if !full {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the project holds about %s of history, less than the %d days asked for", spanText(o.Now.Sub(earliest)), o.Days))
	}
	if w := downsampledWarning(o.Now.Sub(earliest), o.Step); w != "" {
		rep.Warnings = append(rep.Warnings, w)
	}

	p := newPulled()
	p.gke, p.gkeNodes = true, map[string]*contract.Node{}
	var jobs []job
	add := func(c *Client, name, q string, into func([]Series)) {
		jobs = append(jobs, job{client: c, name: name, query: q, into: into})
	}
	cs := func(kind string, dst map[cKey][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				dst[cKey{kind, s.Labels["namespace_name"], s.Labels[gkeController], s.Labels["container_name"]}] = toPoints(s.Points)
			}
		}
	}
	for _, kind := range []string{Deployment, StatefulSet} {
		kind := kind
		add(sys, kind+" cpu usage", GKECPUUsage(kind, o.Step), cs(kind, p.cpuUse))
		add(sys, kind+" memory usage", GKEMemoryUsage(kind), cs(kind, p.memUse))
		if hasReplicas {
			add(ksm, kind+" replicas", GKEReplicas(kind), func(ss []Series) {
				for _, s := range ss {
					p.replicas[wKey{kind, s.Labels["namespace"], s.Labels["workload"]}] = toPoints(s.Points)
				}
			})
		}
		if !hasRequests {
			continue
		}
		add(sys, kind+" cpu requests", GKERequests(kind, "cpu"), cs(kind, p.cpuReq))
		add(sys, kind+" memory requests", GKERequests(kind, "memory"), cs(kind, p.memReq))
		add(sys, kind+" cpu limits", GKELimits(kind, "cpu"), cs(kind, p.cpuLim))
		add(sys, kind+" memory limits", GKELimits(kind, "memory"), cs(kind, p.memLim))
	}
	nodes := func(dst map[string][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				n := s.Labels["node_name"]
				dst[n] = toPoints(s.Points)
				p.gkeNodes[n] = gkeNodeAttrs(n, s.Labels["location"])
			}
		}
	}
	add(sys, "node cpu", GKENodeAllocatable("cpu"), nodes(p.nodeCPU))
	add(sys, "node memory", GKENodeAllocatable("memory"), nodes(p.nodeMem))
	if err := runJobs(ctx, o, rep.Start, jobs); err != nil {
		return nil, rep, err
	}
	f, err := assemble(ctx, o, p, hasRequests, rep)
	if err != nil {
		return nil, rep, err
	}
	return f, rep, nil
}

// downsampledWarning warns when part of the history is older than Cloud
// Monitoring keeps at full resolution and the step is finer than the
// downsampled data.
func downsampledWarning(span, step time.Duration) string {
	if span <= GKEFullResolution || step >= GKEDownsampledStep {
		return ""
	}
	return fmt.Sprintf("Cloud Monitoring keeps about %d days at full resolution and older data at %s steps, so at a %s step the older days have gaps; pull at most %d days, or use --step %s",
		int(GKEFullResolution.Hours()/24), shortStep(GKEDownsampledStep), shortStep(step), int(GKEFullResolution.Hours()/24), shortStep(GKEDownsampledStep))
}

func shortStep(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// gkeOneCluster fails when the system metrics hold more than one cluster.
func gkeOneCluster(ctx context.Context, c *Client, now time.Time, window time.Duration) error {
	for i, q := range GKEClusterValues(window) {
		var ss []Series
		var err error
		if i == 0 {
			ss, err = c.queryWindow(ctx, func(w time.Duration) string { return GKEClusterValues(w)[0] }, now, window)
		} else {
			ss, err = c.Query(ctx, q, now)
		}
		if err != nil {
			return fmt.Errorf("probe cluster labels: %w", err)
		}
		for _, l := range []string{"cluster_name", "location"} {
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

var gkeZone = regexp.MustCompile(`^([a-z]+-[a-z]+[0-9]+)-[a-z]$`)

// gkeNodeAttrs describes a node from its system metric labels. The location
// is the cluster's zone or region; the instance type and capacity type are
// not in the metrics.
func gkeNodeAttrs(name, location string) *contract.Node {
	n := nodeAttrs(name, nil, nil)
	n.Provider = "gcp"
	if m := gkeZone.FindStringSubmatch(location); m != nil {
		n.Region = m[1]
	} else if location != "" {
		n.Region = location
	}
	return n
}
