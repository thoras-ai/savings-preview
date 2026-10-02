package polling

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

// source yields records in time order up to and including until. With want,
// records may be limited to the entries whose key want accepts.
type source func(until int64, want func(key string) bool, fn func(*record) error) error

func dirSource(dir string) source {
	return func(until int64, want func(string) bool, fn func(*record) error) error {
		return scan(dir, until, want, fn)
	}
}

func recordSource(recs ...*record) source {
	return func(until int64, _ func(string) bool, fn func(*record) error) error {
		for _, r := range recs {
			if r.T > until {
				break
			}
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// Summary describes the readings collected so far.
type Summary struct {
	Readings    int   // readings taken, including those rolled up
	Records     int   // stored records: raw readings plus rollup steps
	First, Last int64 // first and last record times
	Covered     int64 // seconds the records cover, leaving out gaps between runs
	LastStep    int
	Steps       map[int]bool
	NoUsage     int
	Skipped     int
	MetricsErr  string
}

func (s *Summary) add(rec *record) {
	if s.Records == 0 {
		s.First = rec.T
		s.Steps = map[int]bool{}
	}
	s.Records++
	s.Covered += int64(rec.Step)
	s.Readings += rec.Readings
	s.Last, s.LastStep = rec.T, rec.Step
	s.Steps[rec.Step] = true
	s.NoUsage += rec.NoUsage
	s.Skipped = rec.Skipped
	if rec.MetricsErr != "" {
		s.MetricsErr = rec.MetricsErr
	}
}

// Span is the time the readings cover.
func (s *Summary) Span() int64 {
	if s.Records == 0 {
		return 0
	}
	return s.Last + int64(s.LastStep) - s.First
}

func (s *Summary) coarsest() int {
	c := 0
	for st := range s.Steps {
		c = max(c, st)
	}
	return c
}

func duration(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", max(sec, 0))
	}
	d, h, m := sec/86400, sec%86400/3600, sec%3600/60
	var b strings.Builder
	if d > 0 {
		fmt.Fprintf(&b, "%dd", d)
	}
	if h > 0 || d > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	fmt.Fprintf(&b, "%dm", m)
	return b.String()
}

// keepRaw is the --keep-raw the readings were taken with, or -1 when the
// directory does not record it.
func (m Meta) keepRaw() time.Duration {
	if m.KeepRaw == nil {
		return -1
	}
	return time.Duration(*m.KeepRaw) * time.Second
}

// Progress says how much has been collected and how much more each tier needs.
// keepRaw is how long readings stay at the polling step before being rolled up
// to m.RollupStep; zero means they are never rolled up, and a negative value
// that it is not known.
func (s *Summary) Progress(m Meta, keepRaw time.Duration) string {
	span := s.Span()
	tier := contract.TierFor(s.coarsest(), s.Covered)
	var b strings.Builder
	fmt.Fprintf(&b, "%d reading", s.Readings)
	if s.Readings != 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, " covering %s at %s", duration(s.Covered), time.Duration(m.Step)*time.Second)
	if gap := span - s.Covered; gap >= 3600 {
		fmt.Fprintf(&b, " over %s with %s of gaps", duration(span), duration(gap))
	}
	if s.Steps[m.RollupStep] && m.RollupStep != m.Step {
		if keepRaw > 0 {
			fmt.Fprintf(&b, " (older than %s kept at %s)", duration(int64(keepRaw/time.Second)), time.Duration(m.RollupStep)*time.Second)
		} else {
			fmt.Fprintf(&b, " (older readings kept at %s)", time.Duration(m.RollupStep)*time.Second)
		}
	}
	fmt.Fprintf(&b, ", fidelity tier %d.", tier)
	if s.Readings == 1 {
		b.WriteString(" A single reading understates peaks.")
	}
	const day = 86400
	// The step a run of this length ends up with once old readings roll up.
	stepAt := func(days int64) int {
		if m.RollupStep == m.Step {
			return m.Step
		}
		if keepRaw < 0 && s.Steps[m.RollupStep] || keepRaw > 0 && int64(keepRaw/time.Second) < days*day {
			return m.RollupStep
		}
		return m.Step
	}
	for _, t := range []struct {
		tier    int
		step    int
		days    int64
		purpose string
	}{{2, 300, 7, ""}, {1, 60, 14, " (the forecaster's target)"}} {
		if tier <= t.tier {
			continue
		}
		if st := stepAt(t.days); st > t.step {
			if m.Step > t.step {
				fmt.Fprintf(&b, " Tier %d%s needs an --interval of %ds or less.", t.tier, t.purpose, t.step)
			} else {
				fmt.Fprintf(&b, " Tier %d%s needs %d days at %ds; set --keep-raw to %dh to keep them.", t.tier, t.purpose, t.days, t.step, t.days*24)
			}
			continue
		}
		fmt.Fprintf(&b, " Tier %d%s needs %d days: %s more.", t.tier, t.purpose, t.days, duration(t.days*day-s.Covered))
	}
	return b.String()
}

// Notes are warnings about what the readings lack.
func (s *Summary) Notes() []string {
	var out []string
	if s.NoUsage > 0 {
		why := s.MetricsErr
		if why == "" {
			why = "it returned no pod metrics"
		}
		out = append(out, fmt.Sprintf("metrics-server gave no usage in %d of %d readings (%s). Containers without usage are listed in thoras.missing_metrics and are not priced.",
			s.NoUsage, s.Readings, why))
	}
	if s.Skipped > 0 {
		out = append(out, fmt.Sprintf("%d running pods are not owned by a Deployment or StatefulSet and were left out.", s.Skipped))
	}
	return out
}

// pointBytes is the in-memory cost of one point during export.
const pointBytes = 16

// DefaultExportBudget bounds the memory export uses for points.
const DefaultExportBudget = 64 << 20

type resKind int

const (
	resWorkload resKind = iota
	resContainer
	resNode
)

type resKey struct {
	kind resKind
	id   string
}

// storeKey is the key the store files the resource under.
func (k resKey) storeKey() string {
	return string([]byte{kindWorkload, kindContainer, kindNode}[k.kind]) + k.id
}

// series of one resource built during an export pass.
type acc struct {
	spans   []contract.StepSpan
	covered int64
	series  map[string][]contract.Point
	hasMax  bool
	node    NodeAttrs
}

func (a *acc) at(t int64, step int) {
	a.covered += int64(step)
	if n := len(a.spans); n > 0 && a.spans[n-1].Step == step {
		a.spans[n-1].End = t + int64(step)
		return
	}
	a.spans = append(a.spans, contract.StepSpan{Start: t, End: t + int64(step), Step: step})
}

func (a *acc) put(name string, t int64, v float64) {
	a.series[name] = append(a.series[name], contract.Point{T: t, V: v})
}

const nano = 1e9

func cores(n int64) float64 { return float64(n) / nano }

// Export writes the contract file for every record of src to w, one batch of
// resources per pass over src, so at most about budget bytes of points are
// held at a time.
func Export(src source, cluster string, w io.Writer, budget int64) (Summary, error) {
	var sum Summary
	keys := map[resKey]int{} // resource -> series count
	err := src(math.MaxInt64, nil, func(rec *record) error {
		sum.add(rec)
		for _, c := range rec.Containers {
			keys[resKey{resContainer, containerID(c.Key)}] = 8
		}
		for _, wl := range rec.Workloads {
			keys[resKey{resWorkload, workloadID(wl.Key)}] = 1
		}
		for _, n := range rec.Nodes {
			keys[resKey{resNode, n.Name}] = 2
		}
		return nil
	})
	if err != nil {
		return sum, err
	}
	until := sum.Last
	order := make([]resKey, 0, len(keys))
	for k := range keys {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].kind != order[j].kind {
			return order[i].kind < order[j].kind
		}
		return order[i].id < order[j].id
	})

	cw := contract.NewWriter(w)
	for len(order) > 0 {
		batch := map[resKey]*acc{}
		inBatch := map[string]bool{}
		var cost int64
		n := 0
		for n < len(order) && (n == 0 || cost+int64(keys[order[n]]*sum.Records*pointBytes) <= budget) {
			cost += int64(keys[order[n]] * sum.Records * pointBytes)
			batch[order[n]] = &acc{series: map[string][]contract.Point{}}
			inBatch[order[n].storeKey()] = true
			n++
		}
		want := func(key string) bool {
			// Node keys carry the node's attributes after its name.
			if key != "" && key[0] == kindNode {
				name, _, _ := strings.Cut(key[1:], "\x00")
				return inBatch[string(kindNode)+name]
			}
			return inBatch[key]
		}
		err := src(until, want, func(rec *record) error {
			for _, c := range rec.Containers {
				a := batch[resKey{resContainer, containerID(c.Key)}]
				if a == nil {
					continue
				}
				a.at(rec.T, rec.Step)
				a.put(contract.CPURequest, rec.T, cores(c.CPURequest))
				a.put(contract.MemoryRequest, rec.T, float64(c.MemoryRequest))
				if c.HasCPULimit {
					a.put(contract.CPULimit, rec.T, cores(c.CPULimit))
				}
				if c.HasMemLimit {
					a.put(contract.MemoryLimit, rec.T, float64(c.MemoryLimit))
				}
				if c.HasUsage {
					a.put(contract.CPUUsage, rec.T, cores(c.CPUUsage))
					a.put(contract.MemoryWorkingSet, rec.T, float64(c.MemoryUsage))
					// Rollups are older than raw readings, so once a resource has
					// peaks every later point gets one too, its own value when raw.
					cmax, mmax := c.CPUUsage, c.MemoryUsage
					if rec.HasMax {
						a.hasMax = true
						cmax, mmax = c.CPUMax, c.MemMax
					}
					if a.hasMax {
						a.put(contract.CPUUsageMax, rec.T, cores(cmax))
						a.put(contract.MemoryWorkingSetMax, rec.T, float64(mmax))
					}
				}
			}
			for _, wl := range rec.Workloads {
				if a := batch[resKey{resWorkload, workloadID(wl.Key)}]; a != nil {
					a.at(rec.T, rec.Step)
					name := contract.DeploymentPods
					if wl.Key.Kind == "statefulset" {
						name = contract.StatefulSetPods
					}
					a.put(name, rec.T, float64(wl.Replicas))
				}
			}
			for _, nd := range rec.Nodes {
				if a := batch[resKey{resNode, nd.Name}]; a != nil {
					a.at(rec.T, rec.Step)
					a.node = nd.NodeAttrs
					a.put(contract.NodeCPU, rec.T, cores(nd.CPU))
					a.put(contract.NodeMemory, rec.T, float64(nd.Memory))
				}
			}
			return nil
		})
		if err != nil {
			return sum, err
		}
		for _, k := range order[:n] {
			if len(batch[k].spans) == 0 {
				continue
			}
			if err := cw.Resource(build(cluster, k, batch[k])); err != nil {
				return sum, err
			}
		}
		order = order[n:]
	}
	return sum, cw.Close()
}

