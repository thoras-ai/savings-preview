package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	savingspreview "github.com/thoras-ai/savings-preview"
	"github.com/thoras-ai/savings-preview/internal/adapters/datadog"
	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/contract"
	"github.com/thoras-ai/savings-preview/internal/kube"
	"github.com/thoras-ai/savings-preview/internal/report"
	"github.com/thoras-ai/savings-preview/internal/savings"
	"github.com/thoras-ai/savings-preview/internal/validate"
)

type runOptions struct {
	kubeconfig, context, cluster string
	// contexts, allContexts and contextsFile name several clusters to read
	// in one run; see fleet.go.
	contexts                    contextList
	allContexts                 bool
	contextsFile                string
	fleet                       bool      // several clusters were asked for
	member                      bool      // reading one cluster of several: access is printed once for all
	log                         io.Writer // where progress goes; nil is stderr
	out                         string
	source                      string
	target                      *prometheus.Candidate
	direct                      *directFlags
	datadog                     *datadog.Flags
	useDatadog                  bool // read from Datadog before anything else
	selector                    string
	promSelector                string
	ddFilters                   []string
	days                        int
	step                        time.Duration
	duration, interval          time.Duration
	writeEvery, keepRaw, rollup time.Duration
	noOpen                      bool
	now                         time.Time // end of the history pulled; zero is now
}

// addRunFlags registers run's flags on fs. finish checks them after fs is
// parsed.
func addRunFlags(fs *flag.FlagSet) (o *runOptions, finish func() error) {
	o = &runOptions{}
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to the kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	fs.Var(&o.contexts, "context", "kubeconfig context (default: the current context); give it more than once to read several clusters in one run")
	fs.BoolVar(&o.allContexts, "all-contexts", false, "read every context in the kubeconfig, one cluster each")
	fs.StringVar(&o.contextsFile, "contexts-file", "", "read the contexts named in this file, one per line; # starts a comment")
	fs.StringVar(&o.cluster, "cluster-name", "", "cluster name recorded in the file (default: the kubeconfig cluster name, or for Datadog the cluster tag of the series)")
	fs.StringVar(&o.out, "out", report.DataName, "contract file to write; the zip is written next to it")
	fs.StringVar(&o.out, "o", report.DataName, "same as --out")
	fs.StringVar(&o.source, "source", "auto", "where to read usage: auto (Datadog when its keys are given, else a store, else polling), prometheus, datadog or poll")
	target := fs.String("target", "", "store to use as [scheme:]namespace/service:port[/path-prefix], skipping discovery, or as an http(s) URL like --url")
	o.direct = addDirectFlags(fs)
	o.datadog = datadog.AddFlags(fs)
	fs.StringVar(&o.selector, "selector", "", `filters added to every query, to read one cluster from a store that holds several: label matchers for a Prometheus-compatible store, e.g. 'cluster="prod-us"', or tag filters for Datadog, e.g. 'kube_cluster_name:prod-us'`)
	fs.IntVar(&o.days, "days", 14, "days of history to pull from a store")
	fs.DurationVar(&o.step, "step", time.Minute, "resolution of the series pulled from a store")
	fs.DurationVar(&o.duration, "duration", 0, "how long to poll when there is no store (default: until Ctrl-C)")
	fs.DurationVar(&o.interval, "interval", time.Minute, "time between readings when polling")
	fs.DurationVar(&o.writeEvery, "write-every", 10*time.Minute, "when polling, rewrite the contract file this often")
	fs.DurationVar(&o.keepRaw, "keep-raw", 48*time.Hour, "when polling, keep readings at --interval for this long, then roll them up to --rollup (0: never roll up)")
	fs.DurationVar(&o.rollup, "rollup", 5*time.Minute, "when polling, step of rolled-up readings, which keep the mean and the peak")
	fs.BoolVar(&o.noOpen, "no-open", false, "print the report URL instead of opening a browser")
	return o, func() error {
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		return o.check(*target)
	}
}

