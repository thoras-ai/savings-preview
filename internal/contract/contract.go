// Package contract writes the Savings Preview OTLP-JSON file described in
// docs/contract.md.
package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
)

const (
	SchemaURL    = "https://opentelemetry.io/schemas/1.44.0"
	ScopeName    = "thoras.savings_preview"
	ScopeVersion = "1.2.0"
	day          = 86400
)

// Instrument is how a metric is encoded in OTLP.
type Instrument int

const (
	Gauge Instrument = iota
	UpDownCounter
	DeltaCounter
)

// Spec describes one contract metric.
type Spec struct {
	Name       string
	Instrument Instrument
	Unit       string
	Int        bool
	Required   bool
}

// Metric names.
const (
	CPUUsage            = "container.cpu.usage"
	MemoryWorkingSet    = "container.memory.working_set"
	CPUUsageMax         = "thoras.container.cpu.usage.max"
	MemoryWorkingSetMax = "thoras.container.memory.working_set.max"
	CPURequest          = "k8s.container.cpu.request.desired"
	MemoryRequest       = "k8s.container.memory.request.desired"
	CPULimit            = "k8s.container.cpu.limit.desired"
	MemoryLimit         = "k8s.container.memory.limit.desired"
	OOMKills            = "thoras.container.oom_kills"
	DeploymentPods      = "k8s.deployment.pod.desired"
	StatefulSetPods     = "k8s.statefulset.pod.desired"
	NodeCPU             = "k8s.node.cpu.allocatable"
	NodeMemory          = "k8s.node.memory.allocatable"
)

var specs = map[string]Spec{
	CPUUsage:            {CPUUsage, Gauge, "{cpu}", false, true},
	MemoryWorkingSet:    {MemoryWorkingSet, UpDownCounter, "By", true, true},
	CPUUsageMax:         {CPUUsageMax, Gauge, "{cpu}", false, false},
	MemoryWorkingSetMax: {MemoryWorkingSetMax, UpDownCounter, "By", true, false},
	CPURequest:          {CPURequest, UpDownCounter, "{cpu}", false, true},
	MemoryRequest:       {MemoryRequest, UpDownCounter, "By", true, true},
	CPULimit:            {CPULimit, UpDownCounter, "{cpu}", false, false},
	MemoryLimit:         {MemoryLimit, UpDownCounter, "By", true, false},
	OOMKills:            {OOMKills, DeltaCounter, "{oom_kill}", true, false},
	DeploymentPods:      {DeploymentPods, UpDownCounter, "{pod}", true, true},
	StatefulSetPods:     {StatefulSetPods, UpDownCounter, "{pod}", true, true},
	NodeCPU:             {NodeCPU, UpDownCounter, "{cpu}", false, true},
	NodeMemory:          {NodeMemory, UpDownCounter, "By", true, true},
}

// SpecOf returns the spec of a contract metric.
func SpecOf(name string) (Spec, bool) { s, ok := specs[name]; return s, ok }

// RequiredContainerMetrics are the series a container needs to be priced.
var RequiredContainerMetrics = []string{CPUUsage, MemoryWorkingSet, CPURequest, MemoryRequest}

// Point is one sample at a whole second.
type Point struct {
	T int64
	V float64
}

// Series is one metric's data points.
type Series struct {
	Name   string
	Points []Point
}

// Common is recorded on every resource.
type Common struct {
	Cluster     string
	Source      string
	StepSeconds int
	Tier        int
}

// Node holds the extra attributes of a node resource.
type Node struct {
	Name         string
	InstanceType string
	Provider     string
	Region       string
	CapacityType string // on_demand, spot or unknown
}

// StepSpan says that a resource's points from Start up to End (unix seconds,
// End exclusive) are spaced Step seconds apart.
type StepSpan struct {
	Start, End int64
	Step       int
}

// Resource is one container, workload or node. Set Node for a node; otherwise
// set Namespace, Kind ("deployment" or "statefulset") and Workload, and also
// Container for a container resource.
type Resource struct {
	Common
	Namespace string
	Kind      string
	Workload  string
	Container string
	Node      *Node
	Missing   []string
	Notes     []string // thoras.fidelity_notes
	// StepSpans is set when the points use more than one step; StepSeconds is
	// then the finest of them.
	StepSpans []StepSpan
	Metrics   []Series
}

// File is one cluster's data.
type File struct {
	Resources []Resource
}

// TierFor returns the best fidelity tier a series of this step and span supports.
func TierFor(stepSeconds int, spanSeconds int64) int {
	switch {
	case stepSeconds <= 60 && spanSeconds >= 14*day:
		return 1
	case stepSeconds <= 300 && spanSeconds >= 7*day:
		return 2
	}
	return 3
}

type attr struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func str(k, v string) attr { return attr{k, map[string]string{"stringValue": v}} }
func num(k string, v int) attr {
	return attr{k, map[string]string{"intValue": strconv.Itoa(v)}}
}
func strs(k string, vs []string) attr {
	vals := make([]map[string]string, len(vs))
	for i, v := range vs {
		vals[i] = map[string]string{"stringValue": v}
	}
	return attr{k, map[string]any{"arrayValue": map[string]any{"values": vals}}}
}

