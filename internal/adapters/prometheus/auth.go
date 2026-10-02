package prometheus

import (
	"fmt"
	"net/http"
)

// Auth adds credentials to requests for a store. Managed backends plug in here.
type Auth interface {
	Wrap(hc *http.Client) (*http.Client, error)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withHeader(hc *http.Client, set func(*http.Request)) *http.Client {
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := *hc
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		set(r)
		return base.RoundTrip(r)
	})
	return &c
}

// Bearer sends an Authorization: Bearer header.
type Bearer struct{ Token string }

func (b Bearer) Wrap(hc *http.Client) (*http.Client, error) {
	return withHeader(hc, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+b.Token) }), nil
}

// Basic sends HTTP basic credentials.
type Basic struct{ User, Password string }

func (b Basic) Wrap(hc *http.Client) (*http.Client, error) {
	return withHeader(hc, func(r *http.Request) { r.SetBasicAuth(b.User, b.Password) }), nil
}

// Unsupported stands in for a managed-backend scheme that is not implemented yet.
type Unsupported struct{ Scheme string }

func (u Unsupported) Wrap(*http.Client) (*http.Client, error) {
	return nil, fmt.Errorf("authentication scheme %q is not implemented", u.Scheme)
}
