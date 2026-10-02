package prometheus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
)

// Auth adds credentials to requests for a store. Managed backends plug in here.
type Auth interface {
	Wrap(hc *http.Client) (*http.Client, error)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withHeader(hc *http.Client, set func(*http.Request)) *http.Client {
	return withRequest(hc, func(r *http.Request) error { set(r); return nil })
}

func withRequest(hc *http.Client, edit func(*http.Request) error) *http.Client {
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := *hc
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		if err := edit(r); err != nil {
			return nil, err
		}
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

// Header sends one fixed header, such as an API key or a Mimir tenant.
type Header struct{ Name, Value string }

func (h Header) Wrap(hc *http.Client) (*http.Client, error) {
	return withHeader(hc, func(r *http.Request) { r.Header.Set(h.Name, h.Value) }), nil
}

// TenantHeader selects the tenant of a multi-tenant Mimir or Cortex.
const TenantHeader = "X-Scope-OrgID"

// Chain applies several Auths in order. A signing Auth such as SigV4 must
// come last so that it signs the headers the others add.
type Chain []Auth

func (c Chain) Wrap(hc *http.Client) (*http.Client, error) {
	// Each Wrap puts its step in front of the transport it is given, so
	// wrapping in reverse runs c[0] first and the last Auth just before the
	// request is sent.
	for i := len(c) - 1; i >= 0; i-- {
		var err error
		if hc, err = c[i].Wrap(hc); err != nil {
			return nil, err
		}
	}
	return hc, nil
}

// SigV4 signs requests with AWS Signature Version 4, as Amazon Managed
// Service for Prometheus requires.
type SigV4 struct {
	Region      string
	Service     string // "aps" when empty
	Credentials aws.CredentialsProvider
	Now         func() time.Time // for tests
}

// NewSigV4 loads credentials through the standard AWS chain: environment
// variables, the shared config and credentials files (AWS_PROFILE, SSO,
// assumed roles), web identity, and the container or instance role.
func NewSigV4(ctx context.Context, region string) (*SigV4, error) {
	if region == "" {
		return nil, fmt.Errorf("SigV4 needs an AWS region")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return &SigV4{Region: region, Credentials: cfg.Credentials}, nil
}

var emptyPayloadHash = func() string {
	h := sha256.Sum256(nil)
	return hex.EncodeToString(h[:])
}()

func (s *SigV4) Wrap(hc *http.Client) (*http.Client, error) {
	if s.Credentials == nil {
		return nil, fmt.Errorf("SigV4: no AWS credentials provider")
	}
	service := s.Service
	if service == "" {
		service = "aps"
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	signer := v4.NewSigner()
	return withRequest(hc, func(r *http.Request) error {
		if r.Body != nil && r.Body != http.NoBody {
			return fmt.Errorf("SigV4: only requests without a body are signed")
		}
		creds, err := s.Credentials.Retrieve(r.Context())
		if err != nil {
			return fmt.Errorf("AWS credentials: %w", err)
		}
		return signer.SignHTTP(r.Context(), creds, r, emptyPayloadHash, service, s.Region, now())
	}), nil
}

// Unsupported stands in for a managed-backend scheme that is not implemented yet.
type Unsupported struct{ Scheme string }

func (u Unsupported) Wrap(*http.Client) (*http.Client, error) {
	return nil, fmt.Errorf("authentication scheme %q is not implemented", u.Scheme)
}
