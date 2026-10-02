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
	err = runPrometheus([]string{"--url", "http://prom.example", "--aws-region", "us-east-1", "--cluster-name", "c"})
	if err == nil || !strings.Contains(err.Error(), "--aws-region applies") {
		t.Errorf("got %v", err)
	}
	err = runPrometheus([]string{"--target", "monitoring/prometheus:9090", "--tenant", "t", "--cluster-name", "c"})
	if err == nil || !strings.Contains(err.Error(), "apply only with --url") {
		t.Errorf("got %v", err)
	}
}

func TestDirectHintForGateway(t *testing.T) {
	err := directHint(&prometheus.APIError{Status: 401, Message: "<html>401 Authorization Required</html>"}, prometheus.Backend{Name: prometheus.BackendMimir})
	if err == nil || !strings.Contains(err.Error(), "--basic-auth-file") {
		t.Errorf("got %v", err)
	}
}

func TestDirectFlagsGMP(t *testing.T) {
	const base = "https://monitoring.googleapis.com/v1/projects/shop-prod/location/global/prometheus"
	d := &directFlags{project: "shop-prod"}
	target := ""
	if err := d.check(&target); err != nil || d.url != base || d.backend != prometheus.BackendGMP {
		t.Errorf("--project: %+v, %v", d, err)
	}
	d = &directFlags{project: "shop-prod", url: base + "/api/v1/query"}
	if err := d.check(&target); err != nil || d.backend != prometheus.BackendGMP {
		t.Errorf("--project with a matching --url: %+v, %v", d, err)
	}
	for _, c := range []struct {
		d   directFlags
		err string
	}{
		{directFlags{project: "other", url: base}, "does not match"},
		{directFlags{project: "shop-prod", backend: prometheus.BackendAMP}, "--project applies to Google Managed Prometheus"},
		{directFlags{project: "Shop Prod"}, "not a Google Cloud project ID"},
		{directFlags{backend: prometheus.BackendGMP}, "needs --project or --url"},
		{directFlags{googleCreds: "key.json"}, "apply only with --url"},
		{directFlags{project: "shop-prod", gkeMetrics: "kube"}, "--gke-metrics must be one of"},
	} {
		if err := c.d.check(&target); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: got %v, want %q", c.d, err, c.err)
		}
	}
	d = &directFlags{url: "http://prom.example", gkeMetrics: prometheus.GKESystem}
	if err := d.check(&target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.open(context.Background()); err == nil || !strings.Contains(err.Error(), "apply to Google Managed Prometheus") {
		t.Errorf("--gke-metrics with another store: %v", err)
	}
	if got := (&directFlags{}).gkeMode(prometheus.Backend{Name: prometheus.BackendGMP}); got != prometheus.GKEAuto {
		t.Errorf("default mode %q", got)
	}
	if got := (&directFlags{gkeMetrics: prometheus.GKESystem}).gkeMode(prometheus.Backend{Name: prometheus.BackendMimir}); got != "" {
		t.Errorf("mode for Mimir %q", got)
	}
}

func TestDirectAuthGMP(t *testing.T) {
	t.Setenv(envBearer, "")
	t.Setenv(envBasic, "")
	t.Setenv(envHeaders, "")
	var gotFile string
	stub := prometheus.Bearer{Token: "stub"}
	old := newGoogle
	newGoogle = func(_ context.Context, file string) (prometheus.Auth, error) { gotFile = file; return stub, nil }
	t.Cleanup(func() { newGoogle = old })

	b := backend(t, "https://monitoring.googleapis.com/v1/projects/shop-prod/location/global/prometheus", "", false)
	got, err := directAuth(context.Background(), b, credentialFiles{google: "key.json"}, "")
	if err != nil || got != stub || gotFile != "key.json" {
		t.Errorf("got %#v, %v, file %q", got, err, gotFile)
	}
	t.Setenv(envHeaders, "X-Goog-User-Project: billing-proj")
	got, err = directAuth(context.Background(), b, credentialFiles{}, "")
	want := prometheus.Chain{prometheus.Header{Name: "X-Goog-User-Project", Value: "billing-proj"}, stub}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("headers: %#v, %v", got, err)
	}
	t.Setenv(envHeaders, "")

	// A token given directly, as from gcloud auth print-access-token, is
	// used without loading Google credentials.
	gotFile = "unset"
	t.Setenv(envBearer, "ya29.from-gcloud")
	got, err = directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil || got != (prometheus.Bearer{Token: "ya29.from-gcloud"}) || gotFile != "unset" {
		t.Errorf("bearer with GMP: %#v, %v, file %q", got, err, gotFile)
	}
	if _, err := directAuth(context.Background(), b, credentialFiles{google: "key.json"}, ""); err == nil || !strings.Contains(err.Error(), "only one of --google-credentials-file") {
		t.Errorf("bearer and key file: %v", err)
	}
	t.Setenv(envBearer, "")
	t.Setenv(envHeaders, "authorization: Bearer ya29.header")
	got, err = directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil || got != (prometheus.Header{Name: "authorization", Value: "Bearer ya29.header"}) || gotFile != "unset" {
		t.Errorf("Authorization header with GMP: %#v, %v, file %q", got, err, gotFile)
	}
	t.Setenv(envHeaders, "")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, "team-a"); err == nil || !strings.Contains(err.Error(), "--tenant") {
		t.Errorf("tenant with GMP: %v", err)
	}
}

