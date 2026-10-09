// Package savings computes the savings totals the report page shows. It is a
// port of analyze in src/js/model.js (without the combined chart series), so
// the CLI summary matches the page for the same file. Keep the two in step:
// the parity test in savings_test.go runs both when node is available.
//
// Input is assumed to have passed the contract validator, so malformed files
// are not handled the way the JS would handle them.
package savings

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	cpuUse  = "container.cpu.usage"
	memUse  = "container.memory.working_set"
	cpuPeak = "thoras.container.cpu.usage.max"
	memPeak = "thoras.container.memory.working_set.max"
	cpuReq  = "k8s.container.cpu.request.desired"
	memReq  = "k8s.container.memory.request.desired"
)

var required = []string{cpuUse, memUse, cpuReq, memReq}

// Summary is the JSON written next to the report.
type Summary struct {
	Cluster string `json:"cluster"`
	// DisplayName is the short name the report shows for Cluster. For a
	// fleet it lists the clusters read, as Cluster does.
	DisplayName string `json:"display_name,omitempty"`
	Source      string `json:"source"`
	Tier        int    `json:"tier"`
	// StepSeconds is the coarsest step the data was collected at. For a
	// compact file that is its source step, and CompactStepSeconds is the
	// step of its points.
	StepSeconds        int       `json:"step_seconds"`
	CompactStepSeconds int       `json:"compact_step_seconds,omitempty"`
	Start              time.Time `json:"start"`
	End                time.Time `json:"end"`
	SpanSeconds        int64     `json:"span_seconds"`
	Snapshot           bool      `json:"snapshot"`
	Pricing            string    `json:"pricing"`
	Totals             Totals    `json:"totals"`
	// FidelityNotes is every thoras.fidelity_notes value in the file, sorted.
	FidelityNotes []string `json:"fidelity_notes,omitempty"`
	// Clusters is set for a file with several clusters, or with a cluster
	// that could not be read. The figures above are then the fleet's: totals
	// summed over the clusters read, the worst tier, the coarsest step and
	// the earliest start to the latest end.
	Clusters []ClusterSummary `json:"clusters,omitempty"`
}

