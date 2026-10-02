// Command savings-preview reads a cluster's usage history, writes it as one
// OTel metrics file and shows what Thoras would have saved.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/thoras-ai/savings-preview/internal/adapters/datadog"
	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
)

const usage = `usage: savings-preview [command] [flags]

Commands:
  run          Find usage data, write and check the file, open the report (default)
  prometheus   Write the file from a Prometheus-compatible store, reached through the Kubernetes API server
               or by URL (Mimir, Grafana Cloud, Amazon Managed Prometheus)
  poll         Write the file from requests, nodes and metrics-server usage read at an interval (alias: snapshot)
  datadog      Write the file from the Datadog metrics query API
  export       Write a compact copy of a file, or the file for a poll's readings
  check        Say whether run can read usage data, from where and at what tier, without reading any history

Run "savings-preview <command> -h" for the flags of a command.
"savings-preview --version" prints the version.
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help":
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		case "-version", "--version":
			fmt.Println("savings-preview", versionString())
			return
		}
	}
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = runRun(args)
	case "prometheus":
		err = runPrometheus(args)
	case "poll", "snapshot":
		err = polling.Main(cmd, args)
	case "datadog":
		err = datadog.Main(args)
	case "export":
		err = runExport(args)
	case "check":
		os.Exit(runCheck(args))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
