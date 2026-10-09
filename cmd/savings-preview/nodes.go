package main

import (
	"context"
	"fmt"
	"io"

	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/kube"
	"github.com/thoras-ai/savings-preview/internal/livenodes"
)

// listNodes lists the cluster's nodes. Tests replace it.
var listNodes = func(ctx context.Context, conn *kube.Conn) ([]contract.Node, error) {
	return livenodes.List(ctx, conn.Clientset)
}

// fillNodes completes the node attributes the store left unknown from the
// cluster's live nodes, when conn is set and some are missing, and says what
// it found. A failed list leaves the file as the store gave it.
func fillNodes(ctx context.Context, conn *kube.Conn, f *contract.File, w io.Writer) {
	if conn == nil || f == nil || !livenodes.Needed(f) {
		return
	}
	live, err := listNodes(ctx, conn)
	if err != nil {
		fmt.Fprintf(w, "warning: could not list the nodes to read the instance types the store lacks, so nodes are priced from the store's labels only: %v\n", err)
		return
	}
	fmt.Fprintln(w, "nodes:", livenodes.Fill(f, live).Text())
}