func runFlags(args []string) (*runOptions, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	o, finish := addRunFlags(fs)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview [run] [flags]

Reads usage history from Datadog when its keys are given, else from a
Prometheus-compatible store in the cluster or at --url, or polls metrics-server
when there is none. A store named with --url or --target, or a source chosen
with --source, is used or the run fails; it does not fall back. The run writes
the contract file, checks it, and opens the report on a local address. It also
writes a compact copy of the file at 15-minute steps, which the report reads.
A zip with a summary and a self-contained copy of the report, which carries
the compact copy, is written next to the contract file; the full file stays on
disk. Everything is read-only; nothing is uploaded.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return o, nil
}

// check validates the parsed flags and derives the options that follow from
// them. target is the value of --target.
func (o *runOptions) check(target string) error {
	if len(o.contexts) == 1 {
		o.context = o.contexts[0]
	}
	o.fleet = len(o.contexts) > 1 || o.allContexts || o.contextsFile != ""
	if o.fleet {
		if err := o.checkFleet(target); err != nil {
			return err
		}
	}
	switch o.source {
	case "auto", "prometheus", "datadog", "poll":
	default:
		return fmt.Errorf("--source must be auto, prometheus, datadog or poll, not %q", o.source)
	}
	if err := o.direct.check(&target); err != nil {
		return err
	}
	named := o.direct.url != "" || target != ""
	if named && (o.source == "datadog" || o.source == "poll") {
		return fmt.Errorf("--url and --target name a Prometheus-compatible store; they do not apply with --source %s", o.source)
	}
	o.useDatadog = o.source == "datadog" || (o.source == "auto" && !named && !o.fleet && o.datadog.KeysGiven())
	if o.useDatadog {
		filters, err := datadog.ParseSelector(o.selector)
		if err != nil {
			return fmt.Errorf("--selector: %w", err)
		}
		o.ddFilters = filters
		if o.step < time.Minute || o.step%time.Minute != 0 {
			return errors.New("--step must be a whole number of minutes with Datadog")
		}
	} else {
		m, err := prometheus.ParseSelector(o.selector)
		if err != nil {
			return fmt.Errorf("--selector: %w", err)
		}
		o.promSelector = m
	}
	if target != "" {
		c, err := parseTarget(target)
		if err != nil {
			return err
		}
		o.target = &c
	}
	if o.days < 1 {
		return errors.New("--days must be at least 1")
	}
	if o.step < time.Second || o.step%time.Second != 0 {
		return errors.New("--step must be a whole number of seconds")
	}
	if o.interval < polling.MinInterval || o.interval%time.Second != 0 {
		return fmt.Errorf("--interval must be a whole number of seconds and at least %s", polling.MinInterval)
	}
	if o.rollup <= 0 || o.rollup%time.Second != 0 {
		return errors.New("--rollup must be a positive whole number of seconds")
	}
	if o.duration < 0 || o.keepRaw < 0 || o.writeEvery <= 0 {
		return errors.New("--duration and --keep-raw must not be negative, and --write-every must be positive")
	}
	return nil
}

func runRun(args []string) error {
	o, err := runFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.fleet {
		b, err := runFleet(ctx, o, os.Stderr)
		if err != nil {
			return err
		}
		stop()
		return serve(b.page, b.compact, !o.noOpen)
	}
	s, err := newSources(o, os.Stderr)
	if err != nil {
		return err
	}
	if err := collect(ctx, s, o, os.Stderr); err != nil {
		return err
	}

	b, err := buildReport(o.out)
	if err != nil {
		return err
	}
	sum := b.summary
	fmt.Fprintf(os.Stderr, "\n%s: %s a year in requests, %s a year in savings (%d of %d workloads counted, tier %d, %s)\n",
		sum.Cluster, money(sum.Totals.AnnualSpend), money(sum.Totals.AnnualSavings), sum.Totals.Counted, sum.Totals.Workloads,
		sum.Tier, spanText(time.Duration(sum.SpanSeconds)*time.Second))
	fmt.Fprintf(os.Stderr, "wrote %s with a summary and report.html, a copy of the report that carries the compact file and opens without this tool\n", b.zip)
	fmt.Fprintf(os.Stderr, "the full-resolution file stays at %s (%s); it is not in the zip\n", o.out, megabytes(b.full.Bytes))

	// The first Ctrl-C may have ended polling; a fresh one stops the server.
	stop()
	return serve(b.page, b.compact, !o.noOpen)
}

