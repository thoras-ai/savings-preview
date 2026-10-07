package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/thoras-ai/savings-preview/internal/fleet"
	"github.com/thoras-ai/savings-preview/internal/kube"
)

// fleetConcurrency is how many clusters are read at once. Polling is the
// exception: every cluster that polls does so in the same window.
const fleetConcurrency = 3

// contextList is the value of --context, which may be given more than once.
type contextList []string

func (c *contextList) String() string { return strings.Join(*c, ",") }
func (c *contextList) Set(v string) error {
	*c = append(*c, v)
	return nil
}

// checkFleet refuses the flags that name one cluster or one store when
// several clusters are asked for.
func (o *runOptions) checkFleet(target string) error {
	switch {
	case o.direct.url != "" || strings.Contains(target, "://"):
		return errors.New("--url names one store; it cannot be used with several contexts. Read a store that holds several clusters once per cluster with --selector, and open the files together in the report")
	case o.cluster != "":
		return errors.New("--cluster-name names one cluster; with several contexts each cluster is named after its kubeconfig cluster")
	case o.source == "datadog":
		return errors.New("--source datadog reads one cluster per run; run it once per cluster with --selector, and open the files together in the report")
	}
	return nil
}

// fleetContexts is the contexts named by --context, --contexts-file and
// --all-contexts, in that order, without repeats.
func fleetContexts(o *runOptions) ([]string, error) {
	names := slices.Clone([]string(o.contexts))
	if o.contextsFile != "" {
		b, err := os.ReadFile(o.contextsFile)
		if err != nil {
			return nil, fmt.Errorf("--contexts-file: %w", err)
		}
		names = append(names, parseContexts(string(b))...)
	}
	if o.allContexts {
		all, err := kube.Contexts(o.kubeconfig)
		if err != nil {
			return nil, err
		}
		names = append(names, all...)
	}
	var out []string
	for _, n := range names {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no contexts to read: --contexts-file and --all-contexts named none")
	}
	return out, nil
}

