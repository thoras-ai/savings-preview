package prometheus

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

func TestHeaderAndChain(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()
	auth := Chain{Header{Name: "X-Api-Key", Value: "k1"}, Header{Name: TenantHeader, Value: "team-a"}, Basic{User: "123456", Password: "glc_token"}}
	c := &Client{G: &HTTPGetter{Base: srv.URL, Auth: auth}, Retries: 1, sleep: noSleep}
	if _, err := c.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Api-Key") != "k1" || got.Get(TenantHeader) != "team-a" {
		t.Errorf("headers: %v", got)
	}
	if u, p, ok := (&http.Request{Header: got}).BasicAuth(); !ok || u != "123456" || p != "glc_token" {
		t.Errorf("basic auth: %q %q %v", u, p, ok)
	}
}

func TestChainRunsInOrder(t *testing.T) {
	var order []string
	step := func(name string) Auth {
		return authFunc(func(hc *http.Client) (*http.Client, error) {
			return withHeader(hc, func(*http.Request) { order = append(order, name) }), nil
		})
	}
	hc, err := Chain{step("a"), step("b"), step("c")}.Wrap(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("done")
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hc.Get("http://example.invalid/")
	if strings.Join(order, "") != "abc" {
		t.Errorf("order %v", order)
	}
}

type authFunc func(*http.Client) (*http.Client, error)

func (f authFunc) Wrap(hc *http.Client) (*http.Client, error) { return f(hc) }

// verifySigV4 checks a request the way AWS does, from the raw request as it
// arrives, independently of the SDK signer.
func verifySigV4(r *http.Request, accessKey, secret, region, service string) error {
	authz := r.Header.Get("Authorization")
	const algo = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(authz, algo) {
		return fmt.Errorf("authorization %q is not SigV4", authz)
	}
	fields := map[string]string{}
	for _, f := range strings.Split(strings.TrimPrefix(authz, algo), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		fields[k] = v
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if len(amzDate) < 8 {
		return fmt.Errorf("X-Amz-Date %q", amzDate)
	}
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", amzDate[:8], region, service)
	if fields["Credential"] != accessKey+"/"+scope {
		return fmt.Errorf("credential scope %q, want %q", fields["Credential"], accessKey+"/"+scope)
	}

	// '+' is a literal plus to AWS, not a space.
	enc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	var pairs []string
	for _, kv := range strings.Split(r.URL.RawQuery, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		dk, err1 := url.PathUnescape(k)
		dv, err2 := url.PathUnescape(v)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad query encoding %q", kv)
		}
		pairs = append(pairs, enc(dk)+"="+enc(dv))
	}
	sort.Strings(pairs)
	signed := strings.Split(fields["SignedHeaders"], ";")
	var canonHeaders strings.Builder
	for _, h := range signed {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	empty := sha256.Sum256(nil)
	canonical := strings.Join([]string{r.Method, r.URL.EscapedPath(), strings.Join(pairs, "&"),
		canonHeaders.String(), fields["SignedHeaders"], hex.EncodeToString(empty[:])}, "\n")
	ch := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(ch[:])}, "\n")
	mac := func(key []byte, s string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(s))
		return h.Sum(nil)
	}
	key := mac(mac(mac(mac([]byte("AWS4"+secret), amzDate[:8]), region), service), "aws4_request")
	if want := hex.EncodeToString(mac(key, toSign)); fields["Signature"] != want {
		return fmt.Errorf("signature mismatch for canonical request:\n%s", canonical)
	}
	return nil
}

