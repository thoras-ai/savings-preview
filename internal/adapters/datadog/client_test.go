package datadog

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveSite(t *testing.T) {
	cases := map[string]string{
		"":                         "https://api.datadoghq.com",
		"us1":                      "https://api.datadoghq.com",
		"US3":                      "https://api.us3.datadoghq.com",
		"us5":                      "https://api.us5.datadoghq.com",
		"eu1":                      "https://api.datadoghq.eu",
		"eu":                       "https://api.datadoghq.eu",
		"ap1":                      "https://api.ap1.datadoghq.com",
		"ap2":                      "https://api.ap2.datadoghq.com",
		"us1-fed":                  "https://api.ddog-gov.com",
		"datadoghq.eu":             "https://api.datadoghq.eu",
		"us3.datadoghq.com":        "https://api.us3.datadoghq.com",
		"app.datadoghq.com":        "https://api.datadoghq.com",
		"https://dd-proxy.local/":  "https://dd-proxy.local",
		"http://127.0.0.1:8080/dd": "http://127.0.0.1:8080/dd",
	}
	for in, want := range cases {
		got, err := ResolveSite(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"us4", "example.com", "ftp://x", "https://user:pw@x", "https://x/?k=1"} {
		if _, err := ResolveSite(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestParseSelector(t *testing.T) {
	got, err := ParseSelector(" {kube_cluster_name:prod-us, env:prod,!team:data*} ")
	if err != nil || strings.Join(got, ",") != "kube_cluster_name:prod-us,env:prod,!team:data*" {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"prod-us", "kube_cluster_name:a} by {x", "a:b OR c:d", "a:(b)", ":b", "a:"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestTimeseriesDecodes(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.Header.Get("DD-API-KEY") != "k1" || r.Header.Get("DD-APPLICATION-KEY") != "k2" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		// Times in seconds, a null, and a unit pair whose second entry is null.
		io.WriteString(w, `{"data":{"type":"timeseries_response","attributes":{
			"series":[{"group_tags":["kube_namespace:a","kube_container_name:c"],"query_index":1,"unit":[{"name":"nanocore"},null]}],
			"times":[1790000040,1790000100,1790000160],"values":[[1.5,null,2.5]]}}}`)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, APIKey: "k1", AppKey: "k2"}
	ss, spacing, err := c.Timeseries(context.Background(), time.Unix(1790000040, 0), time.Unix(1790000220, 0), time.Minute,
		[]Query{{Name: "a", Query: "x"}, {Name: "b", Query: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"from":1790000040000,"to":1790000220000,"interval":60000`) ||
		!strings.Contains(gotBody, `{"data_source":"metrics","name":"b","query":"y"}`) {
		t.Errorf("request body %s", gotBody)
	}
	if spacing != time.Minute || len(ss) != 1 {
		t.Fatalf("spacing %v, series %v", spacing, ss)
	}
	s := ss[0]
	if s.Query != 1 || s.Tags["kube_namespace"] != "a" || s.Unit != "nanocore" || len(s.Points) != 2 || s.Points[1] != (Point{1790000160, 2.5}) {
		t.Errorf("series %+v", s)
	}
}

func TestTimeseriesErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"errors":["Error parsing query"]}`, "Datadog returned 400: Error parsing query"},
		{403, `{"errors":["Forbidden"]}`, "Datadog returned 403: Forbidden"},
		{200, `{"data":{"attributes":{"series":[],"times":[],"values":[]}},"errors":"metric not allowed"}`, "metric not allowed"},
	}
	for _, c := range cases {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		cl := &Client{Base: srv.URL, APIKey: "secret-api", AppKey: "secret-app"}
		_, _, err := cl.Timeseries(context.Background(), time.Unix(0, 0), time.Unix(60, 0), time.Minute, []Query{{Name: "a", Query: "x"}})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d: got %v", c.status, err)
		}
		if calls != 1 {
			t.Errorf("%d: %d calls; client errors are not retried", c.status, calls)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("error shows a key: %v", err)
		}
	}
}

func TestRetriesServerErrorsWithBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	c, ns := testClient(srv.URL)
	if err := c.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ns.waits) != 2 || ns.waits[0] != time.Second || ns.waits[1] != 2*time.Second {
		t.Errorf("waits %v", ns.waits)
	}

	c, _ = testClient(srv.URL)
	c.Retries = 1
	atomic.StoreInt32(&calls, -10)
	var ae *APIError
	if err := c.Validate(context.Background()); !errors.As(err, &ae) || ae.Status != http.StatusBadGateway {
		t.Errorf("after the retries: %v", err)
	}
}

func TestPausesWhenTheWindowIsUsedUp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("X-RateLimit-Reset", "7")
		w.Header().Set("X-RateLimit-Remaining", map[bool]string{true: "0", false: "5"}[n == 1])
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	c, ns := testClient(srv.URL)
	for i := 0; i < 3; i++ {
		if err := c.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Only the request after the one that used up the window waits.
	if len(ns.waits) != 1 || ns.waits[0] != 8*time.Second {
		t.Errorf("waits %v", ns.waits)
	}
}

func TestRateLimitWithoutHeadersBacksOff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	c, ns := testClient(srv.URL)
	if err := c.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ns.waits) != 1 || ns.waits[0] != time.Second {
		t.Errorf("waits %v", ns.waits)
	}
}

func TestCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(" from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAppKey, "")
	t.Setenv(EnvAppKeyAlt, "from-alt")
	if v, err := credential("", EnvAppKey, EnvAppKeyAlt); err != nil || v != "from-alt" {
		t.Errorf("env: %q, %v", v, err)
	}
	t.Setenv(EnvAppKey, "from-env")
	if v, _ := credential("", EnvAppKey, EnvAppKeyAlt); v != "from-env" {
		t.Errorf("%s takes precedence: %q", EnvAppKey, v)
	}
	if v, err := credential(path, EnvAppKey); err != nil || v != "from-file" {
		t.Errorf("file: %q, %v", v, err)
	}
	if _, err := credential(filepath.Join(dir, "none"), EnvAppKey); err == nil {
		t.Error("a missing file is an error")
	}
}

func TestMainNeedsBothKeys(t *testing.T) {
	t.Setenv(EnvAPIKey, "a")
	t.Setenv(EnvAppKey, "")
	t.Setenv(EnvAppKeyAlt, "")
	err := Main([]string{"--site", "us1"})
	if err == nil || !strings.Contains(err.Error(), "application key") {
		t.Errorf("got %v", err)
	}
	for _, args := range [][]string{{"--step", "90s"}, {"--days", "0"}, {"--selector", "prod"}, {"--site", "nowhere"}, {"extra"}} {
		if err := Main(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestRateLimitWaitsDoNotUseUpRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 10 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	c, ns := testClient(srv.URL)
	c.Retries = 1
	if err := c.Validate(context.Background()); err != nil {
		t.Fatalf("ten rate-limit waits with one retry: %v", err)
	}
	if len(ns.waits) != 10 {
		t.Errorf("waits %v", ns.waits)
	}
}

func TestWaitTurnHonorsAnExtendedPause(t *testing.T) {
	c, ns := testClient("http://unused")
	c.pauseUntil = ns.now.Add(5 * time.Second)
	extended := false
	c.sleep = func(_ context.Context, d time.Duration) error {
		ns.waits = append(ns.waits, d)
		ns.now = ns.now.Add(d)
		if !extended {
			// Another request pushes the pause out while this one sleeps.
			extended = true
			c.mu.Lock()
			c.pauseUntil = ns.now.Add(3 * time.Second)
			c.mu.Unlock()
		}
		return nil
	}
	if err := c.waitTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ns.waits) != 2 || ns.waits[1] != 3*time.Second {
		t.Errorf("waits %v", ns.waits)
	}
}
