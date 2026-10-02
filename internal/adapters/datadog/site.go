package datadog

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Sites maps the Datadog site names to their domains. The API of a site is
// served at https://api.DOMAIN.
var Sites = map[string]string{
	"us1":     "datadoghq.com",
	"us3":     "us3.datadoghq.com",
	"us5":     "us5.datadoghq.com",
	"eu1":     "datadoghq.eu",
	"ap1":     "ap1.datadoghq.com",
	"ap2":     "ap2.datadoghq.com",
	"us1-fed": "ddog-gov.com",
	"us2-fed": "us2.ddog-gov.com",
	"uk1":     "uk1.datadoghq.com",
}

// SiteNames lists the keys of Sites in a stable order.
func SiteNames() []string {
	out := make([]string, 0, len(Sites))
	for k := range Sites {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResolveSite returns the API base URL for a site given as a name (us1, eu1),
// as the site domain Datadog's own tools take in DD_SITE (datadoghq.eu,
// us3.datadoghq.com), or as an http(s) URL, used as is.
func ResolveSite(site string) (string, error) {
	s := strings.TrimSpace(site)
	if s == "" {
		s = "us1"
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return "", fmt.Errorf("site %q is not an http(s) URL", site)
		}
		if u.User != nil || u.RawQuery != "" {
			return "", fmt.Errorf("site URL must not carry credentials or a query string")
		}
		return strings.TrimRight(u.String(), "/"), nil
	}
	lower := strings.ToLower(s)
	if d, ok := Sites[lower]; ok {
		return "https://api." + d, nil
	}
	if lower == "eu" {
		return "https://api." + Sites["eu1"], nil
	}
	d := strings.TrimPrefix(strings.TrimPrefix(lower, "app."), "api.")
	for _, known := range Sites {
		if d == known {
			return "https://api." + d, nil
		}
	}
	return "", fmt.Errorf("unknown Datadog site %q; use one of %s, the site domain (such as datadoghq.eu), or an https:// URL", site, strings.Join(SiteNames(), ", "))
}
