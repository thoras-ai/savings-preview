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
