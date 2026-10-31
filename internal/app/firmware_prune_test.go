package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

type pruneFake struct {
	firmware.Cache
	res   firmware.DeleteResult
	err   error
	calls []int
}

func (f *pruneFake) Prune(_ context.Context, keep int) (firmware.DeleteResult, error) {
	f.calls = append(f.calls, keep)
	return f.res, f.err
}

type recordedAudit struct{ events []model.AuditEvent }

func (r *recordedAudit) Record(_ context.Context, e model.AuditEvent) { r.events = append(r.events, e) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// Why: an admin who sees disk space come back must be able to find out what was deleted; the
// audit entry carries the count and the bytes, and "kept in use" shows that running updates
// were respected.
func TestPruneFirmwareCacheAuditsWhatItDeleted(t *testing.T) {
	f := &pruneFake{res: firmware.DeleteResult{Deleted: []model.Artifact{{Size: 100}, {Size: 50}}, Freed: 150, Kept: 3}}
	rec := &recordedAudit{}
	pruneFirmwareCache(context.Background(), f, 3, rec, quiet)
	if len(f.calls) != 1 || f.calls[0] != 3 {
		t.Fatalf("Prune calls = %v, want one with keep=3", f.calls)
	}
	if len(rec.events) != 1 {
		t.Fatalf("%d audit events, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Action != "firmware.prune" || ev.Actor != "system" || ev.Result != "ok" ||
		!strings.Contains(ev.Detail, `"files":2`) || !strings.Contains(ev.Detail, `"bytes":150`) || !strings.Contains(ev.Detail, `"kept_in_use":3`) {
		t.Fatalf("event = %+v", ev)
	}
}

// Why: the run happens after every refresh and daily; an audit row per no-op would bury real
// events. Keep 0 is the admin's "never delete" and must not even call the cache.
func TestPruneFirmwareCacheIsSilentWhenNothingToDoAndOffAtZero(t *testing.T) {
	f := &pruneFake{}
	rec := &recordedAudit{}
	pruneFirmwareCache(context.Background(), f, 3, rec, quiet)
	pruneFirmwareCache(context.Background(), f, 0, rec, quiet)
	if len(rec.events) != 0 {
		t.Fatalf("audited a no-op: %+v", rec.events)
	}
	if len(f.calls) != 1 {
		t.Fatalf("Prune calls = %v, want only the keep=3 run", f.calls)
	}
}

// Why: a failing prune (for example when the in-use lookup fails and nothing is deleted) is
// visible in the audit log, not swallowed.
func TestPruneFirmwareCacheAuditsFailure(t *testing.T) {
	f := &pruneFake{err: errors.New("db down")}
	rec := &recordedAudit{}
	pruneFirmwareCache(context.Background(), f, 3, rec, quiet)
	if len(rec.events) != 1 || !strings.HasPrefix(rec.events[0].Result, "failed: ") {
		t.Fatalf("events = %+v", rec.events)
	}
}

// Why: the refresh is when newer releases appear, so the supervisor must run the hook after a
// successful refresh.
func TestSuperviseFirmwareIndexPrunesAfterRefresh(t *testing.T) {
	f := &fakeIndex{}
	ran := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		superviseFirmwareIndex(ctx, f, metrics.NewStandard(metrics.NewRegistry(), "test", false).FirmwareIndexAge, firmwareIndexOptions{Clock: fixedClock{}, Interval: 0, GaugeEvery: time.Hour, Log: quiet, OnRefreshed: func(context.Context) { ran <- struct{}{} }})
		close(done)
	}()
	select {
	case <-ran:
	case <-done:
		t.Fatal("supervisor ended without running the hook")
	}
	cancel()
	<-done
}
