package validate

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fixture = "../../fixtures/savings-preview.otlp.json"

type obj = map[string]any

var fixtureBytes []byte

func load(t *testing.T) obj {
	t.Helper()
	if fixtureBytes == nil {
		b, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		fixtureBytes = b
	}
	dec := json.NewDecoder(bytes.NewReader(fixtureBytes))
	dec.UseNumber()
	var doc obj
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func encode(t *testing.T, doc any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resources(doc obj) []any { return doc["resourceMetrics"].([]any) }

func attr(rm obj, key string) obj {
	for _, a := range rm["resource"].(obj)["attributes"].([]any) {
		if a.(obj)["key"] == key {
			return a.(obj)
		}
	}
	return nil
}

func valueOf(rm obj, key string) obj {
	if a := attr(rm, key); a != nil {
		return a["value"].(obj)
	}
	return nil
}

func addAttr(rm obj, key string, value obj) {
	res := rm["resource"].(obj)
	res["attributes"] = append(res["attributes"].([]any), obj{"key": key, "value": value})
}

// find returns the first resource whose string attributes match.
func find(t *testing.T, doc obj, want map[string]string) obj {
	t.Helper()
outer:
	for _, r := range resources(doc) {
		rm := r.(obj)
		for k, v := range want {
			if av := valueOf(rm, k); av == nil || av["stringValue"] != v {
				continue outer
			}
		}
		return rm
	}
	t.Fatalf("no resource matches %v", want)
	return nil
}

func scope(rm obj) obj { return rm["scopeMetrics"].([]any)[0].(obj) }

func metrics(rm obj) []any { return scope(rm)["metrics"].([]any) }

func setMetrics(rm obj, ms []any) { scope(rm)["metrics"] = ms }

func metric(rm obj, name string) obj {
	for _, m := range metrics(rm) {
		if m.(obj)["name"] == name {
			return m.(obj)
		}
	}
	return nil
}

func body(m obj) obj {
	if g, ok := m["gauge"].(obj); ok {
		return g
	}
	return m["sum"].(obj)
}

func points(m obj) []any { return body(m)["dataPoints"].([]any) }

func TestFixtureIsValid(t *testing.T) {
	if errs := Validate(encode(t, load(t))); len(errs) != 0 {
		t.Fatalf("fixture invalid: %v", errs)
	}
}

func TestFixtureCoversTheCases(t *testing.T) {
	doc := load(t)
	names := map[string]bool{}
	for _, r := range resources(doc) {
		for _, k := range []string{"k8s.deployment.name", "k8s.statefulset.name"} {
			if av := valueOf(r.(obj), k); av != nil {
				names[av["stringValue"].(string)] = true
			}
		}
	}
	for _, n := range []string{"checkout-api", "ledger-db", "report-worker", "search-indexer"} {
		if !names[n] {
			t.Errorf("fixture lacks workload %s", n)
		}
	}
	if len(names) != 4 {
		t.Errorf("workloads = %v", names)
	}
	worker := find(t, doc, map[string]string{"k8s.container.name": "worker"})
	if metric(worker, "container.memory.working_set") != nil {
		t.Error("worker should omit container.memory.working_set")
	}
	app := find(t, doc, map[string]string{"k8s.container.name": "app"})
	if n := len(points(metrics(app)[0].(obj))); n != 14*288 {
		t.Errorf("app has %d points, want %d", n, 14*288)
	}
}

func TestReadme(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if errs := Validate(b); len(errs) != 1 || !strings.HasPrefix(errs[0], "cannot read: ") {
		t.Fatalf("Validate(README.md) = %v", errs)
	}
}

func TestNotOTLP(t *testing.T) {
	for _, doc := range []string{`[]`, `{"resourceMetrics": []}`, `{}`, `not json`, `{"a": NaN}`, `{} {}`} {
		if errs := Validate([]byte(doc)); len(errs) != 1 {
			t.Errorf("Validate(%s) = %v, want one error", doc, errs)
		}
	}
}

type mutation struct {
	name   string
	mutate func(t *testing.T, doc, app, worker obj)
	want   string // empty means the result must be valid
}

func mutations() []mutation {
	return []mutation{
		{"wrong schema url", func(t *testing.T, doc, app, worker obj) {
			app["schemaUrl"] = "https://opentelemetry.io/schemas/1.30.0"
		}, "schemaUrl"},
		{"unknown metric", func(t *testing.T, doc, app, worker obj) {
			metrics(app)[0].(obj)["name"] = "container.cpu.time"
		}, "not part of the contract"},
		{"wrong unit", func(t *testing.T, doc, app, worker obj) {
			metrics(app)[0].(obj)["unit"] = "s"
		}, "unit is 's'"},
		{"wrong instrument", func(t *testing.T, doc, app, worker obj) {
			m := metrics(app)[0].(obj)
			m["sum"] = m["gauge"]
			delete(m, "gauge")
		}, "must be a gauge"},
		{"cumulative required for updown", func(t *testing.T, doc, app, worker obj) {
			body(metric(app, "container.memory.working_set"))["aggregationTemporality"] = 1
		}, "aggregationTemporality"},
		{"string temporality accepted", func(t *testing.T, doc, app, worker obj) {
			body(metric(app, "container.memory.working_set"))["aggregationTemporality"] = "AGGREGATION_TEMPORALITY_CUMULATIVE"
		}, ""},
		{"wrong string temporality", func(t *testing.T, doc, app, worker obj) {
			body(metric(app, "container.memory.working_set"))["aggregationTemporality"] = "AGGREGATION_TEMPORALITY_DELTA"
		}, "aggregationTemporality must be 2"},
		{"monotonic updown", func(t *testing.T, doc, app, worker obj) {
			body(metric(app, "container.memory.working_set"))["isMonotonic"] = true
		}, "isMonotonic must be false"},
		{"required metric absent without declaration", func(t *testing.T, doc, app, worker obj) {
			setMetrics(app, metrics(app)[1:])
		}, "required metric container.cpu.usage is absent"},
		{"declared missing but present", func(t *testing.T, doc, app, worker obj) {
			addAttr(app, "thoras.missing_metrics", obj{"arrayValue": obj{"values": []any{obj{"stringValue": "container.cpu.usage"}}}})
		}, "listed in thoras.missing_metrics but present"},
		{"missing list rejects optional or unknown", func(t *testing.T, doc, app, worker obj) {
			arr := valueOf(worker, "thoras.missing_metrics")["arrayValue"].(obj)
			arr["values"] = append(arr["values"].([]any), obj{"stringValue": "thoras.container.oom_kills"})
		}, "not a required metric"},
		{"missing attribute", func(t *testing.T, doc, app, worker obj) {
			res := app["resource"].(obj)
			var kept []any
			for _, a := range res["attributes"].([]any) {
				if a.(obj)["key"] != "k8s.namespace.name" {
					kept = append(kept, a)
				}
			}
			res["attributes"] = kept
		}, "k8s.namespace.name"},
		{"both workload kinds", func(t *testing.T, doc, app, worker obj) {
			addAttr(app, "k8s.statefulset.name", obj{"stringValue": "x"})
		}, "exactly one of"},
		{"mixed clusters", func(t *testing.T, doc, app, worker obj) {
			valueOf(app, "k8s.cluster.name")["stringValue"] = "other"
		}, "mixes clusters"},
		{"duplicate resource", func(t *testing.T, doc, app, worker obj) {
			doc["resourceMetrics"] = append(resources(doc), app)
		}, "duplicate resource"},
		{"missing workload resource", func(t *testing.T, doc, app, worker obj) {
			var kept []any
			for _, r := range resources(doc) {
				rm := r.(obj)
				dep := valueOf(rm, "k8s.deployment.name")
				if dep != nil && attr(rm, "k8s.container.name") == nil && dep["stringValue"] == "checkout-api" {
					continue
				}
				kept = append(kept, r)
			}
			doc["resourceMetrics"] = kept
		}, "no workload resource"},
		{"step mismatch", func(t *testing.T, doc, app, worker obj) {
			valueOf(app, "thoras.step_seconds")["intValue"] = "60"
			m := metrics(app)[0].(obj)
			pts := points(m)
			pts = append(pts[:5:5], pts[6:]...)
			p := pts[5].(obj)
			p["timeUnixNano"] = addSeconds(t, p["timeUnixNano"], 7)
			body(m)["dataPoints"] = pts
		}, "not a multiple of step"},
		{"gap in whole steps is allowed", func(t *testing.T, doc, app, worker obj) {
			m := metrics(app)[0].(obj)
			pts := points(m)
			body(m)["dataPoints"] = append(pts[:100:100], pts[110:]...)
		}, ""},
		{"out of order", func(t *testing.T, doc, app, worker obj) {
			pts := points(metrics(app)[0].(obj))
			pts[3], pts[4] = pts[4], pts[3]
		}, "not a multiple of step"},
		{"partial second", func(t *testing.T, doc, app, worker obj) {
			p := points(metrics(app)[0].(obj))[2].(obj)
			s := p["timeUnixNano"].(string)
			p["timeUnixNano"] = s[:len(s)-1] + "1"
		}, "not a whole second"},
		{"numeric timestamp", func(t *testing.T, doc, app, worker obj) {
			points(metrics(app)[0].(obj))[0].(obj)["timeUnixNano"] = json.Number("1789516800000000000")
		}, "timeUnixNano must be a digit string"},
		{"negative value", func(t *testing.T, doc, app, worker obj) {
			points(metrics(app)[0].(obj))[0].(obj)["asDouble"] = -0.1
		}, "finite number >= 0"},
		{"bool value", func(t *testing.T, doc, app, worker obj) {
			points(metrics(app)[0].(obj))[0].(obj)["asDouble"] = true
		}, "finite number >= 0"},
		{"huge value", func(t *testing.T, doc, app, worker obj) {
			points(metrics(app)[0].(obj))[0].(obj)["asDouble"] = json.Number("1e400")
		}, "finite number >= 0"},
		{"int metric needs asInt", func(t *testing.T, doc, app, worker obj) {
			p := points(metric(app, "k8s.container.memory.request.desired"))[0].(obj)
			p["asDouble"] = json.Number(p["asInt"].(string) + ".0")
			delete(p, "asInt")
		}, "asInt"},
		{"empty series", func(t *testing.T, doc, app, worker obj) {
			body(metrics(app)[0].(obj))["dataPoints"] = []any{}
		}, "no data points"},
		{"tier cannot exceed resolution", func(t *testing.T, doc, app, worker obj) {
			valueOf(app, "thoras.fidelity_tier")["intValue"] = "1"
		}, "claims more than"},
		{"optional series does not widen tier span", func(t *testing.T, doc, app, worker obj) {
			for _, mv := range metrics(app) {
				m := mv.(obj)
				if m["name"] != "thoras.container.oom_kills" {
					pts := points(m)
					body(m)["dataPoints"] = pts[len(pts)-288*3:]
				}
			}
			setMetrics(app, append(metrics(app), obj{"name": "thoras.container.oom_kills", "unit": "{oom_kill}",
				"sum": obj{"isMonotonic": true, "aggregationTemporality": 1, "dataPoints": []any{
					obj{"timeUnixNano": "1789516800000000000", "asInt": "1"}}}}))
			valueOf(app, "thoras.fidelity_tier")["intValue"] = "2"
		}, "claims more than"},
		{"tier value", func(t *testing.T, doc, app, worker obj) {
			valueOf(app, "thoras.fidelity_tier")["intValue"] = "4"
		}, "fidelity_tier"},
		{"step must be an int", func(t *testing.T, doc, app, worker obj) {
			v := valueOf(app, "thoras.step_seconds")
			delete(v, "intValue")
			v["doubleValue"] = json.Number("300.0")
		}, "step_seconds must be a positive integer"},
		{"bad intValue", func(t *testing.T, doc, app, worker obj) {
			valueOf(app, "thoras.step_seconds")["intValue"] = "five"
		}, "bad attributes: invalid literal for int() with base 10: 'five'"},
		{"node needs capacity type", func(t *testing.T, doc, app, worker obj) {
			node := find(t, doc, map[string]string{"host.type": "m5.xlarge"})
			valueOf(node, "thoras.node.capacity_type")["stringValue"] = "reserved"
		}, "capacity_type"},
		{"scope version", func(t *testing.T, doc, app, worker obj) {
			scope(app)["scope"].(obj)["version"] = "2.0.0"
		}, "scope version"},
		{"datapoint attributes rejected", func(t *testing.T, doc, app, worker obj) {
			points(metrics(app)[0].(obj))[0].(obj)["attributes"] = []any{
				obj{"key": "cpu.mode", "value": obj{"stringValue": "user"}}}
		}, "data point attributes"},
		{"nodes are optional", func(t *testing.T, doc, app, worker obj) {
			var kept []any
			for _, r := range resources(doc) {
				if attr(r.(obj), "k8s.node.name") == nil {
					kept = append(kept, r)
				}
			}
			doc["resourceMetrics"] = kept
		}, ""},
	}
}

func addSeconds(t *testing.T, ns any, s int64) string {
	t.Helper()
	n, err := strconv.ParseInt(ns.(string), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(n+s*1_000_000_000, 10)
}

func mutated(t *testing.T, m mutation) []byte {
	t.Helper()
	doc := load(t)
	app := find(t, doc, map[string]string{"k8s.container.name": "app"})
	worker := find(t, doc, map[string]string{"k8s.container.name": "worker"})
	m.mutate(t, doc, app, worker)
	return encode(t, doc)
}

func TestRejects(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			errs := Validate(mutated(t, m))
			if m.want == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			for _, e := range errs {
				if strings.Contains(e, m.want) {
					return
				}
			}
			t.Fatalf("%q not in %v", m.want, errs)
		})
	}
}