func TestDirectHintForGMP(t *testing.T) {
	store := prometheus.Backend{Name: prometheus.BackendGMP, Project: "shop-prod"}
	err := directHint(&prometheus.APIError{Status: 403, Message: "Permission monitoring.timeSeries.list denied"}, store)
	if err == nil || !strings.Contains(err.Error(), "roles/monitoring.viewer") || !strings.Contains(err.Error(), "shop-prod") {
		t.Errorf("403: %v", err)
	}
	err = directHint(&prometheus.APIError{Status: 404, Message: "not found"}, store)
	if err == nil || !strings.Contains(err.Error(), "Cloud Monitoring API") {
		t.Errorf("404: %v", err)
	}
}

func TestDirectAuthAzure(t *testing.T) {
	t.Setenv(envBearer, "")
	t.Setenv(envBasic, "")
	t.Setenv(envHeaders, "")
	t.Setenv(envAzureTenant, "")
	t.Setenv(envAzureClient, "")
	t.Setenv(envAzureSecret, "")
	var got prometheus.AzureCredentials
	var gotAudience string
	stub := prometheus.Bearer{Token: "stub"}
	old := newAzure
	newAzure = func(_ context.Context, audience string, c prometheus.AzureCredentials) (prometheus.Auth, error) {
		gotAudience, got = audience, c
		return stub, nil
	}
	t.Cleanup(func() { newAzure = old })
	b := backend(t, "https://ws-1.usgovvirginia.prometheus.monitor.azure.us", "", false)

	// Without an app registration, the Azure CLI login is used.
	a, err := directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil || a != stub || gotAudience != "https://prometheus.monitor.azure.us" || !reflect.DeepEqual(got, (prometheus.AzureCredentials{})) {
		t.Errorf("CLI: %#v, %v, %q %+v", a, err, gotAudience, got)
	}
	// AZURE_CLIENT_ID alone names a managed identity to the Azure SDKs and
	// is not an app registration here.
	t.Setenv(envAzureClient, "mi-client")
	t.Setenv(envAzureTenant, "contoso.onmicrosoft.com")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, ""); err != nil || !reflect.DeepEqual(got, (prometheus.AzureCredentials{TenantID: "contoso.onmicrosoft.com"})) {
		t.Errorf("client ID without a secret: %+v, %v", got, err)
	}
	t.Setenv(envAzureSecret, "env-secret")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, ""); err != nil ||
		!reflect.DeepEqual(got, (prometheus.AzureCredentials{TenantID: "contoso.onmicrosoft.com", ClientID: "mi-client", ClientSecret: "env-secret"})) {
		t.Errorf("from the environment: %+v, %v", got, err)
	}
	// Flags and the secret file take precedence over the environment.
	_, err = directAuth(context.Background(), b, credentialFiles{azureTenant: "t2", azureClientID: "app-2", azureSecret: writeFile(t, "file-secret\n")}, "")
	if err != nil || !reflect.DeepEqual(got, (prometheus.AzureCredentials{TenantID: "t2", ClientID: "app-2", ClientSecret: "file-secret"})) {
		t.Errorf("from flags: %+v, %v", got, err)
	}
	t.Setenv(envAzureSecret, "")
	t.Setenv(envAzureClient, "")
	t.Setenv(envAzureTenant, "")

	// A token given directly, as from az account get-access-token, is used
	// as is.
	got = prometheus.AzureCredentials{ClientID: "unset"}
	t.Setenv(envBearer, "eyJ.from-az")
	a, err = directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil || a != (prometheus.Bearer{Token: "eyJ.from-az"}) || got.ClientID != "unset" {
		t.Errorf("bearer: %#v, %v", a, err)
	}
	for _, files := range []credentialFiles{{azureClientID: "app"}, {azureTenant: "t"}} {
		if _, err := directAuth(context.Background(), b, files, ""); err == nil || !strings.Contains(err.Error(), "only one of") {
			t.Errorf("bearer and %+v: %v", files, err)
		}
	}
	t.Setenv(envBearer, "")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, "team-a"); err == nil || !strings.Contains(err.Error(), "--tenant applies to Mimir") {
		t.Errorf("tenant with Azure: %v", err)
	}
}

func TestDirectFlagsAzure(t *testing.T) {
	target := ""
	d := &directFlags{azureClientID: "app"}
	if err := d.check(&target); err == nil || !strings.Contains(err.Error(), "apply only with --url") {
		t.Errorf("--azure-client-id without --url: %v", err)
	}
	d = &directFlags{url: "http://prom.example", azureTenant: "t"}
	if err := d.check(&target); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.open(context.Background()); err == nil || !strings.Contains(err.Error(), "apply to Azure Monitor managed Prometheus") {
		t.Errorf("--azure-tenant-id with another store: %v", err)
	}
}

