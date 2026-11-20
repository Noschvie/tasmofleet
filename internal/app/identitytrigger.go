package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
)

// watchBackupVersions reads the backup events on ch until ctx ends or the hub closes ch, and asks for an identity read
// (changed, brokerHostReader.identityChanged) of a probed device whose new backup names another firmware version than
// the stored identity (#1003): the backup's Status 2 may be the first read after an update done outside TasmoFleet. The
// backup's values have no MAC check, so nothing of them is stored as the identity; the identity read stores what it
// reads with its own checks (T20). Every file of a run names the same version, and the reader takes a version's ask once
// until an identity read succeeds, and reads the identity for a version at most once per brokerHostRetry. A
// lost event (Resync) only delays the read until the next trigger or the daily identity refresh.
func watchBackupVersions(ctx context.Context, ch <-chan events.Event, get func(context.Context, model.DeviceID) (model.Device, error),
	changed func(model.DeviceID, string), log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, isBackup := e.Payload.(model.Backup)
			if !isBackup || b.DeviceID == 0 || b.Version == "" { // a Resync, the status bar's ping, a deleted device's backup
				continue
			}
			d, err := get(ctx, b.DeviceID)
			if err != nil {
				if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
					log.Warn("backup version: could not read the device", "device", int64(b.DeviceID), "err", err)
				}
				continue
			}
			if !d.Identity.DetectedAt.IsZero() && b.Version != d.Identity.Version {
				changed(d.ID, b.Version)
			}
		}
	}
}
