package main

import (
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
	out                          string
	source                       string
	target                       *prometheus.Candidate
	days                         int
	step                         time.Duration
	duration, interval           time.Duration
	writeEvery, keepRaw, rollup  time.Duration
	noOpen                       bool
}

func runFlags(args []string) (*runOptions, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	o := &runOptions{}
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to the kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.context, "context", "", "kubeconfig context (default: the current context)")
	fs.StringVar(&o.cluster, "cluster-name", "", "cluster name recorded in the file (default: the kubeconfig cluster name)")
	fs.StringVar(&o.out, "out", report.DataName, "contract file to write; the zip is written next to it")
	fs.StringVar(&o.out, "o", report.DataName, "same as --out")
	fs.StringVar(&o.source, "source", "auto", "where to read usage: auto (a store, else polling), prometheus or poll")
	target := fs.String("target", "", "store to use as [scheme:]namespace/service:port[/path-prefix], skipping discovery")
	fs.IntVar(&o.days, "days", 14, "days of history to pull from a store")
	fs.DurationVar(&o.step, "step", time.Minute, "resolution of the series pulled from a store")
	fs.DurationVar(&o.duration, "duration", 0, "how long to poll when there is no store (default: until Ctrl-C)")
	fs.DurationVar(&o.interval, "interval", time.Minute, "time between readings when polling")
	fs.DurationVar(&o.writeEvery, "write-every", 10*time.Minute, "when polling, rewrite the contract file this often")
	fs.DurationVar(&o.keepRaw, "keep-raw", 48*time.Hour, "when polling, keep readings at --interval for this long, then roll them up to --rollup (0: never roll up)")
	fs.DurationVar(&o.rollup, "rollup", 5*time.Minute, "when polling, step of rolled-up readings, which keep the mean and the peak")
	fs.BoolVar(&o.noOpen, "no-open", false, "print the report URL instead of opening a browser")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview [run] [flags]

Reads usage history from a Prometheus-compatible store in the cluster, or polls
metrics-server when there is none, writes the contract file, checks it, and
opens the report on a local address. A zip with the file, a summary and a
self-contained copy of the report is written next to the contract file.
Everything is read-only; nothing is uploaded.

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	switch o.source {
	case "auto", "prometheus", "poll":
	default:
		return nil, fmt.Errorf("--source must be auto, prometheus or poll, not %q", o.source)
	}
	if *target != "" {
		c, err := parseTarget(*target)
		if err != nil {
			return nil, err
		}
		o.target = &c
	}
	if o.days < 1 {
		return nil, errors.New("--days must be at least 1")
	}
	if o.step < time.Second || o.step%time.Second != 0 {
		return nil, errors.New("--step must be a whole number of seconds")
	}
	if o.interval < polling.MinInterval || o.interval%time.Second != 0 {
		return nil, fmt.Errorf("--interval must be a whole number of seconds and at least %s", polling.MinInterval)
	}
	if o.rollup <= 0 || o.rollup%time.Second != 0 {
		return nil, errors.New("--rollup must be a positive whole number of seconds")
	}
	if o.duration < 0 || o.keepRaw < 0 || o.writeEvery <= 0 {
		return nil, errors.New("--duration and --keep-raw must not be negative, and --write-every must be positive")
	}
	return o, nil
}

func runRun(args []string) error {
	o, err := runFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	conn, err := kube.Connect(o.kubeconfig, o.context)
	if err != nil {
		return err
	}
	cluster := o.cluster
	if cluster == "" {
		cluster = conn.ClusterName
	}
	printAccess(os.Stderr, conn, o)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := collect(ctx, conn, cluster, o); err != nil {
		return err
	}

	// The report and the zip use a 15-minute copy that prices the same; the
	// full file can be too large for a browser or an email.
	compactPath := compactName(o.out)
	start := time.Now()
	st, err := compactFile(o.out, compactPath)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, compactReport(o.out, compactPath, st, time.Since(start)))
	data, err := os.ReadFile(compactPath)
	if err != nil {
		return err
	}
	if errs := validate.Validate(data); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		return fmt.Errorf("%s does not follow the contract (%d problems)", compactPath, len(errs))
	}
	fmt.Fprintf(os.Stderr, "%s passes the contract checks\n", compactPath)
	full, err := fullDataOf(o.out)
	if err != nil {
		return err
	}

	sum, err := savings.Analyze(data)
	if err != nil {
		return fmt.Errorf("compute savings: %w", err)
	}
	page, err := savingspreview.ReportPage()
	if err != nil {
		return err
	}
	zipPath, err := writeBundle(o.out, filepath.Base(compactPath), data, page, summaryFile{Summary: *sum, Version: versionString(),
		Generated: time.Now().UTC().Truncate(time.Second), FullData: full})
	if err != nil {
		return fmt.Errorf("write zip: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n%s: %s a year in requests, %s a year in savings (%d of %d workloads counted, tier %d, %s)\n",
		sum.Cluster, money(sum.Totals.AnnualSpend), money(sum.Totals.AnnualSavings), sum.Totals.Counted, sum.Totals.Workloads,
		sum.Tier, spanText(time.Duration(sum.SpanSeconds)*time.Second))
	fmt.Fprintf(os.Stderr, "wrote %s with %s, a summary and a copy of the report that opens without this tool\n", zipPath, filepath.Base(compactPath))
	fmt.Fprintf(os.Stderr, "the full-resolution file stays at %s (%s); it is not in the zip\n", o.out, megabytes(full.Bytes))

	// The first Ctrl-C may have ended polling; a fresh one stops the server.
	stop()
	return serve(page, data, !o.noOpen)
}