// builtReport is what buildReport wrote.
type builtReport struct {
	compact, zip, page string
	summary            *savings.Summary
	full               *fullData
}

// buildReport writes the compact copy of the contract file at out, checks it,
// computes the totals and writes the zip. The report and the zip use the
// compact copy, since the full file can be too large for a browser or an
// email. Every step reads its file as a stream, so memory does not grow with
// the size of the cluster or the length of the history.
func buildReport(out string) (*builtReport, error) {
	if err := compactChecked(out, os.Stderr); err != nil {
		return nil, err
	}
	return finishReport(out)
}

// compactChecked writes the compact copy of the contract file at out and
// checks it against the contract.
func compactChecked(out string, w io.Writer) error {
	compacted := compactName(out)
	start := time.Now()
	st, err := compactFile(out, compacted)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, compactReport(out, compacted, st, time.Since(start)))
	return checkContract(compacted, w)
}

func checkContract(path string, w io.Writer) error {
	if errs := withFile(path, validate.ValidateReader); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(w, "  "+e)
		}
		return fmt.Errorf("%s does not follow the contract (%d problems)", path, len(errs))
	}
	fmt.Fprintf(w, "%s passes the contract checks\n", path)
	return nil
}

// finishReport computes the totals from the checked compact copy of out and
// writes the zip.
func finishReport(out string) (*builtReport, error) {
	b := &builtReport{compact: compactName(out)}
	var err error
	if b.full, err = fullDataOf(out); err != nil {
		return nil, err
	}
	f, err := os.Open(b.compact)
	if err != nil {
		return nil, err
	}
	b.summary, err = savings.AnalyzeReader(bufio.NewReader(f))
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("compute savings: %w", err)
	}
	if b.page, err = savingspreview.ReportPage(); err != nil {
		return nil, err
	}
	b.zip, err = writeBundle(out, b.compact, b.page, summaryFile{Summary: *b.summary, Version: versionString(),
		Generated: time.Now().UTC().Truncate(time.Second), FullData: b.full})
	if err != nil {
		return nil, fmt.Errorf("write zip: %w", err)
	}
	return b, nil
}

// withFile runs check on the file at path, or reports why it cannot be read.
func withFile(path string, check func(io.Reader) []string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{err.Error()}
	}
	defer f.Close()
	return check(bufio.NewReader(f))
}

// pulled is a contract file read from a store or from Datadog.
type pulled struct {
	from                         string
	file                         *contract.File
	containers, workloads, nodes int
	start, end                   time.Time
	notes, warnings              []string
}

func fromPrometheus(from string, f *contract.File, rep *prometheus.Report) *pulled {
	p := &pulled{from: from, file: f, containers: rep.Containers, workloads: rep.Workloads, nodes: rep.Nodes,
		start: rep.Start, end: rep.End, warnings: rep.Warnings}
	if rep.UsageFromPods {
		p.notes = append(p.notes, "the store has no per-container usage series, so container usage is the pod's usage split between its containers by their requests")
	}
	if rep.GKESystemMetrics {
		p.notes = append(p.notes, "usage, requests and node sizes are GKE system metrics; memory usage is their non-evictable memory")
	}
	return p
}

// sources are the places run reads usage from. A nil func is not tried.
// Tests replace them.
type sources struct {
	direct  func(context.Context) (*pulled, error) // the store at --url
	datadog func(context.Context) (*pulled, error)
	store   storeSteps // discovery, or the store named with --target
	poll    func(context.Context) error
}

