// Package validate checks an OTLP-JSON metrics file against docs/contract.md.
// It is a port of scripts/validate_contract.py and reports the same problems,
// in the same order and with the same messages for realistic files.
package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	schemaURL = "https://opentelemetry.io/schemas/1.44.0"
	scopeName = "thoras.savings_preview"
	day       = 86400
)

type instrument int

const (
	gauge instrument = iota
	updown
	deltaCounter
)

type spec struct {
	name     string
	inst     instrument
	unit     string
	isInt    bool
	required bool
}

// Slices rather than maps so errors come out in the Python dicts' order.
var (
	containerSpecs = []spec{
		{"container.cpu.usage", gauge, "{cpu}", false, true},
		{"container.memory.working_set", updown, "By", true, true},
		{"k8s.container.cpu.request.desired", updown, "{cpu}", false, true},
		{"k8s.container.memory.request.desired", updown, "By", true, true},
		{"k8s.container.cpu.limit.desired", updown, "{cpu}", false, false},
		{"k8s.container.memory.limit.desired", updown, "By", true, false},
		{"thoras.container.oom_kills", deltaCounter, "{oom_kill}", true, false},
	}
	deploymentSpecs  = []spec{{"k8s.deployment.pod.desired", updown, "{pod}", true, true}}
	statefulsetSpecs = []spec{{"k8s.statefulset.pod.desired", updown, "{pod}", true, true}}
	nodeSpecs        = []spec{
		{"k8s.node.cpu.allocatable", updown, "{cpu}", false, true},
		{"k8s.node.memory.allocatable", updown, "By", true, true},
	}
	commonAttrs = []string{"k8s.cluster.name", "thoras.source", "thoras.step_seconds", "thoras.fidelity_tier"}
	nodeAttrs   = []string{"host.type", "cloud.provider", "cloud.region", "thoras.node.capacity_type"}
)

func lookup(specs []spec, name string) (spec, bool) {
	i := slices.IndexFunc(specs, func(sp spec) bool { return sp.name == name })
	if i < 0 {
		return spec{}, false
	}
	return specs[i], true
}

// Validate checks an OTLP-JSON document against docs/contract.md and returns
// every problem found; an empty slice means the document is valid.
func Validate(doc []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	if err == nil {
		if _, tail := dec.Token(); tail != io.EOF {
			err = errors.New("extra data after the JSON value")
		}
	}
	if err != nil {
		return []string{"cannot read: " + err.Error()}
	}
	return validate(v)
}

// File reads and validates the document at path. The error is only for I/O.
func File(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Validate(b), nil
}

