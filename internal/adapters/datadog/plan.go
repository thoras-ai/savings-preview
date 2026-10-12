package datadog

import (
	"fmt"
	"path/filepath"

	"github.com/thoras-ai/savings-preview/internal/confirm"
)

// plan is what the datadog command is about to do.
func plan(base, selector, out string, days int, list bool) confirm.Plan {
	p := confirm.Plan{Title: "savings-preview datadog, the Datadog API at " + base}
	if list {
		p.Reads = []string{"query the Datadog metrics API with the keys given for the metrics and clusters it holds (read-only queries)"}
		p.After = "Then it prints what it found and exits."
		return p
	}
	what := "container usage, requests, replica counts and node allocatable"
	if selector != "" {
		what += " for " + selector
	}
	n := fmt.Sprintf("%d days", days)
	if days == 1 {
		n = "1 day"
	}
	p.Reads = []string{fmt.Sprintf("query the Datadog metrics API with the keys given for %s of %s (read-only queries)", n, what)}
	dir, err := filepath.Abs(filepath.Dir(out))
	if err != nil {
		dir = filepath.Dir(out)
	}
	p.Writes = fmt.Sprintf("It writes %s, in %s.", filepath.Base(out), dir)
	return p
}
