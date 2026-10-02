package polling

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/thoras-ai/savings-preview/internal/kube"
)

// MinInterval is the shortest interval allowed. metrics-server refreshes usage
// every 60 seconds by default and every 15 seconds in its standard manifest.
const MinInterval = 15 * time.Second

// Reader reads the cluster once.
type Reader interface {
	Read(ctx context.Context) (*Cluster, error)
}

// KubeReader reads with list calls and the metrics.k8s.io API.
type KubeReader struct {
	Client kubernetes.Interface
}

func (k KubeReader) Read(ctx context.Context) (*Cluster, error) {
	var c Cluster
	all := metav1.ListOptions{}
	pods, err := k.Client.CoreV1().Pods("").List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	rs, err := k.Client.AppsV1().ReplicaSets("").List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list replicasets: %w", err)
	}
	deps, err := k.Client.AppsV1().Deployments("").List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	sts, err := k.Client.AppsV1().StatefulSets("").List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}
	nodes, err := k.Client.CoreV1().Nodes().List(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	c.Pods, c.ReplicaSets, c.Deployments, c.StatefulSets, c.Nodes = pods.Items, rs.Items, deps.Items, sts.Items, nodes.Items

	raw, err := k.Client.Discovery().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/pods").DoRaw(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.MetricsErr = err
		return &c, nil
	}
	var list struct {
		Items []PodMetrics `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		c.MetricsErr = fmt.Errorf("decode pod metrics: %w", err)
		return &c, nil
	}
	c.PodMetrics = list.Items
	if c.PodMetrics == nil {
		c.PodMetrics = []PodMetrics{}
	}
	return &c, nil
}

// Clock is time as the run loop sees it.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Options configures a run.
type Options struct {
	Cluster    string
	Output     string
	Interval   time.Duration
	Duration   time.Duration // 0 runs until ctx is cancelled
	WriteEvery time.Duration
	KeepRaw    time.Duration // 0 keeps every reading at the polling step
	Rollup     time.Duration
	Once       bool
	Export     bool // write the output from saved readings without reading the cluster

	exportBudget int64 // 0 uses DefaultExportBudget
	noSync       bool
}

// ReadingsDir is where readings for output are kept.
func ReadingsDir(output string) string { return output + ".readings" }

// createOutput writes the contract file; tests replace it.
var createOutput = writeAtomic

func writeOutput(path string, src source, cluster string, budget int64) (Summary, error) {
	var sum Summary
	err := createOutput(path, func(w io.Writer) error {
		var err error
		sum, err = Export(src, cluster, w, budget)
		return err
	})
	return sum, err
}

// ExportReadings writes the contract file for the readings saved in dir to w,
// the same file a poll writes. It works while a poll is running.
func ExportReadings(dir string, w io.Writer) (Summary, error) {
	m, err := readMeta(dir)
	if err != nil {
		return Summary{}, fmt.Errorf("no readings to export: %w", err)
	}
	sum, err := Export(dirSource(dir), m.Cluster, w, DefaultExportBudget)
	if err == nil && sum.Records == 0 {
		err = fmt.Errorf("%s has no readings yet", dir)
	}
	return sum, err
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func report(log io.Writer, path string, sum Summary, m Meta, keepRaw time.Duration) {
	for _, n := range sum.Notes() {
		fmt.Fprintln(log, "note: "+n)
	}
	fmt.Fprintf(log, "wrote %s: %s\n", path, sum.Progress(m, keepRaw))
}

// Run takes readings and writes the contract file to opts.Output.
//
// By default it polls: every reading is saved in the readings directory next
// to the output, which a later run with the same output extends. Readings older
// than KeepRaw are rolled up to the Rollup step. The output is rewritten from
// the saved readings every WriteEvery, without pausing the readings, and when
// the run stops. Once takes a single reading and saves nothing; Export only
// rewrites the output from saved readings.
func Run(ctx context.Context, opts Options, rd Reader, clk Clock, logw io.Writer) error {
	log := &lockedWriter{w: logw}
	step := int(opts.Interval / time.Second)
	if !opts.Export && (opts.Interval < MinInterval || opts.Interval%time.Second != 0) {
		return fmt.Errorf("the interval must be a whole number of seconds and at least %s", MinInterval)
	}
	if !opts.Export && !opts.Once && opts.WriteEvery <= 0 {
		return errors.New("the write interval must be positive")
	}
	budget := opts.exportBudget
	if budget == 0 {
		budget = DefaultExportBudget
	}
	dir := ReadingsDir(opts.Output)

	if opts.Export {
		m, err := readMeta(dir)
		if err != nil {
			return fmt.Errorf("no readings to export: %w", err)
		}
		sum, err := writeOutput(opts.Output, dirSource(dir), m.Cluster, budget)
		if err != nil {
			return err
		}
		if sum.Records == 0 {
			return fmt.Errorf("%s has no readings yet", dir)
		}
		report(log, opts.Output, sum, m, opts.KeepRaw)
		return nil
	}

	if opts.Once {
		if _, err := os.Stat(dir); err == nil {
			lk, err := lockFile(filepath.Join(dir, "lock"))
			if errors.Is(err, ErrLocked) {
				return fmt.Errorf("a poller is writing %s; --once would overwrite its output", opts.Output)
			}
			if err != nil {
				return err
			}
			defer lk.Close()
		}
		c, err := rd.Read(ctx)
		if err != nil {
			return err
		}
		rec := fromReading(clk.Now().Unix(), step, Reduce(c))
		sum, err := writeOutput(opts.Output, recordSource(rec), opts.Cluster, budget)
		if err != nil {
			return err
		}
		report(log, opts.Output, sum, Meta{Step: step, RollupStep: step}, 0)
		return nil
	}

	start := clk.Now()
	rollup := rollupStepFor(step, int(opts.Rollup/time.Second))
	st, err := OpenStore(dir, Meta{Cluster: opts.Cluster, Step: step, RollupStep: rollup, Origin: start.Unix()})
	if err != nil {
		return err
	}
	defer st.Close()
	st.sync = !opts.noSync
	m := st.Meta

	var sum Summary
	if err := scan(dir, math.MaxInt64, nil, func(r *record) error { sum.add(r); return nil }); err != nil {
		return err
	}
	if sum.Records > 0 {
		fmt.Fprintf(log, "continuing %s: %s\n", dir, sum.Progress(m, opts.KeepRaw))
	}

	// Readings sit on the grid origin + k*step, also across runs.
	ceilGrid := func(t int64) int64 {
		if t <= m.Origin {
			return m.Origin
		}
		return m.Origin + (t-m.Origin+int64(step)-1)/int64(step)*int64(step)
	}
	next := ceilGrid(start.Unix())
	if sum.Records > 0 {
		next = max(next, ceilGrid(sum.Last+int64(sum.LastStep)))
	}
	end := int64(0)
	if opts.Duration > 0 {
		end = start.Add(opts.Duration).Unix()
	}

	var exporting chan struct{}
	exportNow := func() {
		if exporting != nil {
			select {
			case <-exporting:
			default:
				return // the previous export is still running
			}
		}
		done := make(chan struct{})
		exporting = done
		go func() {
			defer close(done)
			s, err := writeOutput(opts.Output, dirSource(dir), m.Cluster, budget)
			if err != nil {
				fmt.Fprintf(log, "write %s: %v\n", opts.Output, err)
				return
			}
			fmt.Fprintf(log, "wrote %s: %s\n", opts.Output, s.Progress(m, opts.KeepRaw))
		}()
	}

	lastWrite := start
	taken := 0
	var runErr error
	for end == 0 || next <= end {
		if err := clk.Sleep(ctx, time.Unix(next, 0).Sub(clk.Now())); err != nil {
			break
		}
		rctx, cancel := context.WithTimeout(ctx, opts.Interval)
		c, err := rd.Read(rctx)
		cancel()
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			if taken == 0 && sum.Records == 0 {
				runErr = err
				break
			}
			fmt.Fprintf(log, "reading at %s failed, leaving a gap: %v\n", time.Unix(next, 0).UTC().Format(time.RFC3339), err)
		} else {
			rec := fromReading(next, step, Reduce(c))
			if err := st.Append(rec); err != nil {
				runErr = fmt.Errorf("save reading: %w", err)
				break
			}
			sum.add(rec)
			taken++
			if opts.KeepRaw > 0 {
				if err := st.Compact(next - int64(opts.KeepRaw/time.Second)); err != nil {
					runErr = err
					break
				}
			}
			if taken == 1 || clk.Now().Sub(lastWrite) >= opts.WriteEvery {
				exportNow()
				lastWrite = clk.Now()
			}
		}
		next += int64(step)
		if now := clk.Now().Unix(); next <= now {
			next = ceilGrid(now + 1)
		}
	}
	if exporting != nil {
		<-exporting
	}
	if sum.Records > 0 {
		final, err := writeOutput(opts.Output, dirSource(dir), m.Cluster, budget)
		if err != nil {
			if runErr == nil {
				runErr = fmt.Errorf("write %s: %w", opts.Output, err)
			}
		} else {
			report(log, opts.Output, final, m, opts.KeepRaw)
		}
	}
	return runErr
}

// Poll runs opts against a cluster in real time, logging progress to log.
func Poll(ctx context.Context, client kubernetes.Interface, opts Options, log io.Writer) error {
	return Run(ctx, opts, KubeReader{client}, realClock{}, log)
}

// Main runs the poll subcommand; name is what the user typed for it.
func Main(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig file (default: KUBECONFIG or ~/.kube/config)")
	kubeContext := fs.String("context", "", "kubeconfig context (default: the current context)")
	cluster := fs.String("cluster-name", "", "value of k8s.cluster.name (default: the kubeconfig cluster name)")
	output := fs.String("o", "savings-preview.otlp.json", "contract file to write; readings are kept next to it in FILE.readings")
	interval := fs.Duration("interval", time.Minute, "time between readings")
	dur := fs.Duration("duration", 0, "stop after this long (default: run until interrupted)")
	writeEvery := fs.Duration("write-every", 10*time.Minute, "rewrite the contract file this often while running")
	keepRaw := fs.Duration("keep-raw", 48*time.Hour, "keep readings at --interval for this long, then roll them up to --rollup (0: never roll up)")
	rollup := fs.Duration("rollup", 5*time.Minute, "step of rolled-up readings, which keep the mean and the peak")
	once := fs.Bool("once", false, "take one reading, write the file and exit; nothing is saved")
	export := fs.Bool("export", false, "write the file from the readings saved so far and exit, without reading the cluster")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `usage: savings-preview %s [flags]

Reads requests, replica counts, nodes and metrics-server usage with read-only
API calls every --interval until --duration passes or it is interrupted.
Readings are saved as they are taken, and running again with the same -o
extends them; only one run at a time may use an -o. Readings older than
--keep-raw are rolled up to --rollup steps. The contract file is rewritten
every --write-every and on exit; --export writes it from the saved readings
at any time.

`, name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *interval < MinInterval || *interval%time.Second != 0 {
		return fmt.Errorf("--interval must be a whole number of seconds and at least %s", MinInterval)
	}
	if *rollup%time.Second != 0 || *rollup <= 0 {
		return errors.New("--rollup must be a positive whole number of seconds")
	}
	if *dur < 0 || *writeEvery <= 0 || *keepRaw < 0 {
		return errors.New("--duration and --keep-raw must not be negative, and --write-every must be positive")
	}
	if *once && *export {
		return errors.New("--once and --export do not go together")
	}
	opts := Options{Output: *output, Interval: *interval, Duration: *dur, WriteEvery: *writeEvery,
		KeepRaw: *keepRaw, Rollup: *rollup, Once: *once, Export: *export}
	if *export {
		return Run(context.Background(), opts, nil, realClock{}, os.Stderr)
	}
	conn, err := kube.Connect(*kubeconfig, *kubeContext)
	if err != nil {
		return err
	}
	if *cluster == "" {
		*cluster = conn.ClusterName
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// After the first signal the readings are safe; a second one ends the
	// process even during the final write.
	go func() {
		<-ctx.Done()
		stop()
	}()
	opts.Cluster = *cluster
	return Poll(ctx, conn.Clientset, opts, os.Stderr)
}
