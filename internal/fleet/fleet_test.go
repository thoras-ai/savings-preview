package fleet

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

func clusterFile(t *testing.T, dir, name string) string {
	t.Helper()
	common := contract.Common{Cluster: name, Source: "prometheus", StepSeconds: 60, Tier: 3}
	pts := []contract.Point{{T: 1789516800, V: 1}, {T: 1789516860, V: 1}}
	f := &contract.File{Resources: []contract.Resource{
		{Common: common, Namespace: "web", Kind: "deployment", Workload: "api",
			Metrics: []contract.Series{{Name: contract.DeploymentPods, Points: pts}}},
		{Common: common, Namespace: "web", Kind: "deployment", Workload: "api", Container: "app", Metrics: []contract.Series{
			{Name: contract.CPUUsage, Points: pts}, {Name: contract.MemoryWorkingSet, Points: pts},
			{Name: contract.CPURequest, Points: pts}, {Name: contract.MemoryRequest, Points: pts}}},
	}}
	p := filepath.Join(dir, name+".json")
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMerge(t *testing.T) {
	dir := t.TempDir()
	a, b := clusterFile(t, dir, "alpha"), clusterFile(t, dir, "beta")

	var out bytes.Buffer
	if err := Merge(&out, []string{a, b}, []Failed{{Cluster: "gamma", Reason: "list services: Unauthorized"}}); err != nil {
		t.Fatal(err)
	}
	if errs := validate.Validate(out.Bytes()); len(errs) > 0 {
		t.Fatalf("merged file: %v", errs)
	}
	s := out.String()
	if strings.Count(s, `"version":"1.4.0"`) != 5 || strings.Contains(s, `"version":"1.0.0"`) {
		t.Errorf("every resource should be at 1.4.0:\n%s", s)
	}
	if !strings.Contains(s, `"thoras.cluster.error","value":{"stringValue":"list services: Unauthorized"}`) {
		t.Errorf("no record of the cluster that was not read:\n%s", s)
	}

	// One cluster and no failures keep the file's own version.
	out.Reset()
	if err := Merge(&out, []string{a}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "1.4.0") {
		t.Errorf("a single cluster was moved to 1.4.0")
	}
	if errs := validate.Validate(out.Bytes()); len(errs) > 0 {
		t.Fatal(errs)
	}

	if err := Merge(&out, []string{filepath.Join(dir, "missing.json")}, nil); err == nil {
		t.Error("a missing file merged")
	}
}

func TestClustersAndExtract(t *testing.T) {
	dir := t.TempDir()
	a, b := clusterFile(t, dir, "alpha"), clusterFile(t, dir, "beta")
	merged := filepath.Join(dir, "fleet.json")
	f, err := os.Create(merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := Merge(f, []string{a, b}, []Failed{{Cluster: "gamma", Reason: "Unauthorized"}}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	read, failed, err := Clusters(merged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(read, ",") != "alpha,beta" || len(failed) != 1 || failed[0] != (Failed{Cluster: "gamma", Reason: "Unauthorized"}) {
		t.Errorf("Clusters: %q %+v", read, failed)
	}
	var out bytes.Buffer
	if err := Extract(&out, merged, "beta"); err != nil {
		t.Fatal(err)
	}
	if errs := validate.ValidateReader(&out); len(errs) > 0 {
		t.Errorf("extracted file: %v", errs)
	}
	var again bytes.Buffer
	Extract(&again, merged, "beta")
	if s := again.String(); strings.Contains(s, "alpha") || strings.Contains(s, contract.FleetScopeVersion) || !strings.Contains(s, `"beta"`) {
		t.Errorf("extracted file holds other clusters or the fleet version:\n%s", s)
	}
	if err := Extract(io.Discard, merged, "delta"); err == nil {
		t.Error("extracting a cluster that is not in the file succeeded")
	}
}
