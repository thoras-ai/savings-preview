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
		{"thoras.container.cpu.usage.max", gauge, "{cpu}", false, false},
		{"thoras.container.memory.working_set.max", updown, "By", true, false},
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
	// Metrics a resource may write only where they change (thoras.held_metrics).
	levelMetrics = []string{
		"k8s.container.cpu.request.desired", "k8s.container.memory.request.desired",
		"k8s.container.cpu.limit.desired", "k8s.container.memory.limit.desired",
		"k8s.deployment.pod.desired", "k8s.statefulset.pod.desired",
		"k8s.node.cpu.allocatable", "k8s.node.memory.allocatable",
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
		var spans []stepSpan
		if raw, ok := at["thoras.step_spans"]; ok && stepOK {
			spans = parseSpans(where, raw, step, add)
		}
		var sourceStep int64
		if v, ok := at["thoras.source_step_seconds"]; ok {
			n, isInt := v.(int64)
			coarse := step
			for _, s := range spans {
				coarse = max(coarse, s.step)
			}
			switch {
			case !isInt || n <= 0:
				add("%s: thoras.source_step_seconds must be a positive integer", where)
			case stepOK && n > coarse:
				add("%s: thoras.source_step_seconds must not be coarser than the step of the points", where)
			default:
				sourceStep = n
			}
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
		if nv, ok := at["thoras.fidelity_notes"]; ok {
			if notes, good := stringList(nv); !good || slices.Contains(notes, "") {
				add("%s: thoras.fidelity_notes must be an array of non-empty strings", where)
			}
		}
		var held []string
		if hv, ok := at["thoras.held_metrics"]; ok {
			var good bool
			if held, good = stringList(hv); !good {
				add("%s: thoras.held_metrics must be an array of strings", where)
			}
		}
		for _, m := range held {
			if _, ok := lookup(specs, m); !ok || !slices.Contains(levelMetrics, m) {
				add("%s: thoras.held_metrics lists %s, which is not a request, limit, replica or allocatable metric here", where, m)
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
				times := checkPoints(where+" "+name, sp, metric, step, add)
				if times != nil && spans != nil {
					checkSpans(where, name, times, spans, add)
				}
				// Optional series must not widen the span a tier is judged on.
				if times != nil && sp.required {
					first, last := times[0], times[len(times)-1]
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
			if slices.Contains(held, sp.name) && slices.Contains(levelMetrics, sp.name) && !present[sp.name] {
				add("%s: thoras.held_metrics lists %s, which is absent", where, sp.name)
			}
		}

		if tierOK && stepOK && spanned {
			// Mixed steps are judged by the coarsest, since that is what the
			// older data can resolve.
			coarse := step
			for _, s := range spans {
				coarse = max(coarse, s.step)
			}
			span := hi - lo + coarse
			// A compact file keeps the peaks of its source, so the tier
			// follows the source step.
			judged := coarse
			if sourceStep > 0 {
				judged = sourceStep
			}
			if floor := tierFloor(judged, span); tier < floor {
				add("%s: fidelity tier %d claims more than step %ds over %.1f days supports (best is %d)",
					where, tier, judged, float64(span)/day, floor)
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

// stepSpan is one thoras.step_spans entry: points in [start, end) are step
// seconds apart.
type stepSpan struct{ start, end, step int64 }

// parseSpans reads thoras.step_spans, or returns nil after reporting why it
// is invalid.
func parseSpans(where string, raw any, step int64, add func(string, ...any)) []stepSpan {
	entries, ok := stringList(raw)
	if !ok || len(entries) == 0 {
		add("%s: thoras.step_spans must be a non-empty array of strings", where)
		return nil
	}
	var spans []stepSpan
	for _, x := range entries {
		var n [3]int64
		parts := strings.Split(x, "/")
		ok := len(parts) == 3
		for i := 0; ok && i < 3; i++ {
			var err error
			n[i], err = strconv.ParseInt(parts[i], 10, 64)
			ok = isDigits(parts[i]) && err == nil
		}
		if !ok {
			add("%s: thoras.step_spans entry %s is not START/END/STEP", where, quote(x))
			return nil
		}
		s := stepSpan{n[0], n[1], n[2]}
		if s.step <= 0 || s.step%step != 0 || s.end <= s.start || (s.end-s.start)%s.step != 0 {
			add("%s: thoras.step_spans entry %s needs END > START, a step that is a multiple of %ds, "+
				"and a length that is a multiple of its step", where, quote(x), step)
			return nil
		}
		if len(spans) > 0 && s.start < spans[len(spans)-1].end {
			add("%s: thoras.step_spans entries overlap or are out of order", where)
			return nil
		}
		spans = append(spans, s)
	}
	finest := spans[0].step
	for _, s := range spans {
		finest = min(finest, s.step)
	}
	if finest != step {
		add("%s: thoras.step_seconds must equal the finest step in thoras.step_spans", where)
		return nil
	}
	return spans
}

// checkSpans reports the first point that is outside every span or off its
// span's grid.
func checkSpans(where, name string, times []int64, spans []stepSpan, add func(string, ...any)) {
	for _, t := range times {
		i := slices.IndexFunc(spans, func(s stepSpan) bool { return s.start <= t && t < s.end })
		if i < 0 {
			add("%s %s: point at %d is outside thoras.step_spans", where, name, t)
			return
		}
		if s := spans[i]; (t-s.start)%s.step != 0 {
			add("%s %s: point at %d is off the %ds grid of its step span", where, name, t, s.step)
			return
		}
	}
}

// checkPoints validates one metric's data points and returns their timestamps
// in seconds, or nil if the series was rejected.
func checkPoints(where string, sp spec, metric map[string]any, step int64, add func(string, ...any)) []int64 {
	if metric["unit"] != sp.unit {
		add("%s: unit is %s, expected %s", where, quote(metric["unit"]), quote(sp.unit))
	}
	var body map[string]any
	var ok bool
	if sp.inst == gauge {
		if body, ok = metric["gauge"].(map[string]any); !ok {
			add("%s: must be a gauge", where)
			return nil
		}
	} else {
		if body, ok = metric["sum"].(map[string]any); !ok {
			add("%s: must be a sum", where)
			return nil
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
		return nil
	}
	times := make([]int64, 0, len(points))
	for i, pv := range points {
		p := asMap(pv)
		ts, _ := p["timeUnixNano"].(string)
		ns, err := strconv.ParseInt(ts, 10, 64)
		if !isDigits(ts) || err != nil {
			add("%s: point %d timeUnixNano must be a digit string", where, i)
			return nil
		}
		if ns%1e9 != 0 {
			add("%s: point %d timestamp is not a whole second", where, i)
			return nil
		}
		t := ns / 1e9
		if i > 0 {
			if d := t - times[i-1]; d <= 0 || d%step != 0 {
				add("%s: point %d is %ds after the previous, not a multiple of step %ds", where, i, d, step)
				return nil
			}
		}
		times = append(times, t)
		if sp.isInt {
			if s, _ := p["asInt"].(string); !isDigits(s) {
				add("%s: point %d must carry asInt as a non-negative digit string", where, i)
				return nil
			}
		} else {
			n, isNum := p["asDouble"].(json.Number)
			f, err := strconv.ParseFloat(string(n), 64)
			if !isNum || err != nil || math.IsInf(f, 0) || f < 0 {
				add("%s: point %d asDouble must be a finite number >= 0", where, i)
				return nil
			}
		}
		if len(asList(p["attributes"])) > 0 {
			add("%s: data point attributes are not allowed", where)
			return nil
		}
	}
	return times
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
