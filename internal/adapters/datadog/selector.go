package datadog

import (
	"fmt"
	"regexp"
	"strings"
)

// A tag filter is key:value, optionally negated with "!" and with "*"
// wildcards in the value, as in the Datadog query language. Anything that
// could close the scope or start a function is refused, so a filter cannot
// change the shape of a query.
var tagFilter = regexp.MustCompile(`^!?[A-Za-z][A-Za-z0-9_.\-/]*:[A-Za-z0-9_.:\-/*]+$`)

// ParseSelector reads comma-separated tag filters such as
// `kube_cluster_name:prod-us` or `kube_cluster_name:prod-us,env:prod` and
// returns them in canonical form. Every filter must match for a series to be
// selected.
func ParseSelector(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if !tagFilter.MatchString(f) {
			return nil, fmt.Errorf("%q is not a tag filter; want tag:value, for example kube_cluster_name:prod-us", f)
		}
		out = append(out, f)
	}
	return out, nil
}
