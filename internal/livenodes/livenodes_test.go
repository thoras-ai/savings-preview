package livenodes

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/thoras-ai/savings-preview/internal/contract"
)

func TestListReadsEveryPage(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls []string
	cs.PrependReactor("list", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		opts := a.(k8stesting.ListActionImpl).GetListOptions()
		calls = append(calls, fmt.Sprintf("limit=%d continue=%q", opts.Limit, opts.Continue))
		page := &corev1.NodeList{}
		name := "a"
		if opts.Continue == "" {
			page.Continue = "next"
		} else {
			name = "b"
		}
		page.Items = []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"node.kubernetes.io/instance-type": "m5.large", "karpenter.sh/nodepool": "default", "karpenter.sh/capacity-type": "spot"}},
			Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-" + name}}}
		return true, page, nil
	})
	nodes, err := List(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != `limit=500 continue=""` || calls[1] != `limit=500 continue="next"` {
		t.Errorf("calls: %q", calls)
	}
	want := contract.Node{Name: "b", InstanceType: "m5.large", Provider: "aws", Region: "unknown", CapacityType: "spot", Pool: "default"}
	if len(nodes) != 2 || nodes[0].Name != "a" || nodes[1] != want {
		t.Errorf("nodes: %+v", nodes)
	}
}

func file(nodes ...contract.Node) *contract.File {
	f := &contract.File{}
	for _, n := range nodes {
		n := n
		f.Resources = append(f.Resources, contract.Resource{Node: &n})
	}
	return f
}

func TestNeeded(t *testing.T) {
	full := contract.Node{Name: "a", InstanceType: "m5.large", Provider: "aws", Region: "unknown", CapacityType: "on_demand"}
	if Needed(file(full)) {
		t.Error("a node with type, cloud and capacity type needs nothing")
	}
	for _, n := range []contract.Node{
		{Name: "a", InstanceType: "unknown", Provider: "aws", CapacityType: "spot"},
		{Name: "a", InstanceType: "m5.large", Provider: "unknown", CapacityType: "spot"},
		{Name: "a", InstanceType: "m5.large", Provider: "aws", CapacityType: "unknown"},
	} {
		if !Needed(file(full, n)) {
			t.Errorf("%+v needs the API", n)
		}
	}
}

// A list from another cluster than the store's fills nothing.
func TestFillOtherCluster(t *testing.T) {
	f := file(contract.Node{Name: "a", InstanceType: "unknown"}, contract.Node{Name: "b", InstanceType: "c5.large"})
	res := Fill(f, []contract.Node{{Name: "x", InstanceType: "m5.large"}})
	if !res.Mismatch || res.Live != 0 || res.Gone != 0 || res.FromStore != 1 || res.Untyped != 1 {
		t.Errorf("result: %+v", res)
	}
	if n := f.Resources[0].Node; n.InstanceType != "unknown" || n.TypeSource != "" {
		t.Errorf("node a: %+v", n)
	}
	if got, want := res.Text(), "none of the 2 nodes in the data is among the 1 nodes the Kubernetes API lists, so instance types come from the store only"; got != want {
		t.Errorf("text %q", got)
	}
}
