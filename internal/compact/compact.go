// Package compact rewrites a contract file at a 15-minute step. The savings
// math takes its target from the peak over 15 minutes, so a compact file that
// keeps the peak and the mean of usage and the requests in each step prices
// the same as the file it came from, at a fraction of its size.
package compact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// DefaultStep is the step of a compact file: the window the target's peak is
// taken over.
const DefaultStep = 900

// Stats counts what Compact read and wrote.
type Stats struct {
	Resources   int   // resources written
	Compacted   int   // resources rewritten at the compact step
	Unchanged   int   // resources already at or above the compact step, written as they were
	PointsIn    int64 // data points read
	PointsOut   int64 // data points written
	SourceStep  int   // coarsest step of the compacted resources
	FinestStep  int   // finest step of the compacted resources
	Step        int   // the compact step
	FirstBucket int64 // start of the first compact step, unix seconds
}

// Compact reads the contract file from r one resource at a time and writes it
// to w with every resource finer than step rewritten at step. Steps are
// counted from the first point in the file, so data on one grid stays on one
// grid. Resources whose coarsest step is already step or more are written as
// they are.
//
// In a rewritten resource each point summarizes the source points in its
// step: usage carries their mean, weighted by the step of each point, and the
// .max metrics their peak, taken over both the usage and any .max points.
// Requests, limits, replica counts and node allocatable carry their weighted
// mean, and are written only where they change (thoras.held_metrics) when
// they have a value in every step from their first to their last. OOM kills
// are summed. The resource records its source step in
// thoras.source_step_seconds and keeps its fidelity tier.
func Compact(r io.Reader, w io.Writer, step int) (Stats, error) {
	st := Stats{Step: step}
	if step <= 0 {
		return st, errors.New("the compact step must be positive")
	}
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return st, err
	}
	cw := contract.NewWriter(w)
	c := &compactor{step: int64(step), st: &st}
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return st, err
		}
		if key != "resourceMetrics" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return st, err
			}
			continue
		}
		found = true
		if err := expectDelim(dec, '['); err != nil {
			return st, err
		}
		for i := 0; dec.More(); i++ {
			var rm resourceIn
			if err := dec.Decode(&rm); err != nil {
				return st, fmt.Errorf("resource %d: %w", i, err)
			}
			res, err := c.resource(&rm)
			if err != nil {
				return st, fmt.Errorf("resource %d: %w", i, err)
			}
			if err := cw.Resource(res); err != nil {
				return st, fmt.Errorf("resource %d: %w", i, err)
			}
			st.Resources++
		}
		if err := expectDelim(dec, ']'); err != nil {
			return st, err
		}
	}
	if !found {
		return st, errors.New("no resourceMetrics found; this is not a Savings Preview file")
	}
	st.FirstBucket = c.origin
	return st, cw.Close()
}

func expectDelim(dec *json.Decoder, d json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != d {
		return fmt.Errorf("expected %q in the contract file, found %v", d, tok)
	}
	return nil
}

// OTLP-JSON as far as compaction reads it. Integers may be strings or numbers.
type resourceIn struct {
	Resource struct {
		Attributes []struct {
			Key   string
			Value struct {
				StringValue *string
				IntValue    json.RawMessage
				ArrayValue  *struct {
					Values []struct{ StringValue *string }
				}
			}
		}
	}
	ScopeMetrics []struct {
		Scope struct {
			Version string
		}
		Metrics []struct {
			Name       string
			Gauge, Sum *struct {
				DataPoints []struct {
					TimeUnixNano json.RawMessage
					AsDouble     *float64
					AsInt        json.RawMessage
				}
			}
		}
	}
}

