// Command savings-preview reads a cluster's usage history and writes it as one
// OTel metrics file.
package main

import (
	"fmt"
	"os"

	"github.com/thoras-ai/savings-preview/internal/adapters/datadog"
	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
)

const usage = `usage: savings-preview <source> [flags]

Sources:
  prometheus   Prometheus-compatible store, reached through the Kubernetes API server
               or by URL (Mimir, Grafana Cloud, Amazon Managed Prometheus)
  poll         Requests, nodes and metrics-server usage read at an interval (alias: snapshot)
  datadog      Datadog metrics query API

Run "savings-preview <source> -h" for the flags of a source.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "prometheus":
		err = runPrometheus(os.Args[2:])
	case "poll", "snapshot":
		err = polling.Main(os.Args[1], os.Args[2:])
	case "datadog":
		err = datadog.Main(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown source %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
