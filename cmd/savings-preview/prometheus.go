package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
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

	client := store.NewClient(getter, matchers)
	f, rep, err := prometheus.Run(ctx, prometheus.Options{Client: client, Cluster: clusterName, Days: *days, Step: *step,
		Concurrency: *concurrency, Log: logf, AdaptiveMetrics: store.Name == prometheus.BackendGrafanaCloud, GKE: direct.gkeMode(store),
		CloudWatch: cloudWatchRegion(store)})
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
	if rep.GKESystemMetrics {
		fmt.Fprintln(os.Stderr, "read GKE system metrics")
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
	project, googleCreds, gkeMetrics            string
	azureTenant, azureClientID, azureSecretFile string
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
	fs.StringVar(&d.awsRegion, "aws-region", "", "AWS region of an Amazon Managed Prometheus workspace or of CloudWatch (default: from the URL); with --backend cloudwatch and no --url, the region's CloudWatch PromQL endpoint")
	fs.StringVar(&d.project, "project", "", "Google Cloud project to read from Google Managed Prometheus, in place of --url; implies --backend gmp")
	fs.StringVar(&d.googleCreds, "google-credentials-file", "", "service account key for Google Managed Prometheus (default: Google Application Default Credentials)")
	fs.StringVar(&d.gkeMetrics, "gke-metrics", "", "metrics to read from Google Managed Prometheus: "+strings.Join(prometheus.GKEModes, ", ")+
		" (default: auto, which reads cAdvisor and kube-state-metrics series when the project has both, else GKE system metrics)")
	fs.StringVar(&d.azureTenant, "azure-tenant-id", "", "Entra ID tenant of the app registration or login for Azure Monitor managed Prometheus (or set "+envAzureTenant+")")
	fs.StringVar(&d.azureClientID, "azure-client-id", "", "client ID of an app registration for Azure Monitor managed Prometheus (or set "+envAzureClient+"); without one, the Azure CLI login is used")
	fs.StringVar(&d.azureSecretFile, "azure-client-secret-file", "", "file holding the app registration's client secret (or set "+envAzureSecret+")")
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
	if d.project != "" {
		if d.backend != "" && d.backend != prometheus.BackendGMP {
			return fmt.Errorf("--project applies to Google Managed Prometheus, not to %s", d.backend)
		}
		if d.url != "" {
			b, err := prometheus.ResolveBackend(d.url, prometheus.BackendGMP, false)
			if err != nil {
				return err
			}
			if b.Project != d.project {
				return fmt.Errorf("--project %s does not match the project in --url (%s)", d.project, b.Project)
			}
		} else {
			u, err := prometheus.GMPURL(d.project)
			if err != nil {
				return fmt.Errorf("--project: %w", err)
			}
			d.url = u
		}
		d.backend = prometheus.BackendGMP
	}
	if d.url == "" && d.backend == prometheus.BackendGMP {
		return errors.New("--backend gmp needs --project or --url")
	}
	if d.url == "" && d.backend == prometheus.BackendCloudWatch {
		if d.awsRegion == "" {
			return errors.New("--backend cloudwatch needs --aws-region or --url")
		}
		u, err := prometheus.CloudWatchURL(d.awsRegion)
		if err != nil {
			return fmt.Errorf("--aws-region: %w", err)
		}
		d.url = u
	}
	if d.url == "" && (d.backend != "" || d.tenant != "" || d.headerFile != "" || d.awsRegion != "" || d.googleCreds != "" ||
		d.azureTenant != "" || d.azureClientID != "" || d.azureSecretFile != "") {
		return errors.New("--backend, --tenant, --header-file, --aws-region, --google-credentials-file and the --azure flags apply only with --url")
	}
	if d.gkeMetrics != "" && !slices.Contains(prometheus.GKEModes, d.gkeMetrics) {
		return fmt.Errorf("--gke-metrics must be one of %s, not %q", strings.Join(prometheus.GKEModes, ", "), d.gkeMetrics)
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
		if store.Name != prometheus.BackendAMP && store.Name != prometheus.BackendCloudWatch {
			return nil, store, fmt.Errorf("--aws-region applies to Amazon Managed Prometheus and CloudWatch, not to %s", store.Name)
		}
		store.Region = d.awsRegion
	}
	if (d.googleCreds != "" || d.gkeMetrics != "") && store.Name != prometheus.BackendGMP {
		return nil, store, fmt.Errorf("--google-credentials-file and --gke-metrics apply to Google Managed Prometheus, not to %s", store.Name)
	}
	if (d.azureTenant != "" || d.azureClientID != "" || d.azureSecretFile != "") && store.Name != prometheus.BackendAzure {
		return nil, store, fmt.Errorf("--azure-tenant-id, --azure-client-id and --azure-client-secret-file apply to Azure Monitor managed Prometheus, not to %s", store.Name)
	}
	auth, err := directAuth(ctx, store, credentialFiles{token: d.tokenFile, basic: d.basicFile, headers: d.headerFile, google: d.googleCreds,
		azureTenant: d.azureTenant, azureClientID: d.azureClientID, azureSecret: d.azureSecretFile}, d.tenant)
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

func hasAuthorization(chain prometheus.Chain) bool {
	for _, a := range chain {
		if h, ok := a.(prometheus.Header); ok && strings.EqualFold(h.Name, "Authorization") {
			return true
		}
	}
	return false
}

// cloudWatchRegion is the region of a CloudWatch store, empty for any other.
func cloudWatchRegion(store prometheus.Backend) string {
	if store.Name != prometheus.BackendCloudWatch {
		return ""
	}
	return store.Region
}

// newAzure builds Entra ID authentication; tests replace it.
var newAzure = func(ctx context.Context, audience string, c prometheus.AzureCredentials) (prometheus.Auth, error) {
	return prometheus.NewAzure(ctx, audience, c)
}

// newGoogle loads Google credentials; tests replace it.
var newGoogle = func(ctx context.Context, file string) (prometheus.Auth, error) {
	return prometheus.NewGoogle(ctx, file)
}

// gkeMode is the --gke-metrics value for a store, empty when it is not Google
// Managed Prometheus.
func (d *directFlags) gkeMode(store prometheus.Backend) string {
	switch {
	case store.Name != prometheus.BackendGMP:
		return ""
	case d.gkeMetrics == "":
		return prometheus.GKEAuto
	}
	return d.gkeMetrics
}

// Environment variables that hold credentials for --url, as an alternative to
// the files.
const (
	envBearer  = "SAVINGS_PREVIEW_BEARER_TOKEN"
	envBasic   = "SAVINGS_PREVIEW_BASIC_AUTH"
	envHeaders = "SAVINGS_PREVIEW_HEADERS"
)

// The variables the Azure SDKs and CLI read for a service principal.
const (
	envAzureTenant = "AZURE_TENANT_ID"
	envAzureClient = "AZURE_CLIENT_ID"
	envAzureSecret = "AZURE_CLIENT_SECRET"
)

type credentialFiles struct {
	token, basic, headers, google           string
	azureTenant, azureClientID, azureSecret string // azureSecret is a file
}

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
		if store.Name != prometheus.BackendPrometheus && store.Name != prometheus.BackendMimir {
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
	case prometheus.BackendAMP, prometheus.BackendCloudWatch:
		product, service := "Amazon Managed Prometheus", prometheus.SigV4AMP
		if store.Name == prometheus.BackendCloudWatch {
			product, service = "CloudWatch", prometheus.SigV4CloudWatch
		}
		if authz != nil {
			return nil, fmt.Errorf("%s uses AWS credentials (SigV4); remove the bearer token or basic auth", product)
		}
		if store.Region == "" {
			return nil, fmt.Errorf("--aws-region is required when the region is not in the %s URL", product)
		}
		sig, err := prometheus.NewSigV4(ctx, store.Region)
		if err != nil {
			return nil, err
		}
		sig.Service = service
		// Signing comes last so that it covers the headers added before it.
		return append(chain, sig), nil
	case prometheus.BackendGMP:
		// A token given directly, such as one from gcloud auth
		// print-access-token, is used as is, as before this backend existed.
		if authz != nil || hasAuthorization(chain) {
			if files.google != "" {
				return nil, errors.New("use only one of --google-credentials-file and a bearer token, basic auth or Authorization header")
			}
			break
		}
		g, err := newGoogle(ctx, files.google)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			return g, nil
		}
		return append(chain, g), nil
	case prometheus.BackendAzure:
		explicit := files.azureClientID != "" || files.azureSecret != ""
		if authz != nil || hasAuthorization(chain) {
			// A token given directly, such as one from az account
			// get-access-token, is used as is.
			if explicit || files.azureTenant != "" {
				return nil, errors.New("use only one of --azure-tenant-id, --azure-client-id or --azure-client-secret-file and a bearer token or Authorization header")
			}
			break
		}
		creds := prometheus.AzureCredentials{TenantID: files.azureTenant, ClientID: files.azureClientID}
		if creds.TenantID == "" {
			creds.TenantID = strings.TrimSpace(os.Getenv(envAzureTenant))
		}
		if creds.ClientID == "" {
			creds.ClientID = strings.TrimSpace(os.Getenv(envAzureClient))
		}
		if creds.ClientSecret, err = credential(files.azureSecret, envAzureSecret); err != nil {
			return nil, err
		}
		if creds.ClientSecret == "" && !explicit {
			// AZURE_CLIENT_ID alone names a managed identity or a workload
			// identity to the Azure SDKs; the CLI login decides here.
			creds.ClientID = ""
		}
		a, err := newAzure(ctx, store.Audience, creds)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			return a, nil
		}
		return append(chain, a), nil
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
	case ae.Status == http.StatusNotFound && store.Name == prometheus.BackendGMP:
		return fmt.Errorf("%w (check the project ID, and that the Cloud Monitoring API is enabled in the project)", err)
	case ae.Status == http.StatusNotFound && store.Name == prometheus.BackendCloudWatch:
		return fmt.Errorf("%w (CloudWatch answers PromQL at https://monitoring.REGION.amazonaws.com, with no path, in the regions its documentation lists)", err)
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
		case prometheus.BackendCloudWatch:
			return fmt.Errorf("%w (the AWS identity needs cloudwatch:GetMetricData and cloudwatch:ListMetrics; see docs/prometheus-adapter.md)", err)
		case prometheus.BackendGMP:
			return fmt.Errorf("%w (the Google identity needs the Monitoring Viewer role, roles/monitoring.viewer, on project %s; see docs/prometheus-adapter.md)", err, store.Project)
		case prometheus.BackendAzure:
			return fmt.Errorf("%w (the Entra ID identity needs the Monitoring Data Reader role on the Azure Monitor workspace; see docs/prometheus-adapter.md)", err)
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