func (r *Resource) attrs() []attr {
	a := []attr{
		str("k8s.cluster.name", r.Cluster),
		str("thoras.source", r.Source),
		num("thoras.step_seconds", r.StepSeconds),
		num("thoras.fidelity_tier", r.Tier),
	}
	if len(r.Missing) > 0 {
		m := append([]string(nil), r.Missing...)
		sort.Strings(m)
		a = append(a, strs("thoras.missing_metrics", m))
	}
	if len(r.Notes) > 0 {
		a = append(a, strs("thoras.fidelity_notes", r.Notes))
	}
	if len(r.StepSpans) > 0 {
		spans := make([]string, len(r.StepSpans))
		for i, sp := range r.StepSpans {
			spans[i] = fmt.Sprintf("%d/%d/%d", sp.Start, sp.End, sp.Step)
		}
		a = append(a, strs("thoras.step_spans", spans))
	}
	if r.Node != nil {
		return append(a,
			str("k8s.node.name", r.Node.Name),
			str("host.type", r.Node.InstanceType),
			str("cloud.provider", r.Node.Provider),
			str("cloud.region", r.Node.Region),
			str("thoras.node.capacity_type", r.Node.CapacityType),
		)
	}
	a = append(a, str("k8s.namespace.name", r.Namespace))
	if r.Kind == "statefulset" {
		a = append(a, str("k8s.statefulset.name", r.Workload))
	} else {
		a = append(a, str("k8s.deployment.name", r.Workload))
	}
	if r.Container != "" {
		a = append(a, str("k8s.container.name", r.Container))
	}
	return a
}

func (r *Resource) sortKey() string {
	if r.Node != nil {
		return "3/" + r.Node.Name
	}
	rank := "1/"
	if r.Container != "" {
		rank = "2/"
	}
	return rank + r.Namespace + "/" + r.Kind + "/" + r.Workload + "/" + r.Container
}

type dataPoint struct {
	TimeUnixNano string   `json:"timeUnixNano"`
	AsInt        *string  `json:"asInt,omitempty"`
	AsDouble     *float64 `json:"asDouble,omitempty"`
}

type body struct {
	DataPoints             []dataPoint `json:"dataPoints"`
	AggregationTemporality int         `json:"aggregationTemporality,omitempty"`
	IsMonotonic            *bool       `json:"isMonotonic,omitempty"`
}

type metric struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Gauge *body  `json:"gauge,omitempty"`
	Sum   *body  `json:"sum,omitempty"`
}

func encode(s Series) (metric, error) {
	sp, ok := specs[s.Name]
	if !ok {
		return metric{}, fmt.Errorf("unknown contract metric %q", s.Name)
	}
	pts := make([]dataPoint, 0, len(s.Points))
	for _, p := range s.Points {
		if math.IsNaN(p.V) || math.IsInf(p.V, 0) || p.V < 0 {
			return metric{}, fmt.Errorf("%s: value %v at %d is not a finite non-negative number", s.Name, p.V, p.T)
		}
		dp := dataPoint{TimeUnixNano: strconv.FormatInt(p.T, 10) + "000000000"}
		if sp.Int {
			v := strconv.FormatInt(int64(math.Round(p.V)), 10)
			dp.AsInt = &v
		} else {
			v := p.V
			dp.AsDouble = &v
		}
		pts = append(pts, dp)
	}
	m := metric{Name: sp.Name, Unit: sp.Unit}
	switch sp.Instrument {
	case Gauge:
		m.Gauge = &body{DataPoints: pts}
	case UpDownCounter:
		f := false
		m.Sum = &body{DataPoints: pts, AggregationTemporality: 2, IsMonotonic: &f}
	case DeltaCounter:
		t := true
		m.Sum = &body{DataPoints: pts, AggregationTemporality: 1, IsMonotonic: &t}
	}
	return m, nil
}

type scope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type scopeMetrics struct {
	Scope     scope    `json:"scope"`
	SchemaURL string   `json:"schemaUrl"`
	Metrics   []metric `json:"metrics"`
}

type resourceAttrs struct {
	Attributes []attr `json:"attributes"`
}

type resourceMetrics struct {
	Resource     resourceAttrs  `json:"resource"`
	SchemaURL    string         `json:"schemaUrl"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

func (r *Resource) encode() (resourceMetrics, error) {
	ser := append([]Series(nil), r.Metrics...)
	sort.SliceStable(ser, func(a, b int) bool { return ser[a].Name < ser[b].Name })
	ms := make([]metric, 0, len(ser))
	for _, s := range ser {
		if len(s.Points) == 0 {
			return resourceMetrics{}, fmt.Errorf("%s: metric %s has no points; leave it out and list it as missing", r.sortKey(), s.Name)
		}
		m, err := encode(s)
		if err != nil {
			return resourceMetrics{}, fmt.Errorf("%s: %w", r.sortKey(), err)
		}
		ms = append(ms, m)
	}
	return resourceMetrics{
		Resource:  resourceAttrs{Attributes: r.attrs()},
		SchemaURL: SchemaURL,
		ScopeMetrics: []scopeMetrics{{
			Scope:     scope{ScopeName, ScopeVersion},
			SchemaURL: SchemaURL,
			Metrics:   ms,
		}},
	}, nil
}

// Write encodes f as OTLP JSON. Resources and metrics come out in a stable order.
func (f *File) Write(w io.Writer) error {
	res := append([]Resource(nil), f.Resources...)
	sort.SliceStable(res, func(i, j int) bool { return res[i].sortKey() < res[j].sortKey() })
	var out struct {
		ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
	}
	out.ResourceMetrics = []resourceMetrics{}
	for i := range res {
		rm, err := res[i].encode()
		if err != nil {
			return err
		}
		out.ResourceMetrics = append(out.ResourceMetrics, rm)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	return enc.Encode(out)
}