// newSources connects to the cluster when the run may need it and prints
// what the run will read.
func newSources(o *runOptions, w io.Writer) (sources, error) {
	var s sources
	var conn *kube.Conn
	var connErr error
	connect := func() (*kube.Conn, error) {
		if conn == nil && connErr == nil {
			conn, connErr = kube.Connect(o.kubeconfig, o.context)
			connErr = kube.WithCommand(connErr, "savings-preview")
			if connErr == nil {
				fmt.Fprintf(w, "Context %s (cluster %s, API server %s)\n", conn.Context, conn.ClusterName, conn.Config.Host)
			}
		}
		return conn, connErr
	}
	clusterName := func() (string, error) {
		if o.cluster != "" {
			return o.cluster, nil
		}
		c, err := connect()
		if err != nil {
			return "", err
		}
		return c.ClusterName, nil
	}

	// Datadog and a store at --url are reached without the cluster, so the
	// kubeconfig is read only when the run needs a cluster name or may fall
	// back to the cluster.
	var cluster string
	switch {
	case o.direct.url != "":
		var err error
		if cluster, err = clusterName(); err != nil {
			return s, fmt.Errorf("--cluster-name is required with --url when the kubeconfig cannot be read: %w", err)
		}
	case !o.useDatadog:
		if _, err := connect(); err != nil {
			return s, err
		}
	case o.source == "auto":
		// The cluster is read only if Datadog fails, so a kubeconfig that
		// cannot be read stops the run only then.
		if _, err := connect(); err != nil {
			fmt.Fprintf(w, "Cannot read the kubeconfig; only Datadog can be read.\n%s\n", indent(err.Error(), "  "))
		}
	}
	if !o.member {
		printAccess(w, o)
	}

	if o.direct.url != "" {
		s.direct = func(ctx context.Context) (*pulled, error) { return pullDirect(ctx, o, cluster, w) }
		return s, nil
	}
	if o.useDatadog {
		s.datadog = func(ctx context.Context) (*pulled, error) { return pullDatadog(ctx, o, w) }
	}
	if o.source == "datadog" {
		return s, nil
	}
	s.store = storeSteps{
		discover: func(ctx context.Context) (*prometheus.Discovery, error) {
			c, err := connect()
			if err != nil {
				return nil, err
			}
			return prometheus.Discover(ctx, c.Clientset, c.Dynamic)
		},
		check: func(ctx context.Context, c prometheus.Candidate) error {
			cl, err := proxyClient(connect, c, o)
			if err != nil {
				return err
			}
			cl.Retries = 1
			return cl.Check(ctx)
		},
		pull: func(ctx context.Context, c prometheus.Candidate) (*contract.File, *prometheus.Report, error) {
			cl, err := proxyClient(connect, c, o)
			if err != nil {
				return nil, nil, err
			}
			cluster, err := clusterName()
			if err != nil {
				return nil, nil, err
			}
			fmt.Fprintf(w, "pulling %s of history from %s\n", days(o.days), c)
			return prometheus.Run(ctx, prometheus.Options{Client: cl, Cluster: cluster, Days: o.days, Step: o.step, Concurrency: 2,
				Log: func(string, ...any) {}})
		},
	}
	s.poll = func(ctx context.Context) error {
		c, err := connect()
		if err != nil {
			return err
		}
		cluster, err := clusterName()
		if err != nil {
			return err
		}
		return pollWithProgress(ctx, c, cluster, o)
	}
	return s, nil
}

func proxyClient(connect func() (*kube.Conn, error), c prometheus.Candidate, o *runOptions) (*prometheus.Client, error) {
	conn, err := connect()
	if err != nil {
		return nil, err
	}
	px, err := conn.Proxy(c.Namespace, c.Service, c.Port, c.Scheme, c.Prefix)
	if err != nil {
		return nil, err
	}
	return &prometheus.Client{G: px, Selector: o.promSelector, MaxRange: o.direct.maxRange}, nil
}