func validate(doc any) []string {
	top, _ := doc.(map[string]any)
	rms, ok := top["resourceMetrics"].([]any)
	if !ok {
		return []string{"top level must be an object with a resourceMetrics array"}
	}
	if len(rms) == 0 {
		return []string{"resourceMetrics is empty"}
	}

	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	clusters := map[string]bool{}
	seen := map[string]bool{}
	workloads := map[string]bool{}
	containerWL := map[string]bool{}

	for ri, rmv := range rms {
		rm := asMap(rmv)
		at, err := attrs(asMap(rm["resource"])["attributes"])
		if err != nil {
			add("resource %d: bad attributes: %s", ri, err)
			continue
		}
		where := fmt.Sprintf("resource %d", ri)
		if rm["schemaUrl"] != schemaURL {
			add("%s: schemaUrl must be %s", where, schemaURL)
		}

		for _, k := range commonAttrs {
			if _, ok := at[k]; !ok {
				add("%s: missing attribute %s", where, k)
			}
		}
		step, stepOK := at["thoras.step_seconds"].(int64)
		if stepOK = stepOK && step > 0; !stepOK {
			add("%s: thoras.step_seconds must be a positive integer", where)
		}
		tier, tierOK := at["thoras.fidelity_tier"].(int64)
		if tierOK = tierOK && tier >= 1 && tier <= 3; !tierOK {
			add("%s: thoras.fidelity_tier must be one of (1, 2, 3)", where)
		}
		if str(at, "thoras.source") == "" {
			add("%s: thoras.source must be a non-empty string", where)
		}
		if c, ok := at["k8s.cluster.name"]; ok {
			clusters[pyStr(c)] = true
		}

		dep, sts, container := str(at, "k8s.deployment.name"), str(at, "k8s.statefulset.name"), str(at, "k8s.container.name")
		var kind string
		var specs []spec
		var ident []any
		if node, ok := at["k8s.node.name"]; ok && dep == "" && sts == "" && container == "" {
			kind, specs = "node", nodeSpecs
			for _, k := range nodeAttrs {
				if str(at, k) == "" {
					add("%s: node resource missing attribute %s", where, k)
				}
			}
			switch str(at, "thoras.node.capacity_type") {
			case "on_demand", "spot", "unknown":
			default:
				add("%s: thoras.node.capacity_type must be one of ('on_demand', 'spot', 'unknown')", where)
			}
			ident = []any{node}
		} else {
			if (dep == "") == (sts == "") {
				add("%s: set exactly one of k8s.deployment.name or k8s.statefulset.name", where)
				continue
			}
			if str(at, "k8s.namespace.name") == "" {
				add("%s: missing attribute k8s.namespace.name", where)
			}
			wkind, name := "statefulset", sts
			if dep != "" {
				wkind, name = "deployment", dep
			}
			wl := join([]any{at["k8s.namespace.name"], wkind, name})
			if container != "" {
				kind, specs = "container", containerSpecs
				ident = []any{wl, container}
				containerWL[wl] = true
			} else {
				kind, specs = "workload", statefulsetSpecs
				if dep != "" {
					specs = deploymentSpecs
				}
				ident = []any{wl}
				workloads[wl] = true
			}
		}
		id := join(ident)
		if key := kind + "/" + id; seen[key] {
			add("%s: duplicate resource %s", where, id)
		} else {
			seen[key] = true
		}
		where = fmt.Sprintf("%s (%s)", where, id)

		var missing []string
		if mv, ok := at["thoras.missing_metrics"]; ok {
			var good bool
			if missing, good = stringList(mv); !good {
				add("%s: thoras.missing_metrics must be an array of strings", where)
			}
		}
		for _, m := range missing {
			if sp, ok := lookup(specs, m); !ok || !sp.required {
				add("%s: thoras.missing_metrics lists %s, which is not a required metric here", where, m)
			}
		}

		scopes := asList(rm["scopeMetrics"])
		if len(scopes) == 0 {
			add("%s: no scopeMetrics", where)
		}
		present := map[string]bool{}
		var lo, hi int64
		spanned := false
		for _, smv := range scopes {
			sm := asMap(smv)
			sc := asMap(sm["scope"])
			if sc["name"] != scopeName {
				add("%s: scope name must be %s", where, scopeName)
			}
			if v, _ := sc["version"].(string); !strings.HasPrefix(v, "1.") {
				add("%s: scope version must start with 1.", where)
			}
			if sm["schemaUrl"] != schemaURL {
				add("%s: scope schemaUrl must be %s", where, schemaURL)
			}
			for _, mv := range asList(sm["metrics"]) {
				metric := asMap(mv)
				name, _ := metric["name"].(string)
				sp, ok := lookup(specs, name)
				if !ok {
					add("%s: metric %s is not part of the contract for a %s resource", where, quote(metric["name"]), kind)
					continue
				}
				if present[name] {
					add("%s: metric %s appears twice", where, name)
					continue
				}
				present[name] = true
				if !stepOK {
					continue
				}
				first, last, ok := checkPoints(where+" "+name, sp, metric, step, add)
				// Optional series must not widen the span a tier is judged on.
				if ok && sp.required {
					if !spanned {
						lo, hi, spanned = first, last, true
					}
					lo, hi = min(lo, first), max(hi, last)
				}
			}
		}
		for _, sp := range specs {
			listed := slices.Contains(missing, sp.name)
			if sp.required && !present[sp.name] && !listed {
				add("%s: required metric %s is absent and not listed in thoras.missing_metrics", where, sp.name)
			}
			if listed && present[sp.name] {
				add("%s: %s is listed in thoras.missing_metrics but present", where, sp.name)
			}
		}

		if tierOK && stepOK && spanned {
			span := hi - lo + step
			if floor := tierFloor(step, span); tier < floor {
				add("%s: fidelity tier %d claims more than step %ds over %.1f days supports (best is %d)",
					where, tier, step, float64(span)/day, floor)
			}
		}
	}

	if len(clusters) > 1 {
		names := slices.Sorted(maps.Keys(clusters))
		for i, c := range names {
			names[i] = quote(c)
		}
		add("file mixes clusters: [%s]", strings.Join(names, ", "))
	}
	var orphans []string
	for wl := range containerWL {
		if !workloads[wl] {
			orphans = append(orphans, wl)
		}
	}
	// Python sorts the (ns, kind, name) tuples by their repr, where a quote
	// ends each field; swapping the separator for one keeps that order.
	tupleKey := func(wl string) string { return strings.ReplaceAll(wl, "/", "'") }
	sort.Slice(orphans, func(i, j int) bool { return tupleKey(orphans[i]) < tupleKey(orphans[j]) })
	for _, wl := range orphans {
		add("workload %s has containers but no workload resource carrying its replica count", wl)
	}
	return errs
}

func tierFloor(step, span int64) int64 {
	switch {
	case step <= 60 && span >= 14*day:
		return 1
	case step <= 300 && span >= 7*day:
		return 2
	}
	return 3
}

