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
		target      = fs.String("target", "", "store to use as [scheme:]namespace/service:port[/path-prefix], skipping discovery, or as an http(s) URL like --url")
		direct      = addDirectFlags(fs)
		selector    = fs.String("selector", "", `label matchers added to every query, to read one cluster from a store that holds several, e.g. 'cluster="prod-us"'`)
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
	if err := direct.check(target); err != nil {
		return err
	}
	matchers, err := prometheus.ParseSelector(*selector)
	if err != nil {
		return fmt.Errorf("--selector: %w", err)
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
	var store prometheus.Backend
	clusterName := *cluster
	if direct.url == "" || *list {
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
	case direct.url != "":
		getter, store, err = direct.open(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "using %s\n", storeName(store))
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
		store.MaxRange = direct.maxRange
	}

	client := &prometheus.Client{G: getter, Selector: matchers, MaxRange: store.MaxRange}
	f, rep, err := prometheus.Run(ctx, prometheus.Options{Client: client, Cluster: clusterName, Days: *days, Step: *step,
		Concurrency: *concurrency, Log: logf, AdaptiveMetrics: store.Name == prometheus.BackendGrafanaCloud})
	if err != nil {
		if direct.url != "" {
			return directHint(err, store)
		}
		return err
	}
	if err := writeContract(*out, f); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s: %d containers in %d workloads, %d nodes; data from %s to %s\n", *out,
		rep.Containers, rep.Workloads, rep.Nodes, rep.Start.UTC().Format(time.RFC3339), rep.End.UTC().Format(time.RFC3339))
	if rep.OOMKills > 0 {
		fmt.Fprintf(os.Stderr, "%d out-of-memory kills\n", rep.OOMKills)
	}
	if rep.UsageFromPods {
		fmt.Fprintln(os.Stderr, "note: the store has no per-container usage series (the container runtime does not label cAdvisor metrics), "+
			"so container usage is the pod's usage split between its containers by their requests; these containers carry the fidelity note "+
			prometheus.UsageFromPodsNote)
	}
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
	c, err := pickStore(d.Candidates, func(c prometheus.Candidate) error {
		px, err := conn.Proxy(c.Namespace, c.Service, c.Port, c.Scheme, c.Prefix)
		if err != nil {
			return err
		}
		return (&prometheus.Client{G: px, Retries: 1}).Check(ctx)
	}, func(m string) { fmt.Fprintln(os.Stderr, m) })
	if err != nil {
		return nil, err
	}
	if len(d.Candidates) > 1 {
		fmt.Fprintf(os.Stderr, "found %d stores; pass --target to choose another (--list shows all)\n", len(d.Candidates))
	}
	return c, nil
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

// directFlags are the flags that reach a store by URL, shared by prometheus
// and run.
type directFlags struct {
	url, backend, tenant                        string
	tokenFile, basicFile, headerFile, awsRegion string
	maxRange                                    time.Duration
}

func addDirectFlags(fs *flag.FlagSet) *directFlags {
	d := &directFlags{}
	fs.StringVar(&d.url, "url", "", "query a store directly at this base URL instead of through the Kubernetes API server")
	fs.StringVar(&d.backend, "backend", "", "store behind --url: "+strings.Join(prometheus.Backends, ", ")+" (default: detected from the URL)")
	fs.StringVar(&d.tenant, "tenant", "", "Mimir tenant, sent as the "+prometheus.TenantHeader+" header with --url")
	fs.StringVar(&d.tokenFile, "bearer-token-file", "", "file holding a bearer token for --url (or set "+envBearer+")")
	fs.StringVar(&d.basicFile, "basic-auth-file", "", "file holding user:password for --url (or set "+envBasic+")")
	fs.StringVar(&d.headerFile, "header-file", "", "file of 'Name: value' lines, one header each, sent with --url (or set "+envHeaders+")")
	fs.StringVar(&d.awsRegion, "aws-region", "", "AWS region of an Amazon Managed Prometheus workspace (default: from the URL)")
	fs.DurationVar(&d.maxRange, "max-query-range", 0, "longest time range the store accepts in one query (default: the backend's limit, if any)")
	return d
}

