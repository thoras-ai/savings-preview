// Command gen_demo writes the demo cluster: 60 workloads in 8 namespaces on
// four node pools, one of them spot, with 14 days of usage at 5-minute steps.
// It writes the compact file the report page reads unless -full is given.
// With -fleet it writes the fleet sample instead: three clusters built from
// the demo cluster's workloads, and a fourth that could not be read, in one
// compact file. Output is deterministic.
//
// Usage, from the repository root:
//
//	go run ./scripts/gen_demo [-full] [-o FILE]
//	go run ./scripts/gen_demo -fleet [-o FILE]
package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thoras-ai/savings-preview/internal/compact"
	"github.com/thoras-ai/savings-preview/internal/fleet"
)

const (
	defaultOut = "fixtures/demo-cluster.compact.otlp.json"
	fleetOut   = "fixtures/fleet.compact.otlp.json"
)

func main() {
	full := flag.Bool("full", false, "write the file at full resolution instead of compact")
	fleetFlag := flag.Bool("fleet", false, "write the fleet sample instead of the demo cluster")
	out := flag.String("o", "", "file to write (default "+defaultOut+", or "+fleetOut+" with -fleet)")
	flag.Parse()
	var err error
	switch {
	case *fleetFlag && *full:
		err = fmt.Errorf("-fleet writes a compact file; -full does not apply")
	case *fleetFlag:
		*out = cmp.Or(*out, fleetOut)
		err = runFleet(*out)
	default:
		*out = cmp.Or(*out, defaultOut)
		err = run(*out, *full)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen_demo:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", *out)
}

func run(out string, full bool) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if full {
		err = write(f)
	} else {
		err = writeCompact(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeCompact(w io.Writer) error { return writeCompactCluster(w, demo) }

func writeCompactCluster(w io.Writer, sp spec) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeCluster(pw, sp)) }()
	_, err := compact.Compact(pr, w, compact.DefaultStep)
	pr.CloseWithError(err)
	return err
}

// The fleet sample: clusters of different sizes in two regions, and one whose
// credentials had expired.
var (
	fleetClusters = []spec{
		{name: "prod-us-east-1", region: "us-east-1", replicas: 1, pools: pools,
			namespaces: []string{"web", "api", "workers", "data", "kube-system"}},
		{name: "prod-eu-west-1", region: "eu-west-1", replicas: 0.6, seed: 1000,
			namespaces: []string{"web", "api", "kube-system"},
			pools:      []pool{{"m6i.2xlarge", "on_demand", 8, 7.91, 29.9, 1}, {"r6i.2xlarge", "on_demand", 2, 7.91, 61.4, 2}, {"m6i.large", "on_demand", 2, 1.93, 6.9, 4}}},
		{name: "staging-us-east-1", region: "us-east-1", replicas: 0.3, seed: 2000,
			namespaces: []string{"web", "batch"},
			pools:      []pool{{"m6i.xlarge", "on_demand", 5, 3.92, 14.8, 1}}},
	}
	fleetFailed = []fleet.Failed{{Cluster: "dev-us-west-2",
		Reason: "list services: Unauthorized: the credentials for this context have expired"}}
)

// runFleet writes each cluster's compact file, as run does, and joins them.
func runFleet(out string) error {
	dir, err := os.MkdirTemp("", "gen_fleet")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var paths []string
	for _, sp := range fleetClusters {
		p := filepath.Join(dir, sp.name+".json")
		f, err := os.Create(p)
		if err != nil {
			return err
		}
		err = writeCompactCluster(f, sp)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		paths = append(paths, p)
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	err = fleet.Merge(f, paths, fleetFailed)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
