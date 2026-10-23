package firmware

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
)

const (
	refreshBackoffStart = 30 * time.Second
	refreshBackoffMax   = 30 * time.Minute
)

// IndexStatus describes the upstream index for the firmware page and the
// index-age gauge.
type IndexStatus struct {
	Loaded      bool      // an index has been fetched successfully at least once
	LastSuccess time.Time // zero until Loaded
	LastAttempt time.Time // zero until the first attempt
	LastError   string    // error of the last attempt; empty if it succeeded
}

// StatusReporter is implemented by the cache returned by New.
type StatusReporter interface {
	IndexStatus() IndexStatus
}

// RefreshOptions configure RunRefresher.
type RefreshOptions struct {
	// Interval between successful refreshes; <= 0 means only the start-up
	// refresh (the loop ends after the first success).
	Interval time.Duration
	// Jitter is the fraction (0..1) by which every wait is randomly spread
	// around its nominal value; 0 = exact.
	Jitter float64
	// Rand returns a number in [0,1); nil = math/rand/v2.
	Rand   func() float64
	Clock  clock.Clock // nil = real time
	Logger *slog.Logger
}

// RunRefresher refreshes the index now and then every Interval until ctx
// ends. A failure is retried with backoff (30 s, 1 min, 2 min ... capped at
// 30 min) until it succeeds. It blocks; run it in a goroutine. A failed
// refresh never discards the previous index (Refresh only replaces it on
// success).
func RunRefresher(ctx context.Context, r Refresher, o RefreshOptions) {
	clk, log, rnd := o.Clock, o.Logger, o.Rand
	if clk == nil {
		clk = clock.Real{}
	}
	if log == nil {
		log = slog.Default()
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	backoff := refreshBackoffStart
	for {
		var wait time.Duration
		if err := r.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = backoff
			log.Warn("firmware: index refresh failed, will retry", "err", err, "retry_in", wait)
			if backoff *= 2; backoff > refreshBackoffMax {
				backoff = refreshBackoffMax
			}
		} else {
			backoff = refreshBackoffStart
			if o.Interval <= 0 {
				return
			}
			wait = o.Interval
			log.Info("firmware: index refreshed", "next_in", wait)
		}
		if o.Jitter > 0 {
			wait = time.Duration(float64(wait) * (1 + o.Jitter*(2*rnd()-1)))
		}
		select {
		case <-ctx.Done():
			return
		case <-clk.After(wait):
		}
	}
}

// IndexStatus returns the state of the last index fetches.
func (c *cache) IndexStatus() IndexStatus {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	return c.status
}

// recordIndex notes the outcome of an index fetch. errs holds the failed
// sources; with ok set they are partial failures that stay visible.
func (c *cache) recordIndex(now time.Time, ok bool, errs []error) {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	c.status.LastAttempt = now
	if ok {
		c.status.Loaded, c.status.LastSuccess = true, now
	}
	c.status.LastError = ""
	if len(errs) > 0 {
		c.status.LastError = errors.Join(errs...).Error()
	}
}
