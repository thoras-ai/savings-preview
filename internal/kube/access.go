package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Access is one kind of request a command sends, in the terms of
// kubectl auth can-i.
type Access struct {
	Verb        string
	Group       string // "" is the core group
	Resource    string
	Subresource string
	Namespace   string // "" with AllNamespaces false is a cluster-scoped resource
	Name        string
	// AllNamespaces is a request across every namespace, as with -A.
	AllNamespaces bool
}

// String reads like the arguments of kubectl auth can-i.
func (a Access) String() string {
	r := a.Resource
	if a.Group != "" {
		r += "." + a.Group
	}
	if a.Subresource != "" {
		r += "/" + a.Subresource
	}
	s := a.Verb + " " + r
	if a.Name != "" {
		s += " " + a.Name
	}
	switch {
	case a.AllNamespaces:
		s += " -A"
	case a.Namespace != "":
		s += " -n " + a.Namespace
	}
	return s
}

// CanI asks the API server whether the connection's identity may make the
// request, as kubectl auth can-i does. The review is evaluated and not
// stored. The reason, when the server gives one, says which rule decided.
func CanI(ctx context.Context, cs kubernetes.Interface, a Access) (allowed bool, reason string, err error) {
	ns := a.Namespace
	if a.AllNamespaces {
		ns = metav1.NamespaceAll
	}
	r, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: a.Verb, Group: a.Group, Resource: a.Resource, Subresource: a.Subresource, Namespace: ns, Name: a.Name,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	if r.Status.EvaluationError != "" && !r.Status.Allowed {
		return false, r.Status.Reason, fmt.Errorf("%s", r.Status.EvaluationError)
	}
	return r.Status.Allowed, r.Status.Reason, nil
}

// Identity is the user the API server sees.
type Identity struct {
	User   string
	Groups []string
}

// WhoAmI asks the API server who the connection's credentials belong to, as
// kubectl auth whoami does. Servers before Kubernetes 1.28 may not answer.
func WhoAmI(ctx context.Context, cs kubernetes.Interface) (*Identity, error) {
	r, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return &Identity{User: r.Status.UserInfo.Username, Groups: r.Status.UserInfo.Groups}, nil
}

// MetricsPath is the metrics-server list of pod usage that polling reads.
const MetricsPath = "/apis/metrics.k8s.io/v1beta1/pods"

// PodMetrics lists pod usage from metrics-server and returns how many pods
// it returned. It asks for one pod, though metrics-server may return all.
func PodMetrics(ctx context.Context, c rest.Interface) (int, error) {
	raw, err := c.Get().AbsPath(MetricsPath).Param("limit", "1").DoRaw(ctx)
	if err != nil {
		return 0, metricsError(err)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, fmt.Errorf("read %s: %w", MetricsPath, err)
	}
	return len(list.Items), nil
}

func metricsError(err error) error {
	if apierrors.IsForbidden(err) {
		return errors.New("forbidden: the identity may not list pods.metrics.k8s.io")
	}
	msg := err.Error()
	if strings.Contains(msg, "the server could not find the requested resource") {
		return fmt.Errorf("metrics.k8s.io is not served; metrics-server is not installed (%w)", err)
	}
	return err
}
