// Package livenodes lists a cluster's nodes from the Kubernetes API and fills
// in the node attributes a metrics store did not give.
package livenodes

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
	"github.com/thoras-ai/savings-preview/internal/contract"
)

// PoolLabels are the node labels that name a node pool or node group, in the
// order they are read.
var PoolLabels = []string{
	"karpenter.sh/nodepool",
	"karpenter.sh/provisioner-name",
	"eks.amazonaws.com/nodegroup",
	"cloud.google.com/gke-nodepool",
	"kubernetes.azure.com/agentpool",
	"agentpool",
}

const pageSize = 500

// List lists the cluster's nodes, a page at a time, and reads their
// attributes.
func List(ctx context.Context, cs kubernetes.Interface) ([]contract.Node, error) {
	var out []contract.Node
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		l, err := cs.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, Of(&l.Items[i]))
		}
		if l.Continue == "" {
			return out, nil
		}
		opts.Continue = l.Continue
	}
}

// Of reads a node's attributes from its labels and provider ID.
func Of(n *corev1.Node) contract.Node {
	a := polling.Attrs(n)
	node := contract.Node{Name: a.Name, InstanceType: a.InstanceType, Provider: a.Provider, Region: a.Region, CapacityType: a.CapacityType}
	for _, k := range PoolLabels {
		if v := n.Labels[k]; v != "" {
			node.Pool = v
			break
		}
	}
	return node
}

func known(v string) bool { return v != "" && v != "unknown" }

// Needed reports whether any node resource in f lacks an instance type, a
// cloud provider or a capacity type, so that listing the nodes could add
// something.
func Needed(f *contract.File) bool {
	for _, r := range f.Resources {
		if n := r.Node; n != nil && r.Error == "" && (!known(n.InstanceType) || !known(n.Provider) || !known(n.CapacityType)) {
			return true
		}
	}
	return false
}

// Result counts the node resources of a file after Fill.
type Result struct {
	Nodes     int // node resources in the file
	Listed    int // nodes the API listed
	Live      int // node resources whose node the API listed
	FromStore int // instance type given by the store
	FromAPI   int // instance type read from the API
	Gone      int // no instance type and not in the API's list: priced by the live mix
	Untyped   int // no instance type, though the API lists the node
	// Current counts the nodes that report at the end of the data, and
	// CurrentListed those of them the API lists.
	Current, CurrentListed int
	// Mismatch is set when fewer than half of the nodes that report at the
	// end of the data are in the API's list, so the data and the API are not
	// the same cluster and nothing was filled.
	Mismatch bool
}

// Fill completes the node resources of f from the live nodes: the instance
// type, cloud provider, region, capacity type and pool the store left
// unknown. What the store gives is kept. It records where each known
// instance type came from in TypeSource, and marks a node with no instance
// type that the API does not list, one that left the cluster, to be priced by
// the instance mix of the others.
func Fill(f *contract.File, live []contract.Node) Result {
	byName := make(map[string]contract.Node, len(live))
	for _, n := range live {
		byName[n.Name] = n
	}
	res := Result{Listed: len(live)}
	var end int64
	for _, r := range f.Resources {
		if r.Node != nil && r.Error == "" {
			res.Nodes++
			if _, ok := byName[r.Node.Name]; ok {
				res.Live++
			}
			end = max(end, last(r))
		}
	}
	// The nodes that report at the end of the data are the ones running when
	// the cluster was read, so most of them must be in the list. A name or
	// two in common is not enough: node names can repeat across clusters.
	current, listed := 0, 0
	for _, r := range f.Resources {
		if r.Node != nil && r.Error == "" && last(r) >= end-max(3*int64(r.StepSeconds), 900) {
			current++
			if _, ok := byName[r.Node.Name]; ok {
				listed++
			}
		}
	}
	res.Current, res.CurrentListed = current, listed
	res.Mismatch = res.Nodes > 0 && (res.Live == 0 || 2*listed < current)
	for _, r := range f.Resources {
		n := r.Node
		if n == nil || r.Error != "" {
			continue
		}
		if known(n.InstanceType) && n.TypeSource == "" {
			n.TypeSource = contract.TypeFromStore
		}
		l, ok := byName[n.Name]
		if !ok && !res.Mismatch && !known(n.InstanceType) {
			n.TypeSource = contract.TypeFromLiveMix
		}
		if ok && !res.Mismatch {
			if !known(n.InstanceType) && known(l.InstanceType) {
				n.InstanceType, n.TypeSource = l.InstanceType, contract.TypeFromAPI
			}
			for _, x := range []struct {
				dst *string
				src string
			}{
				{&n.Provider, l.Provider}, {&n.Region, l.Region}, {&n.CapacityType, l.CapacityType}, {&n.Pool, l.Pool},
			} {
				if !known(*x.dst) && known(x.src) {
					*x.dst = x.src
				}
			}
		}
		switch n.TypeSource {
		case contract.TypeFromStore:
			res.FromStore++
		case contract.TypeFromAPI:
			res.FromAPI++
		case contract.TypeFromLiveMix:
			res.Gone++
		default:
			res.Untyped++
		}
	}
	return res
}

// Text describes where the nodes' instance types came from, for the run's
// output.
func (r Result) Text() string {
	switch {
	case r.Mismatch:
		return fmt.Sprintf("only %d of the %d nodes reporting at the end of the data are among the %d nodes the Kubernetes API lists, so the data and the API seem to be different clusters and instance types come from the store only", r.CurrentListed, r.Current, r.Listed)
	case r.Nodes == 0:
		return "no node resources to price"
	}
	s := fmt.Sprintf("%d of %d nodes were live; instance types of %d from the store and %d from the Kubernetes API", r.Live, r.Nodes, r.FromStore, r.FromAPI)
	if r.Untyped > 0 {
		s += fmt.Sprintf("; %d live %s no instance type", r.Untyped, plural(r.Untyped, "node has", "nodes have"))
	}
	switch {
	case r.FromStore+r.FromAPI == 0:
		s += "; no node has an instance type, so workloads are priced with the default node"
	case r.Gone > 0:
		s += fmt.Sprintf("; %d gone from the cluster with no instance type %s priced by the live instance mix", r.Gone, plural(r.Gone, "is", "are"))
	}
	return s
}

// last is the time of a node resource's latest point.
func last(r contract.Resource) int64 {
	var t int64
	for _, m := range r.Metrics {
		if n := len(m.Points); n > 0 {
			t = max(t, m.Points[n-1].T)
		}
	}
	return t
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
