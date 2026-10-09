package savings

import (
	"regexp"
	"strings"
)

var (
	eksARN  = regexp.MustCompile(`^arn:aws[a-z-]*:eks:([^:]*):([^:]*):cluster/(.+)$`)
	gkeName = regexp.MustCompile(`^gke_([^_]+)_([^_]+)_(.+)$`)
)

type clusterName struct{ full, short, region, account string }

func splitName(full string) clusterName {
	if m := eksARN.FindStringSubmatch(full); m != nil {
		return clusterName{full, m[3], m[1], m[2]}
	}
	if m := gkeName.FindStringSubmatch(full); m != nil {
		return clusterName{full, m[3], m[2], m[1]}
	}
	return clusterName{full, full, "", ""}
}

// DisplayNames maps each cluster name to a short one: the name after
// cluster/ in an EKS ARN, the cluster in a GKE context name
// (gke_PROJECT_LOCATION_NAME), else the name itself. Names that come out the
// same take the region, then the account or project, then both, and keep the
// full name when none tells them apart. displayNames in src/js/model.js is
// the JS port.
func DisplayNames(names []string) map[string]string {
	out := map[string]string{}
	groups := map[string][]clusterName{}
	var order []string
	for _, n := range names {
		if _, ok := out[n]; ok {
			continue
		}
		p := splitName(n)
		out[n] = p.short
		if groups[p.short] == nil {
			order = append(order, p.short)
		}
		groups[p.short] = append(groups[p.short], p)
	}
	qualified := func(p clusterName, q string) string {
		if q == "" {
			return p.full
		}
		return p.short + " (" + q + ")"
	}
	tries := []func(clusterName) string{
		func(p clusterName) string { return qualified(p, p.region) },
		func(p clusterName) string { return qualified(p, p.account) },
		func(p clusterName) string {
			var q []string
			for _, s := range []string{p.region, p.account} {
				if s != "" {
					q = append(q, s)
				}
			}
			return qualified(p, strings.Join(q, ", "))
		},
	}
	for _, short := range order {
		g := groups[short]
		if len(g) < 2 {
			continue
		}
		var labels []string
		for _, try := range tries {
			l := make([]string, len(g))
			for i, p := range g {
				l[i] = try(p)
			}
			if unique(l) {
				labels = l
				break
			}
		}
		for i, p := range g {
			if labels != nil {
				out[p.full] = labels[i]
			} else {
				out[p.full] = p.full
			}
		}
	}
	count := map[string]int{}
	for _, l := range out {
		count[l]++
	}
	for n, l := range out {
		if count[l] > 1 && l != n {
			out[n] = n
		}
	}
	return out
}

func unique(list []string) bool {
	seen := map[string]bool{}
	for _, s := range list {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
