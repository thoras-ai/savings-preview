package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/kube"
)

func TestFillNodes(t *testing.T) {
	f := &contract.File{Resources: []contract.Resource{
		{Node: &contract.Node{Name: "a", InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown"}},
		{Node: &contract.Node{Name: "gone", InstanceType: "unknown", Provider: "unknown", Region: "unknown", CapacityType: "unknown"}},
	}}
	old := listNodes
	defer func() { listNodes = old }()
	calls := 0
	listNodes = func(context.Context, *kube.Conn) ([]contract.Node, error) {
		calls++
		return []contract.Node{{Name: "a", InstanceType: "m5.large", Provider: "aws", Region: "us-east-1", CapacityType: "on_demand"}}, nil
	}
	var w bytes.Buffer
	fillNodes(context.Background(), nil, f, &w)
	if calls != 0 || w.Len() != 0 {
		t.Fatalf("without a connection the nodes are not listed: %d calls, %q", calls, w.String())
	}
	fillNodes(context.Background(), &kube.Conn{}, f, &w)
	want := "nodes: 1 of 2 nodes were live; instance types of 0 from the store and 1 from the Kubernetes API; 1 gone from the cluster with no instance type is priced by the live instance mix\n"
	if w.String() != want || f.Resources[0].Node.InstanceType != "m5.large" || f.Resources[1].Node.TypeSource != contract.TypeFromLiveMix {
		t.Errorf("output %q, nodes %+v %+v", w.String(), f.Resources[0].Node, f.Resources[1].Node)
	}
	// Nothing left to fill: no second list.
	f.Resources = f.Resources[:1]
	w.Reset()
	fillNodes(context.Background(), &kube.Conn{}, f, &w)
	if calls != 1 {
		t.Errorf("listed again: %d", calls)
	}

	listNodes = func(context.Context, *kube.Conn) ([]contract.Node, error) {
		return nil, errors.New(`nodes is forbidden: User "sa" cannot list resource "nodes"`)
	}
	g := &contract.File{Resources: []contract.Resource{{Node: &contract.Node{Name: "a", InstanceType: "unknown"}}}}
	fillNodes(context.Background(), &kube.Conn{}, g, &w)
	if !strings.Contains(w.String(), "warning: could not list the nodes") || g.Resources[0].Node.TypeSource != "" {
		t.Errorf("forbidden list: %q %+v", w.String(), g.Resources[0].Node)
	}
}

func TestCheckInstanceTypes(t *testing.T) {
	for _, c := range []struct {
		name         string
		typed, total int
		live         []contract.Node
		liveErr      error
		source, note string
	}{
		{"store", 3, 3, nil, nil, "store", "the store labels all 3 nodes with an instance type"},
		{"api", 0, 0, []contract.Node{{Name: "a", InstanceType: "m5.large"}, {Name: "b", InstanceType: "unknown"}}, nil, "kubernetes_api",
			"the store has no node labels (kube_node_labels); 1 of 2 live nodes have one in the Kubernetes API, and nodes gone from the cluster would be priced by the live instance mix"},
		{"minikube", 0, 1, []contract.Node{{Name: "minikube", InstanceType: "unknown"}}, nil, "none",
			"the store labels 0 of 1 nodes with an instance type, and none of the 1 live nodes has an instance-type label, so workloads would be priced with the default node"},
		{"forbidden", 0, 2, nil, errors.New("forbidden"), "none", "the store labels 0 of 2 nodes with an instance type; the nodes cannot be listed for the rest: forbidden"},
	} {
		e := checkEnv{
			storeTypes: func(context.Context, prometheus.Candidate) (int, int, error) { return c.typed, c.total, nil },
			nodes:      func(context.Context) ([]contract.Node, error) { return c.live, c.liveErr },
		}
		source, note := instanceTypes(context.Background(), e, storeA)
		if source != c.source || note != c.note {
			t.Errorf("%s: %q, %q", c.name, source, note)
		}
	}
}
