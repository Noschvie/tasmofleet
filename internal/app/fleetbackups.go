package app

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// fleetStore is what the Backups page reads (*store.SQLite).
type fleetStore interface {
	BackupUsage(ctx context.Context) ([]store.BackupUsage, error)
	LastBackupRun(ctx context.Context) (model.BackupRun, bool, error)
	DevicesSeen(ctx context.Context) (map[model.DeviceID]time.Time, error)
}

// fleetBackups is web.BackupFleet (#409): the last scheduled run, one row per device and what the
// next prune removes.
type fleetBackups struct {
	store     fleetStore
	health    func() map[model.DeviceID]model.Health
	now       func() time.Time
	pruneNext func(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
}

var _ web.BackupFleet = (*fleetBackups)(nil)

func (f *fleetBackups) LastRun(ctx context.Context) (model.BackupRun, bool, error) {
	return f.store.LastBackupRun(ctx)
}

// Overview has one row per device, in device order, from three reads and no device request: the
// storage per device, the last scheduled run's items and when each device was last heard. The
// files columns (Files, FilesNote) are the page's to fill: they come from the backups' manifests.
func (f *fleetBackups) Overview(ctx context.Context) ([]model.BackupDeviceRow, error) {
	usage, err := f.store.BackupUsage(ctx)
	if err != nil {
		return nil, err
	}
	run, _, err := f.store.LastBackupRun(ctx)
	if err != nil {
		return nil, err
	}
	seen, err := f.store.DevicesSeen(ctx)
	if err != nil {
		return nil, err
	}
	last := map[model.DeviceID]model.BackupRunItem{}
	for _, it := range run.Items {
		if it.DeviceID != 0 {
			last[it.DeviceID] = it
		}
	}
	health, now := f.health(), f.now()
	var rows []model.BackupDeviceRow
	for _, u := range usage {
		if u.Device == 0 { // backups of deleted devices, no device
			continue
		}
		r := model.BackupDeviceRow{DeviceID: u.Device, Name: u.Name, NewestAt: u.Last, Runs: u.Runs, Kept: u.Kept, Bytes: u.Bytes,
			SkipReason: backupSkipReason(health[u.Device], seen[u.Device], true, now)}
		if it, ok := last[u.Device]; ok {
			r.LastOutcome, r.LastReason, r.LastAt = it.Outcome, it.Reason, it.At
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// PruneNext lists, over all devices, the backups the next backup of each deletes.
func (f *fleetBackups) PruneNext(ctx context.Context) ([]model.Backup, error) {
	usage, err := f.store.BackupUsage(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Backup
	for _, u := range usage {
		if u.Device == 0 || u.Runs == 0 {
			continue
		}
		list, err := f.pruneNext(ctx, u.Device)
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// fleetBackups wires the page's reads to the store, the tracker and the backup manager.
func (a *runtime) fleetBackups() *fleetBackups {
	health := func() map[model.DeviceID]model.Health { return nil }
	if a.tracker != nil {
		health = a.tracker.All
	}
	f := &fleetBackups{store: a.svc.Store, health: health, now: time.Now}
	if a.backups != nil {
		f.pruneNext = a.backups.PrunedAtNextRun
	} else {
		f.pruneNext = func(context.Context, model.DeviceID) ([]model.Backup, error) { return nil, nil }
	}
	return f
}
