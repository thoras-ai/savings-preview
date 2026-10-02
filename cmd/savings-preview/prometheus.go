package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/kube"
)

func runPrometheus(args []string) error {
	fs := flag.NewFlagSet("prometheus", flag.ExitOnError)
	var (
		kubeconfig  = fs.String("kubeconfig", "", "path to the kubeconfig (default: KUBECONFIG or ~/.kube/config)")
		kctx        = fs.String("context", "", "kubeconfig context (default: the current context)")
		cluster     = fs.String("cluster-name", "", "cluster name recorded in the file (default: the kubeconfig cluster name)")
		out         = fs.String("out", "savings-preview.otlp.json", "output file")
		days        = fs.Int("days", 14, "days of history to pull")
		step        = fs.Duration("step", time.Minute, "resolution of the pulled series")
		target      = fs.String("target", "", "store to use as [scheme:]namespace/service:port[/path-prefix], skipping discovery")
		directURL   = fs.String("url", "", "query a store directly at this base URL instead of through the Kubernetes API server")
		token       = fs.String("bearer-token", "", "bearer token for --url")
		basic       = fs.String("basic-auth", "", "user:password for --url")
		list        = fs.Bool("list", false, "print what discovery finds and exit")
		concurrency = fs.Int("concurrency", 2, "queries in flight at once")
		verbose     = fs.Bool("v", false, "log each query")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *days < 1 {
		return errors.New("--days must be at least 1")
	}
	if *step < time.Second || *step%time.Second != 0 {
		return errors.New("--step must be a whole number of seconds")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	logf := func(f string, a ...any) {
		if *verbose {
			fmt.Fprintf(os.Stderr, f+"\n", a...)
		}
	}

	var conn *kube.Conn
	var getter prometheus.Getter
	clusterName := *cluster
	if *directURL == "" || *list {
		var err error
		conn, err = kube.Connect(*kubeconfig, *kctx)
		if err != nil {
			return err
		}
		if clusterName == "" {
			clusterName = conn.ClusterName
		}
	}
	if clusterName == "" {
		return errors.New("--cluster-name is required with --url")
	}

	switch {
	case *directURL != "":
		var auth prometheus.Auth
		switch {
		case *token != "":
			auth = prometheus.Bearer{Token: *token}
		case *basic != "":
			u, p, _ := strings.Cut(*basic, ":")
			auth = prometheus.Basic{User: u, Password: p}
		}
		getter = &prometheus.HTTPGetter{Base: *directURL, HC: &http.Client{Timeout: 5 * time.Minute}, Auth: auth}
		fmt.Fprintf(os.Stderr, "using store at %s\n", prometheus.RedactURL(*directURL))
	default:
		cand, err := chooseStore(ctx, conn, *target, *list)
		if err != nil {
			return err
		}
		if cand == nil {
			return nil
		}
		getter, err = conn.Proxy(cand.Namespace, cand.Service, cand.Port, cand.Scheme, cand.Prefix)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "using %s\n", cand)
	}

	client := &prometheus.Client{G: getter}
	f, rep, err := prometheus.Run(ctx, prometheus.Options{Client: client, Cluster: clusterName, Days: *days, Step: *step,
		Concurrency: *concurrency, Log: logf})
	if err != nil {
		return err
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
	fmt.Fprintf(os.Stderr, "wrote %s: %d containers in %d workloads, %d nodes; data from %s to %s\n", *out,
		rep.Containers, rep.Workloads, rep.Nodes, rep.Start.UTC().Format(time.RFC3339), rep.End.UTC().Format(time.RFC3339))
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return nil
}

// chooseStore finds the store to query. With list it prints the findings and
// returns nil.
func chooseStore(ctx context.Context, conn *kube.Conn, target string, list bool) (*prometheus.Candidate, error) {
	if target != "" && !list {
		c, err := parseTarget(target)
		return &c, err
	}
	d, err := prometheus.Discover(ctx, conn.Clientset, conn.Dynamic)
	if err != nil {
		return nil, err
	}
	if list {
		for _, c := range d.Candidates {
			fmt.Println(c)
		}
		for _, a := range d.Agents {
			fmt.Printf("agent-mode Prometheus %s/%s remote_write: %s\n", a.Namespace, a.Name, strings.Join(a.RemoteWrite, ", "))
		}
		return nil, nil
	}
	if len(d.Candidates) == 0 {
		if len(d.Agents) > 0 {
			a := d.Agents[0]
			return nil, fmt.Errorf("the only Prometheus found (%s/%s) runs in agent mode and cannot be queried; its data is sent to: %s",
				a.Namespace, a.Name, strings.Join(a.RemoteWrite, ", "))
		}
		return nil, errors.New("no Prometheus-compatible store found in the cluster; pass --target or --url")
	}
	var lastErr error
	for i := range d.Candidates {
		c := d.Candidates[i]
		px, perr := conn.Proxy(c.Namespace, c.Service, c.Port, c.Scheme, c.Prefix)
		if perr != nil {
			return nil, perr
		}
		probe := &prometheus.Client{G: px, Retries: 1}
		var agent *prometheus.ErrAgentMode
		err := probe.Check(ctx)
		if err == nil {
			if len(d.Candidates) > 1 {
				fmt.Fprintf(os.Stderr, "found %d stores, using the first; pass --target to choose another (--list shows all)\n", len(d.Candidates))
			}
			return &c, nil
		}
		if errors.As(err, &agent) {
			return nil, fmt.Errorf("%s: %w", c, err)
		}
		lastErr = fmt.Errorf("%s: %w", c, err)
		fmt.Fprintf(os.Stderr, "skipping %s\n", lastErr)
	}
	return nil, lastErr
}

// parseTarget reads [scheme:]namespace/service:port[/prefix].
func parseTarget(s string) (prometheus.Candidate, error) {
	c := prometheus.Candidate{Scheme: "http", Reason: "--target"}
	if strings.HasPrefix(s, "https:") {
		c.Scheme, s = "https", strings.TrimPrefix(s, "https:")
	} else {
		s = strings.TrimPrefix(s, "http:")
	}
	ns, rest, ok := strings.Cut(s, "/")
	if !ok || ns == "" {
		return c, fmt.Errorf("--target %q: want [scheme:]namespace/service:port[/path-prefix]", s)
	}
	hostport, prefix, _ := strings.Cut(rest, "/")
	svc, port, ok := strings.Cut(hostport, ":")
	if !ok || svc == "" || port == "" {
		return c, fmt.Errorf("--target %q: want [scheme:]namespace/service:port[/path-prefix]", s)
	}
	c.Namespace, c.Service, c.Port = ns, svc, port
	if prefix != "" {
		c.Prefix = "/" + strings.Trim(prefix, "/")
	}
	return c, nil
}
