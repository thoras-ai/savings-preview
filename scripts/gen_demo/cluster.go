package main

import (
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

const (
	start  = 1789516800 // 2026-09-16T00:00:00Z, a Wednesday
	step   = 300
	days   = 14
	points = days * 86400 / step
	gib    = 1 << 30
)

// spec is one cluster to write: the demo cluster, or a smaller one of the
// fleet sample built from some of its namespaces.
type spec struct {
	name, region string
	namespaces   []string // nil for all
	replicas     float64  // replica counts are scaled by this
	pools        []pool
	seed         uint64
}

var demo = spec{name: "prod-us-east-1", region: "us-east-1", replicas: 1, pools: pools}

func (sp spec) has(ns string) bool {
	if sp.namespaces == nil {
		return true
	}
	for _, n := range sp.namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

func (sp spec) scale(n int) int { return max(1, int(math.Round(float64(n)*sp.replicas))) }

// pattern is the shape of a workload's load over the window.
type pattern int

const (
	steady  pattern = iota // flat with noise
	daily                  // business-hours peak, lighter at weekends
	bursty                 // daily, plus short queue bursts
	nightly                // idle except a run after midnight
	weekly                 // idle except a long run on Sunday morning
)

type container struct {
	name           string
	cpu, mem       [2]float64 // usage at the low and the high of the pattern; mem in GiB
	cpuReq, memReq float64    // per pod; memReq in GiB
	cpuLim, memLim float64    // 0 when no limit is set
	leak           bool       // memory climbs until it hits memLim and the container is OOM killed
	cpuReqFrom     int        // day from which cpuReqAfter applies; 0 when the request never changes
	cpuReqAfter    float64
}

type workload struct {
	ns, kind, name string
	pattern        pattern
	replicas       [2]int // [min, max]; max follows load when it is above min
	containers     []container
}

func c(name string, cpuLo, cpuHi, memLo, memHi, cpuReq, memReq float64) container {
	return container{name: name, cpu: [2]float64{cpuLo, cpuHi}, mem: [2]float64{memLo, memHi}, cpuReq: cpuReq, memReq: memReq}
}

func (k container) limits(cpu, mem float64) container { k.cpuLim, k.memLim = cpu, mem; return k }
func (k container) leaking() container                { k.leak = true; return k }
func (k container) resized(day int, cpu float64) container {
	k.cpuReqFrom, k.cpuReqAfter = day, cpu
	return k
}

func dep(ns, name string, p pattern, min, max int, cs ...container) workload {
	return workload{ns, "deployment", name, p, [2]int{min, max}, cs}
}

func sts(ns, name string, p pattern, n int, cs ...container) workload {
	return workload{ns, "statefulset", name, p, [2]int{n, n}, cs}
}

// workloads is the demo cluster. Most workloads ask for well over their peak;
// a few are right-sized and a few ask for less than they use.
var workloads = []workload{
	dep("web", "storefront", daily, 4, 12, c("app", 0.08, 0.55, 0.45, 0.7, 1.2, 2)),
	dep("web", "storefront-ssr", daily, 3, 8, c("app", 0.1, 0.7, 0.6, 0.9, 1.5, 2)),
	dep("web", "checkout-web", daily, 3, 3, c("app", 0.05, 0.3, 0.3, 0.4, 1, 1)),
	dep("web", "marketing-site", daily, 2, 2, c("app", 0.01, 0.05, 0.12, 0.15, 0.5, 1)),
	dep("web", "admin-portal", daily, 2, 2, c("app", 0.01, 0.08, 0.2, 0.25, 0.5, 1)),
	dep("web", "docs-site", steady, 2, 2, c("app", 0.01, 0.02, 0.08, 0.1, 0.25, 0.5)),
	dep("web", "image-resizer", daily, 2, 2, c("app", 0.1, 0.9, 0.5, 1.1, 2, 2)),
	dep("web", "cdn-origin", daily, 2, 2, c("nginx", 0.02, 0.1, 0.05, 0.07, 0.1, 0.128)),

	dep("api", "gateway", daily, 3, 3,
		c("app", 0.15, 0.6, 0.35, 0.5, 1, 1),
		c("envoy", 0.04, 0.2, 0.08, 0.12, 0.25, 0.25)),
	dep("api", "orders-api", daily, 3, 3,
		c("app", 0.12, 0.5, 0.9, 1.2, 1, 3),
		c("envoy", 0.03, 0.15, 0.08, 0.1, 0.25, 0.25)),
	dep("api", "payments-api", daily, 3, 3, c("app", 0.2, 0.68, 0.9, 1.4, 0.8, 1.6)),
	dep("api", "users-api", daily, 3, 3, c("app", 0.05, 0.3, 0.4, 0.5, 2, 2).resized(10, 0.5)),
	dep("api", "catalog-api", daily, 4, 4, c("app", 0.2, 0.75, 0.6, 0.8, 0.5, 1.5)),
	dep("api", "search-api", daily, 3, 3, c("app", 0.1, 0.5, 1.2, 1.5, 1, 3)),
	dep("api", "inventory-api", daily, 2, 2, c("app", 0.05, 0.2, 0.4, 0.5, 1, 2)),
	dep("api", "notifications-api", daily, 2, 2, c("app", 0.02, 0.1, 0.2, 0.25, 0.5, 1)),
	dep("api", "auth-service", daily, 3, 3, c("app", 0.08, 0.35, 0.25, 0.3, 0.4, 0.5)),
	dep("api", "pricing-service", daily, 2, 2, c("app", 0.05, 0.25, 1.5, 2.2, 1, 4)),

	dep("workers", "email-sender", bursty, 2, 2, c("worker", 0.02, 0.15, 0.2, 0.3, 0.5, 1)),
	dep("workers", "order-events-consumer", bursty, 3, 3, c("worker", 0.1, 0.4, 0.4, 0.6, 1, 2)),
	dep("workers", "webhook-dispatcher", bursty, 2, 2, c("worker", 0.03, 0.2, 0.15, 0.2, 0.5, 0.5)),
	dep("workers", "image-processor", bursty, 2, 2, c("worker", 0.3, 1.2, 1.2, 1.8, 1.5, 2).limits(2, 2).leaking()),
	dep("workers", "payment-reconciler", bursty, 1, 1, c("worker", 0.05, 0.3, 0.5, 0.7, 1, 2)),
	dep("workers", "search-indexer", bursty, 2, 2, c("worker", 0.3, 1.1, 2.4, 3.6, 1, 3).limits(2, 4).leaking()),
	dep("workers", "notification-worker", bursty, 2, 2, c("worker", 0.02, 0.1, 0.15, 0.2, 0.5, 1)),
	dep("workers", "fraud-scorer", bursty, 2, 2, c("worker", 0.4, 1.4, 2.5, 3, 2, 4)),

	sts("data", "postgres-orders", daily, 3, c("postgres", 0.8, 2.6, 13.5, 14.5, 3, 16)),
	sts("data", "postgres-users", daily, 2, c("postgres", 0.2, 0.6, 5.5, 6, 2, 8)),
	sts("data", "redis-cache", daily, 3, c("redis", 0.05, 0.2, 3.5, 4.5, 1, 8)),
	sts("data", "redis-sessions", daily, 3, c("redis", 0.05, 0.18, 1.6, 1.8, 0.25, 2)),
	sts("data", "kafka", bursty, 3, c("kafka", 0.4, 1.5, 5, 6, 2, 8)),
	sts("data", "zookeeper", steady, 3, c("zookeeper", 0.02, 0.04, 0.4, 0.45, 0.5, 1)),
	sts("data", "opensearch", daily, 3, c("opensearch", 0.6, 2.2, 13, 15.5, 2, 12)),
	sts("data", "clickhouse", nightly, 2, c("clickhouse", 0.3, 3.2, 10, 22, 4, 24)),

	dep("batch", "nightly-etl", nightly, 2, 2, c("job", 0.02, 3.4, 0.3, 9, 6, 12)),
	dep("batch", "report-builder", weekly, 1, 1, c("job", 0.01, 1.8, 0.2, 5, 2, 8)),
	dep("batch", "invoice-generator", nightly, 1, 1, c("job", 0.01, 1.2, 0.2, 2.5, 2, 4)),
	dep("batch", "ml-feature-backfill", weekly, 2, 2, c("job", 0.02, 3.5, 0.5, 14, 4, 16)),
	dep("batch", "data-export", nightly, 1, 1, c("job", 0.01, 0.6, 0.1, 1.2, 1, 4)),
	dep("batch", "log-compactor", nightly, 1, 1, c("job", 0.02, 0.9, 0.2, 1.5, 1, 2)),
	dep("batch", "sitemap-builder", weekly, 1, 1, c("job", 0.01, 0.4, 0.1, 0.8, 0.5, 2)),

	dep("platform", "ingress-nginx-controller", daily, 3, 3, c("controller", 0.1, 0.45, 0.3, 0.4, 0.5, 0.5)),
	dep("platform", "cert-manager", steady, 1, 1, c("cert-manager", 0.005, 0.01, 0.08, 0.1, 0.1, 0.25)),
	dep("platform", "external-dns", steady, 1, 1, c("external-dns", 0.002, 0.005, 0.04, 0.05, 0.1, 0.128)),
	dep("platform", "argocd-server", daily, 2, 2, c("argocd-server", 0.01, 0.05, 0.1, 0.15, 0.5, 0.5)),
	dep("platform", "argocd-repo-server", bursty, 2, 2, c("repo-server", 0.02, 0.3, 0.3, 0.6, 1, 2)),
	sts("platform", "argocd-application-controller", steady, 1, c("application-controller", 0.2, 0.4, 0.9, 1.1, 1, 2)),
	sts("platform", "vault", steady, 3, c("vault", 0.02, 0.05, 0.2, 0.25, 0.5, 1)),
	dep("platform", "keda-operator", steady, 1, 1, c("keda-operator", 0.01, 0.03, 0.1, 0.12, 0.1, 0.25)),

	sts("monitoring", "prometheus-server", steady, 2,
		c("prometheus", 0.6, 1.0, 8, 10.5, 2, 16),
		c("config-reloader", 0.001, 0.002, 0.01, 0.015, 0.05, 0.05)),
	sts("monitoring", "alertmanager", steady, 2, c("alertmanager", 0.003, 0.006, 0.03, 0.04, 0.1, 0.25)),
	dep("monitoring", "grafana", daily, 1, 1, c("grafana", 0.01, 0.08, 0.15, 0.2, 0.5, 1)),
	sts("monitoring", "loki", daily, 3, c("loki", 0.2, 0.7, 1.5, 2.6, 1, 4)),
	dep("monitoring", "kube-state-metrics", steady, 1, 1, c("kube-state-metrics", 0.01, 0.02, 0.12, 0.14, 0.1, 0.25)),
	dep("monitoring", "otel-collector", daily, 2, 2, c("otel-collector", 0.1, 0.4, 0.3, 0.45, 0.25, 0.5)),

	dep("kube-system", "coredns", daily, 2, 2, c("coredns", 0.02, 0.08, 0.05, 0.06, 0.1, 0.07).limits(0, 0.17)),
	dep("kube-system", "metrics-server", steady, 1, 1, c("metrics-server", 0.005, 0.01, 0.03, 0.035, 0.1, 0.2)),
	dep("kube-system", "cluster-autoscaler", steady, 1, 1, c("cluster-autoscaler", 0.005, 0.02, 0.08, 0.1, 0.1, 0.6)),
	dep("kube-system", "aws-load-balancer-controller", steady, 2, 2, c("controller", 0.002, 0.005, 0.04, 0.05, 0.1, 0.2)),
	dep("kube-system", "ebs-csi-controller", steady, 2, 2,
		c("ebs-plugin", 0.002, 0.004, 0.03, 0.035, 0.01, 0.04),
		c("csi-provisioner", 0.001, 0.003, 0.02, 0.025, 0.01, 0.04)),
}

// pool is a node group: count nodes of one instance type.
type pool struct {
	instance, capacity string
	count              int
	cpu, memGiB        float64 // allocatable per node
	subnet             int
}

var pools = []pool{
	{"m6i.2xlarge", "on_demand", 16, 7.91, 29.9, 1},
	{"r6i.2xlarge", "on_demand", 4, 7.91, 61.4, 2},
	{"m6i.xlarge", "spot", 6, 3.92, 14.8, 3},
	{"m6i.large", "on_demand", 2, 1.93, 6.9, 4},
}

// spotReplacements are the spot nodes reclaimed during the window: the node in
// slot is gone from at, and its replacement joins ten minutes later.
var spotReplacements = []struct {
	slot int
	at   int64
}{
	{2, start + 3*86400 + 13*3600 + 20*60},
	{0, start + 8*86400 + 2*3600 + 5*60},
	{2, start + 11*86400 + 19*3600 + 40*60},
}

func at(i int) int64 { return start + int64(i)*step }

// load is the pattern's level at point i, between 0 and about 1. A burst can
// take a bursty workload above 1.
func load(p pattern, i int, bursts []float64) float64 {
	t := time.Unix(at(i), 0).UTC()
	hour := float64(t.Hour()) + float64(t.Minute())/60
	weekend := t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
	// Peaks at 18:00 UTC, the middle of the working day in the US.
	day := 0.5 - 0.5*math.Cos((hour-6)/24*2*math.Pi)
	if weekend {
		day *= 0.6
	}
	switch p {
	case steady:
		return 0.5
	case daily:
		return day
	case bursty:
		return 0.7*day + bursts[i]
	case nightly:
		if hour >= 1 && hour < 3.5 {
			return 1
		}
	case weekly:
		if t.Weekday() == time.Sunday && hour >= 3 && hour < 9 {
			return 1
		}
	}
	return 0
}

// burstsFor places a few queue bursts of 15 to 45 minutes on each day.
func burstsFor(rng *rand.Rand) []float64 {
	b := make([]float64, points)
	perDay := 86400 / step
	for d := range days {
		for range 2 + rng.IntN(3) {
			from := d*perDay + rng.IntN(perDay)
			height := 0.4 + 0.6*rng.Float64()
			for j := range 3 + rng.IntN(7) {
				if from+j < points {
					b[from+j] = height
				}
			}
		}
	}
	return b
}

func lerp(r [2]float64, x float64) float64 { return r[0] + (r[1]-r[0])*x }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// write streams the demo cluster as a full-resolution contract file.
func write(w io.Writer) error { return writeCluster(w, demo) }

// writeCluster streams one cluster as a full-resolution contract file.
func writeCluster(w io.Writer, sp spec) error {
	cw := contract.NewWriter(w)
	common := contract.Common{Cluster: sp.name, Source: "prometheus", StepSeconds: step, Tier: contract.TierFor(step, days*86400)}
	for wi, wl := range workloads {
		if !sp.has(wl.ns) {
			continue
		}
		rng := rand.New(rand.NewPCG(uint64(wi)+sp.seed, 1))
		bursts := burstsFor(rng)
		replicas := make([]float64, points)
		lo, hi := sp.scale(wl.replicas[0]), sp.scale(wl.replicas[1])
		for i := range replicas {
			n := float64(lo)
			if hi > lo {
				n = math.Round(lerp([2]float64{float64(lo), float64(hi)}, load(wl.pattern, i, bursts)))
			}
			replicas[i] = n
		}
		pods := contract.DeploymentPods
		if wl.kind == "statefulset" {
			pods = contract.StatefulSetPods
		}
		base := contract.Resource{Common: common, Namespace: wl.ns, Kind: wl.kind, Workload: wl.name}
		res := base
		res.Metrics = []contract.Series{{Name: pods, Points: series(replicas)}}
		if err := cw.Resource(res); err != nil {
			return err
		}
		for _, k := range wl.containers {
			res := base
			res.Container = k.name
			res.Metrics = containerSeries(k, wl.pattern, bursts, rng)
			if err := cw.Resource(res); err != nil {
				return err
			}
		}
	}
	for _, p := range sp.pools {
		for slot := range p.count {
			for _, n := range nodeLives(sp, p, slot) {
				if err := cw.Resource(n); err != nil {
					return err
				}
			}
		}
	}
	return cw.Close()
}

func series(vs []float64) []contract.Point {
	pts := make([]contract.Point, len(vs))
	for i, v := range vs {
		pts[i] = contract.Point{T: at(i), V: v}
	}
	return pts
}

func flat(v float64) []float64 {
	vs := make([]float64, points)
	for i := range vs {
		vs[i] = v
	}
	return vs
}

func containerSeries(k container, p pattern, bursts []float64, rng *rand.Rand) []contract.Series {
	cpu := make([]float64, points)
	mem := make([]float64, points)
	var kills []contract.Point
	leaked := 0.0
	for i := range points {
		x := load(p, i, bursts)
		cpu[i] = round(math.Max(0.001, lerp(k.cpu, x)*(1+0.15*(2*rng.Float64()-1))), 4)
		m := lerp(k.mem, math.Min(x, 1)) * (1 + 0.02*(2*rng.Float64()-1))
		if k.leak {
			// Grows about 0.15% of the limit an hour on top of the load, so
			// the busiest hours of the day push it over the limit.
			leaked += k.memLim * 0.0015 * step / 3600 * (0.5 + rng.Float64())
			if m+leaked >= k.memLim {
				kills = append(kills, contract.Point{T: at(i), V: 1})
				leaked = 0
				m = k.mem[0]
			}
			m += leaked
		}
		mem[i] = math.Round(m * gib)
	}
	cpuReq := flat(k.cpuReq)
	if k.cpuReqFrom > 0 {
		for i := k.cpuReqFrom * 86400 / step; i < points; i++ {
			cpuReq[i] = k.cpuReqAfter
		}
	}
	out := []contract.Series{
		{Name: contract.CPUUsage, Points: series(cpu)},
		{Name: contract.MemoryWorkingSet, Points: series(mem)},
		{Name: contract.CPURequest, Points: series(cpuReq)},
		{Name: contract.MemoryRequest, Points: series(flat(k.memReq * gib))},
	}
	if k.cpuLim > 0 {
		out = append(out, contract.Series{Name: contract.CPULimit, Points: series(flat(k.cpuLim))})
	}
	if k.memLim > 0 {
		out = append(out, contract.Series{Name: contract.MemoryLimit, Points: series(flat(math.Round(k.memLim * gib)))})
	}
	if len(kills) > 0 {
		out = append(out, contract.Series{Name: contract.OOMKills, Points: kills})
	}
	return out
}

// nodeLives returns the node resources that held one slot of a pool over the
// window: one node, or for a reclaimed spot node, it and its replacements.
func nodeLives(sp spec, p pool, slot int) []contract.Resource {
	type life struct{ from, to int } // point indexes, to exclusive
	lives := []life{{0, points}}
	if p.capacity == "spot" {
		for _, r := range spotReplacements {
			if r.slot != slot {
				continue
			}
			cut := int((r.at - start) / step)
			next := life{cut + 2, lives[len(lives)-1].to}
			lives[len(lives)-1].to = cut
			lives = append(lives, next)
		}
	}
	var out []contract.Resource
	for n, l := range lives {
		var cpu, mem []contract.Point
		for i := l.from; i < l.to; i++ {
			cpu = append(cpu, contract.Point{T: at(i), V: p.cpu})
			mem = append(mem, contract.Point{T: at(i), V: math.Round(p.memGiB * gib)})
		}
		out = append(out, contract.Resource{
			Common: contract.Common{Cluster: sp.name, Source: "prometheus", StepSeconds: step, Tier: contract.TierFor(step, int64(l.to-l.from)*step)},
			Node: &contract.Node{
				Name:         nodeName(p.subnet, slot, n),
				InstanceType: p.instance,
				Provider:     "aws",
				Region:       sp.region,
				CapacityType: p.capacity,
			},
			Metrics: []contract.Series{{Name: contract.NodeCPU, Points: cpu}, {Name: contract.NodeMemory, Points: mem}},
		})
	}
	return out
}

func nodeName(subnet, slot, generation int) string {
	return fmt.Sprintf("ip-10-0-%d-%d.ec2.internal", subnet*16+slot, 11+(37*slot+53*generation)%240)
}
