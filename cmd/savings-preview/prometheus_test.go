package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in      string
		want    prometheus.Candidate
		wantErr bool
	}{
		{"mon/prom:9090", prometheus.Candidate{Namespace: "mon", Service: "prom", Port: "9090", Scheme: "http"}, false},
		{"https:mon/prom:web", prometheus.Candidate{Namespace: "mon", Service: "prom", Port: "web", Scheme: "https"}, false},
		{"vm/vmselect:8481/select/0/prometheus", prometheus.Candidate{Namespace: "vm", Service: "vmselect", Port: "8481", Scheme: "http", Prefix: "/select/0/prometheus"}, false},
		{"prom:9090", prometheus.Candidate{}, true},
		{"mon/prom", prometheus.Candidate{}, true},
		{"/prom:9090", prometheus.Candidate{}, true},
	}
	for _, c := range cases {
		got, err := parseTarget(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v", c.in, err)
			continue
		}
		if err == nil {
			got.Reason = ""
			if got != c.want {
				t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
			}
		}
	}
}

func cand(name string) prometheus.Candidate {
	return prometheus.Candidate{Kind: prometheus.KindPrometheus, Namespace: "mon", Service: name, Port: "9090"}
}

func TestPickStoreSkipsAgentModeWhenAnotherStoreWorks(t *testing.T) {
	agent := &prometheus.ErrAgentMode{RemoteWrite: []string{"https://mimir.example.com/push"}}
	var warned []string
	got, err := pickStore([]prometheus.Candidate{cand("agent"), cand("server")}, func(c prometheus.Candidate) error {
		if c.Service == "agent" {
			return agent
		}
		return nil
	}, func(m string) { warned = append(warned, m) })
	if err != nil || got.Service != "server" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(warned) != 1 {
		t.Errorf("the skipped candidate should be reported once, got %v", warned)
	}
}

func TestPickStoreReportsAgentModeWhenNothingElseWorks(t *testing.T) {
	agent := &prometheus.ErrAgentMode{RemoteWrite: []string{"https://mimir.example.com/push"}}
	_, err := pickStore([]prometheus.Candidate{cand("agent"), cand("down")}, func(c prometheus.Candidate) error {
		if c.Service == "agent" {
			return agent
		}
		return errors.New("connection refused")
	}, func(string) {})
	var am *prometheus.ErrAgentMode
	if !errors.As(err, &am) || !strings.Contains(err.Error(), "mimir.example.com") {
		t.Errorf("want the agent-mode error with its remote_write target, got %v", err)
	}
}

func TestPickStoreReturnsLastErrorWithoutAgents(t *testing.T) {
	_, err := pickStore([]prometheus.Candidate{cand("a"), cand("b")}, func(prometheus.Candidate) error {
		return errors.New("refused")
	}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAuthFromFiles(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	basic := filepath.Join(dir, "basic")
	bad := filepath.Join(dir, "bad")
	_ = os.WriteFile(tok, []byte("secret-token\n"), 0o600)
	_ = os.WriteFile(basic, []byte("user:p:ss\n"), 0o600)
	_ = os.WriteFile(bad, []byte("nocolon"), 0o600)

	a, err := authFromFiles(tok, "")
	if b, ok := a.(prometheus.Bearer); err != nil || !ok || b.Token != "secret-token" {
		t.Errorf("bearer: %v %v", a, err)
	}
	a, err = authFromFiles("", basic)
	if b, ok := a.(prometheus.Basic); err != nil || !ok || b.User != "user" || b.Password != "p:ss" {
		t.Errorf("basic: %v %v", a, err)
	}
	if _, err := authFromFiles("", bad); err == nil {
		t.Error("a basic-auth file without a colon must fail")
	}
	if _, err := authFromFiles(tok, basic); err == nil {
		t.Error("both credential files must be rejected")
	}
	if a, err := authFromFiles("", ""); a != nil || err != nil {
		t.Errorf("no files means no auth: %v %v", a, err)
	}
}
