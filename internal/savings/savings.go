// Package savings computes the savings totals the report page shows. It is a
// port of analyze in src/js/model.js (without the combined chart series), so
// the CLI summary matches the page for the same file. Keep the two in step:
// the parity test in savings_test.go runs both when node is available.
//
// Input is assumed to have passed the contract validator, so malformed files
// are not handled the way the JS would handle them.
package savings

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	gib               = 1073741824
	hoursPerYear      = 8760
	peakWindowSeconds = 900
	targetHeadroom    = 1.1
)

var defaultNode = struct{ cores, gib, hourly float64 }{4, 16, 0.192}

// On-demand Linux list prices in USD per hour, AWS us-east-1.
var nodePrices = map[string]float64{
	"t3.medium": 0.0416, "t3.large": 0.0832, "t3.xlarge": 0.1664, "t3.2xlarge": 0.3328,
	"m5.large": 0.096, "m5.xlarge": 0.192, "m5.2xlarge": 0.384, "m5.4xlarge": 0.768, "m5.8xlarge": 1.536,
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384, "m6i.4xlarge": 0.768, "m6i.8xlarge": 1.536,
	"c5.large": 0.085, "c5.xlarge": 0.17, "c5.2xlarge": 0.34, "c5.4xlarge": 0.68, "c5.9xlarge": 1.53,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34, "c6i.4xlarge": 0.68,
	"r5.large": 0.126, "r5.xlarge": 0.252, "r5.2xlarge": 0.504, "r5.4xlarge": 1.008, "r5.8xlarge": 2.016,
	"r6i.large": 0.126, "r6i.xlarge": 0.252, "r6i.2xlarge": 0.504, "r6i.4xlarge": 1.008,
}

const (
	cpuUse = "container.cpu.usage"
	memUse = "container.memory.working_set"
	cpuReq = "k8s.container.cpu.request.desired"
	memReq = "k8s.container.memory.request.desired"
)

var required = []string{cpuUse, memUse, cpuReq, memReq}

