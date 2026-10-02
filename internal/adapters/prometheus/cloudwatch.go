package prometheus

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CloudWatchClusterLabel is the resource attribute that names the cluster on
// OTel Container Insights metrics.
const CloudWatchClusterLabel = "@resource.k8s.cluster.name"

// ContainerInsightsProbe finds OTel Container Insights metrics. The add-on
// sends cAdvisor series under their Prometheus names.
const ContainerInsightsProbe = `count(container_memory_working_set_bytes{})`

// AddOnMissingError reports a CloudWatch account and region without OTel
// Container Insights metrics.
type AddOnMissingError struct {
	Region string
	// Clusters send the metrics but do not match the selector.
	Clusters []string
}

func (e *AddOnMissingError) Error() string {
	if len(e.Clusters) > 0 {
		shown := make([]string, len(e.Clusters))
		for i, c := range e.Clusters {
			shown[i] = strconv.Quote(c)
		}
		return fmt.Sprintf("no OTel Container Insights metrics match the selector; clusters that send them to this account in %s: %s. Choose one with --selector '%s=%s'",
			e.regionName(), strings.Join(shown, ", "), QuoteLabel(CloudWatchClusterLabel), shown[0])
	}
	return fmt.Sprintf("CloudWatch has no OTel Container Insights metrics in this account in %s (no container_memory_working_set_bytes series in the last five minutes). "+
		"OTel Container Insights is on only when the amazon-cloudwatch-observability EKS add-on, version 6.2.0 or later, runs with otelContainerInsights.enabled set to true; "+
		"Container Insights without it (classic, or enhanced observability) cannot be read with PromQL. "+
		"If the add-on is enabled, check that the region is the cluster's and that the AWS credentials belong to the account that receives the metrics", e.regionName())
}

func (e *AddOnMissingError) regionName() string {
	if e.Region == "" {
		return "this region"
	}
	return e.Region
}

// checkContainerInsights fails with an AddOnMissingError when the store has
// no OTel Container Insights metrics, or none that match the selector.
func checkContainerInsights(ctx context.Context, c *Client, region string, now time.Time) error {
	if ok, err := c.present(ctx, ContainerInsightsProbe, now); err != nil {
		return fmt.Errorf("probe OTel Container Insights metrics: %w", err)
	} else if ok {
		return nil
	}
	e := &AddOnMissingError{Region: region}
	if c.Selector != "" {
		all := *c
		all.Selector = ""
		ss, err := all.Query(ctx, fmt.Sprintf(`count by (%s) (container_memory_working_set_bytes{})`, QuoteLabel(CloudWatchClusterLabel)), now)
		if err != nil {
			return fmt.Errorf("probe OTel Container Insights metrics: %w", err)
		}
		for _, s := range ss {
			if v := s.Labels[CloudWatchClusterLabel]; v != "" {
				e.Clusters = append(e.Clusters, v)
			}
		}
		sort.Strings(e.Clusters)
	}
	return e
}

// NamespaceContainers counts the containers of each namespace's pods.
const NamespaceContainers = `count by (namespace) (kube_pod_container_info{})`

// maxShardMatcher bounds the matcher that selects the namespaces outside the
// shards, which lists every namespace and goes into every selector of a
// query's URL.
const maxShardMatcher = 4000

// namespaceShards splits the queries of a store with a series limit into
// groups of namespaces whose containers stay within half the limit. The
// namespaces come from kube-state-metrics at now. A last group selects the
// namespaces that are not in any other, such as ones deleted during the
// window; when the matcher for it would be too long, it is left out and the
// returned warning says so. A store without a limit, or a cluster that fits
// in one query, gets c alone.
func namespaceShards(ctx context.Context, c *Client, now time.Time) ([]*Client, string, error) {
	if c.MaxSeries <= 0 {
		return []*Client{c}, "", nil
	}
	ss, err := c.Query(ctx, NamespaceContainers, now)
	if err != nil {
		return nil, "", fmt.Errorf("count containers by namespace: %w", err)
	}
	type ns struct {
		name string
		n    int
	}
	var all []ns
	total := 0
	for _, s := range ss {
		if len(s.Points) == 0 || s.Labels["namespace"] == "" {
			continue
		}
		n := int(s.Points[0].V)
		all = append(all, ns{s.Labels["namespace"], n})
		total += n
	}
	budget := c.MaxSeries / 2
	if total <= budget {
		return []*Client{c}, "", nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	var groups [][]string
	var cur []string
	size := 0
	for _, x := range all {
		if len(cur) > 0 && size+x.n > budget {
			groups = append(groups, cur)
			cur, size = nil, 0
		}
		cur = append(cur, regexp.QuoteMeta(x.name))
		size += x.n
	}
	groups = append(groups, cur)

	with := func(m string) *Client {
		cc := *c
		if cc.Selector != "" {
			m = cc.Selector + "," + m
		}
		cc.Selector = m
		return &cc
	}
	out := make([]*Client, 0, len(groups)+1)
	var names []string
	for _, g := range groups {
		out = append(out, with(`namespace=~`+strconv.Quote(strings.Join(g, "|"))))
		names = append(names, g...)
	}
	rest := `namespace!~` + strconv.Quote(strings.Join(names, "|"))
	if len(rest) > maxShardMatcher {
		return out, "the cluster has too many namespaces to query the ones without pods now as well: workloads of namespaces deleted during the window are left out", nil
	}
	return append(out, with(rest)), "", nil
}
