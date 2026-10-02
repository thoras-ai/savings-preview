package prometheus

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Backends reached directly by URL.
const (
	BackendPrometheus   = "prometheus"
	BackendMimir        = "mimir"
	BackendGrafanaCloud = "grafana-cloud"
	BackendAMP          = "amp"
	BackendGMP          = "gmp"
	BackendAzure        = "azure"
	BackendCloudWatch   = "cloudwatch"
)

// Backends lists the names ResolveBackend accepts.
var Backends = []string{BackendPrometheus, BackendMimir, BackendGrafanaCloud, BackendAMP, BackendGMP, BackendAzure, BackendCloudWatch}

// Backend is a store reached directly by URL, with its query limits.
type Backend struct {
	Name     string
	Base     string        // base URL of the Prometheus query API
	Region   string        // AWS region, for Amazon Managed Prometheus and CloudWatch
	Project  string        // Google Cloud project, for Google Managed Prometheus
	Audience string        // Entra ID token audience, for Azure Monitor managed Prometheus
	MaxRange time.Duration // longest time range of one query; zero for none
	// OneMetricPerQuery is set for stores that reject a query reading more
	// than one metric, as Azure Monitor managed Prometheus does.
	OneMetricPerQuery bool
	// MaxSeries is the most series the store returns for one query; it
	// truncates the rest without failing. Zero for no limit.
	MaxSeries int
}

// NewClient returns a client for the store with its query limits.
func (b Backend) NewClient(g Getter, selector string) *Client {
	c := &Client{G: g, Selector: selector, MaxRange: b.MaxRange, OneMetricPerQuery: b.OneMetricPerQuery, MaxSeries: b.MaxSeries}
	if b.Name == BackendCloudWatch {
		c.ClusterLabels = CloudWatchClusterLabels
	}
	return c
}

// Query limits of the managed stores.
const (
	GrafanaCloudMaxRange = 32 * 24 * time.Hour
	AMPMaxRange          = 95 * 24 * time.Hour
	AzureMaxRange        = 32 * 24 * time.Hour
	// CloudWatch counts range selectors and the lookback in the range.
	CloudWatchMaxRange  = 7 * 24 * time.Hour
	CloudWatchMaxSeries = 500
)

// CloudWatchClusterLabels tell clusters apart in CloudWatch: the cluster name
// OpenTelemetry Container Insights sets as a resource attribute, and the
// account that sent the metrics, which differs for metrics shared from
// other accounts.
var CloudWatchClusterLabels = []string{"@resource.k8s.cluster.name", "@aws.account"}

// SigV4 service names.
const (
	SigV4AMP        = "aps"
	SigV4CloudWatch = "monitoring"
)

// Azure Monitor workspaces answer PromQL at a host under one of these
// domains, one per Azure cloud, and accept Entra ID tokens for the audience
// of the same name.
var azureDomains = []string{"prometheus.monitor.azure.com", "prometheus.monitor.azure.us", "prometheus.monitor.azure.cn"}

// AzureAudience is the token audience of Azure Monitor managed Prometheus in
// the public cloud.
const AzureAudience = "https://prometheus.monitor.azure.com"

func azureDomain(host string) string {
	for _, d := range azureDomains {
		if strings.HasSuffix(host, "."+d) {
			return d
		}
	}
	return ""
}

var (
	ampHost        = regexp.MustCompile(`(?:^|\.)aps-workspaces\.([a-z0-9-]+)\.(?:vpce\.)?amazonaws\.com(?:\.cn)?$`)
	cloudWatchHost = regexp.MustCompile(`(?:^|\.)monitoring\.([a-z0-9-]+)\.(?:vpce\.)?amazonaws\.com$`)
)

// CloudWatchURL is the PromQL endpoint of CloudWatch in an AWS region.
func CloudWatchURL(region string) (string, error) {
	if !awsRegion.MatchString(region) {
		return "", fmt.Errorf("%q is not an AWS region", region)
	}
	return "https://monitoring." + region + ".amazonaws.com", nil
}