// ClusterSummary is one cluster of a fleet: its own summary, or why it has
// none.
type ClusterSummary struct {
	Cluster     string `json:"cluster"`
	DisplayName string `json:"display_name"`
	Error       string `json:"error,omitempty"`
	*Summary
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
type otlpResource struct {
	Resource     struct{ Attributes []otlpAttr }
	ScopeMetrics []struct{ Metrics []otlpMetric }
}

type otlpAttr struct {
	Key   string
	Value struct {
		StringValue, IntValue *string
		DoubleValue           *float64
		ArrayValue            *struct {
			Values []struct{ StringValue *string }
		}
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

// span is one thoras.step_spans entry: points in [start, end) are step apart.
type span struct{ start, end, step float64 }

// resource is a container (id and kind set) or a workload.
type resource struct {
	seq              int // position in the file
	step, tier       float64
	spans            []span
	coarseStep       float64
	sourceStep       float64
	source, id, kind string
	series           map[string]series
	held             map[string]bool
}

type node struct {
	seq                                  int
	hostType, provider, region, capacity string
	cores, gib                           float64
}

// Analyze computes the summary of an OTLP-JSON Savings Preview file.
func Analyze(doc []byte) (*Summary, error) {
	return AnalyzeReader(bytes.NewReader(doc))
}

// AnalyzeReader is Analyze for a file read from r. It decodes one resource at
// a time and keeps only the numbers of each series.
//
// A file with several clusters, or with a cluster that could not be read,
// gets a Summary of the fleet: each cluster is priced from its own nodes and
// listed in Clusters, and the top-level figures cover all of them.
func AnalyzeReader(rd io.Reader) (*Summary, error) {
	byName := map[string]*clusterData{}
	var clusters []*clusterData
	get := func(name string) *clusterData {
		cd := byName[name]
		if cd == nil {
			cd = &clusterData{name: name, workloads: map[string]*resource{}}
			byName[name] = cd
			clusters = append(clusters, cd)
		}
		return cd
	}
	// Resources without a cluster name belong to the first cluster named in
	// the file, as they did when a file held one, wherever they come.
	first := ""
	named := false
	seq := 0
	n, err := eachResource(rd, func(rm *otlpResource) {
		seq++
		strs, nums := map[string]string{}, map[string]float64{}
		// Array entries without a stringValue stay nil, the JS undefined.
		lists := map[string][]*string{}
		for _, a := range rm.Resource.Attributes {
			switch v := a.Value; {
			case v.StringValue != nil:
				strs[a.Key] = *v.StringValue
			case v.IntValue != nil:
				nums[a.Key], _ = strconv.ParseFloat(*v.IntValue, 64)
			case v.DoubleValue != nil:
				nums[a.Key] = *v.DoubleValue
			case v.ArrayValue != nil:
				l := []*string{}
				for _, x := range v.ArrayValue.Values {
					l = append(l, x.StringValue)
				}
				lists[a.Key] = l
			}
		}
		name := strs["k8s.cluster.name"]
		if !named && name != "" {
			first, named = name, true
		}
		if name == "" {
			name = first
		}
		cd := get(name)
		if e := strs["thoras.cluster.error"]; e != "" && strs["k8s.node.name"] == "" && strs["k8s.container.name"] == "" &&
			strs["k8s.deployment.name"] == "" && strs["k8s.statefulset.name"] == "" {
			cd.err = e
			return
		}
		cd.resources++
		for _, x := range lists["thoras.fidelity_notes"] {
			if x != nil {
				cd.notes = append(cd.notes, *x)
			}
		}
		r := &resource{seq: seq, step: nums["thoras.step_seconds"], tier: nums["thoras.fidelity_tier"], source: strs["thoras.source"],
			series: map[string]series{}, held: map[string]bool{}}
		for _, x := range lists["thoras.held_metrics"] {
			if x != nil {
				r.held[*x] = true
			}
		}
		if r.tier == 0 {
			r.tier = 3
		}
		r.spans, r.coarseStep = parseSpans(lists["thoras.step_spans"]), r.step
		if r.spans != nil {
			r.coarseStep = r.spans[0].step
			for _, sp := range r.spans[1:] {
				r.coarseStep = math.Max(r.coarseStep, sp.step)
			}
		}
		r.sourceStep = r.coarseStep
		if n := nums["thoras.source_step_seconds"]; n > 0 && n < r.coarseStep {
			r.sourceStep = n
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
			cd.nodes = append(cd.nodes, node{seq: seq, hostType: strs["host.type"], provider: strs["cloud.provider"], region: strs["cloud.region"], capacity: strs["thoras.node.capacity_type"],
				cores: r.levelMean("k8s.node.cpu.allocatable"), gib: r.levelMean("k8s.node.memory.allocatable") / gib})
		case r.kind != "":
			r.id = strs["k8s.namespace.name"] + "/" + r.kind + "/" + strs["k8s."+r.kind+".name"]
			if strs["k8s.container.name"] != "" {
				cd.containers = append(cd.containers, r)
			} else {
				cd.workloads[r.id] = r
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("No resourceMetrics found. This is not a Savings Preview file.")
	}
	if pre := byName[""]; pre != nil && named {
		into := byName[first]
		into.resources += pre.resources
		into.containers = append(pre.containers, into.containers...)
		into.nodes = append(pre.nodes, into.nodes...)
		into.notes = append(into.notes, pre.notes...)
		for id, w := range pre.workloads {
			if into.workloads[id] == nil {
				into.workloads[id] = w
			}
		}
		slices.SortStableFunc(into.containers, func(a, b *resource) int { return a.seq - b.seq })
		slices.SortStableFunc(into.nodes, func(a, b node) int { return a.seq - b.seq })
		clusters = slices.DeleteFunc(clusters, func(c *clusterData) bool { return c == pre })
	}
	if len(clusters) == 1 && clusters[0].err == "" {
		return clusters[0].summary()
	}
	return fleet(clusters)
}

// clusterData is what AnalyzeReader keeps of one cluster's resources.
type clusterData struct {
	name       string
	err        string // thoras.cluster.error: the cluster could not be read
	resources  int
	containers []*resource
	nodes      []node
	workloads  map[string]*resource
	notes      []string
}

func (cd *clusterData) summary() (*Summary, error) {
	containers, nodes, workloads, notes := cd.containers, cd.nodes, cd.workloads, cd.notes
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

	// Fidelity: worst tier and coarsest step, span from the first to one step
	// past the last usage point.
	slices.Sort(notes)
	s := &Summary{Cluster: cd.name, DisplayName: DisplayNames([]string{cd.name})[cd.name], Pricing: rates.basis, Totals: t, FidelityNotes: slices.Compact(notes)}
	tier, step, sourceStep := 1.0, 0.0, 0.0
	t0, t1 := math.Inf(1), math.Inf(-1)
	var sources []string
	for _, c := range containers {
		tier, step, sourceStep = math.Max(tier, c.tier), math.Max(step, c.coarseStep), math.Max(sourceStep, c.sourceStep)
		if c.source != "" && !slices.Contains(sources, c.source) {
			sources = append(sources, c.source)
		}
		for _, name := range []string{cpuUse, memUse} {
			if ser := c.series[name]; len(ser.t) > 0 {
				last := ser.t[len(ser.t)-1]
				t0, t1 = math.Min(t0, ser.t[0]), math.Max(t1, last+stepAt(c.spans, last, c.step))
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
	if sourceStep < step {
		s.StepSeconds, s.CompactStepSeconds = int(sourceStep), int(step)
	}
	return s, nil
}

// fleet sums the clusters' summaries. A cluster that could not be read, or
// whose data has nothing to price, is listed with the reason and adds nothing.
func fleet(clusters []*clusterData) (*Summary, error) {
	s := &Summary{}
	var names, shown, sources, pricing, notes []string
	var all []string
	for _, cd := range clusters {
		all = append(all, cd.name)
	}
	labels := DisplayNames(all)
	read := 0
	for _, cd := range clusters {
		cs := ClusterSummary{Cluster: cd.name, DisplayName: labels[cd.name], Error: cd.err}
		if cd.err == "" {
			sum, err := cd.summary()
			if err != nil {
				cs.Error = err.Error()
			} else {
				cs.Summary = sum
			}
		}
		s.Clusters = append(s.Clusters, cs)
		if cs.Summary == nil {
			continue
		}
		c := cs.Summary
		read++
		names, shown = append(names, cd.name), append(shown, labels[cd.name])
		for _, src := range strings.Split(c.Source, ", ") {
			if src != "" && !slices.Contains(sources, src) {
				sources = append(sources, src)
			}
		}
		if !slices.Contains(pricing, c.Pricing) {
			pricing = append(pricing, c.Pricing)
		}
		notes = append(notes, c.FidelityNotes...)
		t := &s.Totals
		t.AnnualSpend += c.Totals.AnnualSpend
		t.AnnualSavings += c.Totals.AnnualSavings
		t.WasteCores += c.Totals.WasteCores
		t.WasteGiB += c.Totals.WasteGiB
		t.RequestedCores += c.Totals.RequestedCores
		t.RequestedGiB += c.Totals.RequestedGiB
		t.Workloads += c.Totals.Workloads
		t.Counted += c.Totals.Counted
		s.Tier = max(s.Tier, c.Tier)
		s.StepSeconds = max(s.StepSeconds, c.StepSeconds)
		s.CompactStepSeconds = max(s.CompactStepSeconds, c.CompactStepSeconds)
		if !c.Start.IsZero() && (s.Start.IsZero() || c.Start.Before(s.Start)) {
			s.Start = c.Start
		}
		if c.End.After(s.End) {
			s.End = c.End
		}
	}
	if read == 0 {
		var why []string
		for _, c := range s.Clusters {
			why = append(why, c.Cluster+": "+c.Error)
		}
		return nil, fmt.Errorf("no cluster in the file has data to analyze (%s)", strings.Join(why, "; "))
	}
	s.Cluster, s.DisplayName, s.Source = strings.Join(names, ", "), strings.Join(shown, ", "), strings.Join(sources, ", ")
	s.Pricing = pricing[0]
	if len(pricing) > 1 {
		s.Pricing = "each cluster priced from its own nodes, see clusters"
	}
	if !s.Start.IsZero() {
		s.SpanSeconds = int64(s.End.Sub(s.Start) / time.Second)
	}
	step := s.StepSeconds
	if s.CompactStepSeconds > 0 {
		step = s.CompactStepSeconds
	}
	s.Snapshot = step <= 0 || s.SpanSeconds <= int64(step)
	slices.Sort(notes)
	s.FidelityNotes = slices.Compact(notes)
	return s, nil
}

// eachResource calls fn with each element of the resourceMetrics array of the
// JSON object read from r, and returns how many there were.
func eachResource(r io.Reader, fn func(*otlpResource)) (int, error) {
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil {
		return 0, err
	} else if tok != json.Delim('{') {
		return 0, errors.New("No resourceMetrics found. This is not a Savings Preview file.")
	}
	n := 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return n, err
		}
		if k, _ := key.(string); !strings.EqualFold(k, "resourceMetrics") {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return n, err
			}
			continue
		}
		if tok, err := dec.Token(); err != nil {
			return n, err
		} else if tok != json.Delim('[') {
			return n, errors.New("resourceMetrics is not an array")
		}
		for dec.More() {
			var rm otlpResource
			if err := dec.Decode(&rm); err != nil {
				return n, err
			}
			fn(&rm)
			n++
		}
		if _, err := dec.Token(); err != nil {
			return n, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return n, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return n, errors.New("extra data after the JSON value")
	}
	return n, nil
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

// levelMean is the mean of a level series over the time it covers. A held
// series weights each point by the time until the next one.
func (r *resource) levelMean(name string) float64 {
	s := r.series[name]
	if !r.held[name] || len(s.t) == 0 {
		return mean(s.v, nil)
	}
	w := make([]float64, len(s.t))
	for i := range s.t {
		if i+1 < len(s.t) {
			w[i] = s.t[i+1] - s.t[i]
		} else {
			w[i] = stepAt(r.spans, s.t[i], r.step)
		}
	}
	return mean(s.v, w)
}

func isFinite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// mean of the finite values, weighted when weights is not nil.
func mean(arr, weights []float64) float64 {
	s, n := 0.0, 0.0
	for i, x := range arr {
		if !isFinite(x) {
			continue
		}
		w := 1.0
		if weights != nil {
			w = weights[i]
		}
		s, n = s+x*w, n+w
	}
	// JS tests n for truth, which NaN fails too.
	if n == 0 || math.IsNaN(n) {
		return math.NaN()
	}
	return s / n
}

// parseSpans reads thoras.step_spans entries "START/END/STEP", or returns nil
// when the resource has one step. Fields convert like JS Number, so a short or
// garbled entry gives NaN bounds that no time falls in.
func parseSpans(list []*string) []span {
	if len(list) == 0 {
		return nil
	}
	// The file passed the contract checks, so every entry is START/END/STEP.
	out := make([]span, len(list))
	for i, x := range list {
		var v [3]float64
		if x != nil {
			for k, f := range strings.SplitN(*x, "/", 3) {
				n, _ := strconv.ParseInt(f, 10, 64)
				v[k] = float64(n)
			}
		}
		out[i] = span{v[0], v[1], v[2]}
	}
	return out
}

// stepAt is the spacing of a resource's points around time t.
func stepAt(spans []span, t, step float64) float64 {
	for _, sp := range spans {
		if t >= sp.start && t < sp.end {
			return sp.step
		}
	}
	return step
}

type rates struct {
	perCore, perGiB float64
	basis           string
}

// pricing gives the rates the product would set from these nodes: the
// average priced node's hourly cost over its vCPUs, and over its GiB. With no
// node priced it falls back to the product's default node.
func pricing(nodes []node) rates {
	var cost, cores, g float64
	priced := 0
	clouds := map[string]bool{}
	spot, azureSpot, regions := false, false, false
	for _, n := range nodes {
		p := priceNode(n)
		if p.reason != "" {
			continue
		}
		priced++
		cost, cores, g = cost+p.hourly, cores+p.cores, g+p.gib
		clouds[p.cloud] = true
		spot = spot || p.spot
		azureSpot = azureSpot || p.azureSpot
		regions = regions || p.otherRegion
	}
	if priced == 0 {
		return rates{defaultNode.hourly / defaultNode.cores, defaultNode.hourly / defaultNode.gib, "default node (4 cores, 16 GiB, $0.192 per hour)"}
	}
	var where, refs []string
	for _, c := range cloudOrder {
		if clouds[c] {
			where = append(where, cloudNames[c]+" "+priceTable[c].Region)
			refs = append(refs, priceTable[c].Region)
		}
	}
	kind := "on-demand"
	if spot {
		kind = "on-demand and spot"
	}
	basis := fmt.Sprintf("%d of %d nodes at %s %s list prices", priced, len(nodes), listJoin(where), kind)
	if azureSpot {
		basis += "; Azure spot nodes are priced at on-demand"
	}
	if regions && len(refs) == 1 {
		basis += "; nodes outside " + refs[0] + " are priced as if in it"
	} else if regions {
		basis += "; nodes outside those regions are priced as if in them"
	}
	return rates{cost / cores, cost / g, basis}
}

// listJoin joins "a", "a and b", "a, b and c".
func listJoin(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func nans(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// resample takes the latest point at or before each grid time, only while it
// is younger than the series step, or the step of the span the point is in.
// A held series has points only where its value changes, so every point but
// the last stays current until the next one.
func resample(s series, grid []float64, step float64, spans []span, held bool) []float64 {
	out := nans(len(grid))
	if len(s.t) == 0 {
		return out
	}
	j := 0
	for i, g := range grid {
		for j+1 < len(s.t) && s.t[j+1] <= g {
			j++
		}
		age := math.Inf(1)
		if st := stepAt(spans, s.t[j], step); st > 0 {
			age = st
		}
		if s.t[j] <= g && (g-s.t[j] < age || held && j+1 < len(s.t)) {
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
	replicas := resample(rep, grid, wl.step, wl.spans, wl.held["k8s."+parts[0].kind+".pod.desired"])
	n := len(grid)

	// Sums in the order of required: cpu use, mem use, cpu request, mem request.
	// Zero-filled like Float64Array(n), so NaN spreads in from any running container.
	scale := [4]float64{1, gib, 1, gib}
	sums := [4][]float64{make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)}
	pkCPU, pkMem := make([]float64, n), make([]float64, n)
	// Means are weighted by the step of the span each grid point is in, so a
	// rolled-up point counts for its whole step.
	weight := make([]float64, n)
	// Containers with both usage values at each point. One without them while
	// others have them is not running then, and adds no usage and no request.
	running := make([]int, n)
	for _, c := range usable {
		at := func(name string) []float64 { return resample(c.series[name], grid, c.step, c.spans, c.held[name]) }
		var v [4][]float64
		for k, name := range required {
			v[k] = at(name)
		}
		// Rolled-up points carry the peak of the readings they summarize. Where
		// a peak point is missing, the usage stands in for it.
		pc, pm := v[0], v[1]
		if _, ok := c.series[cpuPeak]; ok {
			pc = at(cpuPeak)
		}
		if _, ok := c.series[memPeak]; ok {
			pm = at(memPeak)
		}
		for i := range n {
			w := stepAt(c.spans, grid[i], c.step)
			if w == 0 || math.IsNaN(w) { // JS `|| 1`
				w = 1
			}
			weight[i] = math.Max(weight[i], w)
			if !isFinite(v[0][i]) || !isFinite(v[1][i]) {
				continue
			}
			running[i]++
			for k := range sums {
				sums[k][i] += v[k][i] / scale[k] * replicas[i]
			}
			if !isFinite(pc[i]) {
				pc[i] = v[0][i]
			}
			if !isFinite(pm[i]) {
				pm[i] = v[1][i]
			}
			pkCPU[i] += pc[i] * replicas[i]
			pkMem[i] += pm[i] / gib * replicas[i]
		}
	}
	anyValid := false
	for i := range n {
		if running[i] > 0 && isFinite(sums[0][i]) && isFinite(sums[1][i]) && isFinite(sums[2][i]) && isFinite(sums[3][i]) {
			anyValid = true
		} else {
			for k := range sums {
				sums[k][i] = math.NaN()
			}
			pkCPU[i], pkMem[i] = math.NaN(), math.NaN()
		}
	}
	if !anyValid {
		return
	}
	reqCPU, reqMem := sums[2], sums[3]
	pc, pm := rollingMax(grid, pkCPU, peakWindowSeconds), rollingMax(grid, pkMem, peakWindowSeconds)
	cur, opt, wasteCPU, wasteMem := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for k := range n {
		tgtCPU, tgtMem := pc[k]*targetHeadroom, pm[k]*targetHeadroom
		cur[k] = math.Max(reqCPU[k]*r.perCore, reqMem[k]*r.perGiB)
		opt[k] = math.Max(math.Min(reqCPU[k], tgtCPU)*r.perCore, math.Min(reqMem[k], tgtMem)*r.perGiB)
		wasteCPU[k] = math.Max(0, reqCPU[k]-tgtCPU)
		wasteMem[k] = math.Max(0, reqMem[k]-tgtMem)
	}
	t.Counted++
	t.AnnualSpend += mean(cur, weight) * hoursPerYear
	t.AnnualSavings += (mean(cur, weight) - mean(opt, weight)) * hoursPerYear
	t.WasteCores += mean(wasteCPU, weight)
	t.WasteGiB += mean(wasteMem, weight)
	t.RequestedCores += mean(reqCPU, weight)
	t.RequestedGiB += mean(reqMem, weight)
}
