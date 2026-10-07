package kube

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Kinds of ConfigError.
const (
	NoKubeconfig     = "no-kubeconfig"      // no kubeconfig file was found
	NoContexts       = "no-contexts"        // the kubeconfig has no contexts
	NoCurrentContext = "no-current-context" // no --context and the kubeconfig sets no current context
	UnknownContext   = "unknown-context"    // --context, or the current context, is not in the kubeconfig
)

// maxListed is how many context names a ConfigError lists.
const maxListed = 10

// ConfigError says why no cluster could be chosen from the kubeconfig and
// what to run instead.
type ConfigError struct {
	Kind     string
	Files    []string // kubeconfig files read, or looked for when none was found
	Source   string   // where Files came from: "--kubeconfig", "KUBECONFIG" or "default"
	Context  string   // the context asked for
	Current  bool     // Context is the kubeconfig's current context rather than --context
	Contexts []string // the contexts in the kubeconfig, sorted

	// Command is the command line that the example extends, such as
	// "savings-preview check". Kubeconfig is the --kubeconfig given.
	Command    string
	Kubeconfig string

	Err error // the error from client-go
}

func (e *ConfigError) Error() string {
	var b strings.Builder
	switch e.Kind {
	case NoKubeconfig:
		switch e.Source {
		case "--kubeconfig":
			fmt.Fprintf(&b, "no kubeconfig found: the file given with --kubeconfig (%s) does not exist.\n", e.Files[0])
			b.WriteString("Check the path, or set up access to the cluster first so that kubectl can reach it.")
			return b.String()
		case "KUBECONFIG":
			fmt.Fprintf(&b, "no kubeconfig found: none of the files in KUBECONFIG exist (%s).\n", strings.Join(e.Files, ", "))
		default:
			fmt.Fprintf(&b, "no kubeconfig found: %s does not exist and KUBECONFIG is not set.\n", strings.Join(e.Files, ", "))
		}
		b.WriteString("Pass the file with --kubeconfig, or set up access to the cluster first so that kubectl can reach it.")
	case NoContexts:
		fmt.Fprintf(&b, "the kubeconfig (%s) has no contexts.\n", strings.Join(e.Files, ", "))
		b.WriteString("Pass another file with --kubeconfig, or set up access to the cluster first so that kubectl can reach it.")
	case NoCurrentContext:
		b.WriteString("the kubeconfig sets no current context, so choose one with --context.\n")
		e.writeContexts(&b)
	case UnknownContext:
		if e.Current {
			fmt.Fprintf(&b, "the kubeconfig's current context %q does not exist, so choose one with --context.\n", e.Context)
		} else {
			fmt.Fprintf(&b, "the kubeconfig has no context %q.\n", e.Context)
		}
		e.writeContexts(&b)
	default:
		return "load kubeconfig: " + e.Err.Error()
	}
	return b.String()
}

func (e *ConfigError) writeContexts(b *strings.Builder) {
	names := e.Contexts
	more := ""
	if len(names) > maxListed {
		more = fmt.Sprintf(", and %d more", len(names)-maxListed)
		names = names[:maxListed]
	}
	fmt.Fprintf(b, "Contexts in %s: %s%s.\n", strings.Join(e.Files, ", "), strings.Join(names, ", "), more)
	fmt.Fprintf(b, "For example: %s", e.example())
}

func (e *ConfigError) example() string {
	cmd := e.Command
	if cmd == "" {
		cmd = "savings-preview"
	}
	if e.Kubeconfig != "" {
		cmd += " --kubeconfig " + shellQuote(e.Kubeconfig)
	}
	return cmd + " --context " + shellQuote(e.Contexts[0])
}

// Short is a one-line reason, for places that refer to the full message
// shown elsewhere.
func (e *ConfigError) Short() string {
	switch e.Kind {
	case NoKubeconfig:
		return "no kubeconfig found"
	case NoContexts:
		return "the kubeconfig has no contexts"
	case NoCurrentContext:
		return "the kubeconfig sets no current context and --context is not given"
	case UnknownContext:
		if e.Current {
			return fmt.Sprintf("the kubeconfig's current context %q does not exist", e.Context)
		}
		return fmt.Sprintf("the kubeconfig has no context %q", e.Context)
	}
	return "cannot read the kubeconfig"
}

func (e *ConfigError) Unwrap() error { return e.Err }

// WithCommand sets the command line that a ConfigError's example extends.
// Other errors are returned as they are.
func WithCommand(err error, command string) error {
	var ce *ConfigError
	if errors.As(err, &ce) {
		ce.Command = command
	}
	return err
}

// configError explains err, which client-go returned while loading the
// kubeconfig. It is called only after loading failed, so a kubeconfig that
// works, or the in-cluster config used when there is none, is never refused.
// raw is nil when the files could not be read.
func configError(rules *clientcmd.ClientConfigLoadingRules, kubeContext string, raw *clientcmdapi.Config, err error) *ConfigError {
	e := &ConfigError{Context: kubeContext, Kubeconfig: rules.ExplicitPath, Err: err}
	var looked []string
	switch {
	case rules.ExplicitPath != "":
		e.Source, looked = "--kubeconfig", []string{rules.ExplicitPath}
	case os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "":
		e.Source, looked = "KUBECONFIG", rules.Precedence
	default:
		e.Source, looked = "default", rules.Precedence
	}
	for _, f := range looked {
		if _, statErr := os.Stat(f); statErr == nil {
			e.Files = append(e.Files, f)
		}
	}
	if len(e.Files) == 0 {
		e.Kind, e.Files = NoKubeconfig, looked
		if len(looked) == 0 {
			e.Files = []string{filepath.Join(clientcmd.RecommendedConfigDir, clientcmd.RecommendedFileName)}
		}
		return e
	}
	if raw == nil {
		return e
	}
	for name := range raw.Contexts {
		e.Contexts = append(e.Contexts, name)
	}
	slices.Sort(e.Contexts)

	switch {
	case len(e.Contexts) == 0:
		e.Kind = NoContexts
	case kubeContext != "":
		if _, ok := raw.Contexts[kubeContext]; !ok {
			e.Kind = UnknownContext
		}
	case raw.CurrentContext == "":
		e.Kind = NoCurrentContext
	default:
		if _, ok := raw.Contexts[raw.CurrentContext]; !ok {
			e.Kind, e.Context, e.Current = UnknownContext, raw.CurrentContext, true
		}
	}
	return e
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
