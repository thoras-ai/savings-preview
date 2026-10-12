package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLatestTagReadsTheRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s, want HEAD", r.Method)
		}
		http.Redirect(w, r, "https://github.com/thoras-ai/savings-preview/releases/tag/v0.2.6", http.StatusFound)
	}))
	defer srv.Close()
	tag, err := latestTag(context.Background(), http.DefaultTransport, srv.URL)
	if err != nil || tag != "v0.2.6" {
		t.Fatalf("latestTag = %q, %v; want v0.2.6", tag, err)
	}
}

func TestLatestTagFailsWithoutARedirectToATag(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"ok":       func(w http.ResponseWriter, r *http.Request) {},
		"not tag":  func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) },
		"error":    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"rc":       func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/tag/v0.3.0-rc.1", http.StatusFound) },
		"no slash": func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", "x"); w.WriteHeader(http.StatusFound) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if tag, err := latestTag(context.Background(), http.DefaultTransport, srv.URL); err == nil {
				t.Fatalf("latestTag = %q, want an error", tag)
			}
		})
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.6", "v0.2.5", true},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.0", true},
		{"v0.2.5", "v0.2.5", false},
		{"v0.2.4", "v0.2.5", false},
		{"v0.3.0-rc.1", "v0.2.5", false},
		{"v0.2.6", "dev-abc123", false},
		{"", "v0.2.5", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestStartUpdateCheckSkips(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, u := range map[string]*updateCheck{
		"not a terminal": startUpdateCheck("v0.2.5", false, env(nil)),
		"turned off":     startUpdateCheck("v0.2.5", true, env(map[string]string{noUpdateCheckEnv: "1"})),
		"dev build":      startUpdateCheck("dev-abc123", true, env(nil)),
		"prerelease":     startUpdateCheck("v0.3.0-rc.1", true, env(nil)),
	} {
		if u != nil {
			t.Errorf("%s: started a check", name)
		}
	}
}

func finished(current, latest string) *updateCheck {
	u := &updateCheck{current: current, latest: latest, done: make(chan struct{})}
	close(u.done)
	return u
}

func TestReportPrintsOnlyForANewerRelease(t *testing.T) {
	var b bytes.Buffer
	finished("v0.2.5", "v0.2.6").report(&b, 0, "brew upgrade savings-preview")
	if got := b.String(); !strings.Contains(got, "savings-preview v0.2.6 is available; this is v0.2.5. To upgrade: brew upgrade savings-preview") {
		t.Errorf("report = %q", got)
	}
	for _, latest := range []string{"v0.2.5", "v0.2.4", ""} {
		b.Reset()
		finished("v0.2.5", latest).report(&b, 0, "x")
		if b.Len() != 0 {
			t.Errorf("latest %q: report = %q, want nothing", latest, b.String())
		}
	}
}

func TestReportGivesUpOnALookupStillRunning(t *testing.T) {
	var b bytes.Buffer
	u := &updateCheck{current: "v0.2.5", latest: "v0.2.6", done: make(chan struct{})}
	u.report(&b, 0, "x")
	if b.Len() != 0 {
		t.Errorf("report = %q, want nothing", b.String())
	}
	var none *updateCheck
	none.report(&b, 0, "x")
}

func TestUpgradeHint(t *testing.T) {
	noEnv := func(string) string { return "" }
	if h := upgradeHint("/usr/local/bin/savings-preview", func(k string) string {
		if k == viaNpxEnv {
			return "1"
		}
		return ""
	}); !strings.Contains(h, "npx @thoras-ai/savings-preview@latest") {
		t.Errorf("npm hint = %q", h)
	}
	dir := t.TempDir()
	cellar := filepath.Join(dir, "Cellar", "savings-preview", "0.2.5", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(cellar, "savings-preview")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "savings-preview")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if h := upgradeHint(link, noEnv); h != "brew upgrade savings-preview" {
		t.Errorf("brew hint = %q", h)
	}
	if h := upgradeHint(filepath.Join(dir, "elsewhere"), noEnv); !strings.Contains(h, "install.sh | sh") {
		t.Errorf("install.sh hint = %q", h)
	}
}
