package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	savingspreview "github.com/thoras-ai/savings-preview"
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
	noOpen                       bool
}

func runFlags(args []string) (*runOptions, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	o := &runOptions{}
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to the kubeconfig (default: KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.context, "context", "", "kubeconfig context (default: the current context)")
	fs.StringVar(&o.cluster, "cluster-name", "", "cluster name recorded in the file (default: the kubeconfig cluster name)")
	fs.StringVar(&o.out, "out", report.DataName, "contract file to write; the zip is written next to it")
	fs.StringVar(&o.source, "source", "auto", "where to read usage: auto (a store, else polling), prometheus or poll")
	target := fs.String("target", "", "store to use as [scheme:]namespace/service:port[/path-prefix], skipping discovery")
	fs.IntVar(&o.days, "days", 14, "days of history to pull from a store")
	fs.DurationVar(&o.step, "step", time.Minute, "resolution of the series pulled from a store")
	fs.DurationVar(&o.duration, "duration", 0, "how long to poll when there is no store (default: until Ctrl-C)")
	fs.DurationVar(&o.interval, "interval", time.Minute, "time between readings when polling")
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
	if o.interval < time.Second || o.interval%time.Second != 0 {
		return nil, errors.New("--interval must be a whole number of seconds")
	}
	if o.duration < 0 {
		return nil, errors.New("--duration must not be negative")
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
	source, err := collect(ctx, conn, cluster, o)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(o.out)
	if err != nil {
		return err
	}
	if errs := validate.Validate(data); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		return fmt.Errorf("%s does not follow the contract (%d problems)", o.out, len(errs))
	}
	fmt.Fprintf(os.Stderr, "%s passes the contract checks\n", o.out)

	sum, err := savings.Analyze(data)
	if err != nil {
		return fmt.Errorf("compute savings: %w", err)
	}
	page, err := savingspreview.ReportPage()
	if err != nil {
		return err
	}
	zipPath, err := writeBundle(o.out, data, page, summaryFile{Summary: *sum, Source: source, Version: versionString(),
		Generated: time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		return fmt.Errorf("write zip: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n%s: %s a year in requests, %s a year in savings (%d of %d workloads counted, tier %d, %s)\n",
		sum.Cluster, money(sum.Totals.AnnualSpend), money(sum.Totals.AnnualSavings), sum.Totals.Counted, sum.Totals.Workloads,
		sum.Tier, spanText(time.Duration(sum.SpanSeconds)*time.Second))
	fmt.Fprintf(os.Stderr, "wrote %s with the contract file, a summary and a copy of the report that opens without this tool\n", zipPath)

	// The first Ctrl-C may have ended polling; a fresh one stops the server.
	stop()
	return serve(page, data, !o.noOpen)
}

// collect writes the contract file to o.out and names the source it used.
func collect(ctx context.Context, conn *kube.Conn, cluster string, o *runOptions) (string, error) {
	if o.source != "poll" {
		res, err := tryStore(ctx, newStoreSteps(conn, cluster, o), o.target, func(m string) { fmt.Fprintln(os.Stderr, m) })
		switch {
		case err == nil:
			if err := writeContract(o.out, res.file); err != nil {
				return "", err
			}
			rep := res.report
			fmt.Fprintf(os.Stderr, "wrote %s from %s: %d containers in %d workloads, %d nodes; data from %s to %s\n", o.out, res.store,
				rep.Containers, rep.Workloads, rep.Nodes, rep.Start.UTC().Format(time.RFC3339), rep.End.UTC().Format(time.RFC3339))
			for _, w := range rep.Warnings {
				fmt.Fprintln(os.Stderr, "warning:", w)
			}
			return "prometheus", nil
		case !isFallback(err) || o.source == "prometheus":
			return "", err
		}
		fmt.Fprintf(os.Stderr, "Not using a metrics store: %v.\nFalling back to polling metrics-server.\n\n", err)
	}
	if err := pollWithProgress(ctx, conn, cluster, o); err != nil {
		return "", err
	}
	return "poll", nil
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
	fmt.Fprintf(w, "Output goes to %s and %s. Nothing is uploaded.\n\n", o.out, zipName(o.out))
}

// pollWithProgress polls until o.duration passes or the user presses Ctrl-C,
// printing what the result will support and how far along it is.
func pollWithProgress(ctx context.Context, conn *kube.Conn, cluster string, o *runOptions) error {
	step := int(o.interval / time.Second)
	if o.duration > 0 {
		tier := contract.TierFor(step, int64(o.duration/time.Second))
		fmt.Fprintf(os.Stderr, "Polling every %s for %s (Ctrl-C stops early and builds the report from what was read).\n", shortDur(o.interval), shortDur(o.duration))
		fmt.Fprintf(os.Stderr, "Accuracy: %s of readings gives tier %d (%s).\n", spanText(o.duration), tier, tierText(tier))
	} else {
		fmt.Fprintf(os.Stderr, "Polling every %s until Ctrl-C, which stops and builds the report.\n", shortDur(o.interval))
		fmt.Fprintln(os.Stderr, "Accuracy: under 7 days of readings gives tier 3 (limited history); 7 days at 5-minute steps or finer gives tier 2.")
	}
	fmt.Fprintln(os.Stderr, "Longer runs capture more peaks, so the target and the savings come out more reliable.")

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
	err := poll(ctx, conn, pollOptions{Cluster: cluster, Out: o.out, Interval: o.interval, Duration: o.duration}, os.Stderr)
	if err != nil && ctx.Err() == nil {
		return err
	}
	if _, statErr := os.Stat(o.out); statErr != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("polling stopped before %s was written", o.out)
	}
	return nil
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

func money(v float64) string {
	if v >= 1e6 || v <= -1e6 {
		return fmt.Sprintf("$%.2fM", v/1e6)
	}
	return fmt.Sprintf("$%.0f", v)
}

// writeContract writes f to path, replacing it only once the write succeeded.
func writeContract(path string, f *contract.File) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".savings-preview-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := f.Write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
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
	Source    string    `json:"source_adapter"`
	Version   string    `json:"version"`
	Generated time.Time `json:"generated_at"`
}

func writeBundle(out string, data []byte, page string, sum summaryFile) (string, error) {
	inline, err := report.Inline(page, data)
	if err != nil {
		return "", err
	}
	js, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return "", err
	}
	path := zipName(out)
	return path, report.WriteZip(path, []report.Entry{
		{Name: report.DataName, Data: data},
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
