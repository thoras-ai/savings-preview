package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// kubeconfig writes a kubeconfig with the given contexts, all on one
// unreachable cluster, and returns its path.
func kubeconfig(t *testing.T, current string, contexts ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\n")
	if current != "" {
		fmt.Fprintf(&b, "current-context: %q\n", current)
	}
	b.WriteString("clusters:\n- name: c\n  cluster: {server: https://127.0.0.1:1}\nusers:\n- name: u\n  user: {token: t}\ncontexts:\n")
	for _, c := range contexts {
		fmt.Fprintf(&b, "- name: %q\n  context: {cluster: c, user: u}\n", c)
	}
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// isolate points KUBECONFIG and the default kubeconfig into an empty
// directory, so the machine's own kubeconfig is not read.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	home := clientcmd.RecommendedHomeFile
	clientcmd.RecommendedHomeFile = filepath.Join(dir, ".kube", "config")
	t.Cleanup(func() { clientcmd.RecommendedHomeFile = home })
	return dir
}

func connectErr(t *testing.T, path, kubeContext, command string) *ConfigError {
	t.Helper()
	_, err := Connect(path, kubeContext)
	err = WithCommand(err, command)
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a ConfigError", err)
	}
	return ce
}

func TestConnectNoCurrentContext(t *testing.T) {
	isolate(t)
	path := kubeconfig(t, "", "prod", "dev")
	t.Setenv("KUBECONFIG", path)
	ce := connectErr(t, "", "", "savings-preview check")
	if ce.Kind != NoCurrentContext {
		t.Fatalf("kind %q: %v", ce.Kind, ce)
	}
	want := "the kubeconfig sets no current context, so choose one with --context.\n" +
		"Contexts in " + path + ": dev, prod.\n" +
		"For example: savings-preview check --context dev"
	if ce.Error() != want {
		t.Errorf("got\n%s\nwant\n%s", ce.Error(), want)
	}
	if strings.Contains(ce.Error(), "KUBERNETES_MASTER") {
		t.Error("client-go's advice leaked into the message")
	}
}

func TestConnectNoCurrentContextListsTen(t *testing.T) {
	isolate(t)
	var names []string
	for i := range 13 {
		names = append(names, fmt.Sprintf("ctx-%02d", i))
	}
	path := kubeconfig(t, "", names...)
	msg := connectErr(t, path, "", "").Error()
	if !strings.Contains(msg, "ctx-09, and 3 more.") || strings.Contains(msg, "ctx-10") {
		t.Errorf("message:\n%s", msg)
	}
	if !strings.HasSuffix(msg, "savings-preview --kubeconfig "+path+" --context ctx-00") {
		t.Errorf("the example does not keep --kubeconfig:\n%s", msg)
	}
}

func TestConnectUnknownContext(t *testing.T) {
	isolate(t)
	path := kubeconfig(t, "dev", "dev", "prod")
	ce := connectErr(t, path, "staging", "savings-preview poll")
	if ce.Kind != UnknownContext || ce.Current {
		t.Fatalf("kind %q current %v", ce.Kind, ce.Current)
	}
	for _, s := range []string{`no context "staging"`, ": dev, prod.", "savings-preview poll --kubeconfig"} {
		if !strings.Contains(ce.Error(), s) {
			t.Errorf("message lacks %q:\n%s", s, ce.Error())
		}
	}
}

func TestConnectCurrentContextMissing(t *testing.T) {
	isolate(t)
	ce := connectErr(t, kubeconfig(t, "gone", "dev"), "", "")
	if ce.Kind != UnknownContext || !ce.Current || !strings.Contains(ce.Error(), `current context "gone" does not exist`) {
		t.Fatalf("kind %q: %v", ce.Kind, ce)
	}
}

func TestConnectNoKubeconfig(t *testing.T) {
	dir := isolate(t)
	t.Run("default", func(t *testing.T) {
		ce := connectErr(t, "", "", "")
		if ce.Kind != NoKubeconfig || !strings.Contains(ce.Error(), filepath.Join(dir, ".kube", "config")+" does not exist") {
			t.Fatalf("kind %q: %v", ce.Kind, ce)
		}
	})
	t.Run("KUBECONFIG", func(t *testing.T) {
		a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
		t.Setenv("KUBECONFIG", a+string(os.PathListSeparator)+b)
		ce := connectErr(t, "", "", "")
		if ce.Kind != NoKubeconfig || !strings.Contains(ce.Error(), "none of the files in KUBECONFIG exist ("+a+", "+b+")") {
			t.Fatalf("kind %q: %v", ce.Kind, ce)
		}
	})
	t.Run("--kubeconfig", func(t *testing.T) {
		p := filepath.Join(dir, "missing")
		ce := connectErr(t, p, "dev", "")
		if ce.Kind != NoKubeconfig || !strings.Contains(ce.Error(), "--kubeconfig ("+p+") does not exist") {
			t.Fatalf("kind %q: %v", ce.Kind, ce)
		}
	})
}

func TestConnectNoContexts(t *testing.T) {
	isolate(t)
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ce := connectErr(t, p, "", ""); ce.Kind != NoContexts {
		t.Fatalf("kind %q: %v", ce.Kind, ce)
	}
}

func TestConnectUnreadableKubeconfigKeepsClientGoMessage(t *testing.T) {
	isolate(t)
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ce := connectErr(t, p, "", "")
	if ce.Kind != "" || !strings.HasPrefix(ce.Error(), "load kubeconfig: ") || ce.Short() != "cannot read the kubeconfig" {
		t.Fatalf("kind %q: %v", ce.Kind, ce)
	}
}

func TestConnectWithContext(t *testing.T) {
	isolate(t)
	c, err := Connect(kubeconfig(t, "", "dev", "prod"), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "prod" || c.ClusterName != "c" || c.User != "u" {
		t.Errorf("got %+v", c)
	}
}

func TestExampleQuotesContext(t *testing.T) {
	isolate(t)
	msg := connectErr(t, kubeconfig(t, "", "team a's cluster"), "", "savings-preview").Error()
	if !strings.HasSuffix(msg, `--context 'team a'\''s cluster'`) {
		t.Errorf("message:\n%s", msg)
	}
}
