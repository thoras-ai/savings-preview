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
)

// Backends lists the names ResolveBackend accepts.
var Backends = []string{BackendPrometheus, BackendMimir, BackendGrafanaCloud, BackendAMP}

// Backend is a store reached directly by URL, with its query limits.
type Backend struct {
	Name     string
	Base     string        // base URL of the Prometheus query API
	Region   string        // AWS region, for Amazon Managed Prometheus
	MaxRange time.Duration // longest time range of one query; zero for none
}

// Query limits of the managed stores.
const (
	GrafanaCloudMaxRange = 32 * 24 * time.Hour
	AMPMaxRange          = 95 * 24 * time.Hour
)

var ampHost = regexp.MustCompile(`(?:^|\.)aps-workspaces\.([a-z0-9-]+)\.(?:vpce\.)?amazonaws\.com(?:\.cn)?$`)

// ResolveBackend works out the backend for a store URL and normalizes the
// URL to the base of its query API. name is empty to detect the backend from
// the host: Amazon Managed Prometheus workspaces and Grafana Cloud stacks are
// recognized, a tenant implies Mimir, and anything else is a plain
// Prometheus-compatible store.
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
	if b.Name == "" {
		switch {
		case m != nil:
			b.Name = BackendAMP
		case strings.HasSuffix(host, ".grafana.net"):
			b.Name = BackendGrafanaCloud
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
	default:
		return Backend{}, fmt.Errorf("unknown backend %q; want one of %s", name, strings.Join(Backends, ", "))
	}
	u.Path, u.RawPath = path, ""
	b.Base = u.String()
	return b, nil
}