func build(cluster string, k resKey, a *acc) contract.Resource {
	fine, coarse := a.spans[0].Step, a.spans[0].Step
	for _, sp := range a.spans {
		fine, coarse = min(fine, sp.Step), max(coarse, sp.Step)
	}
	// The tier counts the time the points cover, not gaps between runs, so it
	// can be lower than the span alone would allow.
	r := contract.Resource{Common: contract.Common{
		Cluster: cluster, Source: Source, StepSeconds: fine, Tier: contract.TierFor(coarse, a.covered),
	}}
	if fine != coarse {
		r.StepSpans = a.spans
	}
	add := func(name string) {
		if pts := a.series[name]; len(pts) > 0 {
			r.Metrics = append(r.Metrics, contract.Series{Name: name, Points: pts})
		} else if sp, _ := contract.SpecOf(name); sp.Required {
			r.Missing = append(r.Missing, name)
		}
	}
	parts := strings.Split(k.id, "\x00")
	switch k.kind {
	case resContainer:
		r.Namespace, r.Kind, r.Workload, r.Container = parts[0], parts[1], parts[2], parts[3]
		for _, n := range []string{contract.CPUUsage, contract.MemoryWorkingSet, contract.CPURequest, contract.MemoryRequest, contract.CPULimit, contract.MemoryLimit} {
			add(n)
		}
		if a.hasMax {
			add(contract.CPUUsageMax)
			add(contract.MemoryWorkingSetMax)
		}
	case resWorkload:
		r.Namespace, r.Kind, r.Workload = parts[0], parts[1], parts[2]
		if r.Kind == "statefulset" {
			add(contract.StatefulSetPods)
		} else {
			add(contract.DeploymentPods)
		}
	case resNode:
		n := a.node
		r.Node = &contract.Node{Name: n.Name, InstanceType: n.InstanceType, Provider: n.Provider, Region: n.Region, CapacityType: n.CapacityType}
		add(contract.NodeCPU)
		add(contract.NodeMemory)
	}
	return r
}
