package savings

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/livenodes"
)

// storeNodes are node resources as a store without node labels gives them:
// only the names are known, unless set.
func storeNodes(cluster string, nodes ...contract.Node) contract.File {
	var f contract.File
	pts := []contract.Point{{T: 1_700_000_000, V: 4}, {T: 1_700_000_300, V: 4}}
	mem := []contract.Point{{T: 1_700_000_000, V: 16 << 30}, {T: 1_700_000_300, V: 16 << 30}}
	for _, n := range nodes {
		n := n
		for _, p := range []*string{&n.InstanceType, &n.Provider, &n.Region, &n.CapacityType} {
			if *p == "" {
				*p = "unknown"
			}
		}
		f.Resources = append(f.Resources, contract.Resource{
			Common:  contract.Common{Cluster: cluster, Source: "prometheus", StepSeconds: 300, Tier: 1},
			Node:    &n,
			Metrics: []contract.Series{{Name: contract.NodeCPU, Points: pts}, {Name: contract.NodeMemory, Points: mem}},
		})
	}
	return f
}

func apiNode(name, providerID string, labels map[string]string) contract.Node {
	return livenodes.Of(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.NodeSpec{ProviderID: providerID}})
}

// withNodes replaces the fixture's nodes with f's, in cluster name.
func withNodes(t *testing.T, name string, f contract.File) []any {
	t.Helper()
	var keep []any
	for _, rm := range renamed(t, "../../fixtures/savings-preview.otlp.json", name) {
		node := false
		for _, a := range rm.(map[string]any)["resource"].(map[string]any)["attributes"].([]any) {
			node = node || a.(map[string]any)["key"] == "k8s.node.name"
		}
		if !node {
			keep = append(keep, rm)
		}
	}
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return append(keep, doc["resourceMetrics"].([]any)...)
}

func nodesOf(f contract.File) []node {
	var out []node
	for _, r := range f.Resources {
		n := r.Node
		out = append(out, node{hostType: n.InstanceType, provider: n.Provider, region: n.Region, capacity: n.CapacityType, pool: n.Pool, typeSource: n.TypeSource, cores: 4, gib: 16})
	}
	return out
}

