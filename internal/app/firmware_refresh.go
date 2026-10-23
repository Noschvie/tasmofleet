package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
)

const (
	firmwareGaugeEvery = 30 * time.Second
	firmwareJitter     = 0.2
)

type firmwareIndexOptions struct {
	Clock      clock.Clock
	Interval   time.Duration // FIRMWARE_REFRESH_INTERVAL
	GaugeEvery time.Duration
	Log        *slog.Logger
}

// startFirmwareRefresh refreshes the upstream index at start and then
// periodically in the background. It never blocks startup and /readyz does not
// depend on it.
func (a *runtime) startFirmwareRefresh() {
	if a.cache == nil {
		return
	}
	o := firmwareIndexOptions{Clock: clock.Real{}, Interval: a.cfg.FWRefreshInterval, GaugeEvery: firmwareGaugeEvery, Log: a.log}
	a.goBG("firmware-index", func() error {
		superviseFirmwareIndex(a.bgContext, a.cache, a.svc.Std.FirmwareIndexAge, o)
		return nil
	})
}

// superviseFirmwareIndex runs the refresher and keeps the
// tasmofleet_firmware_index_age_seconds gauge current until ctx ends.
func superviseFirmwareIndex(ctx context.Context, c firmware.Cache, age *metrics.Vec, o firmwareIndexOptions) {
	start := o.Clock.Now()
	var wg sync.WaitGroup
	if r, ok := c.(firmware.Refresher); ok {
		wg.Go(func() {
			firmware.RunRefresher(ctx, r, firmware.RefreshOptions{Interval: o.Interval, Jitter: firmwareJitter, Clock: o.Clock, Logger: o.Log})
		})
	} else {
		o.Log.Warn("firmware cache cannot refresh its index in the background")
	}
	if sr, ok := c.(firmware.StatusReporter); ok {
		set := func() { age.With().Set(firmwareIndexAge(sr.IndexStatus(), start, o.Clock.Now())) }
		set()
		t := time.NewTicker(o.GaugeEvery)
		defer t.Stop()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-t.C:
				set()
			}
		}
	}
	wg.Wait()
}

// firmwareIndexAge is the gauge value. A never-loaded index counts from process
// start so that an "index too old" alert also fires when the upstream is
// unreachable from the very beginning.
func firmwareIndexAge(st firmware.IndexStatus, start, now time.Time) float64 {
	ref := start
	if st.Loaded {
		ref = st.LastSuccess
	}
	return max(0, now.Sub(ref).Seconds())
}

// indexTTL is how long reads reuse the index: a little longer than the refresh
// interval, so the background refresh (not a page view) does the fetching.
// 0 keeps the cache default.
func indexTTL(refresh time.Duration) time.Duration {
	if refresh <= 0 {
		return 0
	}
	return refresh + refresh/10
}