// parseContexts reads one context name per line; # starts a comment.
func parseContexts(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// member is one cluster of a fleet run.
type member struct {
	context, cluster string
	conn             *kube.Conn
	out              string    // the cluster's own contract file
	log              io.Writer // progress, prefixed with the context
	opts             runOptions
	err              error
	poll             bool // the cluster is read by polling, with the others
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// clustersDir holds each cluster's own files, next to out.
func clustersDir(out string) string {
	base := strings.TrimSuffix(out, filepath.Ext(out))
	return strings.TrimSuffix(base, ".otlp") + ".clusters"
}

var errPollLater = errors.New("poll with the other clusters")

// The cluster calls of a fleet run; tests replace them.
var (
	connectFleet  = kube.Connect
	memberSources = newSources
	memberPoll    = pollWithProgress
)

// runFleet reads each context's cluster, writes every cluster's resources to
// o.out with its compact copy, and builds the report and the zip. A cluster
// that cannot be read is reported and left out; the run fails only when no
// cluster can be read.
func runFleet(ctx context.Context, o *runOptions, w io.Writer) (*builtReport, error) {
	names, err := fleetContexts(o)
	if err != nil {
		return nil, err
	}
	dir := clustersDir(o.out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "Reading %d %s: %s\n", len(names), plural(len(names), "cluster", "clusters"), strings.Join(names, ", "))
	if o.source == "auto" && o.datadog.KeysGiven() {
		fmt.Fprintln(w, "Datadog keys are set, but Datadog reads one cluster per run, so each cluster is read through its own API server.")
	}
	printAccess(w, o)

	var mu sync.Mutex
	members := make([]*member, len(names))
	for i, n := range names {
		pw := &prefixWriter{mu: &mu, w: w, prefix: "[" + n + "] "}
		m := &member{context: n, log: pw}
		members[i] = m
		if m.conn, m.err = connectFleet(o.kubeconfig, n); m.err == nil {
			m.cluster = m.conn.ClusterName
		}
	}
	nameClusters(members, w)
	used := map[string]bool{}
	for _, m := range members {
		if m.err != nil {
			m.cluster = cmp.Or(m.cluster, m.context)
			continue
		}
		file := unsafeName.ReplaceAllString(m.cluster, "_")
		for k := 2; used[file]; k++ {
			file = fmt.Sprintf("%s-%d", unsafeName.ReplaceAllString(m.cluster, "_"), k)
		}
		used[file] = true
		m.out = filepath.Join(dir, file+".otlp.json")
		m.opts = *o
		m.opts.context, m.opts.contexts, m.opts.allContexts, m.opts.contextsFile = m.context, nil, false, ""
		m.opts.fleet, m.opts.member = false, true
		m.opts.out, m.opts.cluster, m.opts.log = m.out, m.cluster, m.log
	}

	// Stores are read a few clusters at a time. A cluster without a usable
	// store waits, so that every cluster that polls polls in one window.
	each(members, fleetConcurrency, func(m *member) {
		if m.err != nil {
			return
		}
		s, err := memberSources(&m.opts, m.log)
		if err != nil {
			m.err = err
			return
		}
		s.poll = func(context.Context) error { return errPollLater }
		switch err := collect(ctx, s, &m.opts, m.log); {
		case errors.Is(err, errPollLater):
			m.poll = true
		case err != nil:
			m.err = err
		}
	})
	var polling []*member
	for _, m := range members {
		if m.err == nil && m.poll {
			polling = append(polling, m)
		}
	}
	if len(polling) > 0 {
		var ns []string
		for _, m := range polling {
			ns = append(ns, m.context)
		}
		fmt.Fprintf(w, "\nPolling %d %s in the same window: %s\n", len(polling), plural(len(polling), "cluster", "clusters"), strings.Join(ns, ", "))
		each(polling, len(polling), func(m *member) {
			m.err = memberPoll(ctx, m.conn, m.cluster, &m.opts)
		})
	}

	var fulls, compacts []string
	var failed []fleet.Failed
	each(members, fleetConcurrency, func(m *member) {
		if m.err == nil {
			m.err = compactChecked(m.out, m.log)
		}
	})
	for _, m := range members {
		if m.err != nil {
			fmt.Fprintf(m.log, "not read: %v\n", m.err)
			failed = append(failed, fleet.Failed{Cluster: m.cluster, Reason: oneLine(m.err.Error())})
			continue
		}
		fulls, compacts = append(fulls, m.out), append(compacts, compactName(m.out))
	}
	for _, m := range members {
		m.log.(*prefixWriter).Flush()
	}
	if len(fulls) == 0 {
		return nil, fmt.Errorf("none of the %d clusters could be read", len(members))
	}

	fmt.Fprintln(w)
	if err := replaceFile(o.out, func(f io.Writer) error { return fleet.Merge(f, fulls, failed) }); err != nil {
		return nil, err
	}
	if err := replaceFile(compactName(o.out), func(f io.Writer) error { return fleet.Merge(f, compacts, failed) }); err != nil {
		return nil, err
	}
	if err := checkContract(compactName(o.out), w); err != nil {
		return nil, err
	}
	b, err := finishReport(o.out)
	if err != nil {
		return nil, err
	}
	printFleet(w, b, members)
	fmt.Fprintf(w, "wrote %s with a summary and report.html, a copy of the report that carries the compact file and opens without this tool\n", b.zip)
	fmt.Fprintf(w, "the full-resolution file for every cluster stays at %s (%s); each cluster's own files are in %s\n", o.out, megabytes(b.full.Bytes), dir)
	return b, nil
}

// nameClusters gives each cluster a name of its own. Contexts that point at
// the same kubeconfig cluster are named after the context, and each counts
// as a cluster of its own.
func nameClusters(members []*member, w io.Writer) {
	by := map[string][]*member{}
	for _, m := range members {
		if m.err == nil {
			by[m.cluster] = append(by[m.cluster], m)
		}
	}
	for _, m := range members {
		same := by[m.cluster]
		if m.err != nil || len(same) < 2 || same[0] != m {
			continue
		}
		var ctxs []string
		for _, x := range same {
			ctxs = append(ctxs, x.context)
		}
		fmt.Fprintf(w, "Contexts %s all point at cluster %s; each is read and counted as a cluster of its own, named after its context.\n",
			strings.Join(ctxs, ", "), m.cluster)
		for _, x := range same {
			x.cluster = x.context
		}
	}
}

func printFleet(w io.Writer, b *builtReport, members []*member) {
	sum := b.summary
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "cluster\tsource\ttier\trequests/yr\tsavings/yr")
	read := 0
	for _, c := range sum.Clusters {
		if c.Summary == nil {
			fmt.Fprintf(tw, "%s\tnot read\n", c.Cluster)
			continue
		}
		read++
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", c.Cluster, c.Source, c.Tier, money(c.Totals.AnnualSpend), money(c.Totals.AnnualSavings))
	}
	tw.Flush()
	for _, c := range sum.Clusters {
		if c.Summary == nil {
			fmt.Fprintf(w, "not read: %s: %s\n", c.Cluster, c.Error)
		}
	}
	fmt.Fprintf(w, "\nFleet, %d of %d clusters: %s a year in requests, %s a year in savings (%d of %d workloads counted)\n",
		read, len(sum.Clusters), money(sum.Totals.AnnualSpend), money(sum.Totals.AnnualSavings), sum.Totals.Counted, sum.Totals.Workloads)
}

// each runs fn on every member, at most n at a time.
func each(ms []*member, n int, fn func(*member)) {
	sem := make(chan struct{}, max(n, 1))
	var wg sync.WaitGroup
	for _, m := range ms {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(m)
		}()
	}
	wg.Wait()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// prefixWriter writes whole lines to w, each but blank ones after prefix.
// Writers that share mu do not interleave within a line. Flush writes a
// last line that has no newline.
type prefixWriter struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := p.buf[:i+1]
		if len(line) > 1 {
			if _, err := io.WriteString(p.w, p.prefix); err != nil {
				return 0, err
			}
		}
		if _, err := p.w.Write(line); err != nil {
			return 0, err
		}
		p.buf = p.buf[i+1:]
	}
}

