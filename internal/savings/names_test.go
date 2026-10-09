package savings

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	east = "arn:aws:eks:us-east-1:111122223333:cluster/prod"
	west = "arn:aws:eks:eu-west-1:111122223333:cluster/prod"
	gke  = "gke_acme-data_us-central1_analytics"
)

var nameCases = []struct {
	names []string
	want  map[string]string
}{
	{[]string{east, gke, "staging"}, map[string]string{east: "prod", gke: "analytics", "staging": "staging"}},
	{[]string{east, west}, map[string]string{east: "prod (us-east-1)", west: "prod (eu-west-1)"}},
	{[]string{east, "arn:aws:eks:us-east-1:444455556666:cluster/prod"},
		map[string]string{east: "prod (111122223333)", "arn:aws:eks:us-east-1:444455556666:cluster/prod": "prod (444455556666)"}},
	{[]string{east, "prod"}, map[string]string{east: "prod (us-east-1)", "prod": "prod"}},
	{[]string{east, west, "prod (us-east-1)"}, map[string]string{east: east, west: "prod (eu-west-1)", "prod (us-east-1)": "prod (us-east-1)"}},
	{[]string{"arn:aws-cn:eks:cn-north-1:1:cluster/a", "gke_p_europe-west1-b_b"},
		map[string]string{"arn:aws-cn:eks:cn-north-1:1:cluster/a": "a", "gke_p_europe-west1-b_b": "b"}},
	{[]string{"arn:aws:eks:x", "my_cluster", ""}, map[string]string{"arn:aws:eks:x": "arn:aws:eks:x", "my_cluster": "my_cluster", "": ""}},
}

func TestDisplayNames(t *testing.T) {
	for _, c := range nameCases {
		if got := DisplayNames(c.names); !maps.Equal(got, c.want) {
			t.Errorf("DisplayNames(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestDisplayNamesMatchModelJS(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	modelJS, err := filepath.Abs("../../src/js/model.js")
	if err != nil {
		t.Fatal(err)
	}
	var lists [][]string
	for _, c := range nameCases {
		lists = append(lists, c.names)
	}
	in, _ := json.Marshal(lists)
	script := `const m = require(process.argv[1]); console.log(JSON.stringify(JSON.parse(process.argv[2]).map(m.displayNames)));`
	out, err := exec.Command(nodeBin, "-e", script, modelJS, string(in)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var want []map[string]string
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	for i, c := range nameCases {
		if got := DisplayNames(c.names); !maps.Equal(got, want[i]) {
			t.Errorf("%q: Go %q, JS %q", c.names, got, want[i])
		}
	}
}

func TestFleetDisplayNames(t *testing.T) {
	b, err := os.ReadFile("../../fixtures/fleet-history.otlp.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join([]string{east, west, gke, "staging"}, ", "); s.Cluster != want {
		t.Errorf("cluster %q, want %q", s.Cluster, want)
	}
	if want := "prod (us-east-1), prod (eu-west-1), analytics, staging"; s.DisplayName != want {
		t.Errorf("display name %q, want %q", s.DisplayName, want)
	}
	var got []string
	for _, c := range s.Clusters {
		got = append(got, c.Cluster+" = "+c.DisplayName)
	}
	want := []string{east + " = prod (us-east-1)", west + " = prod (eu-west-1)", gke + " = analytics", "staging = staging"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("clusters:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// summary.json keeps the full names and adds the short ones.
	j, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cluster     string `json:"cluster"`
		DisplayName string `json:"display_name"`
		Clusters    []struct {
			Cluster     string `json:"cluster"`
			DisplayName string `json:"display_name"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(j, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Cluster != s.Cluster || doc.DisplayName != s.DisplayName || doc.Clusters[0].Cluster != east || doc.Clusters[0].DisplayName != "prod (us-east-1)" {
		t.Errorf("summary.json: %s", j)
	}

	// Each cluster's totals are its own, over its own history.
	var spend float64
	for _, c := range s.Clusters {
		spend += c.Totals.AnnualSpend
	}
	if diff := spend - s.Totals.AnnualSpend; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("fleet spend %v, clusters sum to %v", s.Totals.AnnualSpend, spend)
	}
}