func intOf(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(string(raw), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

type compactor struct {
	step      int64
	origin    int64
	hasOrigin bool
	st        *Stats
}

// bucket is the start of the compact step that holds t.
func (c *compactor) bucket(t int64) int64 {
	k := (t - c.origin) / c.step
	if (t-c.origin)%c.step < 0 {
		k--
	}
	return c.origin + k*c.step
}

func (c *compactor) resource(rm *resourceIn) (contract.Resource, error) {
	res, spans, err := readAttrs(rm)
	if err != nil {
		return res, err
	}
	in := map[string][]contract.Point{}
	var names []string
	for _, sm := range rm.ScopeMetrics {
		if v := sm.Scope.Version; v != "" && v != contract.ScopeVersion {
			return res, fmt.Errorf("the file is contract version %q and this release reads only %s; collect the data again with this release", v, contract.ScopeVersion)
		}
		for _, m := range sm.Metrics {
			body := m.Gauge
			if body == nil {
				body = m.Sum
			}
			if body == nil {
				continue
			}
			pts := make([]contract.Point, 0, len(body.DataPoints))
			for _, p := range body.DataPoints {
				ns, ok := intOf(p.TimeUnixNano)
				if !ok {
					return res, fmt.Errorf("%s: timeUnixNano %s is not an integer", m.Name, p.TimeUnixNano)
				}
				var v float64
				switch {
				case p.AsDouble != nil:
					v = *p.AsDouble
				case len(p.AsInt) > 0:
					n, ok := intOf(p.AsInt)
					if !ok {
						return res, fmt.Errorf("%s: asInt %s is not an integer", m.Name, p.AsInt)
					}
					v = float64(n)
				default:
					return res, fmt.Errorf("%s: point at %d has no value", m.Name, ns/1e9)
				}
				pts = append(pts, contract.Point{T: ns / 1e9, V: v})
			}
			if _, dup := in[m.Name]; !dup {
				names = append(names, m.Name)
			}
			in[m.Name] = pts
			c.st.PointsIn += int64(len(pts))
			if !c.hasOrigin && len(pts) > 0 {
				c.origin, c.hasOrigin = pts[0].T, true
			}
		}
	}

	coarse, fine := res.StepSeconds, res.StepSeconds
	for _, sp := range spans {
		coarse, fine = max(coarse, sp.Step), min(fine, sp.Step)
	}
	if int64(fine) >= c.step || int64(coarse) >= c.step && !c.onGrid(spans) {
		res.StepSpans = spans
		for _, name := range names {
			c.put(&res, name, in[name])
		}
		c.st.Unchanged++
		return res, nil
	}

	stepAt := func(t int64) float64 {
		for _, sp := range spans {
			if t >= sp.Start && t < sp.End {
				return float64(sp.Step)
			}
		}
		return float64(res.StepSeconds)
	}
	peakOf := map[string]string{contract.CPUUsage: contract.CPUUsageMax, contract.MemoryWorkingSet: contract.MemoryWorkingSetMax}
	usageOf := map[string]string{contract.CPUUsageMax: contract.CPUUsage, contract.MemoryWorkingSetMax: contract.MemoryWorkingSet}
	// Steps where a container has usage. Only those are priced, so a level
	// may hold across a gap the usage shares.
	var priced map[int64]bool
	if res.Container != "" {
		priced = map[int64]bool{}
		for _, p := range slices.Concat(in[contract.CPUUsage], in[contract.MemoryWorkingSet]) {
			priced[c.bucket(p.T)] = true
		}
	}
	for _, name := range names {
		pts := in[name]
		switch {
		case name == contract.CPUUsageMax || name == contract.MemoryWorkingSetMax:
			// Folded into the peak of their usage metric when there is one.
			if usage := usageOf[name]; len(in[usage]) == 0 {
				c.put(&res, name, c.reduce(pts, stepAt, peak))
			}
		case peakOf[name] != "":
			c.put(&res, name, c.reduce(pts, stepAt, mean))
			c.put(&res, peakOf[name], c.reduce(slices.Concat(pts, in[peakOf[name]]), stepAt, peak))
		case name == contract.OOMKills:
			c.put(&res, name, c.reduce(pts, stepAt, sum))
		default:
			pts = c.reduce(pts, stepAt, mean)
			if sp, _ := contract.SpecOf(name); sp.Int {
				for i := range pts {
					pts[i].V = math.Round(pts[i].V)
				}
			}
			if slices.Contains(contract.LevelMetrics, name) && c.holds(pts, priced) {
				pts = changes(pts)
				res.Held = append(res.Held, name)
			}
			c.put(&res, name, pts)
		}
	}
	c.st.Compacted++
	c.st.SourceStep = max(c.st.SourceStep, coarse)
	if c.st.FinestStep == 0 || fine < c.st.FinestStep {
		c.st.FinestStep = fine
	}
	// A resource compacted before keeps the step of its original data.
	if res.SourceStepSeconds == 0 {
		res.SourceStepSeconds = coarse
	}
	res.StepSeconds = int(c.step)
	res.StepSpans = c.spans(spans)
	return res, nil
}

// onGrid reports whether every span starts and ends on a compact step and
// every span coarser than the compact step is a whole number of them, so the
// finer spans can be compacted and the coarser ones kept.
func (c *compactor) onGrid(spans []contract.StepSpan) bool {
	for _, sp := range spans {
		if c.bucket(sp.Start) != sp.Start || c.bucket(sp.End) != sp.End || int64(sp.Step) > c.step && int64(sp.Step)%c.step != 0 {
			return false
		}
	}
	return true
}

// spans gives the step spans of a compacted resource: its finer spans become
// compact steps, and it needs none when that leaves one step.
func (c *compactor) spans(in []contract.StepSpan) []contract.StepSpan {
	var out []contract.StepSpan
	for _, sp := range in {
		sp.Step = max(sp.Step, int(c.step))
		if n := len(out); n > 0 && out[n-1].Step == sp.Step && out[n-1].End == sp.Start {
			out[n-1].End = sp.End
			continue
		}
		out = append(out, sp)
	}
	if len(out) == 1 {
		return nil
	}
	return out
}

func (c *compactor) put(res *contract.Resource, name string, pts []contract.Point) {
	if len(pts) > 0 {
		res.Metrics = append(res.Metrics, contract.Series{Name: name, Points: pts})
		c.st.PointsOut += int64(len(pts))
	}
}

// holds reports whether a level series can be written as its changes and
// still give the same value at every time it is read. That needs a point in
// every step from the first to the last, or, for a container, in every such
// step that has usage.
func (c *compactor) holds(pts []contract.Point, priced map[int64]bool) bool {
	if len(pts) == 0 {
		return false
	}
	first, last := pts[0].T, pts[len(pts)-1].T
	if last-first == int64(len(pts)-1)*c.step {
		return true
	}
	if priced == nil {
		return false
	}
	have := make(map[int64]bool, len(pts))
	for _, p := range pts {
		have[p.T] = true
	}
	for b := range priced {
		if b >= first && b <= last && !have[b] {
			return false
		}
	}
	return true
}

// changes keeps the first and last points and every point whose value differs
// from the one before it.
func changes(pts []contract.Point) []contract.Point {
	out := pts[:1]
	for i := 1; i < len(pts); i++ {
		if pts[i].V != pts[i-1].V || i == len(pts)-1 {
			out = append(out, pts[i])
		}
	}
	return out
}

type reduction int

const (
	mean reduction = iota // weighted by the step of each point
	peak
	sum
)

// reduce combines the points that fall in each compact step. pts may join two
// series, so it is not assumed to be in time order.
func (c *compactor) reduce(pts []contract.Point, stepAt func(int64) float64, how reduction) []contract.Point {
	if len(pts) == 0 {
		return nil
	}
	type agg struct{ sum, weight, min, max float64 }
	byStep := map[int64]*agg{}
	var order []int64
	for _, p := range pts {
		b := c.bucket(p.T)
		a := byStep[b]
		if a == nil {
			a = &agg{min: math.Inf(1), max: math.Inf(-1)}
			byStep[b] = a
			order = append(order, b)
		}
		w := 1.0
		if how == mean {
			w = stepAt(p.T)
		}
		a.sum += p.V * w
		a.weight += w
		a.min, a.max = min(a.min, p.V), max(a.max, p.V)
	}
	slices.Sort(order)
	out := make([]contract.Point, len(order))
	for i, b := range order {
		a := byStep[b]
		v := a.sum
		switch how {
		case mean:
			// Nanocore precision is finer than any source reports, and keeps
			// the file short. A steady value stays exact.
			v = math.Round(a.sum/a.weight*1e9) / 1e9
			if a.min == a.max {
				v = a.max
			}
		case peak:
			v = a.max
		}
		out[i] = contract.Point{T: b, V: v}
	}
	return out
}

// readAttrs maps the resource attributes onto a contract.Resource.
func readAttrs(rm *resourceIn) (contract.Resource, []contract.StepSpan, error) {
	var res contract.Resource
	var spans []contract.StepSpan
	node := &contract.Node{}
	for _, a := range rm.Resource.Attributes {
		v := a.Value
		s := ""
		if v.StringValue != nil {
			s = *v.StringValue
		}
		n, _ := intOf(v.IntValue)
		var list []string
		if v.ArrayValue != nil {
			for _, x := range v.ArrayValue.Values {
				if x.StringValue != nil {
					list = append(list, *x.StringValue)
				}
			}
		}
		switch a.Key {
		case "k8s.cluster.name":
			res.Cluster = s
		case "thoras.source":
			res.Source = s
		case "thoras.step_seconds":
			res.StepSeconds = int(n)
		case "thoras.fidelity_tier":
			res.Tier = int(n)
		case "thoras.source_step_seconds":
			res.SourceStepSeconds = int(n)
		case "thoras.held_metrics":
			res.Held = list
		case "thoras.missing_metrics":
			res.Missing = list
		case "thoras.fidelity_notes":
			res.Notes = list
		case "thoras.step_spans":
			for _, x := range list {
				var sp contract.StepSpan
				if _, err := fmt.Sscanf(x, "%d/%d/%d", &sp.Start, &sp.End, &sp.Step); err != nil {
					return res, nil, fmt.Errorf("thoras.step_spans entry %q is not START/END/STEP", x)
				}
				spans = append(spans, sp)
			}
		case "k8s.namespace.name":
			res.Namespace = s
		case "k8s.deployment.name":
			res.Kind, res.Workload = "deployment", s
		case "k8s.statefulset.name":
			res.Kind, res.Workload = "statefulset", s
		case "k8s.container.name":
			res.Container = s
		case "k8s.node.name":
			res.Node, node.Name = node, s
		case "host.type":
			node.InstanceType = s
		case "cloud.provider":
			node.Provider = s
		case "cloud.region":
			node.Region = s
		case "thoras.node.capacity_type":
			node.CapacityType = s
		}
	}
	if res.StepSeconds <= 0 {
		return res, nil, errors.New("thoras.step_seconds must be a positive integer")
	}
	return res, spans, nil
}