// fleetCheck is what check reports for several clusters.
type fleetCheck struct {
	Status   string         `json:"status"`
	ExitCode int            `json:"exit_code"`
	Summary  string         `json:"summary"`
	Clusters []clusterCheck `json:"clusters"`
}

type clusterCheck struct {
	Context string       `json:"context"`
	Report  *checkReport `json:"report"`
}

// newFleetCheckEnv is newCheckEnv; tests replace it.
var newFleetCheckEnv = newCheckEnv

// checkFleet checks each context as check does one, a few at a time, each
// with its own timeout, and prints a line per cluster and one for the fleet.
// It exits 0 when every cluster is ready, 2 when none can run, and 1
// otherwise.
func checkFleet(ctx context.Context, o *runOptions, timeout time.Duration, asJSON, color bool, w io.Writer) int {
	names, err := fleetContexts(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return checkBlocked
	}
	fc := &fleetCheck{Clusters: make([]clusterCheck, len(names))}
	ms := make([]*member, len(names))
	for i, n := range names {
		ms[i] = &member{context: n}
		fc.Clusters[i].Context = n
	}
	now := time.Now()
	each(ms, fleetConcurrency, func(m *member) {
		oc := *o
		oc.context, oc.contexts, oc.allContexts, oc.contextsFile, oc.fleet = m.context, nil, false, "", false
		i := slices.Index(names, m.context)
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		fc.Clusters[i].Report = check(cctx, &oc, newFleetCheckEnv(&oc), now)
	})
	counts := map[int]int{}
	for _, c := range fc.Clusters {
		counts[c.Report.ExitCode]++
	}
	switch {
	case counts[checkBlocked] == len(names):
		fc.ExitCode = checkBlocked
	case counts[checkReady] == len(names):
		fc.ExitCode = checkReady
	default:
		fc.ExitCode = checkLimited
	}
	fc.Status = checkStatus[fc.ExitCode]
	fc.Summary = fmt.Sprintf("%d of %d clusters can run: %d ready, %d limited; %d cannot run",
		len(names)-counts[checkBlocked], len(names), counts[checkReady], counts[checkLimited], counts[checkBlocked])
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(fc); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return checkBlocked
		}
		return fc.ExitCode
	}
	printFleetCheck(w, fc, color)
	return fc.ExitCode
}

func printFleetCheck(w io.Writer, fc *fleetCheck, color bool) {
	p := painter(color)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "context\tcluster\tsource\thistory\ttier\tproblem")
	for _, c := range fc.Clusters {
		r := c.Report
		cluster, source, history, tier, problem := "", "none", "", "", ""
		if r.Kubernetes != nil {
			cluster = r.Kubernetes.Cluster
		}
		if r.Source != nil && r.Source.Name != "" {
			source = r.Source.Name
			tier = fmt.Sprint(r.Source.Tier)
			history = sourceHistory(r)
		}
		switch r.ExitCode {
		case checkBlocked:
			problem = r.Summary
			// The summary points at the Kubernetes lines, which this table does not print.
			if r.Kubernetes != nil && r.Kubernetes.Error != "" {
				problem = r.Kubernetes.Error
			}
			problem = p.bad(oneLine(problem))
		case checkLimited:
			problem = p.warn("tier " + tier)
			if len(r.Source.Limits) > 0 {
				problem = p.warn(r.Source.Limits[0])
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Context, cluster, source, history, tier, problem)
	}
	tw.Flush()
	line := "Fleet: " + fc.Summary
	switch fc.ExitCode {
	case checkReady:
		line = p.good(line)
	case checkLimited:
		line = p.warn(line)
	default:
		line = p.bad(line)
	}
	fmt.Fprintf(w, "\n%s\n", line)
}

// sourceHistory is how much history the source run would read holds.
func sourceHistory(r *checkReport) string {
	switch r.Source.Name {
	case "poll":
		return "none, polls"
	case "datadog":
		return "not checked"
	}
	for _, s := range r.Stores {
		if s.Usable && s.Store == r.Source.From {
			return spanText(s.history())
		}
	}
	return ""
}

func (p *prefixWriter) Flush() {
	if len(p.buf) > 0 {
		p.Write([]byte("\n"))
	}
}
