package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thoras-ai/savings-preview/internal/adapters/prometheus"
	"github.com/thoras-ai/savings-preview/internal/contract"
)

// fallbackError says why the store could not be used, so the run moves on to
// polling.
type fallbackError struct{ reason string }

func (e *fallbackError) Error() string { return e.reason }

func fallback(format string, a ...any) error { return &fallbackError{fmt.Sprintf(format, a...)} }

// storeSteps are the cluster calls tryStore makes; tests replace them.
type storeSteps struct {
	discover func(context.Context) (*prometheus.Discovery, error)
	check    func(context.Context, prometheus.Candidate) error
	pull     func(context.Context, prometheus.Candidate) (*contract.File, *prometheus.Report, error)
}

type storeResult struct {
	store  prometheus.Candidate
	file   *contract.File
	report *prometheus.Report
}

// tryStore finds a store and pulls the history from it, trying each candidate
// in turn. It returns a *fallbackError when the run should poll instead, and
// the context's error when the user stopped the run. A store named with
// target is never a fallback: its failure is an error.
func tryStore(ctx context.Context, s storeSteps, target *prometheus.Candidate, warn func(string)) (*storeResult, error) {
	cands := []prometheus.Candidate{}
	if target != nil {
		cands = append(cands, *target)
	} else {
		d, err := s.discover(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fallback("could not look for a store: %v", err)
		}
		if len(d.Candidates) == 0 {
			if len(d.Agents) > 0 {
				a := d.Agents[0]
				return nil, fallback("the only Prometheus found (%s/%s) runs in agent mode and keeps no history to query; its data goes to %s",
					a.Namespace, a.Name, strings.Join(a.RemoteWrite, ", "))
			}
			return nil, fallback("no Prometheus-compatible store found in the cluster")
		}
		cands = d.Candidates
	}
	var reasons []string
	for _, c := range cands {
		res, err := useStore(ctx, s, c)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if target != nil {
			return nil, err
		}
		reasons = append(reasons, err.Error())
		if len(cands) > 1 {
			warn("skipping " + err.Error())
		}
	}
	return nil, fallback("%s", strings.Join(reasons, "; "))
}

func useStore(ctx context.Context, s storeSteps, c prometheus.Candidate) (*storeResult, error) {
	if err := s.check(ctx, c); err != nil {
		return nil, fmt.Errorf("%s does not answer queries: %w", c, err)
	}
	f, rep, err := s.pull(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be used: %w", c, err)
	}
	if !anyPriced(f) {
		return nil, fmt.Errorf("%s has no container with all of %s, so no savings can be computed from it",
			c, strings.Join(contract.RequiredContainerMetrics, ", "))
	}
	return &storeResult{store: c, file: f, report: rep}, nil
}

// anyPriced reports whether some container has every series the savings math
// needs.
func anyPriced(f *contract.File) bool {
	for _, r := range f.Resources {
		if r.Container != "" && len(r.Missing) == 0 {
			return true
		}
	}
	return false
}

func isFallback(err error) bool {
	var fe *fallbackError
	return errors.As(err, &fe)
}
