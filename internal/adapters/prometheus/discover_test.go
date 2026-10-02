package prometheus

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
)

func svc(ns, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.1", Ports: ports},
	}
}

func port(name string, p int32, target int) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: p, TargetPort: intstr.FromInt(target)}
}

func TestClassifyCommonCharts(t *testing.T) {
	cases := []struct {
		name string
		s    *corev1.Service
		want Candidate
		ok   bool
	}{
		{"community chart on port 80", svc("mon", "prom-server", map[string]string{"app.kubernetes.io/name": "prometheus", "app.kubernetes.io/component": "server"}, port("http", 80, 9090)),
			Candidate{Kind: KindPrometheus, Namespace: "mon", Service: "prom-server", Port: "80", Scheme: "http"}, true},
		{"operator stack service", svc("mon", "kps-prometheus", map[string]string{"app": "kube-prometheus-stack-prometheus"}, port("http-web", 9090, 9090)),
			Candidate{Kind: KindPrometheus, Namespace: "mon", Service: "kps-prometheus", Port: "9090", Scheme: "http"}, true},
		{"openshift style", svc("openshift-monitoring", "prometheus-k8s", nil, port("web", 9091, 9091)),
			Candidate{Kind: KindPrometheus, Namespace: "openshift-monitoring", Service: "prometheus-k8s", Port: "9091", Scheme: "https"}, true},
		{"thanos query", svc("mon", "thanos-query", nil, port("http", 9090, 10902), port("grpc", 10901, 10901)),
			Candidate{Kind: KindThanos, Namespace: "mon", Service: "thanos-query", Port: "9090", Scheme: "http"}, true},
		{"thanos query frontend is not the querier", svc("mon", "thanos-query-frontend", nil, port("http", 9090, 9090)), Candidate{}, false},
		{"vmselect", svc("vm", "vmselect-cluster", nil, port("http", 8481, 8481)),
			Candidate{Kind: KindVictoriaMetrics, Namespace: "vm", Service: "vmselect-cluster", Port: "8481", Scheme: "http", Prefix: "/select/0/prometheus"}, true},
		{"vmsingle", svc("vm", "vmsingle-main", nil, port("http", 8429, 8429)),
			Candidate{Kind: KindVictoriaMetrics, Namespace: "vm", Service: "vmsingle-main", Port: "8429", Scheme: "http"}, true},
		{"pushgateway", svc("mon", "prom-pushgateway", map[string]string{"app.kubernetes.io/name": "prometheus-pushgateway"}, port("http", 9091, 9091)), Candidate{}, false},
		{"alertmanager", svc("mon", "prom-alertmanager", map[string]string{"app.kubernetes.io/name": "alertmanager"}, port("http", 9093, 9093)), Candidate{}, false},
		{"unrelated", svc("app", "web", nil, port("http", 80, 8080)), Candidate{}, false},
	}
	for _, c := range cases {
		got, ok := classify(c.s)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v (%v)", c.name, ok, c.ok, got)
			continue
		}
		if ok {
			got.Reason = ""
			if got != c.want {
				t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
			}
		}
	}
}

func cr(kind, apiVersion, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     spec,
	}}
}

func dyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		prometheusGVR:      "PrometheusList",
		prometheusAgentGVR: "PrometheusAgentList",
	}, objs...)
}

func TestDiscoverPrefersOperatorOverGuess(t *testing.T) {
	cs := kfake.NewSimpleClientset(
		svc("mon", "prometheus-operated", nil, corev1.ServicePort{Name: "web", Port: 9090, TargetPort: intstr.FromString("web")}),
		svc("mon", "thanos-query", nil, port("http", 9090, 9090)),
		svc("other", "random", nil, port("http", 80, 80)),
	)
	d, err := Discover(context.Background(), cs, dyn(cr("Prometheus", "monitoring.coreos.com/v1", "mon", "k8s", nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 2 {
		t.Fatalf("candidates: %v", d.Candidates)
	}
	if d.Candidates[0].Kind != KindThanos || !d.Candidates[1].Operator || d.Candidates[1].Service != "prometheus-operated" {
		t.Errorf("order or contents wrong: %v", d.Candidates)
	}
}

func TestDiscoverWithoutOperatorCRDs(t *testing.T) {
	cs := kfake.NewSimpleClientset(svc("mon", "prom", map[string]string{"app": "prometheus"}, port("http", 9090, 9090)))
	d, err := Discover(context.Background(), cs, nil)
	if err != nil || len(d.Candidates) != 1 {
		t.Fatalf("got %v %v", d, err)
	}
}

func TestDiscoverReportsAgentModeRemoteWrite(t *testing.T) {
	cs := kfake.NewSimpleClientset()
	agent := cr("PrometheusAgent", "monitoring.coreos.com/v1alpha1", "mon", "agent", map[string]any{
		"remoteWrite": []any{map[string]any{"url": "https://user:pw@mimir.example.com/api/v1/push"}},
	})
	d, err := Discover(context.Background(), cs, dyn(agent))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Candidates) != 0 || len(d.Agents) != 1 {
		t.Fatalf("got %+v", d)
	}
	if got := d.Agents[0].RemoteWrite; len(got) != 1 || got[0] != "https://mimir.example.com/api/v1/push" {
		t.Errorf("remote_write = %v", got)
	}
}
