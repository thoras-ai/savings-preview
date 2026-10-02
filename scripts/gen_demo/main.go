// Command gen_demo writes the demo cluster: 60 workloads in 8 namespaces on
// four node pools, one of them spot, with 14 days of usage at 5-minute steps.
// It writes the compact file the report page reads unless -full is given.
// Output is deterministic.
//
// Usage, from the repository root:
//
//	go run ./scripts/gen_demo [-full] [-o FILE]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/thoras-ai/savings-preview/internal/compact"
)

const defaultOut = "fixtures/demo-cluster.compact.otlp.json"

func main() {
	full := flag.Bool("full", false, "write the file at full resolution instead of compact")
	out := flag.String("o", defaultOut, "file to write")
	flag.Parse()
	if err := run(*out, *full); err != nil {
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

func writeCompact(w io.Writer) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(pw)) }()
	_, err := compact.Compact(pr, w, compact.DefaultStep)
	pr.CloseWithError(err)
	return err
}
