// Package kube builds Kubernetes clients from a kubeconfig and reaches
// in-cluster services through the API server's service proxy.
package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Conn holds clients for one kubeconfig context.
type Conn struct {
	Config      *rest.Config
	Clientset   kubernetes.Interface
	Dynamic     dynamic.Interface
	ClusterName string
	Context     string
	User        string // the context's user entry in the kubeconfig
}

// Connect loads the kubeconfig. Empty path and context use the standard
// loading rules (KUBECONFIG, ~/.kube/config) and the current context.
func Connect(kubeconfig, kubeContext string) (*Conn, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	ov := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, ov)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	ctxName := kubeContext
	if ctxName == "" {
		ctxName = raw.CurrentContext
	}
	cluster, user := ctxName, ""
	if c, ok := raw.Contexts[ctxName]; ok {
		if c.Cluster != "" {
			cluster = c.Cluster
		}
		user = c.AuthInfo
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.QPS, cfg.Burst = 5, 10
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{Config: cfg, Clientset: cs, Dynamic: dc, ClusterName: cluster, Context: ctxName, User: user}, nil
}

// ServiceProxy reaches a service through
// /api/v1/namespaces/{ns}/services/{scheme:name:port}/proxy/{prefix}/{path}.
// It uses the plain HTTP response, so a store's JSON error body reaches the
// caller instead of being replaced by a generic Kubernetes error.
type ServiceProxy struct {
	HC        *http.Client
	Base      string // API server URL, including any path prefix
	Namespace string
	Name      string
	Port      string // port number or port name
	Scheme    string // "http" or "https"
	Prefix    string // path prefix inside the service, for example "/select/0/prometheus"
}

// Proxy returns a ServiceProxy that uses the connection's credentials.
func (c *Conn) Proxy(namespace, name, port, scheme, prefix string) (*ServiceProxy, error) {
	hc, err := rest.HTTPClientFor(c.Config)
	if err != nil {
		return nil, err
	}
	base, _, err := rest.DefaultServerURL(c.Config.Host, c.Config.APIPath, schema.GroupVersion{}, rest.IsConfigTransportTLS(*c.Config))
	if err != nil {
		return nil, err
	}
	return &ServiceProxy{HC: hc, Base: base.String(), Namespace: namespace, Name: name, Port: port, Scheme: scheme, Prefix: prefix}, nil
}

func (s *ServiceProxy) resourceName() string {
	n := s.Name + ":" + s.Port
	if s.Scheme == "https" {
		n = "https:" + n
	}
	return n
}

// Get implements the store client's Getter.
func (s *ServiceProxy) Get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	u := strings.TrimRight(s.Base, "/") + "/api/v1/namespaces/" + url.PathEscape(s.Namespace) +
		"/services/" + url.PathEscape(s.resourceName()) + "/proxy" + s.Prefix + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.HC.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	return body, resp.StatusCode, err
}
