package prometheus

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Kind is the type of store behind a candidate.
type Kind string

const (
	KindThanos          Kind = "thanos-query"
	KindVictoriaMetrics Kind = "victoriametrics"
	KindPrometheus      Kind = "prometheus"
)

// Candidate is a service that may serve PromQL.
type Candidate struct {
	Kind      Kind
	Namespace string
	Service   string
	Port      string // port number as a string, used in the proxy path
	Scheme    string // http or https
	Prefix    string // path prefix of the query API inside the service
	Reason    string // how it was found
	Operator  bool   // found through a Prometheus Operator resource
}

func (c Candidate) String() string {
	kind := string(c.Kind)
	if kind == "" {
		kind = "store"
	}
	return fmt.Sprintf("%s %s/%s:%s%s (%s)", kind, c.Namespace, c.Service, c.Port, c.Prefix, c.Reason)
}

// AgentTarget is a Prometheus running in agent mode. It has no query API; its
// data goes to the remote_write targets.
type AgentTarget struct {
	Namespace   string
	Name        string
	RemoteWrite []string
}

// Discovery is what a cluster scan found.
type Discovery struct {
	Candidates []Candidate // best first
	Agents     []AgentTarget
}

var (
	prometheusGVR      = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheuses"}
	prometheusAgentGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1alpha1", Resource: "prometheusagents"}
)

// Discover looks for a PromQL store in the cluster: Prometheus Operator
// resources first, then services that match common chart names.
func Discover(ctx context.Context, cs kubernetes.Interface, dc dynamic.Interface) (*Discovery, error) {
	d := &Discovery{}
	svcs, err := cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("list services: %w (name the store with --target namespace/service:port to skip discovery)", err)
		}
		return nil, fmt.Errorf("list services: %w", err)
	}
	byKey := map[string]*corev1.Service{}
	for i := range svcs.Items {
		s := &svcs.Items[i]
		byKey[s.Namespace+"/"+s.Name] = s
	}

	seen := map[string]bool{}
	add := func(c Candidate) {
		k := fmt.Sprintf("%s/%s:%s", c.Namespace, c.Service, c.Port)
		if !seen[k] {
			seen[k] = true
			d.Candidates = append(d.Candidates, c)
		}
	}

	if dc != nil {
		// The Operator resources only improve the search. A missing CRD or a
		// missing permission to list them is not an error.
		crs, err := dc.Resource(prometheusGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, cr := range crs.Items {
				if s := byKey[cr.GetNamespace()+"/prometheus-operated"]; s != nil {
					if p, ok := pickPort(s, "web", "http", "9090"); ok {
						add(Candidate{Kind: KindPrometheus, Namespace: s.Namespace, Service: s.Name, Port: p, Scheme: "http",
							Reason: "Prometheus Operator resource " + cr.GetName(), Operator: true})
					}
				}
			}
		}
		agents, err := dc.Resource(prometheusAgentGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, a := range agents.Items {
				d.Agents = append(d.Agents, AgentTarget{Namespace: a.GetNamespace(), Name: a.GetName(), RemoteWrite: remoteWriteURLs(&a)})
			}
		}
	}

	for _, s := range svcs.Items {
		if c, ok := classify(&s); ok {
			add(c)
		}
	}
	sort.SliceStable(d.Candidates, func(i, j int) bool { return rank(d.Candidates[i]) < rank(d.Candidates[j]) })
	return d, nil
}

// rank orders candidates. A Thanos or VictoriaMetrics store usually holds more
// history than the Prometheus in front of it, so it goes first.
func rank(c Candidate) int {
	switch {
	case c.Kind == KindThanos:
		return 0
	case c.Kind == KindVictoriaMetrics:
		return 1
	case c.Operator:
		return 2
	}
	return 3
}

func remoteWriteURLs(u *unstructured.Unstructured) []string {
	items, _, _ := unstructured.NestedSlice(u.Object, "spec", "remoteWrite")
	var out []string
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if s, ok := m["url"].(string); ok {
				out = append(out, RedactURL(s))
			}
		}
	}
	return out
}

func classify(s *corev1.Service) (Candidate, bool) {
	if s.Spec.Type == corev1.ServiceTypeExternalName || len(s.Spec.Ports) == 0 {
		return Candidate{}, false
	}
	name := strings.ToLower(s.Name)
	lbl := s.Labels
	app := lbl["app.kubernetes.io/name"]
	if app == "" {
		app = lbl["app"]
	}
	comp := lbl["app.kubernetes.io/component"]
	if comp == "" {
		comp = lbl["component"]
	}
	headless := s.Spec.ClusterIP == corev1.ClusterIPNone
	mk := func(k Kind, reason string, prefix string, ports ...string) (Candidate, bool) {
		p, ok := pickPort(s, ports...)
		if !ok {
			return Candidate{}, false
		}
		scheme := "http"
		if p == "443" || p == "9091" || strings.HasPrefix(p, "https") {
			scheme = "https"
		}
		return Candidate{Kind: k, Namespace: s.Namespace, Service: s.Name, Port: p, Scheme: scheme, Prefix: prefix, Reason: reason}, true
	}

	switch {
	case strings.Contains(name, "thanos") && (strings.Contains(name, "query") || strings.Contains(name, "querier")) &&
		!strings.Contains(name, "frontend") && !strings.Contains(name, "grpc") && !headless:
		return mk(KindThanos, "service name", "", "http", "9090", "10902")
	case strings.Contains(name, "vmselect"):
		return mk(KindVictoriaMetrics, "service name", "/select/0/prometheus", "http", "8481")
	case strings.Contains(name, "vmsingle") || (strings.Contains(name, "victoria-metrics") && strings.Contains(name, "server")):
		return mk(KindVictoriaMetrics, "service name", "", "http", "8428", "8429")
	case app == "prometheus" && !headless && comp != "pushgateway" && comp != "alertmanager" && !strings.Contains(name, "operator"):
		return mk(KindPrometheus, "service labels", "", "http", "web", "http-web", "9090")
	case name == "prometheus-server" || name == "prometheus-k8s" || name == "prometheus":
		return mk(KindPrometheus, "service name", "", "http", "web", "http-web", "9090")
	case strings.HasSuffix(name, "-prometheus") && !strings.Contains(name, "operator") && !headless:
		return mk(KindPrometheus, "service name", "", "http", "web", "http-web", "9090")
	}
	return Candidate{}, false
}

// pickPort returns the service port, by name or number, that matches the first
// of want that the service exposes. A target port equal to a wanted number
// also matches, so a service on port 80 in front of a pod on 9090 is found.
func pickPort(s *corev1.Service, want ...string) (string, bool) {
	for _, w := range want {
		for _, p := range s.Spec.Ports {
			if p.Name == w || fmt.Sprint(p.Port) == w || targetIs(p.TargetPort, w) {
				return fmt.Sprint(p.Port), true
			}
		}
	}
	return "", false
}

func targetIs(t intstr.IntOrString, w string) bool {
	if t.Type == intstr.String {
		return t.StrVal == w
	}
	return fmt.Sprint(t.IntVal) == w
}