func TestLiveNodesPriceAStoreWithoutNodeLabels(t *testing.T) {
	// EKS: two live typed nodes, one live node of a type with no list price,
	// one the store labels, and two that were gone by the time the cluster was
	// read, one of them in a pool the store named.
	eks := storeNodes("eks",
		contract.Node{Name: "ip-10-0-0-1"},
		contract.Node{Name: "ip-10-0-0-2"},
		contract.Node{Name: "ip-10-0-0-3"},
		contract.Node{Name: "ip-10-0-0-4", InstanceType: "m5.xlarge", CapacityType: "on_demand"},
		contract.Node{Name: "ip-10-0-9-1"},
		contract.Node{Name: "ip-10-0-9-2", Pool: "batch"},
	)
	eksLive := []contract.Node{
		apiNode("ip-10-0-0-1", "aws:///us-east-1a/i-1", map[string]string{
			"node.kubernetes.io/instance-type": "m5.large", "topology.kubernetes.io/region": "us-east-1",
			"eks.amazonaws.com/capacityType": "ON_DEMAND", "eks.amazonaws.com/nodegroup": "general"}),
		apiNode("ip-10-0-0-2", "aws:///us-east-1b/i-2", map[string]string{
			"beta.kubernetes.io/instance-type": "c5.xlarge", "topology.kubernetes.io/region": "us-east-1",
			"karpenter.sh/capacity-type": "spot", "karpenter.sh/nodepool": "batch"}),
		apiNode("ip-10-0-0-3", "aws:///us-east-1a/i-3", map[string]string{"node.kubernetes.io/instance-type": "x9.mega"}),
		apiNode("ip-10-0-0-4", "aws:///us-east-1a/i-4", map[string]string{
			"node.kubernetes.io/instance-type": "m5.2xlarge", "karpenter.sh/capacity-type": "spot"}),
		apiNode("ip-10-0-0-5", "aws:///us-east-1a/i-5", map[string]string{"node.kubernetes.io/instance-type": "m5.large"}),
	}
	res := livenodes.Fill(&eks, eksLive)
	if res != (livenodes.Result{Nodes: 6, Listed: 5, Live: 4, FromStore: 1, FromAPI: 3, Gone: 2, Current: 6, CurrentListed: 4}) {
		t.Fatalf("eks fill: %+v", res)
	}
	if got, want := res.Text(), "4 of 6 nodes were live; instance types of 1 from the store and 3 from the Kubernetes API; 2 gone from the cluster with no instance type are priced by the live instance mix"; got != want {
		t.Errorf("text:\n got %s\nwant %s", got, want)
	}
	want := []contract.Node{
		{Name: "ip-10-0-0-1", InstanceType: "m5.large", Provider: "aws", Region: "us-east-1", CapacityType: "on_demand", Pool: "general", TypeSource: "kubernetes_api"},
		{Name: "ip-10-0-0-2", InstanceType: "c5.xlarge", Provider: "aws", Region: "us-east-1", CapacityType: "spot", Pool: "batch", TypeSource: "kubernetes_api"},
		{Name: "ip-10-0-0-3", InstanceType: "x9.mega", Provider: "aws", Region: "unknown", CapacityType: "unknown", TypeSource: "kubernetes_api"},
		// The store's labels win over the API's.
		{Name: "ip-10-0-0-4", InstanceType: "m5.xlarge", Provider: "aws", Region: "unknown", CapacityType: "on_demand", TypeSource: "store"},
		{Name: "ip-10-0-9-1", InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown", TypeSource: "live_mix"},
		{Name: "ip-10-0-9-2", InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown", Pool: "batch", TypeSource: "live_mix"},
	}
	for i, r := range eks.Resources {
		if *r.Node != want[i] {
			t.Errorf("node %d:\n got %+v\nwant %+v", i, *r.Node, want[i])
		}
	}

	// Per-node prices: m5.large on-demand, c5.xlarge at its spot price, no
	// price for x9.mega, the store's m5.xlarge on-demand.
	for i, p := range []struct {
		hourly, cores float64
		reason        string
	}{{0.096, 2, ""}, {0.0714, 4, ""}, {0, 0, "type"}, {0.192, 4, ""}, {0, 0, "type"}, {0, 0, "type"}} {
		got := priceNode(nodesOf(eks)[i])
		if got.reason != p.reason || !same(got.hourly, p.hourly) || got.reason == "" && got.cores != p.cores {
			t.Errorf("node %d price: %+v, want %+v", i, got, p)
		}
	}
	// ip-10-0-9-1 counts as the average of the three priced nodes,
	// ip-10-0-9-2 as the batch pool's c5.xlarge.
	cost, cores, gib := 0.096+0.0714+0.192, 2.0+4+4, 8.0+8+16
	cost, cores, gib = cost+cost/3+0.0714, cores+cores/3+4, gib+gib/3+8
	r := pricing(nodesOf(eks))
	if r.basis != "3 of 6 nodes at AWS us-east-1 on-demand and spot list prices and 2 that were gone from the cluster at the live instance mix" ||
		!same(r.perCore, cost/cores) || !same(r.perGiB, cost/gib) {
		t.Errorf("eks rates: %+v, want %v %v", r, cost/cores, cost/gib)
	}

	// GKE: a custom type and a node that was gone.
	gke := storeNodes("gke", contract.Node{Name: "gke-a"}, contract.Node{Name: "gke-gone"})
	livenodes.Fill(&gke, []contract.Node{apiNode("gke-a", "gce://p/us-central1-a/gke-a", map[string]string{
		"node.kubernetes.io/instance-type": "n2d-custom-8-32768", "topology.kubernetes.io/region": "us-central1",
		"cloud.google.com/gke-nodepool": "pool-1", "cloud.google.com/gke-spot": "false"})})
	hourly := 8*0.0325248 + 32*0.00435855
	if r := pricing(nodesOf(gke)); !same(r.perCore, hourly/8) || !same(r.perGiB, hourly/32) ||
		r.basis != "1 of 2 nodes at Google Cloud us-central1 on-demand list prices and 1 that were gone from the cluster at the live instance mix" {
		t.Errorf("gke rates: %+v", r)
	}

	// minikube: the one live node has no instance type, so the default node.
	mk := storeNodes("minikube", contract.Node{Name: "minikube"})
	if res := livenodes.Fill(&mk, []contract.Node{apiNode("minikube", "", map[string]string{"kubernetes.io/hostname": "minikube"})}); res.Untyped != 1 || res.Gone != 0 {
		t.Errorf("minikube fill: %+v", res)
	}
	if r := pricing(nodesOf(mk)); r.basis != "default node (4 cores, 16 GiB, $0.192 per hour)" {
		t.Errorf("minikube rates: %+v", r)
	}

	// Fallback: no node could be typed, so the gone nodes do not count.
	none := storeNodes("none", contract.Node{Name: "a"}, contract.Node{Name: "b"})
	livenodes.Fill(&none, []contract.Node{apiNode("a", "aws:///x/i-a", nil)})
	if r := pricing(nodesOf(none)); r.basis != "default node (4 cores, 16 GiB, $0.192 per hour)" {
		t.Errorf("no typed node: %+v", r)
	}

	// The summary and the page agree on every cluster.
	rms := append(withNodes(t, "eks", eks), withNodes(t, "gke", gke)...)
	rms = append(rms, withNodes(t, "minikube", mk)...)
	s := analyzeAll(t, rms)
	pricings := map[string]string{}
	for _, c := range s.Clusters {
		pricings[c.Cluster] = c.Pricing
	}
	if pricings["eks"] != r0(t, eks).basis || pricings["minikube"] != "default node (4 cores, 16 GiB, $0.192 per hour)" {
		t.Errorf("summary pricing: %v", pricings)
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	modelJS, err := filepath.Abs("../../src/js/model.js")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"resourceMetrics": rms})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	docPath, script := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "fleet.js")
	if err := os.WriteFile(docPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(liveMixParityScript), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(nodeBin, script, modelJS, docPath).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var js []struct {
		Name, Basis    string
		Spend, Savings float64
		Mixed          int
	}
	if err := json.Unmarshal(out, &js); err != nil {
		t.Fatal(err)
	}
	byName := map[string]ClusterSummary{}
	for _, c := range s.Clusters {
		byName[c.Cluster] = c
	}
	for _, c := range js {
		g := byName[c.Name]
		if g.Summary == nil || g.Pricing != c.Basis || !same(g.Totals.AnnualSpend, c.Spend) || !same(g.Totals.AnnualSavings, c.Savings) {
			t.Errorf("%s: go %+v, js %+v", c.Name, g.Summary, c)
		}
	}
	if len(js) != 3 || js[0].Mixed+js[1].Mixed+js[2].Mixed != 3 {
		t.Errorf("js clusters: %+v", js)
	}
}

func r0(t *testing.T, f contract.File) rates {
	t.Helper()
	return pricing(nodesOf(f))
}

const liveMixParityScript = `
const model = require(process.argv[2]);
const doc = JSON.parse(require('fs').readFileSync(process.argv[3], 'utf8'));
const data = model.prepare(doc);
const r = model.analyze(doc);
console.log(JSON.stringify(r.clusters.map(c => {
  const p = model.pricing(data.nodes.filter(n => n.cluster === c.name));
  return { name: c.name, spend: c.spend, savings: c.savings, basis: p.basis, mixed: p.mixed };
})));
`