var awsRegion = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-[0-9]$`)

// GMPHost serves the PromQL API of Google Cloud Monitoring, which answers for
// Google Managed Prometheus and for Cloud Monitoring metrics such as GKE
// system metrics.
const GMPHost = "monitoring.googleapis.com"

var (
	gmpPath   = regexp.MustCompile(`^/v1/projects/([^/]+)/location/global/prometheus$`)
	projectID = regexp.MustCompile(`^[a-z0-9][a-z0-9.:-]*[a-z0-9]$`)
)

// GMPURL is the base of the PromQL API for a Google Cloud project.
func GMPURL(project string) (string, error) {
	if !projectID.MatchString(project) {
		return "", fmt.Errorf("%q is not a Google Cloud project ID", project)
	}
	return "https://" + GMPHost + "/v1/projects/" + project + "/location/global/prometheus", nil
}

// ResolveBackend works out the backend for a store URL and normalizes the
// URL to the base of its query API. name is empty to detect the backend from
// the host: Amazon Managed Prometheus workspaces, Grafana Cloud stacks, the
// Google Cloud Monitoring PromQL API, Azure Monitor workspaces and the
// CloudWatch PromQL endpoint are recognized, a tenant implies Mimir,
// and anything else is a plain Prometheus-compatible store.
func ResolveBackend(rawURL, name string, tenant bool) (Backend, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Backend{}, fmt.Errorf("%s is not an http or https URL", RedactURL(rawURL))
	}
	u.RawQuery, u.Fragment = "", ""
	host := strings.ToLower(u.Hostname())
	path := strings.TrimRight(u.Path, "/")
	// Accept the query or remote-write URL a console shows, not only the base.
	for _, suffix := range []string{"/api/v1/query_range", "/api/v1/query", "/api/v1/remote_write"} {
		path = strings.TrimSuffix(path, suffix)
	}

	b := Backend{Name: name}
	m := ampHost.FindStringSubmatch(host)
	cw := cloudWatchHost.FindStringSubmatch(host)
	if b.Name == "" {
		switch {
		case m != nil:
			b.Name = BackendAMP
		case cw != nil:
			b.Name = BackendCloudWatch
		case strings.HasSuffix(host, ".grafana.net"):
			b.Name = BackendGrafanaCloud
		case host == GMPHost:
			b.Name = BackendGMP
		case azureDomain(host) != "":
			b.Name = BackendAzure
		case tenant:
			b.Name = BackendMimir
		default:
			b.Name = BackendPrometheus
		}
	}
	switch b.Name {
	case BackendPrometheus:
	case BackendMimir:
		// Mimir serves the Prometheus API under /prometheus by default, and
		// takes remote writes at /api/v1/push.
		path = strings.TrimSuffix(path, "/api/v1/push")
		if path == "" {
			path = "/prometheus"
		}
	case BackendGrafanaCloud:
		path = strings.TrimSuffix(path, "/push")
		if path == "" {
			path = "/api/prom"
		}
		b.MaxRange = GrafanaCloudMaxRange
	case BackendAMP:
		if m != nil {
			b.Region = m[1]
		}
		if !strings.HasPrefix(path, "/workspaces/") || strings.Count(path, "/") != 2 {
			return Backend{}, fmt.Errorf("%s is not an Amazon Managed Prometheus workspace URL; want https://aps-workspaces.REGION.amazonaws.com/workspaces/WORKSPACE_ID", RedactURL(rawURL))
		}
		b.MaxRange = AMPMaxRange
	case BackendGMP:
		m := gmpPath.FindStringSubmatch(path)
		if m == nil || !projectID.MatchString(m[1]) {
			return Backend{}, fmt.Errorf("%s is not a Google Managed Prometheus URL; want https://%s/v1/projects/PROJECT_ID/location/global/prometheus, or pass --project", RedactURL(rawURL), GMPHost)
		}
		b.Project = m[1]
	case BackendAzure:
		if path != "" {
			return Backend{}, fmt.Errorf("%s is not an Azure Monitor workspace query endpoint; want https://WORKSPACE.REGION.prometheus.monitor.azure.com, as the workspace overview in the Azure portal shows it", RedactURL(rawURL))
		}
		b.Audience = AzureAudience
		if d := azureDomain(host); d != "" {
			b.Audience = "https://" + d
		}
		b.MaxRange = AzureMaxRange
		b.OneMetricPerQuery = true
	case BackendCloudWatch:
		if cw != nil {
			b.Region = cw[1]
		}
		if path != "" {
			return Backend{}, fmt.Errorf("%s is not a CloudWatch PromQL endpoint; want https://monitoring.REGION.amazonaws.com", RedactURL(rawURL))
		}
		b.MaxRange = CloudWatchMaxRange
		b.MaxSeries = CloudWatchMaxSeries
	default:
		return Backend{}, fmt.Errorf("unknown backend %q; want one of %s", name, strings.Join(Backends, ", "))
	}
	u.Path, u.RawPath = path, ""
	b.Base = u.String()
	return b, nil
}
