package prometheus

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// requiredLabels are the labels the queries group or join on. A metric that
// has series but none with one of these labels has been aggregated or
// relabeled, and the queries would silently return nothing or wrong sums.
var requiredLabels = []struct {
	metric string
	labels []string
}{
	{"container_cpu_usage_seconds_total", []string{"namespace", "pod"}},
	{"container_memory_working_set_bytes", []string{"namespace", "pod"}},
	{"kube_pod_owner", []string{"namespace", "pod", "owner_kind", "owner_name"}},
	{"kube_replicaset_owner", []string{"namespace", "replicaset", "owner_kind", "owner_name"}},
	{"kube_pod_container_resource_requests", []string{"namespace", "pod", "container", "resource"}},
	{"kube_pod_status_phase", []string{"namespace", "pod", "phase"}},
	{"kube_deployment_spec_replicas", []string{"namespace", "deployment"}},
	{"kube_statefulset_replicas", []string{"namespace", "statefulset"}},
	{"kube_node_status_allocatable", []string{"node", "resource"}},
}

// containerLabels are the labels that tell a container's usage series from
// its pod's.
var containerLabels = []string{"container", "image"}

// LabelsMissingError reports a metric whose series all lack labels the
// queries need.
type LabelsMissingError struct {
	Metric       string
	Labels       []string
	Aggregations bool // the store is known to aggregate series, as Grafana Cloud Adaptive Metrics does
	// Runtime is set when a container runtime that does not label cAdvisor
	// series would look the same.
	Runtime bool
}

func (e *LabelsMissingError) Error() string {
	cause := "a recording or relabeling rule probably removed them"
	fix := "Keep these labels on the metric where it is scraped or stored."
	if e.Aggregations {
		cause = "an Adaptive Metrics aggregation rule probably removed them"
		fix = fmt.Sprintf("Exclude %s from Adaptive Metrics recommendations, or change its rule to keep these labels, and run again once new data has arrived.", e.Metric)
	}
	if e.Runtime {
		cause += ", or the cluster's container runtime does not label cAdvisor series (older containerd setups, minikube), and splitting pod-level usage is not safe on this store"
		fix += " Without such a rule, the runtime needs to label container series."
	}
	return fmt.Sprintf("%s has series but none with the %s label%s: %s. Without them usage and requests cannot be attributed to workloads, and the figures would be wrong. %s",
		e.Metric, strings.Join(e.Labels, ", "), plural(len(e.Labels)), cause, fix)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// missingLabels returns the labels that no series of metric carries, or nil
// when the metric has no series or carries all of them.
func (c *Client) missingLabels(ctx context.Context, metric string, labels []string, at time.Time) ([]string, error) {
	if ok, err := c.present(ctx, fmt.Sprintf("count(%s{})", metric), at); err != nil || !ok {
		return nil, err
	}
	all := make([]string, len(labels))
	for i, l := range labels {
		all[i] = fmt.Sprintf("%s!=\"\"", l)
	}
	if ok, err := c.present(ctx, fmt.Sprintf("count(%s{%s})", metric, strings.Join(all, ",")), at); err != nil || ok {
		return nil, err
	}
	var missing []string
	for i, l := range labels {
		ok, err := c.present(ctx, fmt.Sprintf("count(%s{%s})", metric, all[i]), at)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, l)
		}
	}
	if len(missing) == 0 {
		// Every label exists, but never all on one series.
		missing = labels
	}
	return missing, nil
}

// checkLabels fails on the first metric that lacks a required label.
func checkLabels(ctx context.Context, c *Client, at time.Time, aggregations bool) error {
	for _, r := range requiredLabels {
		missing, err := c.missingLabels(ctx, r.metric, r.labels, at)
		if err != nil {
			return fmt.Errorf("probe labels of %s: %w", r.metric, err)
		}
		if len(missing) > 0 {
			return &LabelsMissingError{Metric: r.metric, Labels: missing, Aggregations: aggregations}
		}
	}
	return nil
}