func TestDirectHintForAzure(t *testing.T) {
	store := backend(t, "https://ws-1.eastus.prometheus.monitor.azure.com", "", false)
	err := directHint(&prometheus.APIError{Status: 403, Message: "Forbidden"}, store)
	if err == nil || !strings.Contains(err.Error(), "Monitoring Data Reader") {
		t.Errorf("403: %v", err)
	}
}

func TestDirectAuthCloudWatch(t *testing.T) {
	t.Setenv(envHeaders, "")
	t.Setenv(envBasic, "")
	t.Setenv(envBearer, "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	b := backend(t, "https://monitoring.eu-west-1.amazonaws.com", "", false)
	got, err := directAuth(context.Background(), b, credentialFiles{}, "")
	if err != nil {
		t.Fatal(err)
	}
	chain, ok := got.(prometheus.Chain)
	if !ok || len(chain) != 1 {
		t.Fatalf("got %#v", got)
	}
	if s, ok := chain[0].(*prometheus.SigV4); !ok || s.Region != "eu-west-1" || s.Service != prometheus.SigV4CloudWatch {
		t.Errorf("got %#v", chain[0])
	}
	t.Setenv(envBearer, "tok")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, ""); err == nil || !strings.Contains(err.Error(), "CloudWatch uses AWS credentials") {
		t.Errorf("bearer with CloudWatch: %v", err)
	}
	t.Setenv(envBearer, "")
	if _, err := directAuth(context.Background(), b, credentialFiles{}, "team-a"); err == nil || !strings.Contains(err.Error(), "--tenant applies to Mimir") {
		t.Errorf("tenant with CloudWatch: %v", err)
	}
	nr := backend(t, "https://cw-proxy.internal", prometheus.BackendCloudWatch, false)
	if _, err := directAuth(context.Background(), nr, credentialFiles{}, ""); err == nil || !strings.Contains(err.Error(), "--aws-region") {
		t.Errorf("no region: %v", err)
	}
}

func TestDirectFlagsCloudWatch(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	target := ""
	d := &directFlags{backend: prometheus.BackendCloudWatch, awsRegion: "us-west-2"}
	if err := d.check(&target); err != nil || d.url != "https://monitoring.us-west-2.amazonaws.com" {
		t.Errorf("--backend cloudwatch --aws-region: %+v, %v", d, err)
	}
	_, store, err := d.open(context.Background())
	if err != nil || store.Name != prometheus.BackendCloudWatch || store.Region != "us-west-2" || store.MaxSeries != prometheus.CloudWatchMaxSeries {
		t.Errorf("open: %+v, %v", store, err)
	}
	if got := cloudWatchRegion(store); got != "us-west-2" {
		t.Errorf("region for the adapter %q", got)
	}
	if c := store.NewClient(nil, ""); !reflect.DeepEqual(c.ClusterLabels, prometheus.CloudWatchClusterLabels) || c.MaxRange != prometheus.CloudWatchMaxRange {
		t.Errorf("client %+v", c)
	}
	d = &directFlags{url: "http://localhost:9090", backend: prometheus.BackendCloudWatch, awsRegion: "us-east-2"}
	if err := d.check(&target); err != nil {
		t.Fatal(err)
	}
	if _, store, err := d.open(context.Background()); err != nil || store.Region != "us-east-2" || store.Base != "http://localhost:9090" {
		t.Errorf("--url with --aws-region: %+v, %v", store, err)
	}
	for _, c := range []struct {
		d   directFlags
		err string
	}{
		{directFlags{backend: prometheus.BackendCloudWatch}, "needs --aws-region or --url"},
		{directFlags{backend: prometheus.BackendCloudWatch, awsRegion: "virginia"}, "not an AWS region"},
		{directFlags{awsRegion: "us-east-1"}, "apply only with --url"},
	} {
		if err := c.d.check(&target); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: got %v, want %q", c.d, err, c.err)
		}
	}
	if got := cloudWatchRegion(backend(t, "https://aps-workspaces.us-east-1.amazonaws.com/workspaces/ws-1", "", false)); got != "" {
		t.Errorf("region for AMP %q", got)
	}
}

func TestDirectHintForCloudWatch(t *testing.T) {
	store := backend(t, "https://monitoring.us-east-1.amazonaws.com", "", false)
	err := directHint(&prometheus.APIError{Status: 403, Message: "AccessDeniedException"}, store)
	if err == nil || !strings.Contains(err.Error(), "cloudwatch:GetMetricData and cloudwatch:ListMetrics") {
		t.Errorf("403: %v", err)
	}
	err = directHint(&prometheus.APIError{Status: 422, Message: "too many samples"}, store)
	if err == nil || !strings.Contains(err.Error(), "--step") {
		t.Errorf("422: %v", err)
	}
	err = directHint(&prometheus.APIError{Status: 404, Message: "not found"}, store)
	if err == nil || !strings.Contains(err.Error(), "monitoring.REGION.amazonaws.com") {
		t.Errorf("404: %v", err)
	}
}