// collect writes the contract file to o.out.
func collect(ctx context.Context, conn *kube.Conn, cluster string, o *runOptions) error {
	if o.source != "poll" {
		res, err := tryStore(ctx, newStoreSteps(conn, cluster, o), o.target, func(m string) { fmt.Fprintln(os.Stderr, m) })
		switch {
		case err == nil:
			if err := writeContract(o.out, res.file); err != nil {
				return err
			}
			rep := res.report
			fmt.Fprintf(os.Stderr, "wrote %s from %s: %d containers in %d workloads, %d nodes; data from %s to %s\n", o.out, res.store,
				rep.Containers, rep.Workloads, rep.Nodes, rep.Start.UTC().Format(time.RFC3339), rep.End.UTC().Format(time.RFC3339))
			for _, w := range rep.Warnings {
				fmt.Fprintln(os.Stderr, "warning:", w)
			}
			return nil
		case !isFallback(err) || o.source == "prometheus":
			return err
		}
		fmt.Fprintf(os.Stderr, "Not using a metrics store: %v.\nFalling back to polling metrics-server.\n\n", err)
	}
	return pollWithProgress(ctx, conn, cluster, o)
}

func newStoreSteps(conn *kube.Conn, cluster string, o *runOptions) storeSteps {
	proxy := func(c prometheus.Candidate) (*prometheus.Client, error) {
		px, err := conn.Proxy(c.Namespace, c.Service, c.Port, c.Scheme, c.Prefix)
		if err != nil {
			return nil, err
		}
		return &prometheus.Client{G: px}, nil
	}
	return storeSteps{
		discover: func(ctx context.Context) (*prometheus.Discovery, error) {
			return prometheus.Discover(ctx, conn.Clientset, conn.Dynamic)
		},
		check: func(ctx context.Context, c prometheus.Candidate) error {
			cl, err := proxy(c)
			if err != nil {
				return err
			}
			cl.Retries = 1
			return cl.Check(ctx)
		},
		pull: func(ctx context.Context, c prometheus.Candidate) (*contract.File, *prometheus.Report, error) {
			cl, err := proxy(c)
			if err != nil {
				return nil, nil, err
			}
			fmt.Fprintf(os.Stderr, "pulling %d days of history from %s\n", o.days, c)
			return prometheus.Run(ctx, prometheus.Options{Client: cl, Cluster: cluster, Days: o.days, Step: o.step, Concurrency: 2,
				Log: func(string, ...any) {}})
		},
	}
}

func printAccess(w io.Writer, conn *kube.Conn, o *runOptions) {
	fmt.Fprintf(w, "Context %s (cluster %s, API server %s)\n", conn.Context, conn.ClusterName, conn.Config.Host)
	fmt.Fprintln(w, "This run only reads from the cluster; it creates, changes and deletes nothing. It will:")
	if o.source != "poll" {
		if o.target == nil {
			fmt.Fprintln(w, "  - list Services, and Prometheus Operator resources if installed, to find a metrics store")
		}
		fmt.Fprintln(w, "  - query that store through the API server's service proxy (GET requests only)")
	}
	if o.source != "prometheus" {
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
	if o.duration > 0 {
		tier := contract.TierFor(int(pollStep(o)/time.Second), int64(o.duration/time.Second))
		fmt.Fprintf(os.Stderr, "Polling every %s for %s (Ctrl-C stops early and builds the report from what was read).\n", shortDur(o.interval), shortDur(o.duration))
		fmt.Fprintf(os.Stderr, "Accuracy: %s of readings gives tier %d (%s).\n", spanText(o.duration), tier, tierText(tier))
	} else {
		fmt.Fprintf(os.Stderr, "Polling every %s until Ctrl-C, which stops and builds the report.\n", shortDur(o.interval))
		fmt.Fprintln(os.Stderr, "Accuracy: under 7 days of readings gives tier 3 (limited history); 7 days with --rollup of 5m or less gives tier 2.")
	}
	fmt.Fprintln(os.Stderr, "Longer runs capture more peaks, so the target and the savings come out more reliable.")

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
					fmt.Fprintf(os.Stderr, "polling: %s of %s\n", shortDur(el), shortDur(o.duration))
				} else {
					fmt.Fprintf(os.Stderr, "polling: %s so far\n", shortDur(el))
				}
			}
		}
	}()
	err := poll(ctx, conn, pollOptions{Cluster: cluster, Out: o.out, Interval: o.interval, Duration: o.duration,
		WriteEvery: o.writeEvery, KeepRaw: o.keepRaw, Rollup: o.rollup}, os.Stderr)
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

// writeBundle writes the zip next to out with the compact contract file as
// name, the summary and a copy of the report that carries the data.
func writeBundle(out, name string, data []byte, page string, sum summaryFile) (string, error) {
	inline, err := report.Inline(page, data)
	if err != nil {
		return "", err
	}
	js, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return "", err
	}
	path := zipName(out)
	comment := ""
	if sum.FullData != nil {
		comment = fmt.Sprintf("%s holds 15-minute peaks and means. The full-resolution file, %s (%s), was written next to this zip and is not in it.",
			name, sum.FullData.File, megabytes(sum.FullData.Bytes))
	}
	return path, report.WriteZip(path, comment, []report.Entry{
		{Name: name, Data: data},
		{Name: "summary.json", Data: append(js, '\n')},
		{Name: "report.html", Data: []byte(inline)},
	})
}

// serve shows the report on a loopback address until Ctrl-C.
func serve(page string, data []byte, open bool) error {
	s, err := report.Serve(page, data)
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