// pullDirect reads the history from the store at --url.
func pullDirect(ctx context.Context, o *runOptions, cluster string, w io.Writer) (*pulled, error) {
	g, store, err := o.direct.open(ctx)
	if err != nil {
		return nil, err
	}
	name := storeName(store)
	fmt.Fprintf(w, "pulling %s of history from the %s\n", days(o.days), name)
	cl := store.NewClient(g, o.promSelector)
	f, rep, err := prometheus.Run(ctx, prometheus.Options{Client: cl, Cluster: cluster, Days: o.days, Step: o.step, Concurrency: 2,
		Log: func(string, ...any) {}, AdaptiveMetrics: store.Name == prometheus.BackendGrafanaCloud, GKE: o.direct.gkeMode(store),
		CloudWatch: cloudWatchRegion(store)})
	if err != nil {
		return nil, directHint(err, store)
	}
	if !anyPriced(f) {
		return nil, fmt.Errorf("the %s has no container with all of %s, so no savings can be computed from it",
			name, strings.Join(contract.RequiredContainerMetrics, ", "))
	}
	return fromPrometheus("the "+name, f, rep), nil
}

// pullDatadog reads the history from the Datadog metrics query API.
func pullDatadog(ctx context.Context, o *runOptions, w io.Writer) (*pulled, error) {
	c, err := o.datadog.Client()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "pulling %s of history from Datadog at %s\n", days(o.days), c.Base)
	f, rep, err := datadog.Run(ctx, datadog.Options{Client: c, Filters: o.ddFilters, Cluster: o.cluster, Days: o.days, Step: o.step,
		Concurrency: 2, Now: o.now})
	if err != nil {
		return nil, datadog.Hint(err)
	}
	if !anyPriced(f) {
		return nil, fmt.Errorf("Datadog has no container with all of %s, so no savings can be computed from it",
			strings.Join(contract.RequiredContainerMetrics, ", "))
	}
	return &pulled{from: "Datadog", file: f, containers: rep.Containers, workloads: rep.Workloads, nodes: rep.Nodes,
		start: rep.Start, end: rep.End, warnings: rep.Warnings}, nil
}

// collect writes the contract file to o.out. Datadog chosen by its keys and
// a store found by discovery fall back to the next source; a store named with
// --url or --target, and any source chosen with --source, do not.
func collect(ctx context.Context, s sources, o *runOptions, w io.Writer) error {
	if o.source == "poll" {
		return s.poll(ctx)
	}
	if s.direct != nil {
		p, err := s.direct(ctx)
		if err != nil {
			return err
		}
		return save(o.out, p, w)
	}
	if s.datadog != nil {
		p, err := s.datadog(ctx)
		var ce *datadog.ClusterError
		switch {
		case err == nil:
			return save(o.out, p, w)
		case o.source == "datadog" || ctx.Err() != nil || errors.As(err, &ce):
			return err
		}
		fmt.Fprintf(w, "Not using Datadog: %v.\n", err)
		if o.selector != "" {
			fmt.Fprintln(w, "--selector holds Datadog tag filters, so it is not applied to the store or to polling. To pass label matchers to a store, run again with --source prometheus.")
		}
		fmt.Fprintln(w)
	}
	res, err := tryStore(ctx, s.store, o.target, func(m string) { fmt.Fprintln(w, m) })
	switch {
	case err == nil:
		return save(o.out, fromPrometheus(res.store.String(), res.file, res.report), w)
	case !isFallback(err) || o.source == "prometheus":
		return err
	}
	fmt.Fprintf(w, "Not using a metrics store: %v.\nFalling back to polling metrics-server.\n\n", err)
	return s.poll(ctx)
}

func days(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

func save(out string, p *pulled, w io.Writer) error {
	if err := writeContract(out, p.file); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s from %s: %d containers in %d workloads, %d nodes; data from %s to %s\n", out, p.from,
		p.containers, p.workloads, p.nodes, p.start.UTC().Format(time.RFC3339), p.end.UTC().Format(time.RFC3339))
	for _, n := range p.notes {
		fmt.Fprintln(w, "note:", n)
	}
	for _, m := range p.warnings {
		fmt.Fprintln(w, "warning:", m)
	}
	return nil
}

