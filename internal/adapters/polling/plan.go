package polling

import (
	"fmt"
	"path/filepath"

	"github.com/thoras-ai/savings-preview/internal/confirm"
)

// plan is what poll is about to do.
func plan(name, kubeContext string, o Options) confirm.Plan {
	title := "savings-preview " + name + ", the current kubeconfig context"
	if kubeContext != "" {
		title = "savings-preview " + name + ", context " + kubeContext
	}
	read := "read Pods, ReplicaSets, Deployments, StatefulSets, Nodes and metrics-server pod usage"
	switch {
	case o.Once:
		read += " once"
	case o.Duration > 0:
		read += fmt.Sprintf(" every %s for %s", o.Interval, o.Duration)
	default:
		read += fmt.Sprintf(" every %s until you press Ctrl-C", o.Interval)
	}
	dir, err := filepath.Abs(filepath.Dir(o.Output))
	if err != nil {
		dir = filepath.Dir(o.Output)
	}
	writes := fmt.Sprintf("It writes %s and keeps the readings in %s.readings, in %s.", filepath.Base(o.Output), filepath.Base(o.Output), dir)
	if o.Once {
		writes = fmt.Sprintf("It writes %s, in %s.", filepath.Base(o.Output), dir)
	}
	return confirm.Plan{Title: title, Reads: []string{read}, Writes: writes}
}
