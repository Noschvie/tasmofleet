package ota

import (
	"context"
	"time"
)

// Bulk options (design v2, N15): "One device at a time" and "Devices at a
// time" (1/3/5) cap how many jobs of one transaction run at once; "Wait up to
// 5 s for each response" bounds how long a command job waits for a device.
// They are stored on every job of the transaction (model.Job.BatchParallel,
// ReplyTimeout), so a resume after a restart and a retry keep them.

// MaxBatchParallel is the largest per-transaction cap. A capped transaction is
// scheduled against its own cap instead of the fleet-wide MaxParallel (that
// limit protects firmware downloads; a capped bulk command or restart is
// bounded by its cap), so the cap itself is bounded too.
const MaxBatchParallel = 5

// BatchOptions are the options of one submission. Zero values keep today's
// behaviour: the fleet-wide limit and the default command timeout.
type BatchOptions struct {
	Parallel     int           // 1..MaxBatchParallel; 0 = fleet-wide limit
	ReplyTimeout time.Duration // > 0: per-device wait of a command job
}

type batchOptsKey struct{}

// WithBatchOptions attaches options to a SubmitCommand (or any policy-style
// submission). Out-of-range values are clamped when the jobs are created.
func WithBatchOptions(ctx context.Context, o BatchOptions) context.Context {
	return context.WithValue(ctx, batchOptsKey{}, o)
}

// BatchOptionsOf returns the options attached to ctx, clamped to their range.
func BatchOptionsOf(ctx context.Context) BatchOptions {
	o, _ := ctx.Value(batchOptsKey{}).(BatchOptions)
	if o.Parallel < 0 {
		o.Parallel = 0
	}
	o.Parallel = min(o.Parallel, MaxBatchParallel)
	if o.ReplyTimeout < 0 {
		o.ReplyTimeout = 0
	}
	return o
}
