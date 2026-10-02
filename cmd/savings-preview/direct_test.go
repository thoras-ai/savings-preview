package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func backend(t *testing.T, url, name string, tenant bool) prometheus.Backend {
	t.Helper()
	b, err := prometheus.ResolveBackend(url, name, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDirectAuthMimir(t *testing.T) {
	t.Setenv(envBearer, "")
	t.Setenv(envBasic, "")
	t.Setenv(envHeaders, "")
	b := backend(t, "http://mimir:8080", "", true)
	got, err := directAuth(context.Background(), b, credentialFiles{basic: writeFile(t, "gateway:s3cret\n")}, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	want := prometheus.Chain{prometheus.Header{Name: "X-Scope-OrgID", Value: "team-a"}, prometheus.Basic{User: "gateway", Password: "s3cret"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	got, err = directAuth(context.Background(), b, credentialFiles{}, "team-a")
	if err != nil || !reflect.DeepEqual(got, prometheus.Header{Name: "X-Scope-OrgID", Value: "team-a"}) {
		t.Errorf("tenant only: %#v, %v", got, err)
	}
}

func TestDirectAuthFromEnv(t *testing.T) {
	t.Setenv(envBasic, "")
	t.Setenv(envBearer, "tok")
	t.Setenv(envHeaders, "X-Api-Key: k1\n# comment\n\nX-Other: v: with colon")
	got, err := directAuth(context.Background(), backend(t, "http://prom:9090", "", false), credentialFiles{}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := prometheus.Chain{prometheus.Header{Name: "X-Api-Key", Value: "k1"}, prometheus.Header{Name: "X-Other", Value: "v: with colon"},
		prometheus.Bearer{Token: "tok"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v", got)
	}
	// A file takes precedence over the environment.
	got, err = directAuth(context.Background(), backend(t, "http://prom:9090", "", false),
		credentialFiles{token: writeFile(t, "from-file"), headers: writeFile(t, "")}, "")
	if err != nil || !reflect.DeepEqual(got, prometheus.Bearer{Token: "from-file"}) {
		t.Errorf("file over env: %#v, %v", got, err)
	}
	t.Setenv(envBasic, "u:p")
	if _, err := directAuth(context.Background(), backend(t, "http://prom:9090", "", false), credentialFiles{}, ""); err == nil {
		t.Error("both bearer and basic in the environment must fail")
	}
}

func TestDirectAuthErrorsHideSecrets(t *testing.T) {
	t.Setenv(envBearer, "")
	t.Setenv(envBasic, "")
	t.Setenv(envHeaders, "Authorization Bearer sekrit-token")
	_, err := directAuth(context.Background(), backend(t, "http://prom:9090", "", false), credentialFiles{}, "")
	if err == nil || strings.Contains(err.Error(), "sekrit") || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("got %v", err)
	}
	t.Setenv(envHeaders, "")
	t.Setenv(envBasic, "sekrit-no-colon")
	_, err = directAuth(context.Background(), backend(t, "http://prom:9090", "", false), credentialFiles{}, "")
	if err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("got %v", err)
	}
}

func TestDirectAuthGrafanaCloud(t *testing.T) {
	t.Setenv(envBearer, "")
	t.Setenv(envHeaders, "")
	t.Setenv(envBasic, "")
	b := backend(t, "https://prometheus-prod-13-prod-us-east-0.grafana.net", "", false)
	if _, err := directAuth(context.Background(), b, credentialFiles{}, ""); err == nil || !strings.Contains(err.Error(), "metrics:read") {
		t.Errorf("no credentials: %v", err)
	}
	if _, err := directAuth(context.Background(), b, credentialFiles{}, "team-a"); err == nil || !strings.Contains(err.Error(), "--tenant") {
		t.Errorf("tenant: %v", err)
	}
	t.Setenv(envBasic, "1234567:glc_abc")
	got, err := directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil || !reflect.DeepEqual(got, prometheus.Basic{User: "1234567", Password: "glc_abc"}) {
		t.Errorf("got %#v, %v", got, err)
	}
}

func TestDirectAuthAMP(t *testing.T) {
	t.Setenv(envHeaders, "")
	t.Setenv(envBasic, "")
	t.Setenv(envBearer, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	b := backend(t, "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1", "", false)
	got, err := directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil {
		t.Fatal(err)
	}
	chain, ok := got.(prometheus.Chain)
	if !ok || len(chain) != 1 {
		t.Fatalf("got %#v", got)
	}
	if s, ok := chain[0].(*prometheus.SigV4); !ok || s.Region != "us-east-1" {
		t.Errorf("got %#v", chain[0])
	}
	t.Setenv(envBearer, "tok")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, ""); err == nil || !strings.Contains(err.Error(), "SigV4") {
		t.Errorf("bearer with AMP: %v", err)
	}
	t.Setenv(envBearer, "")
	nr := backend(t, "https://amp-proxy.internal/workspaces/ws-1", prometheus.BackendAMP, false)
	if _, err := directAuth(context.Background(), nr, credentialFiles{}, ""); err == nil || !strings.Contains(err.Error(), "--aws-region") {
		t.Errorf("no region: %v", err)
	}
}

func TestTargetURLNeedsClusterName(t *testing.T) {
	err := runPrometheus([]string{"--target", "https://prometheus-prod-13-prod-us-east-0.grafana.net/api/prom"})
	if err == nil || !strings.Contains(err.Error(), "--cluster-name is required") {
		t.Errorf("got %v", err)
	}
	err = runPrometheus([]string{"--target", "https://a.example", "--url", "https://b.example", "--cluster-name", "c"})
	if err == nil || !strings.Contains(err.Error(), "only one of --url") {
		t.Errorf("got %v", err)
	}
	err = runPrometheus([]string{"--target", "monitoring/prometheus:9090", "--tenant", "t", "--cluster-name", "c"})
	if err == nil || !strings.Contains(err.Error(), "apply only with --url") {
		t.Errorf("got %v", err)
	}
}
