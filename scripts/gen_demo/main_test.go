package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/validate"
)

// generated writes the compact file into a temporary directory once per test
// binary, as go run ./scripts/gen_demo does; building the full file behind it
// is the slow part.
var generated = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "gen_demo")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, filepath.Base(defaultOut))
	if err := run(out, false); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
})

func compactFile(t *testing.T) []byte {
	t.Helper()
	b, err := generated()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCompactFileIsValid(t *testing.T) {
	if errs := validate.Validate(compactFile(t)); len(errs) > 0 {
		t.Error(errs)
	}
}

func TestClusterShape(t *testing.T) {
	var doc struct {
		ResourceMetrics []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value struct{ StringValue string }
				}
			}
			ScopeMetrics []struct {
				Metrics []struct {
					Name string
					Sum  *struct {
						DataPoints []struct{ TimeUnixNano string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(compactFile(t), &doc); err != nil {
		t.Fatal(err)
	}
	namespaces, workloads := map[string]bool{}, map[string]bool{}
	type span struct{ from, to int64 }
	var spot []span
	onDemand, ooms := 0, 0
	var end int64
	for _, rm := range doc.ResourceMetrics {
		at := map[string]string{}
		for _, a := range rm.Resource.Attributes {
			at[a.Key] = a.Value.StringValue
		}
		metrics := rm.ScopeMetrics[0].Metrics
		if at["k8s.node.name"] != "" {
			pts := metrics[0].Sum.DataPoints
			first, _ := strconv.ParseInt(pts[0].TimeUnixNano, 10, 64)
			last, _ := strconv.ParseInt(pts[len(pts)-1].TimeUnixNano, 10, 64)
			end = max(end, last)
			if at["thoras.node.capacity_type"] == "spot" {
				spot = append(spot, span{first, last})
			} else {
				onDemand++
			}
			continue
		}
		namespaces[at["k8s.namespace.name"]] = true
		workloads[at["k8s.namespace.name"]+"/"+at["k8s.deployment.name"]+at["k8s.statefulset.name"]] = true
		for _, m := range metrics {
			if m.Name == "thoras.container.oom_kills" {
				ooms++
			}
		}
	}
	if len(workloads) != 60 || len(namespaces) != 8 {
		t.Errorf("got %d workloads in %d namespaces, want 60 in 8", len(workloads), len(namespaces))
	}
	if onDemand == 0 {
		t.Error("no on-demand nodes")
	}
	if ooms == 0 {
		t.Error("no container has OOM kills")
	}
	reclaimed := 0
	for _, s := range spot {
		if s.to < end {
			reclaimed++
		}
	}
	if reclaimed != len(spotReplacements) {
		t.Errorf("%d spot nodes end before the window does, want %d", reclaimed, len(spotReplacements))
	}
	for _, s := range spot {
		alive := 0
		for _, o := range spot {
			if o.from <= s.from && s.from <= o.to {
				alive++
			}
		}
		if alive > pools[2].count {
			t.Errorf("%d spot nodes at once, want at most %d", alive, pools[2].count)
		}
	}
}