// Summary is the JSON written next to the report.
type Summary struct {
	Cluster     string    `json:"cluster"`
	Source      string    `json:"source"`
	Tier        int       `json:"tier"`
	StepSeconds int       `json:"step_seconds"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	SpanSeconds int64     `json:"span_seconds"`
	Snapshot    bool      `json:"snapshot"`
	Pricing     string    `json:"pricing"`
	Totals      Totals    `json:"totals"`
}

// Totals are the cluster figures summed over the workloads that could be priced.
type Totals struct {
	AnnualSpend    float64 `json:"annual_spend_usd"`
	AnnualSavings  float64 `json:"annual_savings_usd"`
	WasteCores     float64 `json:"waste_cores"`
	WasteGiB       float64 `json:"waste_gib"`
	RequestedCores float64 `json:"requested_cores"`
	RequestedGiB   float64 `json:"requested_gib"`
	Workloads      int     `json:"workloads"`
	Counted        int     `json:"counted_workloads"`
}

// OTLP-JSON shape, as far as the totals need it. encoding/json matches the
// field names to the camelCase keys without tags.
type otlpDoc struct {
	ResourceMetrics []struct {
		Resource     struct{ Attributes []otlpAttr }
		ScopeMetrics []struct{ Metrics []otlpMetric }
	}
}

type otlpAttr struct {
	Key   string
	Value struct {
		StringValue, IntValue *string
		DoubleValue           *float64
	}
}

type otlpMetric struct {
	Name       string
	Gauge, Sum *struct {
		DataPoints []struct {
			TimeUnixNano string
			AsDouble     *float64
			AsInt        *string
		}
	}
}

type series struct{ t, v []float64 }

// resource is a container (id and kind set) or a workload.
type resource struct {
	step, tier       float64
	source, id, kind string
	series           map[string]series
}

type node struct {
	hostType, region, capacity string
	cores, gib                 float64
}

// Analyze computes the summary of an OTLP-JSON Savings Preview file.
func Analyze(doc []byte) (*Summary, error) {
	var d otlpDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	if len(d.ResourceMetrics) == 0 {
		return nil, errors.New("No resourceMetrics found. This is not a Savings Preview file.")
	}
	cluster := ""
	var containers []*resource
	var nodes []node
	workloads := map[string]*resource{}
	for _, rm := range d.ResourceMetrics {
		strs, nums := map[string]string{}, map[string]float64{}
		for _, a := range rm.Resource.Attributes {
			switch v := a.Value; {
			case v.StringValue != nil:
				strs[a.Key] = *v.StringValue
			case v.IntValue != nil:
				nums[a.Key], _ = strconv.ParseFloat(*v.IntValue, 64)
			case v.DoubleValue != nil:
				nums[a.Key] = *v.DoubleValue
			}
		}
		if cluster == "" {
			cluster = strs["k8s.cluster.name"]
		}
		r := &resource{step: nums["thoras.step_seconds"], tier: nums["thoras.fidelity_tier"], source: strs["thoras.source"], series: map[string]series{}}
		if r.tier == 0 {
			r.tier = 3
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				r.series[m.Name] = readSeries(m)
			}
		}
		if strs["k8s.deployment.name"] != "" {
			r.kind = "deployment"
		} else if strs["k8s.statefulset.name"] != "" {
			r.kind = "statefulset"
		}
		switch {
		case strs["k8s.node.name"] != "":
			nodes = append(nodes, node{hostType: strs["host.type"], region: strs["cloud.region"], capacity: strs["thoras.node.capacity_type"],
				cores: mean(r.series["k8s.node.cpu.allocatable"].v), gib: mean(r.series["k8s.node.memory.allocatable"].v) / gib})
		case r.kind != "":
			r.id = strs["k8s.namespace.name"] + "/" + r.kind + "/" + strs["k8s."+r.kind+".name"]
			if strs["k8s.container.name"] != "" {
				containers = append(containers, r)
			} else {
				workloads[r.id] = r
			}
		}
	}
	if len(containers) == 0 {
		return nil, errors.New("The file has no container series, so there is nothing to analyze.")
	}

	rates := pricing(nodes)
	byWorkload := map[string][]*resource{}
	var order []string
	for _, c := range containers {
		if byWorkload[c.id] == nil {
			order = append(order, c.id)
		}
		byWorkload[c.id] = append(byWorkload[c.id], c)
	}
	t := Totals{Workloads: len(order)}
	for _, id := range order {
		analyzeWorkload(&t, workloads[id], byWorkload[id], rates)
	}
	// The page sums workload means as they are, so a non-finite mean would
	// show as NaN there. That needs infinite inputs; JSON cannot carry NaN or
	// Inf, so such totals are written as 0.
	for _, p := range []*float64{&t.AnnualSpend, &t.AnnualSavings, &t.WasteCores, &t.WasteGiB, &t.RequestedCores, &t.RequestedGiB} {
		if !isFinite(*p) {
			*p = 0
		}
	}

	// Fidelity: worst tier and step, span from the first to one step past the
	// last usage point.
	s := &Summary{Cluster: cluster, Pricing: rates.basis, Totals: t}
	tier, step := 1.0, 0.0
	t0, t1 := math.Inf(1), math.Inf(-1)
	var sources []string
	for _, c := range containers {
		tier, step = math.Max(tier, c.tier), math.Max(step, c.step)
		if c.source != "" && !slices.Contains(sources, c.source) {
			sources = append(sources, c.source)
		}
		for _, name := range []string{cpuUse, memUse} {
			if ser := c.series[name]; len(ser.t) > 0 {
				t0, t1 = math.Min(t0, ser.t[0]), math.Max(t1, ser.t[len(ser.t)-1]+c.step)
			}
		}
	}
	span := 0.0
	if isFinite(t0) {
		span = t1 - t0
		s.Start = time.Unix(int64(t0), 0).UTC()
		s.End = s.Start.Add(time.Duration(span) * time.Second)
	}
	s.Source, s.Tier, s.StepSeconds, s.SpanSeconds = strings.Join(sources, ", "), int(tier), int(step), int64(span)
	s.Snapshot = !(step > 0) || span/step <= 1
	return s, nil
}

func readSeries(m otlpMetric) series {
	var s series
	body := cmp.Or(m.Gauge, m.Sum)
	if body == nil {
		return s
	}
	for _, p := range body.DataPoints {
		ns, _ := strconv.ParseFloat(p.TimeUnixNano, 64)
		// Math.round in the JS sends halves toward +Inf.
		s.t = append(s.t, math.Floor(ns/1e9+0.5))
		// A point with neither value is NaN, like Number(undefined).
		v := math.NaN()
		if p.AsDouble != nil {
			v = *p.AsDouble
		} else if p.AsInt != nil {
			v, _ = strconv.ParseFloat(*p.AsInt, 64)
		}
		s.v = append(s.v, v)
	}
	return s
}

func isFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func mean(arr []float64) float64 {
	s, n := 0.0, 0
	for _, x := range arr {
		if isFinite(x) {
			s += x
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return s / float64(n)
}

type rates struct {
	perCore, perGiB float64
	basis           string
}

func pricing(nodes []node) rates {
	var cost, cores, g float64
	priced, caveat := 0, ""
	spot, regions := false, false
	for _, n := range nodes {
		if price, ok := nodePrices[n.hostType]; ok && n.cores > 0 && n.gib > 0 {
			priced++
			cost, cores, g = cost+price, cores+n.cores, g+n.gib
			spot = spot || n.capacity == "spot"
			regions = regions || (n.region != "" && n.region != "us-east-1")
		}
	}
	if priced == 0 {
		return rates{defaultNode.hourly / defaultNode.cores, defaultNode.hourly / defaultNode.gib, "default node (4 cores, 16 GiB, $0.192 per hour)"}
	}
	if spot {
		caveat += "; spot nodes are priced at on-demand"
	}
	if regions {
		caveat += "; nodes outside us-east-1 are priced as if in it"
	}
	return rates{cost / cores, cost / g, fmt.Sprintf("%d of %d nodes at AWS us-east-1 on-demand list prices%s", priced, len(nodes), caveat)}
}

func nans(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// resample takes the latest point at or before each grid time, only while it
// is younger than the series step.
func resample(s series, grid []float64, step float64) []float64 {
	out := nans(len(grid))
	if len(s.t) == 0 {
		return out
	}
	span := math.Inf(1)
	if step > 0 {
		span = step
	}
	j := 0
	for i, g := range grid {
		for j+1 < len(s.t) && s.t[j+1] <= g {
			j++
		}
		if s.t[j] <= g && g-s.t[j] < span {
			out[i] = s.v[j]
		}
	}
	return out
}

// rollingMax is the trailing max over the last window seconds, counting only
// valid points.
func rollingMax(t, v []float64, window float64) []float64 {
	out := nans(len(v))
	var dq []int
	head := 0
	for i, x := range v {
		if !isFinite(x) {
			continue
		}
		for len(dq) > head && v[dq[len(dq)-1]] <= x {
			dq = dq[:len(dq)-1]
		}
		dq = append(dq, i)
		for t[dq[head]] <= t[i]-window {
			head++
		}
		out[i] = v[dq[head]]
	}
	return out
}

// analyzeWorkload adds the workload's figures to t when it can be priced.
func analyzeWorkload(t *Totals, wl *resource, parts []*resource, r rates) {
	var usable []*resource
	times := map[float64]bool{}
	var grid []float64
	for _, c := range parts {
		if !slices.ContainsFunc(required, func(n string) bool { return len(c.series[n].t) == 0 }) {
			usable = append(usable, c)
			for _, x := range slices.Concat(c.series[cpuUse].t, c.series[memUse].t) {
				if !times[x] {
					times[x] = true
					grid = append(grid, x)
				}
			}
		}
	}
	if len(usable) == 0 || wl == nil {
		return
	}
	rep := wl.series["k8s."+parts[0].kind+".pod.desired"]
	if len(rep.t) == 0 {
		return
	}
	sort.Float64s(grid)
	replicas := resample(rep, grid, wl.step)
	n := len(grid)

	// Sums in the order of required: cpu use, mem use, cpu request, mem request.
	// Zero-filled like Float64Array(n), so NaN spreads in from any container.
	scale := [4]float64{1, gib, 1, gib}
	sums := [4][]float64{make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)}
	for _, c := range usable {
		for k, name := range required {
			v := resample(c.series[name], grid, c.step)
			for i := range n {
				sums[k][i] += v[i] / scale[k] * replicas[i]
			}
		}
	}
	anyValid := false
	for i := range n {
		if isFinite(sums[0][i]) && isFinite(sums[1][i]) && isFinite(sums[2][i]) && isFinite(sums[3][i]) {
			anyValid = true
		} else {
			for k := range sums {
				sums[k][i] = math.NaN()
			}
		}
	}
	if !anyValid {
		return
	}
	useCPU, useMem, reqCPU, reqMem := sums[0], sums[1], sums[2], sums[3]
	pc, pm := rollingMax(grid, useCPU, peakWindowSeconds), rollingMax(grid, useMem, peakWindowSeconds)
	cur, opt, wasteCPU, wasteMem := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for k := range n {
		tgtCPU, tgtMem := pc[k]*targetHeadroom, pm[k]*targetHeadroom
		cur[k] = math.Max(reqCPU[k]*r.perCore, reqMem[k]*r.perGiB)
		opt[k] = math.Max(math.Min(reqCPU[k], tgtCPU)*r.perCore, math.Min(reqMem[k], tgtMem)*r.perGiB)
		wasteCPU[k] = math.Max(0, reqCPU[k]-tgtCPU)
		wasteMem[k] = math.Max(0, reqMem[k]-tgtMem)
	}
	t.Counted++
	t.AnnualSpend += mean(cur) * hoursPerYear
	t.AnnualSavings += (mean(cur) - mean(opt)) * hoursPerYear
	t.WasteCores += mean(wasteCPU)
	t.WasteGiB += mean(wasteMem)
	t.RequestedCores += mean(reqCPU)
	t.RequestedGiB += mean(reqMem)
}
