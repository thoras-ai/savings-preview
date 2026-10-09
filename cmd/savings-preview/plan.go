package main

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/thoras-ai/savings-preview/internal/kube"
	"github.com/thoras-ai/savings-preview/internal/savings"
)

// planTimeout bounds the reads run makes for each cluster before it prints
// the plan, as check's default --timeout does.
const planTimeout = time.Minute

// timeNow is the time the checks measure history up to; tests replace it.
var timeNow = time.Now

// interactive reports whether run may wait for Enter: only when both stdin
// and stdout are terminals, so scripts, pipes and CI never wait.
func interactive(in, out *os.File) bool {
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// preflight finds each cluster's source as check does, prints the plan and,
// when ask is set and --yes is not, waits for Enter. It reports whether the
// run goes on; false means the user quit and nothing was read.
func preflight(ctx context.Context, o *runOptions, in io.Reader, w io.Writer, ask bool) (bool, error) {
	fmt.Fprintln(w, "Looking for usage data...")
	cs, err := planChecks(ctx, o)
	if err != nil {
		return false, err
	}
	if ctx.Err() != nil {
		fmt.Fprintln(w, "Stopped; no history was read.")
		return false, nil
	}
	printPlan(w, o, cs)
	o.planned = true
	if !ask || o.yes {
		fmt.Fprintln(w)
		return true, nil
	}
	if !confirm(ctx, in, w) {
		fmt.Fprintln(w, "Stopped; no history was read.")
		return false, nil
	}
	fmt.Fprintln(w)
	return true, nil
}

// planChecks checks each cluster the run reads.
func planChecks(ctx context.Context, o *runOptions) ([]clusterCheck, error) {
	if !o.fleet {
		cctx, cancel := context.WithTimeout(ctx, planTimeout)
		defer cancel()
		e := newFleetCheckEnv(o)
		e.connErr = kube.WithCommand(e.connErr, "savings-preview")
		return []clusterCheck{{Context: o.context, Report: check(cctx, o, e, timeNow())}}, nil
	}
	names, err := fleetContexts(o)
	if err != nil {
		return nil, err
	}
	return checkContexts(ctx, o, names, planTimeout, "savings-preview"), nil
}

// printPlan prints what each cluster will be read from, what the run reads
// and writes, and what it does at the end.
func printPlan(w io.Writer, o *runOptions, cs []clusterCheck) {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = planName(o, c)
	}
	short := savings.DisplayNames(names)
	fmt.Fprintln(w)
	for i, c := range cs {
		fmt.Fprintf(w, "%s: %s\n", short[names[i]], planSource(o, c.Report))
	}
	fmt.Fprintln(w)
	printReads(w, o)

	dir, err := filepath.Abs(filepath.Dir(o.out))
	if err != nil {
		dir = filepath.Dir(o.out)
	}
	files := fmt.Sprintf("%s, %s and %s", filepath.Base(o.out), filepath.Base(compactName(o.out)), filepath.Base(zipName(o.out)))
	if o.fleet {
		files = fmt.Sprintf("%s, %s, %s and each cluster's files in %s", filepath.Base(o.out), filepath.Base(compactName(o.out)),
			filepath.Base(zipName(o.out)), filepath.Base(clustersDir(o.out)))
	}
	fmt.Fprintf(w, "It writes %s, in %s. Nothing is uploaded.\n", files, dir)
	switch {
	case o.collectOnly:
		fmt.Fprintln(w, "Then it exits. It does not open a browser or serve the report.")
	case o.noOpen:
		fmt.Fprintln(w, "Then it serves the report on a local address and prints the address. It does not open a browser.")
	default:
		fmt.Fprintln(w, "Then it opens the report in your browser.")
	}
}

// planName is the name a cluster goes by in the plan.
func planName(o *runOptions, c clusterCheck) string {
	name := c.Context
	if r := c.Report; name == "" && r.Kubernetes != nil {
		name = r.Kubernetes.Context
	}
	return cmp.Or(o.cluster, name, "cluster")
}

// planSource says where run will read a cluster from, or why it cannot.
func planSource(o *runOptions, r *checkReport) string {
	s := r.Source
	switch s.Name {
	case "prometheus":
		line := storeTitle(s.From)
		if h := sourceHistory(r); h != "" {
			line += ", " + h + " of history"
		}
		return line
	case "datadog":
		return fmt.Sprintf("%s, up to %s of history", s.From, days(o.days))
	case "poll":
		line := "will poll metrics-server "
		if o.source != "poll" {
			line = "no store found, " + line
			if len(r.Stores) > 0 {
				line = "no usable store found, will poll metrics-server "
			}
		}
		if o.duration > 0 {
			return line + "for " + shortDur(o.duration)
		}
		return line + "until you press Ctrl-C"
	}
	problem := r.Summary
	if r.Kubernetes != nil && r.Kubernetes.Error != "" {
		problem = r.Kubernetes.Error
	}
	return "cannot run: " + oneLine(problem)
}

var storeKinds = map[string]string{"prometheus": "Prometheus", "thanos-query": "Thanos Query", "victoriametrics": "VictoriaMetrics"}

// storeTitle shortens a found store's name, "prometheus monitoring/prom:9090
// (how it was found)", to "Prometheus monitoring/prom:9090".
func storeTitle(from string) string {
	name, _, _ := strings.Cut(from, " (")
	kind, rest, ok := strings.Cut(name, " ")
	if t, known := storeKinds[kind]; ok && known {
		return t + " " + rest
	}
	return name
}

// confirm waits for Enter, q or Ctrl-C, and reports whether to go on. The end
// of the input quits.
func confirm(ctx context.Context, in io.Reader, w io.Writer) bool {
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(w, "\nPress Enter to continue, or q to quit. ")
		line := make(chan string, 1)
		go func() {
			s, err := r.ReadString('\n')
			if err != nil && s == "" {
				s = "q"
			}
			line <- s
		}()
		select {
		case <-ctx.Done():
			fmt.Fprintln(w)
			return false
		case s := <-line:
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "", "y", "yes":
				return true
			case "q", "quit", "n", "no":
				return false
			}
		}
	}
}

// printSend says what to do with the zip after a --collect-only run.
func printSend(w io.Writer, zip string) {
	fmt.Fprintf(w, "\nSend %s to your Thoras contact. It holds the report and the data, and nothing else from your environment.\n", zip)
}
