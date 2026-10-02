package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestAccessString(t *testing.T) {
	for want, a := range map[string]Access{
		"list services -A":                           {Verb: "list", Resource: "services", AllNamespaces: true},
		"list nodes":                                 {Verb: "list", Resource: "nodes"},
		"list pods.metrics.k8s.io -A":                {Verb: "list", Group: "metrics.k8s.io", Resource: "pods", AllNamespaces: true},
		"get services/proxy prom:9090 -n monitoring": {Verb: "get", Resource: "services", Subresource: "proxy", Namespace: "monitoring", Name: "prom:9090"},
	} {
		if got := a.String(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCanISendsTheAttributes(t *testing.T) {
	cs := fake.NewClientset()
	var got *authorizationv1.ResourceAttributes
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		got = r.Spec.ResourceAttributes
		r.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: got.Resource != "nodes", Reason: "rule"}
		return true, r, nil
	})
	ok, reason, err := CanI(context.Background(), cs, Access{Verb: "get", Resource: "services", Subresource: "proxy", Namespace: "mon", Name: "prom:9090"})
	if err != nil || !ok || reason != "rule" {
		t.Fatalf("ok %v reason %q err %v", ok, reason, err)
	}
	if got.Verb != "get" || got.Resource != "services" || got.Subresource != "proxy" || got.Namespace != "mon" || got.Name != "prom:9090" {
		t.Errorf("attributes %+v", got)
	}
	if ok, _, _ := CanI(context.Background(), cs, Access{Verb: "list", Resource: "nodes"}); ok {
		t.Error("nodes: want denied")
	}
	if _, _, err := CanI(context.Background(), cs, Access{Verb: "list", Resource: "pods", AllNamespaces: true}); err != nil || got.Namespace != "" {
		t.Errorf("all namespaces: namespace %q, err %v", got.Namespace, err)
	}
}

func TestCanIEvaluationError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		r.Status.EvaluationError = "webhook unreachable"
		return true, r, nil
	})
	if _, _, err := CanI(context.Background(), cs, Access{Verb: "list", Resource: "pods"}); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("err %v", err)
	}
}

func TestWhoAmI(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := &authenticationv1.SelfSubjectReview{}
		r.Status.UserInfo.Username = "system:serviceaccount:savings-preview:savings-preview"
		r.Status.UserInfo.Groups = []string{"system:serviceaccounts", "system:authenticated"}
		return true, r, nil
	})
	id, err := WhoAmI(context.Background(), cs)
	if err != nil || id.User != "system:serviceaccount:savings-preview:savings-preview" || len(id.Groups) != 2 {
		t.Fatalf("%+v %v", id, err)
	}
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("not found")
	})
	if _, err := WhoAmI(context.Background(), cs); err == nil {
		t.Fatal("want the error")
	}
}

func metricsClient(t *testing.T, h http.HandlerFunc) rest.Interface {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return cs.Discovery().RESTClient()
}

func TestPodMetrics(t *testing.T) {
	var path, limit string
	c := metricsClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, limit = r.URL.Path, r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"kind":"PodMetricsList","items":[{"metadata":{"name":"a"}}]}`))
	})
	n, err := PodMetrics(context.Background(), c)
	if err != nil || n != 1 || path != MetricsPath || limit != "1" {
		t.Fatalf("n %d err %v path %s limit %s", n, err, path, limit)
	}
}

func TestPodMetricsWithoutMetricsServer(t *testing.T) {
	c := metricsClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"the server could not find the requested resource"}`))
	})
	_, err := PodMetrics(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "metrics-server is not installed") {
		t.Fatalf("err %v", err)
	}
}
