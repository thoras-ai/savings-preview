package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/thoras-ai/savings-preview/internal/kube"
)

type pollOptions struct {
	Cluster  string
	Out      string
	Interval time.Duration
	Duration time.Duration // 0 polls until ctx is cancelled
}

// poll reads the cluster every o.Interval and writes the contract file to
// o.Out. When ctx is cancelled it writes what it has read and returns nil.
var poll = func(ctx context.Context, conn *kube.Conn, o pollOptions, log io.Writer) error {
	return errors.New("this build has no polling source; name a store with --target or use a build that includes polling")
}
