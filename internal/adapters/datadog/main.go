package datadog

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Environment variables that hold the keys, as an alternative to the files.
// They are the names Datadog's own tools read.
const (
	EnvAPIKey    = "DD_API_KEY"
	EnvAppKey    = "DD_APP_KEY"
	EnvAppKeyAlt = "DD_APPLICATION_KEY"
	EnvSite      = "DD_SITE"
)

// Main runs `savings-preview datadog`.
func Main(args []string) error {
	fs := flag.NewFlagSet("datadog", flag.ContinueOnError)
	var (
		access      = AddFlags(fs)
		selector    = fs.String("selector", "", "tag filters added to every query, to read one cluster, e.g. 'kube_cluster_name:prod-us'")
		cluster     = fs.String("cluster-name", "", "cluster name recorded in the file (default: the cluster tag of the selected series)")
		out         = fs.String("out", "savings-preview.otlp.json", "output file")
		days        = fs.Int("days", 14, "days of history to pull")
		step        = fs.Duration("step", time.Minute, "resolution of the pulled series")
		list        = fs.Bool("list", false, "print the metrics and clusters found and exit")
		concurrency = fs.Int("concurrency", 2, "requests in flight at once")
		verbose     = fs.Bool("v", false, "log each query")
	)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: savings-preview datadog [flags]

Reads container usage, requests, replica counts and node allocatable from the
Datadog metrics query API and writes the contract file. The keys come from
files or environment variables, never from flags.

`)
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
	if *days < 1 {
		return errors.New("--days must be at least 1")
	}
	if *step < time.Minute || *step%time.Minute != 0 {
		return errors.New("--step must be a whole number of minutes")
	}
	if *concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	filters, err := ParseSelector(*selector)
	if err != nil {
		return fmt.Errorf("--selector: %w", err)
	}
	c, err := access.Client()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(f string, a ...any) {
		if *verbose {
			fmt.Fprintf(os.Stderr, f+"\n", a...)
		}
	}
	fmt.Fprintf(os.Stderr, "using Datadog API at %s\n", c.Base)
	o := Options{Client: c, Filters: filters, Cluster: *cluster, Days: *days, Step: *step, Concurrency: *concurrency, Log: logf}
	if *list {
		return Hint(List(ctx, o, os.Stdout))
	}
	f, rep, err := Run(ctx, o)
	if err != nil {
		return Hint(err)
	}
	fh, err := os.Create(*out)
	if err != nil {
		return err
	}
	if err := f.Write(fh); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	PrintReport(os.Stderr, *out, rep)
	return nil
}

// Flags are the flags that reach the Datadog API, shared with run.
type Flags struct {
	Site, APIKeyFile, AppKeyFile string
}

// AddFlags registers --site, --api-key-file and --app-key-file.
func AddFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Site, "site", "", "Datadog site: "+strings.Join(SiteNames(), ", ")+", a site domain such as datadoghq.eu, or an https:// URL (default: "+EnvSite+" or us1)")
	fs.StringVar(&f.APIKeyFile, "api-key-file", "", "file holding the Datadog API key (or set "+EnvAPIKey+")")
	fs.StringVar(&f.AppKeyFile, "app-key-file", "", "file holding the Datadog application key (or set "+EnvAppKey+")")
	return f
}

// KeysGiven reports whether both keys are given, as files or in the
// environment. It does not read the files.
func (f *Flags) KeysGiven() bool {
	given := func(file string, envs ...string) bool {
		if file != "" {
			return true
		}
		for _, e := range envs {
			if strings.TrimSpace(os.Getenv(e)) != "" {
				return true
			}
		}
		return false
	}
	return given(f.APIKeyFile, EnvAPIKey) && given(f.AppKeyFile, EnvAppKey, EnvAppKeyAlt)
}

// Client resolves the site and reads the keys.
func (f *Flags) Client() (*Client, error) {
	siteName := f.Site
	if siteName == "" {
		siteName = os.Getenv(EnvSite)
	}
	base, err := ResolveSite(siteName)
	if err != nil {
		return nil, err
	}
	apiKey, err := credential(f.APIKeyFile, EnvAPIKey)
	if err != nil {
		return nil, err
	}
	appKey, err := credential(f.AppKeyFile, EnvAppKey, EnvAppKeyAlt)
	if err != nil {
		return nil, err
	}
	if apiKey == "" || appKey == "" {
		return nil, fmt.Errorf("Datadog needs an API key and an application key: pass --api-key-file and --app-key-file, or set %s and %s", EnvAPIKey, EnvAppKey)
	}
	return &Client{Base: base, APIKey: apiKey, AppKey: appKey, HTTP: &http.Client{Timeout: 5 * time.Minute},
		Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}, nil
}

// PrintReport prints what a pull wrote to out.
func PrintReport(w io.Writer, out string, rep *Report) {
	fmt.Fprintf(w, "wrote %s for cluster %s: %s in %s, %s; data from %s to %s\n", out, rep.Cluster,
		plural(rep.Containers, "container"), plural(rep.Workloads, "workload"), plural(rep.Nodes, "node"),
		rep.Start.UTC().Format(time.RFC3339), rep.End.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "metrics: %s\n", strings.Join(rep.Metrics, ", "))
	for _, m := range rep.Warnings {
		fmt.Fprintln(w, "warning:", m)
	}
}

// Hint adds the likely fix to an error from the API.
func Hint(err error) error {
	if err == nil {
		return nil
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (check that both keys belong to an organization on this --site, and that the application key has the timeseries_query scope; see docs/datadog-adapter.md)", err)
	case http.StatusNotFound:
		return fmt.Errorf("%w (check --site)", err)
	}
	return err
}

// credential reads a file, or the first environment variable that is set
// when no file is given. Keys never come from flags, so they stay out of
// shell history and process listings.
func credential(file string, envs ...string) (string, error) {
	if file == "" {
		for _, e := range envs {
			if v := strings.TrimSpace(os.Getenv(e)); v != "" {
				return v, nil
			}
		}
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