// check moves a URL given as --target to --url and checks that the flags
// that need --url have it.
func (d *directFlags) check(target *string) error {
	if strings.Contains(*target, "://") {
		if d.url != "" {
			return errors.New("use only one of --url and a URL in --target")
		}
		d.url, *target = *target, ""
	}
	if d.url == "" && (d.backend != "" || d.tenant != "" || d.headerFile != "" || d.awsRegion != "") {
		return errors.New("--backend, --tenant, --header-file and --aws-region apply only with --url")
	}
	if d.maxRange < 0 {
		return errors.New("--max-query-range must not be negative")
	}
	return nil
}

// open resolves the store behind --url and its credentials.
func (d *directFlags) open(ctx context.Context) (prometheus.Getter, prometheus.Backend, error) {
	store, err := prometheus.ResolveBackend(d.url, d.backend, d.tenant != "")
	if err != nil {
		return nil, store, err
	}
	if d.awsRegion != "" {
		if store.Name != prometheus.BackendAMP {
			return nil, store, fmt.Errorf("--aws-region applies to Amazon Managed Prometheus, not to %s", store.Name)
		}
		store.Region = d.awsRegion
	}
	auth, err := directAuth(ctx, store, credentialFiles{token: d.tokenFile, basic: d.basicFile, headers: d.headerFile}, d.tenant)
	if err != nil {
		return nil, store, err
	}
	if d.maxRange > 0 {
		store.MaxRange = d.maxRange
	}
	return &prometheus.HTTPGetter{Base: store.Base, HC: &http.Client{Timeout: 5 * time.Minute}, Auth: auth}, store, nil
}

// storeName names a store reached by URL without its credentials.
func storeName(store prometheus.Backend) string {
	if store.Name == prometheus.BackendPrometheus {
		return "store at " + prometheus.RedactURL(store.Base)
	}
	return fmt.Sprintf("%s store at %s", store.Name, prometheus.RedactURL(store.Base))
}

// Environment variables that hold credentials for --url, as an alternative to
// the files.
const (
	envBearer  = "SAVINGS_PREVIEW_BEARER_TOKEN"
	envBasic   = "SAVINGS_PREVIEW_BASIC_AUTH"
	envHeaders = "SAVINGS_PREVIEW_HEADERS"
)

type credentialFiles struct{ token, basic, headers string }

// directAuth builds the authentication for a store reached by URL.
// Credentials come from files or environment variables, never from flags, so
// they stay out of shell history and process listings. A file takes
// precedence over its environment variable.
func directAuth(ctx context.Context, store prometheus.Backend, files credentialFiles, tenant string) (prometheus.Auth, error) {
	var chain prometheus.Chain
	headers, err := credential(files.headers, envHeaders)
	if err != nil {
		return nil, err
	}
	if headers != "" {
		hs, err := parseHeaders(headers)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourceName(files.headers, envHeaders), err)
		}
		chain = append(chain, hs...)
	}
	if tenant != "" {
		if store.Name == prometheus.BackendAMP || store.Name == prometheus.BackendGrafanaCloud {
			return nil, fmt.Errorf("--tenant applies to Mimir, not to %s", store.Name)
		}
		chain = append(chain, prometheus.Header{Name: prometheus.TenantHeader, Value: tenant})
	}
	authz, err := authFromFiles(files.token, files.basic)
	if err != nil {
		return nil, err
	}
	if authz == nil {
		if authz, err = authFromEnv(); err != nil {
			return nil, err
		}
	}
	switch store.Name {
	case prometheus.BackendAMP:
		if authz != nil {
			return nil, errors.New("Amazon Managed Prometheus uses AWS credentials (SigV4); remove the bearer token or basic auth")
		}
		if store.Region == "" {
			return nil, errors.New("--aws-region is required when the region is not in the workspace URL")
		}
		sig, err := prometheus.NewSigV4(ctx, store.Region)
		if err != nil {
			return nil, err
		}
		// Signing comes last so that it covers the headers added before it.
		return append(chain, sig), nil
	case prometheus.BackendGrafanaCloud:
		if authz == nil {
			return nil, fmt.Errorf("Grafana Cloud needs basic auth: the instance ID as user and an access policy token with the metrics:read scope as password, in --basic-auth-file or %s", envBasic)
		}
	}
	if authz != nil {
		chain = append(chain, authz)
	}
	switch len(chain) {
	case 0:
		return nil, nil
	case 1:
		return chain[0], nil
	}
	return chain, nil
}

