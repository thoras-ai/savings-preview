package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func proxyAgainst(t *testing.T, h http.HandlerFunc, p ServiceProxy) *ServiceProxy {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &Conn{Config: &rest.Config{Host: srv.URL}}
	px, err := c.Proxy(p.Namespace, p.Name, p.Port, p.Scheme, p.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	return px
}

func TestServiceProxyPath(t *testing.T) {
	var gotPath, gotRaw string
	p := proxyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRaw = r.URL.EscapedPath(), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}, ServiceProxy{Namespace: "mon", Name: "prom", Port: "9090", Scheme: "http", Prefix: "/select/0/prometheus"})
	body, status, err := p.Get(context.Background(), "/api/v1/query", url.Values{"query": {"up"}})
	if err != nil || status != 200 || string(body) != `{"status":"success"}` {
		t.Fatalf("body=%s status=%d err=%v", body, status, err)
	}
	if want := "/api/v1/namespaces/mon/services/prom:9090/proxy/select/0/prometheus/api/v1/query"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if gotRaw != "query=up" {
		t.Errorf("query = %s", gotRaw)
	}
}

func TestServiceProxyHTTPSScheme(t *testing.T) {
	var gotPath string
	p := proxyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}, ServiceProxy{Namespace: "mon", Name: "prom", Port: "web", Scheme: "https"})
	if _, _, err := p.Get(context.Background(), "/api/v1/query", nil); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/namespaces/mon/services/https:prom:web/proxy/api/v1/query"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
}

func TestServiceProxyKeepsErrorBodyAndStatus(t *testing.T) {
	p := proxyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"execution","error":"too many samples"}`))
	}, ServiceProxy{Namespace: "mon", Name: "prom", Port: "9090"})
	body, status, err := p.Get(context.Background(), "/api/v1/query", nil)
	if err != nil || status != 422 || !strings.Contains(string(body), "too many samples") {
		t.Fatalf("body=%q status=%d err=%v", body, status, err)
	}
}

func TestServiceProxyForbidden(t *testing.T) {
	p := proxyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"services/proxy is forbidden","reason":"Forbidden","code":403}`))
	}, ServiceProxy{Namespace: "mon", Name: "prom", Port: "9090"})
	_, status, _ := p.Get(context.Background(), "/api/v1/query", nil)
	if status != 403 {
		t.Errorf("status = %d", status)
	}
}
