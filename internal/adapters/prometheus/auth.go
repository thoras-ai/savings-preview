package prometheus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/oauth2/google"
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

// GoogleScope is the OAuth scope for reading Cloud Monitoring data, which
// covers the PromQL API.
const GoogleScope = "https://www.googleapis.com/auth/monitoring.read"

// Google sends an OAuth2 access token from a Google credential, as Google
// Managed Prometheus and the Cloud Monitoring PromQL API require.
type Google struct {
	Source oauth2.TokenSource
}

// NewGoogle loads a Google credential. With a file, it must be a service
// account key. Without one, Application Default Credentials apply:
// GOOGLE_APPLICATION_CREDENTIALS (a service account key or a workload identity
// federation configuration), the gcloud user login, then the metadata server,
// which is how workload identity on GKE and the VM service account are reached.
func NewGoogle(ctx context.Context, credentialsFile string) (*Google, error) {
	if credentialsFile != "" {
		b, err := os.ReadFile(credentialsFile)
		if err != nil {
			return nil, err
		}
		creds, err := google.CredentialsFromJSONWithType(ctx, b, google.ServiceAccount, GoogleScope)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", credentialsFile, err)
		}
		return &Google{Source: oauth2.ReuseTokenSource(nil, creds.TokenSource)}, nil
	}
	creds, err := google.FindDefaultCredentials(ctx, GoogleScope)
	if err != nil {
		return nil, fmt.Errorf("find Google Application Default Credentials: %w", err)
	}
	// HTTPGetter wraps its client on every request, so the token is cached
	// here and refreshed only when it expires.
	return &Google{Source: oauth2.ReuseTokenSource(nil, creds.TokenSource)}, nil
}

func (g *Google) Wrap(hc *http.Client) (*http.Client, error) {
	return withToken(hc, g.Source, "Google")
}

func withToken(hc *http.Client, src oauth2.TokenSource, name string) (*http.Client, error) {
	if src == nil {
		return nil, fmt.Errorf("%s: no token source", name)
	}
	return withRequest(hc, func(r *http.Request) error {
		tok, err := src.Token()
		if err != nil {
			return fmt.Errorf("%s credentials: %w", name, err)
		}
		tok.SetAuthHeader(r)
		return nil
	}), nil
}

// Azure sends an Entra ID access token, as Azure Monitor managed Prometheus
// requires.
type Azure struct {
	Source oauth2.TokenSource
}

func (a *Azure) Wrap(hc *http.Client) (*http.Client, error) {
	return withToken(hc, a.Source, "Azure")
}

// AzureCredentials select how NewAzure gets a token. With a client ID and
// secret, it uses the client credentials grant of that app registration in
// TenantID. Without them, it asks the Azure CLI for a token of the signed-in
// user or managed identity, in TenantID when one is given.
type AzureCredentials struct {
	TenantID, ClientID, ClientSecret string
	// Login is the Entra ID authority; empty for the one of the audience's
	// cloud.
	Login string
	// CLI runs the Azure CLI with args and returns its standard output; nil
	// runs az.
	CLI func(ctx context.Context, args ...string) ([]byte, error)
}

// azureLogins are the Entra ID authorities of the Azure clouds, by the
// domain of their Prometheus audience.
var azureLogins = map[string]string{
	"https://prometheus.monitor.azure.com": "https://login.microsoftonline.com",
	"https://prometheus.monitor.azure.us":  "https://login.microsoftonline.us",
	"https://prometheus.monitor.azure.cn":  "https://login.chinacloudapi.cn",
}

var tenantID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// NewAzure returns an Auth that sends Entra ID tokens for audience.
func NewAzure(ctx context.Context, audience string, c AzureCredentials) (*Azure, error) {
	if c.TenantID != "" && !tenantID.MatchString(c.TenantID) {
		return nil, fmt.Errorf("%q is not an Entra ID tenant ID or domain", c.TenantID)
	}
	if c.ClientID != "" || c.ClientSecret != "" {
		switch {
		case c.ClientID == "":
			return nil, errors.New("an Azure client secret needs the client ID of its app registration")
		case c.ClientSecret == "":
			return nil, errors.New("an Azure client ID needs the client secret of its app registration")
		case c.TenantID == "":
			return nil, errors.New("an Azure app registration needs the tenant ID it belongs to")
		}
		login := c.Login
		if login == "" {
			login = azureLogins[audience]
		}
		if login == "" {
			login = azureLogins[AzureAudience]
		}
		cc := clientcredentials.Config{
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			TokenURL:     strings.TrimRight(login, "/") + "/" + c.TenantID + "/oauth2/v2.0/token",
			Scopes:       []string{audience + "/.default"},
			AuthStyle:    oauth2.AuthStyleInParams,
		}
		return &Azure{Source: cc.TokenSource(ctx)}, nil
	}
	run := c.CLI
	if run == nil {
		run = runAz
	}
	return &Azure{Source: oauth2.ReuseTokenSource(nil, &azCLISource{ctx: ctx, run: run, resource: audience, tenant: c.TenantID})}, nil
}

// azCLISource gets tokens from az account get-access-token.
type azCLISource struct {
	ctx              context.Context
	run              func(ctx context.Context, args ...string) ([]byte, error)
	resource, tenant string
}

func (s *azCLISource) Token() (*oauth2.Token, error) {
	args := []string{"account", "get-access-token", "--resource", s.resource, "--output", "json"}
	if s.tenant != "" {
		args = append(args, "--tenant", s.tenant)
	}
	out, err := s.run(s.ctx, args...)
	if err != nil {
		return nil, err
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		ExpiresOn   string `json:"expiresOn"`  // local time, in every CLI version
		ExpiresAt   int64  `json:"expires_on"` // unix seconds, in CLI 2.54 and later
	}
	if err := json.Unmarshal(out, &r); err != nil || r.AccessToken == "" {
		return nil, errors.New("az account get-access-token printed no access token")
	}
	tok := &oauth2.Token{AccessToken: r.AccessToken, TokenType: "Bearer"}
	switch {
	case r.ExpiresAt > 0:
		tok.Expiry = time.Unix(r.ExpiresAt, 0)
	default:
		if t, err := time.ParseInLocation("2006-01-02 15:04:05.999999", r.ExpiresOn, time.Local); err == nil {
			tok.Expiry = t
		} else {
			// Ask again in a few minutes rather than keep a token of
			// unknown lifetime.
			tok.Expiry = time.Now().Add(5 * time.Minute)
		}
	}
	return tok, nil
}

func runAz(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "az", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the Azure CLI (az) is not installed: install it and run az login, or use an app registration's client secret")
		}
		return nil, fmt.Errorf("az account get-access-token: %w: %s", err, truncate(stderr.String(), 300))
	}
	return stdout.Bytes(), nil
}

// Unsupported stands in for a managed-backend scheme that is not implemented yet.
type Unsupported struct{ Scheme string }

func (u Unsupported) Wrap(*http.Client) (*http.Client, error) {
	return nil, fmt.Errorf("authentication scheme %q is not implemented", u.Scheme)
}