func printAccess(w io.Writer, o *runOptions) {
	fmt.Fprintln(w, "This run only reads; it creates, changes and deletes nothing. It will:")
	next := "  - "
	if o.useDatadog {
		fmt.Fprintln(w, "  - query the Datadog metrics API with the keys given (read-only queries)")
		next = "  - if Datadog has no usable data, "
	}
	switch {
	case o.source == "poll" || o.source == "datadog":
	case o.direct.url != "":
		b, err := prometheus.ResolveBackend(o.direct.url, o.direct.backend, o.direct.tenant != "")
		switch {
		case err == nil && b.Name == prometheus.BackendGMP:
			fmt.Fprintf(w, "  - query Google Cloud Monitoring in project %s with your Google credentials (read-only queries)\n", b.Project)
		case err == nil && b.Name == prometheus.BackendAzure:
			fmt.Fprintf(w, "  - query the Azure Monitor workspace at %s (read-only queries)\n", prometheus.RedactURL(b.Base))
		case err == nil && b.Name == prometheus.BackendCloudWatch:
			fmt.Fprintf(w, "  - query CloudWatch at %s with your AWS credentials (read-only PromQL queries)\n", prometheus.RedactURL(b.Base))
		default:
			fmt.Fprintf(w, "  - query the store at %s directly (read-only queries)\n", prometheus.RedactURL(o.direct.url))
		}
	default:
		if o.target == nil {
			fmt.Fprintln(w, next+"list Services, and Prometheus Operator resources if installed, to find a metrics store")
			next = "  - "
		}
		fmt.Fprintln(w, next+"query that store through the API server's service proxy (GET requests only)")
	}
	if o.source == "auto" && o.direct.url == "" || o.source == "poll" {
		prefix := "  - "
		if o.source == "auto" {
			prefix = "  - if no usable store is found, "
		}
		fmt.Fprintln(w, prefix+"read Pods, ReplicaSets, Deployments, StatefulSets, Nodes and metrics-server pod usage at each interval")
	}
	fmt.Fprintf(w, "Output goes to %s, a 15-minute copy for the report in %s, and %s. Nothing is uploaded.\n\n", o.out, compactName(o.out), zipName(o.out))
}

// pollWithProgress polls until o.duration passes or the user presses Ctrl-C,
// printing what the result will support and how far along it is.
func pollWithProgress(ctx context.Context, conn *kube.Conn, cluster string, o *runOptions) error {
	logw := o.logWriter()
	if o.duration > 0 {
		tier := contract.TierFor(int(pollStep(o)/time.Second), int64(o.duration/time.Second))
		fmt.Fprintf(logw, "Polling every %s for %s (Ctrl-C stops early and builds the report from what was read).\n", shortDur(o.interval), shortDur(o.duration))
		fmt.Fprintf(logw, "Accuracy: %s of readings gives tier %d (%s).\n", spanText(o.duration), tier, tierText(tier))
	} else {
		fmt.Fprintf(logw, "Polling every %s until Ctrl-C, which stops and builds the report.\n", shortDur(o.interval))
		fmt.Fprintln(logw, "Accuracy: under 7 days of readings gives tier 3 (limited history); 7 days with --rollup of 5m or less gives tier 2.")
	}
	fmt.Fprintln(logw, "Longer runs capture more peaks, so the target and the savings come out more reliable.")

	before, statErr := os.Stat(o.out)
	start := time.Now()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(progressEvery(o.interval))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				el := now.Sub(start).Truncate(time.Second)
				if o.duration > 0 {
					fmt.Fprintf(logw, "polling: %s of %s\n", shortDur(el), shortDur(o.duration))
				} else {
					fmt.Fprintf(logw, "polling: %s so far\n", shortDur(el))
				}
			}
		}
	}()
	err := poll(ctx, conn, pollOptions{Cluster: cluster, Out: o.out, Interval: o.interval, Duration: o.duration,
		WriteEvery: o.writeEvery, KeepRaw: o.keepRaw, Rollup: o.rollup}, logw)
	if err != nil && ctx.Err() == nil {
		return err
	}
	// A file left by an earlier run must not pass for this run's output.
	after, afterErr := os.Stat(o.out)
	if afterErr != nil || (statErr == nil && !after.ModTime().After(before.ModTime())) {
		if err != nil {
			return err
		}
		return fmt.Errorf("polling stopped before it wrote %s", o.out)
	}
	return nil
}