// directHint adds the likely fix to an error from a store reached by URL.
func directHint(err error, store prometheus.Backend) error {
	var ae *prometheus.APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch {
	case strings.Contains(ae.Message, "err-mimir-max-query-length"):
		return fmt.Errorf("%w (pass the store's limit as --max-query-range)", err)
	case ae.Status == http.StatusNotFound:
		return fmt.Errorf("%w (check the path in the URL: Mimir serves the query API under /prometheus, Grafana Cloud under /api/prom, Amazon Managed Prometheus under /workspaces/WORKSPACE_ID)", err)
	case ae.Status == http.StatusUnauthorized && strings.Contains(strings.ToLower(ae.Message), "org id"):
		return fmt.Errorf("%w (the store is a multi-tenant Mimir or Cortex: pass the tenant with --tenant)", err)
	case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
		switch store.Name {
		case prometheus.BackendGrafanaCloud:
			return fmt.Errorf("%w (Grafana Cloud wants the stack's Prometheus instance ID as user and a token with the metrics:read scope)", err)
		case prometheus.BackendAMP:
			return fmt.Errorf("%w (the AWS identity needs aps:QueryMetrics on the workspace; see docs/prometheus-adapter.md)", err)
		}
		return fmt.Errorf("%w (pass the store's credentials with --basic-auth-file, --bearer-token-file or --header-file, or %s, %s or %s)", err, envBasic, envBearer, envHeaders)
	}
	return err
}

// credential reads a file, or the environment variable when no file is given.
func credential(file, env string) (string, error) {
	if file == "" {
		return strings.TrimSpace(os.Getenv(env)), nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func sourceName(file, env string) string {
	if file != "" {
		return file
	}
	return env
}

// parseHeaders reads "Name: value" lines. Errors name the line, never its
// content, which may be a secret.
func parseHeaders(s string) ([]prometheus.Auth, error) {
	var out []prometheus.Auth
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("line %d is not a 'Name: value' header", i+1)
		}
		out = append(out, prometheus.Header{Name: name, Value: value})
	}
	return out, nil
}

func authFromEnv() (prometheus.Auth, error) {
	token, basic := strings.TrimSpace(os.Getenv(envBearer)), strings.TrimSpace(os.Getenv(envBasic))
	switch {
	case token != "" && basic != "":
		return nil, fmt.Errorf("set only one of %s and %s", envBearer, envBasic)
	case token != "":
		return prometheus.Bearer{Token: token}, nil
	case basic != "":
		u, p, ok := strings.Cut(basic, ":")
		if !ok {
			return nil, fmt.Errorf("%s must contain user:password", envBasic)
		}
		return prometheus.Basic{User: u, Password: p}, nil
	}
	return nil, nil
}

// authFromFiles reads credentials from files so they stay out of shell history
// and process listings.
func authFromFiles(tokenFile, basicFile string) (prometheus.Auth, error) {
	if tokenFile != "" && basicFile != "" {
		return nil, errors.New("use only one of --bearer-token-file and --basic-auth-file")
	}
	read := func(path string) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	switch {
	case tokenFile != "":
		t, err := read(tokenFile)
		if err != nil {
			return nil, err
		}
		return prometheus.Bearer{Token: t}, nil
	case basicFile != "":
		b, err := read(basicFile)
		if err != nil {
			return nil, err
		}
		u, p, ok := strings.Cut(b, ":")
		if !ok {
			return nil, fmt.Errorf("%s must contain user:password", basicFile)
		}
		return prometheus.Basic{User: u, Password: p}, nil
	}
	return nil, nil
}

// pickStore returns the first candidate that answers queries. A candidate in
// agent mode is reported only when no other candidate works.
func pickStore(cands []prometheus.Candidate, check func(prometheus.Candidate) error, warn func(string)) (*prometheus.Candidate, error) {
	var lastErr, agentErr error
	for i := range cands {
		c := cands[i]
		err := check(c)
		if err == nil {
			return &c, nil
		}
		failure := fmt.Errorf("%s: %w", c, err)
		var agent *prometheus.ErrAgentMode
		if errors.As(err, &agent) {
			agentErr = failure
		} else {
			lastErr = failure
		}
		warn(fmt.Sprintf("skipping %s", failure))
	}
	if agentErr != nil {
		return nil, agentErr
	}
	return nil, lastErr
}
