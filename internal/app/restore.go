package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
)

// setupRestore builds the restore service. It needs the firmware listener
// (devices fetch the dump from it), so it exists only when FIRMWARE_BASE_URL
// is set. Restores that were running when the process stopped are failed:
// their one-time tokens died with it.
func (a *runtime) setupRestore() error {
	if a.cache == nil || a.backups == nil {
		return nil
	}
	n, err := a.svc.Store.FailActiveRestores(context.Background(), "interrupted by a restart of tasmofleet")
	if err != nil {
		return fmt.Errorf("restore recovery: %w", err)
	}
	if n > 0 {
		a.log.Warn("restores interrupted by the restart were marked failed", "count", n)
	}
	m, err := restore.New(restore.Options{
		Devices: a.svc.Store, Backups: a.backups, Store: a.svc.Store, Client: a.svc.Client,
		BaseURL: a.cfg.FWBaseURL, Audit: a.svc.Audit, Log: a.log, Context: a.bgContext,
		Locks: a.deviceLocks(), Jobs: a.svc.Store,
		Hardware: a.svc.Store, Describe: a.svc.Prober.Describe,
		OnServed: func(id model.DeviceID) {
			a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
				return l.NoteYes(ctx, id, FWReachSourceRestoreToken)
			})
		},
	})
	if err != nil {
		return err
	}
	a.restore = m
	a.goBG("restore", func() error { <-a.bgContext.Done(); m.Wait(); return nil })
	return nil
}

// restoreHandler is mounted at /restore/ on the firmware listener (nil when
// restore is off).
func (a *runtime) restoreHandler() http.Handler {
	if a.restore == nil {
		return nil
	}
	return a.restore.Handler()
}

// deviceLocks is the per-device operation lock set. Restore uses it now; OTA,
// settings and console must take the same locks (see docs/security/restore.md).
func (a *runtime) deviceLocks() *devlock.Locks {
	if a.devLocks == nil {
		a.devLocks = devlock.New()
	}
	return a.devLocks
}
