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

// tryStore finds a store and pulls the history from it. It returns a
// *fallbackError when the run should poll instead, and the context's error
// when the user stopped the run.
func tryStore(ctx context.Context, s storeSteps, target *prometheus.Candidate, warn func(string)) (*storeResult, error) {
	stopped := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	var cands []prometheus.Candidate
	if target != nil {
		cands = []prometheus.Candidate{*target}
	} else {
		d, err := s.discover(ctx)
		if err != nil {
			return nil, stopped(fallback("could not look for a store: %v", err))
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
	c, err := pickStore(cands, func(c prometheus.Candidate) error { return s.check(ctx, c) }, warn)
	if err != nil {
		return nil, stopped(fallback("no store answered queries: %v", err))
	}
	f, rep, err := s.pull(ctx, *c)
	if err != nil {
		return nil, stopped(fallback("%s cannot be used: %v", c, err))
	}
	if !anyPriced(f) {
		return nil, fallback("%s has no container with all of %s, so no savings can be computed from it",
			c, strings.Join(contract.RequiredContainerMetrics, ", "))
	}
	return &storeResult{store: *c, file: f, report: rep}, nil
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
