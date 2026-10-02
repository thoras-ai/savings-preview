package main

import (
	"context"
	"io"
	"time"

	"github.com/thoras-ai/savings-preview/internal/adapters/polling"
	"github.com/thoras-ai/savings-preview/internal/kube"
)

type pollOptions struct {
	Cluster    string
	Out        string
	Interval   time.Duration
	Duration   time.Duration // 0 polls until ctx is cancelled
	WriteEvery time.Duration
	KeepRaw    time.Duration
	Rollup     time.Duration
}

// poll reads the cluster every o.Interval and writes the contract file to
// o.Out. When ctx is cancelled it writes what it has read and returns nil.
// Tests replace it.
var poll = func(ctx context.Context, conn *kube.Conn, o pollOptions, log io.Writer) error {
	return polling.Poll(ctx, conn.Clientset, polling.Options{Cluster: o.Cluster, Output: o.Out, Interval: o.Interval,
		Duration: o.Duration, WriteEvery: o.WriteEvery, KeepRaw: o.KeepRaw, Rollup: o.Rollup}, log)
}