// TestMatchesPython runs the Python validator on the same documents and
// checks both report the same lines.
func TestMatchesPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	script, err := filepath.Abs("../../scripts/validate_contract.py")
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string][]byte{
		"fixture":  fixtureOrLoad(t),
		"not json": []byte("not json"),
		"nan":      []byte(`{"resourceMetrics": NaN}`),
		"not otlp": []byte(`[]`),
	}
	for _, m := range mutations() {
		docs[m.name] = mutated(t, m)
	}
	docs["no workload res"] = mutated(t, mutation{mutate: func(t *testing.T, doc, app, worker obj) {
		// Drop every workload resource and rename one cluster and drop a unit for a long, mixed report.
		var kept []any
		for _, r := range resources(doc) {
			rm := r.(obj)
			if attr(rm, "k8s.container.name") == nil && attr(rm, "k8s.node.name") == nil {
				continue
			}
			kept = append(kept, r)
		}
		doc["resourceMetrics"] = kept
		valueOf(app, "k8s.cluster.name")["stringValue"] = "other"
		metrics(app)[0].(obj)["unit"] = nil
	}})

	dir := t.TempDir()
	for name, doc := range docs {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, doc, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(py, script, path).Output()
			pyValid := err == nil
			if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			goErrs := Validate(doc)
			if pyValid != (len(goErrs) == 0) {
				t.Fatalf("python valid=%v, go errors %v\npython:\n%s", pyValid, goErrs, out)
			}
			if strings.HasPrefix(lines[0], path+": cannot read: ") {
				if len(goErrs) != 1 || !strings.HasPrefix(goErrs[0], "cannot read: ") {
					t.Fatalf("python could not read it, go says %v", goErrs)
				}
				return
			}
			var pyErrs []string
			for _, l := range lines[1:] {
				if e, ok := strings.CutPrefix(l, "  "); ok && !strings.HasPrefix(e, "... ") {
					pyErrs = append(pyErrs, e)
				}
			}
			if len(goErrs) > 50 {
				goErrs = goErrs[:50]
			}
			if strings.Join(pyErrs, "\n") != strings.Join(goErrs, "\n") {
				t.Fatalf("errors differ\npython:\n%s\ngo:\n%s", strings.Join(pyErrs, "\n"), strings.Join(goErrs, "\n"))
			}
		})
	}
}

func fixtureOrLoad(t *testing.T) []byte {
	load(t)
	return fixtureBytes
}