func (o *runOptions) logWriter() io.Writer {
	if o.log != nil {
		return o.log
	}
	return os.Stderr
}

// pollStep is the coarsest step the polled file will have: readings older
// than --keep-raw are rolled up, and the tier follows the coarsest step.
func pollStep(o *runOptions) time.Duration {
	if o.keepRaw > 0 && o.duration > o.keepRaw && o.rollup > o.interval {
		return o.rollup
	}
	return o.interval
}

func progressEvery(interval time.Duration) time.Duration {
	if interval < 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func tierText(t int) string {
	switch t {
	case 1:
		return "full history"
	case 2:
		return "reduced resolution"
	}
	return "limited history"
}

func spanText(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%.1f days", d.Hours()/24)
	case d >= 2*time.Hour:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
	return shortDur(d)
}

// shortDur prints 3m instead of 3m0s.
func shortDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// money formats dollars the way the report page does.
func money(v float64) string {
	if math.Abs(v) >= 1e6 {
		return fmt.Sprintf("$%.2fM", v/1e6)
	}
	sign, n := "", int64(math.Round(v))
	if n < 0 {
		sign, n = "-", -n
	}
	d := strconv.FormatInt(n, 10)
	for i := len(d) - 3; i > 0; i -= 3 {
		d = d[:i] + "," + d[i:]
	}
	return "$" + sign + d
}

// writeContract writes f to path, replacing it only once the write succeeded.
func writeContract(path string, f *contract.File) error {
	return replaceFile(path, f.Write)
}

// replaceFile writes path through a temporary file, replacing it only once the
// write succeeded.
func replaceFile(path string, write func(io.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".savings-preview-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func zipName(out string) string {
	base := strings.TrimSuffix(out, filepath.Ext(out))
	base = strings.TrimSuffix(base, ".otlp")
	return base + ".zip"
}

type summaryFile struct {
	savings.Summary
	Version   string    `json:"version"`
	Generated time.Time `json:"generated_at"`
	FullData  *fullData `json:"full_data,omitempty"`
}

// fullData names the full-resolution contract file, which stays next to the
// zip; the zip holds the compact one.
type fullData struct {
	File  string `json:"file"`
	Bytes int64  `json:"bytes"`
}

func fullDataOf(path string) (*fullData, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &fullData{File: filepath.Base(path), Bytes: fi.Size()}, nil
}

// writeBundle writes the zip next to out with the summary and a copy of the
// report that carries the compact contract file at dataPath. The data is in
// the zip once, inside report.html; savings-preview export takes it back out.
func writeBundle(out, dataPath, page string, sum summaryFile) (string, error) {
	js, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return "", err
	}
	path := zipName(out)
	comment := "report.html carries the data as a compact copy at 15-minute steps that keeps the peaks and means of usage; " +
		"savings-preview export -o " + report.DataName + " report.html writes it out."
	if sum.FullData != nil {
		comment += fmt.Sprintf(" The full-resolution file, %s (%s), is not in the zip; it stays next to the zip on the machine that wrote it.",
			sum.FullData.File, megabytes(sum.FullData.Bytes))
	}
	return path, report.WriteZip(path, comment, []report.Entry{
		{Name: "summary.json", Data: append(js, '\n')},
		{Name: "report.html", Write: func(w io.Writer) error {
			f, err := os.Open(dataPath)
			if err != nil {
				return err
			}
			defer f.Close()
			return report.WriteInline(w, page, f)
		}},
	})
}

// serve shows the report on a loopback address until Ctrl-C.
func serve(page, dataPath string, open bool) error {
	s, err := report.Serve(page, dataPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nReport: %s\n", s.URL)
	if open {
		if err := report.Open(s.URL); err != nil {
			fmt.Fprintf(os.Stderr, "could not open a browser (%v); open the address above\n", err)
		}
	}
	fmt.Fprintln(os.Stderr, "Press Ctrl-C to stop serving the report.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