func TestSigV4SignsQueries(t *testing.T) {
	const akid, secret = "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	var verr error
	var sawTenant bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifySigV4(r, akid, secret, "us-west-2", "aps"); err != nil {
			verr = err
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"The request signature we calculated does not match the signature you provided."}`)
			return
		}
		sawTenant = strings.Contains(r.Header.Get("Authorization"), "x-scope-orgid")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()
	sig := &SigV4{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider(akid, secret, "")}
	c := &Client{G: &HTTPGetter{Base: srv.URL + "/workspaces/ws-1234", Auth: Chain{Header{Name: TenantHeader, Value: "t"}, sig}}, Retries: 1, sleep: noSleep}
	// Spaces, quotes, braces, plus signs and newlines all need encoding.
	q := "sum by (namespace) (rate(container_cpu_usage_seconds_total{container!=\"\",image=~\"a+b\"}[5m]))\n* 1"
	if _, err := c.QueryRange(context.Background(), q, time.Unix(t0, 0), time.Unix(t0+3600, 0), time.Minute); err != nil {
		t.Fatalf("%v: %v", err, verr)
	}
	if !sawTenant {
		t.Error("headers added before the signer must be signed")
	}
}

func TestSigV4CredentialError(t *testing.T) {
	sig := &SigV4{Region: "us-east-1", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("no SSO session")
	})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("an unsigned request was sent") }))
	defer srv.Close()
	c := &Client{G: &HTTPGetter{Base: srv.URL, Auth: sig}, Retries: 1, sleep: noSleep}
	_, err := c.Query(context.Background(), "up", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "no SSO session") {
		t.Fatalf("got %v", err)
	}
}

func TestAzureClientCredentials(t *testing.T) {
	var form url.Values
	var tokenPath string
	tokens := 0
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form, tokenPath = r.PostForm, r.URL.Path
		tokens++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"entra-token"}`)
	}))
	defer login.Close()
	var authz []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = append(authz, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer store.Close()

	a, err := NewAzure(context.Background(), "https://prometheus.monitor.azure.us",
		AzureCredentials{TenantID: "contoso.onmicrosoft.com", ClientID: "app-1", ClientSecret: "s3cret", Login: login.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{G: &HTTPGetter{Base: store.URL, Auth: a}, Retries: 1, sleep: noSleep}
	for range 2 {
		if _, err := c.Query(context.Background(), "count(up{})", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if tokenPath != "/contoso.onmicrosoft.com/oauth2/v2.0/token" || form.Get("grant_type") != "client_credentials" ||
		form.Get("client_id") != "app-1" || form.Get("client_secret") != "s3cret" || form.Get("scope") != "https://prometheus.monitor.azure.us/.default" {
		t.Errorf("token request %s %v", tokenPath, form)
	}
	if tokens != 1 || len(authz) != 2 || authz[0] != "Bearer entra-token" || authz[1] != authz[0] {
		t.Errorf("%d token requests, Authorization %v", tokens, authz)
	}
}

func TestAzureCLI(t *testing.T) {
	var calls [][]string
	out := `{"accessToken":"cli-token","expiresOn":"2026-10-02 08:00:00.000000","expires_on":1790006400,"tokenType":"Bearer"}`
	a, err := NewAzure(context.Background(), AzureAudience, AzureCredentials{TenantID: "72f988bf-86f1-41af-91ab-2d7cd011db47",
		CLI: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, args)
			return []byte(out), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.Source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "cli-token" || tok.Expiry.Unix() != 1790006400 {
		t.Errorf("token %+v", tok)
	}
	want := "account get-access-token --resource https://prometheus.monitor.azure.com --output json --tenant 72f988bf-86f1-41af-91ab-2d7cd011db47"
	if len(calls) != 1 || strings.Join(calls[0], " ") != want {
		t.Errorf("az calls %q", calls)
	}

	// Older CLIs print only expiresOn, in local time.
	src := &azCLISource{ctx: context.Background(), resource: AzureAudience, run: func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"accessToken":"old-cli","expiresOn":"2026-10-02 08:00:00.000000"}`), nil
	}}
	tok, err = src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 2, 8, 0, 0, 0, time.Local); !tok.Expiry.Equal(want) {
		t.Errorf("expiry %v, want %v", tok.Expiry, want)
	}

	src.run = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("Please run 'az login' to setup account.")
	}
	if _, err := src.Token(); err == nil || !strings.Contains(err.Error(), "az login") {
		t.Errorf("got %v", err)
	}
	src.run = func(context.Context, ...string) ([]byte, error) { return []byte(`{}`), nil }
	if _, err := src.Token(); err == nil {
		t.Error("an empty token was accepted")
	}
}

func TestNewAzureRejects(t *testing.T) {
	for _, c := range []struct {
		creds AzureCredentials
		err   string
	}{
		{AzureCredentials{TenantID: "t", ClientSecret: "s"}, "needs the client ID"},
		{AzureCredentials{TenantID: "t", ClientID: "c"}, "needs the client secret"},
		{AzureCredentials{ClientID: "c", ClientSecret: "s"}, "needs the tenant ID"},
		{AzureCredentials{TenantID: "../evil", ClientID: "c", ClientSecret: "s"}, "not an Entra ID tenant"},
	} {
		if _, err := NewAzure(context.Background(), AzureAudience, c.creds); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: got %v, want %q", c.creds, err, c.err)
		}
	}
}
