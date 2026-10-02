// Package kube builds Kubernetes clients from a kubeconfig and reaches
// in-cluster services through the API server's service proxy.
package kube

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

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
	cluster := ctxName
	if c, ok := raw.Contexts[ctxName]; ok && c.Cluster != "" {
		cluster = c.Cluster
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
	return &Conn{Config: cfg, Clientset: cs, Dynamic: dc, ClusterName: cluster, Context: ctxName}, nil
}

// ServiceProxy reaches a service through
// /api/v1/namespaces/{ns}/services/{scheme:name:port}/proxy/{prefix}/{path}.
type ServiceProxy struct {
	Client    rest.Interface
	Namespace string
	Name      string
	Port      string // port number or port name
	Scheme    string // "http" or "https"
	Prefix    string // path prefix inside the service, for example "/select/0/prometheus"
}

func (s *ServiceProxy) resourceName() string {
	n := s.Name + ":" + s.Port
	if s.Scheme == "https" {
		n = "https:" + n
	}
	return n
}

// Get implements prom.Getter.
func (s *ServiceProxy) Get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	req := s.Client.Get().
		Namespace(s.Namespace).
		Resource("services").
		Name(s.resourceName()).
		SubResource("proxy").
		Suffix(s.Prefix + path)
	for k, vs := range params {
		for _, v := range vs {
			req = req.Param(k, v)
		}
	}
	var status int
	res := req.Do(ctx)
	res.StatusCode(&status)
	body, err := res.Raw()
	if err != nil && status != 0 {
		// Raw returns the response body alongside an error for non-2xx statuses.
		return body, status, nil
	}
	return body, status, err
}

// PortString formats a numeric port.
func PortString(p int32) string { return strconv.Itoa(int(p)) }
