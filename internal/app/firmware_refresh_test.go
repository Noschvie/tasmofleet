package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time                       { return c.now }
func (c fixedClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type fakeIndex struct {
	firmware.Cache // never used; only Refresher/StatusReporter matter
	st             firmware.IndexStatus
	refreshes      atomic.Int32
}

func (f *fakeIndex) Refresh(context.Context) error     { f.refreshes.Add(1); return nil }
func (f *fakeIndex) IndexStatus() firmware.IndexStatus { return f.st }

func scrape(reg *metrics.Registry) string {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1"
	reg.Handler().ServeHTTP(rec, req)
	return rec.Body.String()
}

// Why: an alert on "index older than X" must also fire when the index never
// loaded, so the age counts from process start in that case.
func TestFirmwareIndexAge(t *testing.T) {
	start := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	now := start.Add(2 * time.Hour)
	if got := firmwareIndexAge(firmware.IndexStatus{}, start, now); got != 7200 {
		t.Errorf("never loaded: %v, want 7200 (since start)", got)
	}
	st := firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-90 * time.Second)}
	if got := firmwareIndexAge(st, start, now); got != 90 {
		t.Errorf("loaded: %v, want 90", got)
	}
}

// The wiring: the refresh happens without a click and the gauge is exported.
func TestSuperviseFirmwareIndexRefreshesAndExportsAge(t *testing.T) {
	reg := metrics.NewRegistry()
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	f := &fakeIndex{st: firmware.IndexStatus{Loaded: true, LastSuccess: now.Add(-time.Minute)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseFirmwareIndex(ctx, f, metrics.NewStandard(reg, "test", false).FirmwareIndexAge, firmwareIndexOptions{Clock: fixedClock{now}, Interval: 0, GaugeEvery: 5 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	// The gauge is set right away while the refresher runs in its own goroutine,
	// so wait for both instead of assuming the refresh happened first.
	for !strings.Contains(scrape(reg), "tasmofleet_firmware_index_age_seconds 60") || f.refreshes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("gauge missing or no refresh (refreshes = %d):\n%s", f.refreshes.Load(), scrape(reg))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1 without any click", f.refreshes.Load())
	}
	cancel()
	<-done
}
