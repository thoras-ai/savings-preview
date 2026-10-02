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

// Stores such as Azure Monitor managed Prometheus reject a query that reads
// more than one metric, so the joins of pull are made here instead. Each
// query reads one metric and aggregates it on the store by an owner key
// taken from the pod name. The owner key is the pod name without its last
// dash-separated part: the ReplicaSet of a Deployment's pod and the
// StatefulSet of a StatefulSet's pod. kube_pod_owner and
// kube_replicaset_owner, read separately, map each owner key to its workload.
// A pod keeps its owner for life, so this mapping does not change over time.

const ownerKeyLabel = "owner_key"

const ownerKeyPattern = `(.+)-[^-]+`

var ownerKeyRE = regexp.MustCompile("^" + ownerKeyPattern + "$")

// ownerKey is the owner key of a pod name, empty when it has none.
func ownerKey(pod string) string {
	if m := ownerKeyRE.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	return ""
}

// byOwnerKey aggregates a per-pod expression by namespace, owner key and the
// given labels.
func byOwnerKey(agg, expr string, by ...string) string {
	labels := strings.Join(append([]string{"namespace", ownerKeyLabel}, by...), ", ")
	return fmt.Sprintf(`%s by (%s) (label_replace(%s, %q, "$1", "pod", %q))`, agg, labels, expr, ownerKeyLabel, ownerKeyPattern)
}

// Per-pod expressions that each read one metric.
func podCPU(step time.Duration) string {
	return fmt.Sprintf(`max by (namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s}[%ds]))`, appContainers, seconds(rateWindow(step)))
}

const podMemory = `max by (namespace, pod, container) (container_memory_working_set_bytes{` + appContainers + `})`

func podResource(metric, resource string) string {
	return fmt.Sprintf(`max by (namespace, pod, container) (%s{resource=%q})`, metric, resource)
}

const podRunning = `max by (namespace, pod) (kube_pod_status_phase{phase="Running"} == 1)`

const statefulSetStatusReplicas = `max by (namespace, workload) (label_replace(kube_statefulset_status_replicas{}, "workload", "$1", "statefulset", "(.+)"))`

// Owner queries, run as instant queries over pieces of the window.
func podOwners(window time.Duration) string {
	return fmt.Sprintf(`max by (namespace, pod, owner_kind, owner_name) (last_over_time(kube_pod_owner{}[%ds]))`, seconds(window))
}

func replicaSetOwners(window time.Duration) string {
	return fmt.Sprintf(`max by (namespace, replicaset, owner_name) (last_over_time(kube_replicaset_owner{owner_kind="Deployment"}[%ds]))`, seconds(window))
}

// ownerPiece is the longest window of one owner query. Shorter pieces keep
// each query under the samples and cost limits a managed store sets per
// query.
const ownerPiece = 24 * time.Hour

type nsName struct{ ns, name string }

// owners maps pods and owner keys to workloads.
type owners struct {
	pods map[nsName]wKey // only pods of a workload
	keys map[nsName]wKey // only keys whose pods all belong to one workload
	// ambiguous lists keys shared by pods of different workloads, or by a
	// workload's pods and other pods.
	ambiguous []string
}

func readOwners(ctx context.Context, o Options, start time.Time, kinds []string) (*owners, error) {
	window := o.Now.Sub(start)
	piece := ownerPiece
	if lim := o.Client.rangeLimit(); lim > 0 && lim < piece {
		piece = lim
	}
	pods, err := o.Client.queryPieces(ctx, podOwners, o.Now, window, piece)
	if err != nil {
		return nil, fmt.Errorf("pod owners: %w", err)
	}
	deployments := map[nsName]string{}
	if containsKind(kinds, Deployment) {
		rs, err := o.Client.queryPieces(ctx, replicaSetOwners, o.Now, window, piece)
		if err != nil {
			return nil, fmt.Errorf("ReplicaSet owners: %w", err)
		}
		for _, s := range rs {
			deployments[nsName{s.Labels["namespace"], s.Labels["replicaset"]}] = s.Labels["owner_name"]
		}
	}
	stateful := containsKind(kinds, StatefulSet)
	own := &owners{pods: map[nsName]wKey{}, keys: map[nsName]wKey{}}
	seen := map[nsName]map[wKey]bool{}
	for _, s := range pods {
		ns, pod := s.Labels["namespace"], s.Labels["pod"]
		var w wKey // zero for a pod of no workload
		switch name := s.Labels["owner_name"]; s.Labels["owner_kind"] {
		case "ReplicaSet":
			if d, ok := deployments[nsName{ns, name}]; ok {
				w = wKey{Deployment, ns, d}
			}
		case "StatefulSet":
			if stateful {
				w = wKey{StatefulSet, ns, name}
			}
		}
		if w != (wKey{}) {
			own.pods[nsName{ns, pod}] = w
		}
		k := nsName{ns, ownerKey(pod)}
		if k.name == "" {
			continue
		}
		if seen[k] == nil {
			seen[k] = map[wKey]bool{}
		}
		seen[k][w] = true
	}
	for k, ws := range seen {
		if len(ws) == 1 {
			for w := range ws {
				if w != (wKey{}) {
					own.keys[k] = w
				}
			}
			continue
		}
		own.ambiguous = append(own.ambiguous, k.ns+"/"+k.name)
	}
	sort.Strings(own.ambiguous)
	return own, nil
}

func containsKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// keyed indexes series by namespace, owner key and container, and then by
// timestamp.
type keyed map[[3]string]map[int64]float64

func index(ss []Series) keyed {
	out := keyed{}
	for _, s := range ss {
		k := [3]string{s.Labels["namespace"], s.Labels[ownerKeyLabel], s.Labels["container"]}
		m := out[k]
		if m == nil {
			m = map[int64]float64{}
			out[k] = m
		}
		for _, p := range s.Points {
			m[p.T] = p.V
		}
	}
	return out
}

// weightedMean combines per-key means into per-workload means, each key
// weighted by its pod count at that time. weight gives the count for a key's
// series; keys without a positive weight at a time are left out then.
func weightedMean(own *owners, means keyed, weight func(k [3]string, t int64) float64) map[cKey][]contract.Point {
	type acc struct{ sum, n float64 }
	accs := map[cKey]map[int64]*acc{}
	for k, pts := range means {
		w, ok := own.keys[nsName{k[0], k[1]}]
		if !ok {
			continue
		}
		ck := cKey{w.kind, w.ns, w.workload, k[2]}
		m := accs[ck]
		if m == nil {
			m = map[int64]*acc{}
			accs[ck] = m
		}
		for t, v := range pts {
			n := weight(k, t)
			if n <= 0 {
				continue
			}
			a := m[t]
			if a == nil {
				a = &acc{}
				m[t] = a
			}
			a.sum += v * n
			a.n += n
		}
	}
	out := map[cKey][]contract.Point{}
	for ck, m := range accs {
		pts := make([]Point, 0, len(m))
		for t, a := range m {
			pts = append(pts, Point{T: t, V: a.sum / a.n})
		}
		if len(pts) == 0 {
			continue
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
		out[ck] = toPoints(pts)
	}
	return out
}

// pullSplit is pull for stores that read one metric per query.
func pullSplit(ctx context.Context, o Options, start time.Time, src sources, kinds []string) (*pulled, error) {
	own, err := readOwners(ctx, o, start, kinds)
	if err != nil {
		return nil, err
	}
	p := newPulled()
	if len(own.ambiguous) > 0 {
		p.warnings = append(p.warnings, fmt.Sprintf("left out the pods of %s: pods of more than one workload share the name prefix the store groups them by",
			strings.Join(own.ambiguous, ", ")))
	}
	var jobs []job
	results := map[string][]Series{}
	add := func(name, q string) {
		jobs = append(jobs, job{name: name, query: q, into: func(ss []Series) { results[name] = ss }})
	}
	usage := []struct {
		name, expr string
		dst        map[cKey][]contract.Point
	}{{"cpu usage", podCPU(o.Step), p.cpuUse}, {"memory usage", podMemory, p.memUse}}
	for _, u := range usage {
		add(u.name, byOwnerKey("avg", u.expr, "container"))
		add(u.name+" pods", byOwnerKey("count", u.expr, "container"))
	}
	type resource struct {
		name, metric, resource string
		dst                    map[cKey][]contract.Point
	}
	var resources []resource
	if src.requests {
		resources = []resource{
			{"cpu requests", "kube_pod_container_resource_requests", "cpu", p.cpuReq},
			{"memory requests", "kube_pod_container_resource_requests", "memory", p.memReq},
			{"cpu limits", "kube_pod_container_resource_limits", "cpu", p.cpuLim},
			{"memory limits", "kube_pod_container_resource_limits", "memory", p.memLim},
		}
		for _, r := range resources {
			add(r.name, byOwnerKey("avg", podResource(r.metric, r.resource), "container"))
		}
	}
	if src.phase {
		add("running pods", byOwnerKey("count", podRunning))
	}
	if src.oom {
		add("oom terminations", `max by (namespace, pod, container) (kube_pod_container_status_last_terminated_reason{reason="OOMKilled"})`)
	}
	for _, kind := range kinds {
		kind := kind
		q := Replicas(kind)
		if kind == StatefulSet {
			// The minimal ingestion profile of Azure Monitor keeps the
			// current replica count of StatefulSets but not the desired one.
			if ok, err := o.Client.present(ctx, `count(kube_statefulset_replicas{})`, o.Now); err != nil {
				return nil, fmt.Errorf("probe kube_statefulset_replicas: %w", err)
			} else if !ok {
				q = statefulSetStatusReplicas
				p.warnings = append(p.warnings, "kube_statefulset_replicas is not in the store, so StatefulSet replica counts are the current ones (kube_statefulset_status_replicas) rather than the desired ones")
			}
		}
		jobs = append(jobs, job{name: kind + " replicas", query: q, into: func(ss []Series) {
			for _, s := range ss {
				p.replicas[wKey{kind, s.Labels["namespace"], s.Labels["workload"]}] = toPoints(s.Points)
			}
		}})
	}
	nodes := func(dst map[string][]contract.Point) func([]Series) {
		return func(ss []Series) {
			for _, s := range ss {
				dst[s.Labels["node"]] = toPoints(s.Points)
			}
		}
	}
	jobs = append(jobs, job{name: "node cpu", query: NodeAllocatable("cpu"), into: nodes(p.nodeCPU)},
		job{name: "node memory", query: NodeAllocatable("memory"), into: nodes(p.nodeMem)})
	if err := runJobs(ctx, o, start, jobs); err != nil {
		return nil, err
	}

	running := index(results["running pods"])
	runningAt := func(k [3]string, t int64) float64 { return running[[3]string{k[0], k[1], ""}][t] }
	for _, u := range usage {
		counts := index(results[u.name+" pods"])
		weight := func(k [3]string, t int64) float64 {
			// cAdvisor keeps reporting deleted containers for a while. An
			// owner key without running pods, such as the old ReplicaSet
			// after a rollout, is left out, as the joins on kube_pod_owner
			// of the other stores leave out its pods.
			if src.phase && runningAt(k, t) <= 0 {
				return 0
			}
			return counts[k][t]
		}
		for k, v := range weightedMean(own, index(results[u.name]), weight) {
			u.dst[k] = v
		}
	}
	if src.requests {
		// A Deployment's ReplicaSets are weighted by their running pods, so
		// that requests kube-state-metrics still exports for finished pods
		// do not count. Within one ReplicaSet every pod has the same
		// requests.
		weight := runningAt
		for _, r := range resources {
			for k, v := range weightedMean(own, index(results[r.name]), weight) {
				r.dst[k] = v
			}
		}
	}
	if src.oom {
		if err := pullOOMKills(ctx, o, start, own, results["oom terminations"], p.oom); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// maxPodsPattern bounds the length of a pod name pattern in one query, to
// keep the request URL short.
const maxPodsPattern = 2000

// pullOOMKills counts the restarts of each step whose last termination was an
// out-of-memory kill, like OOMKills, from the termination series of the
// containers that were killed and the restart counts of their pods.
func pullOOMKills(ctx context.Context, o Options, start time.Time, own *owners, terminated []Series, dst map[cKey][]contract.Point) error {
	type podC struct{ ns, pod, container string }
	killed := map[podC]map[int64]float64{}
	byNS := map[string][]string{}
	for _, s := range terminated {
		pc := podC{s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]}
		if _, ok := own.pods[nsName{pc.ns, pc.pod}]; !ok {
			continue
		}
		if killed[pc] == nil {
			byNS[pc.ns] = append(byNS[pc.ns], pc.pod)
			killed[pc] = map[int64]float64{}
		}
		for _, p := range s.Points {
			killed[pc][p.T] = p.V
		}
	}
	var jobs []job
	var restarts []Series
	for ns, pods := range byNS {
		sort.Strings(pods)
		pods = dedupeStrings(pods)
		for len(pods) > 0 {
			n, size := 0, 0
			for n < len(pods) && (n == 0 || size+len(regexp.QuoteMeta(pods[n]))+1 <= maxPodsPattern) {
				size += len(regexp.QuoteMeta(pods[n])) + 1
				n++
			}
			quoted := make([]string, n)
			for i, pod := range pods[:n] {
				quoted[i] = regexp.QuoteMeta(pod)
			}
			pods = pods[n:]
			q := fmt.Sprintf(`max by (namespace, pod, container) (kube_pod_container_status_restarts_total{namespace=%q,pod=~%q})`, ns, strings.Join(quoted, "|"))
			jobs = append(jobs, job{name: "restarts in " + ns, query: q, into: func(ss []Series) { restarts = append(restarts, ss...) }})
		}
	}
	if err := runJobs(ctx, o, start, jobs); err != nil {
		return err
	}
	stepS := int64(o.Step / time.Second)
	sums := map[cKey]map[int64]float64{}
	for _, s := range restarts {
		pc := podC{s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]}
		reason, ok := killed[pc]
		if !ok {
			continue
		}
		w := own.pods[nsName{pc.ns, pc.pod}]
		ck := cKey{w.kind, w.ns, w.workload, pc.container}
		prev := map[int64]float64{}
		for _, p := range s.Points {
			prev[p.T] = p.V
		}
		for _, p := range s.Points {
			before, ok := prev[p.T-stepS]
			r, killedNow := reason[p.T]
			if !ok || !killedNow || p.V-before <= 0 {
				continue
			}
			if sums[ck] == nil {
				sums[ck] = map[int64]float64{}
			}
			sums[ck][p.T] += (p.V - before) * r
		}
	}
	for ck, m := range sums {
		pts := make([]Point, 0, len(m))
		for t, v := range m {
			pts = append(pts, Point{T: t, V: v})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
		dst[ck] = toPoints(pts)
	}
	return nil
}

func dedupeStrings(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i > 0 && x == out[len(out)-1] {
			continue
		}
		out = append(out, x)
	}
	return out
}
