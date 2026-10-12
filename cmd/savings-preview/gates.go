package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	confirmpkg "github.com/thoras-ai/savings-preview/internal/confirm"
)

// gate prints p and waits for Enter as confirmpkg.Stdio does, with Ctrl-C
// quitting.
func gate(p confirmpkg.Plan, yes bool) bool {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return confirmpkg.Stdio(ctx, p, yes)
}

// contextsTitle names the clusters a command reads, for its plan.
func contextsTitle(cmd string, o *runOptions) string {
	switch {
	case o.fleet:
		names, err := fleetContexts(o)
		if err != nil || len(names) == 0 {
			return cmd + ", the contexts asked for"
		}
		if len(names) > 1 {
			return fmt.Sprintf("%s, contexts %s", cmd, strings.Join(names, ", "))
		}
		return cmd + ", context " + names[0]
	case o.context != "":
		return cmd + ", context " + o.context
	}
	return cmd + ", the current kubeconfig context"
}

// checkPlan is what check is about to do.
func checkPlan(o *runOptions) confirmpkg.Plan {
	p := confirmpkg.Plan{Title: contextsTitle("savings-preview check", o)}
	p.Reads = append(p.Reads,
		"ask the API server who you are and what you may do (as kubectl auth can-i)",
		"ask whether metrics-server answers")
	switch {
	case o.direct.url != "":
		p.Reads = append(p.Reads, fmt.Sprintf("ask the store at %s whether it answers and how far back its history goes", prometheus.RedactURL(o.direct.url)))
	default:
		p.Reads = append(p.Reads,
			"list Services, and Prometheus Operator resources if installed, to find a metrics store",
			"ask each store found, through the API server's service proxy, whether it answers and how far back its history goes")
	}
	if o.useDatadog || o.datadog.KeysGiven() {
		p.Reads = append(p.Reads, "ask the Datadog API whether the keys are valid")
	}
	p.Reads = append(p.Reads, "read no usage history")
	p.After = "Then it prints whether run can read usage data, from where, and at what tier."
	return p
}

// prometheusPlan is what the prometheus command is about to do.
func prometheusPlan(kctx, url, target, out string, nDays int, list bool) confirmpkg.Plan {
	title := "savings-preview prometheus, the current kubeconfig context"
	if kctx != "" {
		title = "savings-preview prometheus, context " + kctx
	}
	if url != "" && !list {
		title = "savings-preview prometheus, " + prometheus.RedactURL(url)
	}
	p := confirmpkg.Plan{Title: title}
	history := fmt.Sprintf("%s of CPU and memory usage, requests and replica counts", days(nDays))
	switch {
	case list:
		p.Reads = append(p.Reads, "list Services, and Prometheus Operator resources if installed, to find metrics stores",
			"ask each store found whether it answers")
		p.After = "Then it prints what it found and exits."
		return p
	case url != "":
		p.Reads = append(p.Reads, fmt.Sprintf("query the store at %s directly for %s (read-only queries)", prometheus.RedactURL(url), history))
	case target != "":
		p.Reads = append(p.Reads, fmt.Sprintf("query %s through the API server's service proxy for %s (GET requests only)", target, history))
	default:
		p.Reads = append(p.Reads, "list Services, and Prometheus Operator resources if installed, to find a metrics store",
			fmt.Sprintf("query that store through the API server's service proxy for %s (GET requests only)", history))
	}
	p.Writes = "It writes " + inDir(out) + "."
	return p
}

// inDir says "name, in /abs/dir".
func inDir(path string) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		dir = filepath.Dir(path)
	}
	return filepath.Base(path) + ", in " + dir
}