// checkPoints validates one metric's data points and returns the first and
// last timestamps in seconds; ok is false if the series was rejected.
func checkPoints(where string, sp spec, metric map[string]any, step int64, add func(string, ...any)) (first, last int64, ok bool) {
	if metric["unit"] != sp.unit {
		add("%s: unit is %s, expected %s", where, quote(metric["unit"]), quote(sp.unit))
	}
	var body map[string]any
	if sp.inst == gauge {
		if body, ok = metric["gauge"].(map[string]any); !ok {
			add("%s: must be a gauge", where)
			return 0, 0, false
		}
	} else {
		if body, ok = metric["sum"].(map[string]any); !ok {
			add("%s: must be a sum", where)
			return 0, 0, false
		}
		mono := sp.inst == deltaCounter
		if b, _ := body["isMonotonic"].(bool); b != mono {
			add("%s: isMonotonic must be %t", where, mono)
		}
		// Both the enum number and its name are valid OTLP JSON.
		want, wantName := "2", "AGGREGATION_TEMPORALITY_CUMULATIVE"
		if mono {
			want, wantName = "1", "AGGREGATION_TEMPORALITY_DELTA"
		}
		if t := body["aggregationTemporality"]; t != json.Number(want) && t != wantName {
			add("%s: aggregationTemporality must be %s", where, want)
		}
	}
	points := asList(body["dataPoints"])
	if len(points) == 0 {
		add("%s: no data points (omit the metric and list it in thoras.missing_metrics)", where)
		return 0, 0, false
	}
	for i, pv := range points {
		p := asMap(pv)
		ts, _ := p["timeUnixNano"].(string)
		ns, err := strconv.ParseInt(ts, 10, 64)
		if !isDigits(ts) || err != nil {
			add("%s: point %d timeUnixNano must be a digit string", where, i)
			return 0, 0, false
		}
		if ns%1e9 != 0 {
			add("%s: point %d timestamp is not a whole second", where, i)
			return 0, 0, false
		}
		t := ns / 1e9
		if i == 0 {
			first = t
		} else if d := t - last; d <= 0 || d%step != 0 {
			add("%s: point %d is %ds after the previous, not a multiple of step %ds", where, i, d, step)
			return 0, 0, false
		}
		last = t
		if sp.isInt {
			if s, _ := p["asInt"].(string); !isDigits(s) {
				add("%s: point %d must carry asInt as a non-negative digit string", where, i)
				return 0, 0, false
			}
		} else {
			n, isNum := p["asDouble"].(json.Number)
			f, err := strconv.ParseFloat(string(n), 64)
			if !isNum || err != nil || math.IsInf(f, 0) || f < 0 {
				add("%s: point %d asDouble must be a finite number >= 0", where, i)
				return 0, 0, false
			}
		}
		if len(asList(p["attributes"])) > 0 {
			add("%s: data point attributes are not allowed", where)
			return 0, 0, false
		}
	}
	return first, last, true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func attrs(items any) (map[string]any, error) {
	out := map[string]any{}
	for _, iv := range asList(items) {
		item := asMap(iv)
		raw, ok := item["value"]
		if !ok {
			return nil, errors.New("'value'")
		}
		v, err := attrValue(raw)
		if err != nil {
			return nil, err
		}
		k, ok := item["key"].(string)
		if !ok {
			return nil, errors.New("'key'")
		}
		out[k] = v
	}
	return out, nil
}

// attrValue unwraps an OTLP AnyValue. intValue is a string in OTLP JSON and
// becomes an int64; doubleValue stays a json.Number so it never passes as an int.
func attrValue(raw any) (any, error) {
	v := asMap(raw)
	if x, ok := v["stringValue"]; ok {
		return x, nil
	}
	if x, ok := v["intValue"]; ok {
		s := fmt.Sprint(x)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid literal for int() with base 10: '%s'", s)
		}
		return n, nil
	}
	if x, ok := v["doubleValue"]; ok {
		return x, nil
	}
	if x, ok := v["boolValue"]; ok {
		return x, nil
	}
	if x, ok := v["arrayValue"]; ok {
		var out []any
		for _, e := range asList(asMap(x)["values"]) {
			ev, err := attrValue(e)
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported attribute value %v", raw)
}

func stringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	for i, e := range list {
		if out[i], ok = e.(string); !ok {
			return nil, false
		}
	}
	return out, true
}

func str(at map[string]any, k string) string {
	s, _ := at[k].(string)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// pyStr and quote render values the way Python's %s and %r do for the strings
// and missing values that realistic files contain.
func pyStr(v any) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprint(v)
}

func quote(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}
	return pyStr(v)
}

func join(vs []any) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = pyStr(v)
	}
	return strings.Join(parts, "/")
}
