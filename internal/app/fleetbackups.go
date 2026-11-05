package app

import (
	"context"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// fleetStore is what the Backups page reads (*store.SQLite).
type fleetStore interface {
	BackupUsage(ctx context.Context) ([]store.BackupUsage, error)
	LastBackupRun(ctx context.Context) (model.BackupRun, bool, error)
	DevicesSeen(ctx context.Context) (map[model.DeviceID]store.DeviceSeen, error)
	NewestRuns(ctx context.Context) (map[model.DeviceID]store.NewestRun, error)
}

// fleetBackups is web.BackupFleet (#409): the last scheduled run, one row per device and what the
// next prune removes.
type fleetBackups struct {
	store  fleetStore
	health func() map[model.DeviceID]model.Health
	// apiOffSkip: see scheduledBackup; nil tries every device.
	apiOffSkip func(ctx context.Context, id model.DeviceID, h model.Health) string
	// files reads the manifest of a run (disk, no device I/O); *backup.Manager.RunFiles.
	files     func(ctx context.Context, id model.BackupID) (backup.RunFiles, error)
	pruneNext func(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
}

var _ web.BackupFleet = (*fleetBackups)(nil)

func (f *fleetBackups) LastRun(ctx context.Context) (model.BackupRun, bool, error) {
	return f.store.LastBackupRun(ctx)
}

// Overview has one row per device, in device order, from four reads and no device request: the
// storage per device, the last scheduled run's items, when each device was last heard and the
// newest run's note and files row. Files is the number of files in the newest run's archive, read
// from its manifest on disk (one read per device that has an archive); FilesNote says why a run
// has no files ("settings only: no file system"), from the run's note.
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
	newest, err := f.store.NewestRuns(ctx)
	if err != nil {
		return nil, err
	}
	last := map[model.DeviceID]model.BackupRunItem{}
	for _, it := range run.Items {
		if it.DeviceID != 0 {
			last[it.DeviceID] = it
		}
	}
	health := f.health()
	var rows []model.BackupDeviceRow
	for _, u := range usage {
		if u.Device == 0 { // backups of deleted devices, no device
			continue
		}
		r := model.BackupDeviceRow{DeviceID: u.Device, Name: u.Name, NewestAt: u.Last, Runs: u.Runs, Kept: u.Kept, Bytes: u.Bytes}
		r.SkipReason, r.SkipSince = backupSkipReason(health[u.Device], seen[u.Device], true)
		if r.SkipReason == "" && f.apiOffSkip != nil {
			if why := f.apiOffSkip(ctx, u.Device, health[u.Device]); why != "" {
				r.SkipReason, r.SkipSince = why, health[u.Device].HTTPRefusedSince
			}
		}
		if it, ok := last[u.Device]; ok {
			r.LastOutcome, r.LastReason, r.LastAt, r.LastSince = it.Outcome, it.Reason, it.At, it.Since
		}
		if n, ok := newest[u.Device]; ok {
			r.Files, r.FilesNote = f.filesOf(ctx, n)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// filesOf is the Files and FilesNote columns of a device's newest run. An archive that cannot be
// read leaves the count empty and says so: the row must still show.
func (f *fleetBackups) filesOf(ctx context.Context, n store.NewestRun) (int, string) {
	if n.FilesID != 0 {
		rf, err := f.files(ctx, n.FilesID)
		switch {
		case err != nil || rf.Manifest == nil:
			return 0, "files archive could not be read"
		default:
			return len(rf.Manifest.Files), ""
		}
	}
	return 0, filesNoteText(backup.FilesNoteOf(n.DumpNote))
}

// filesNoteText words the reason a run has no files for a table cell. reason is the sentence after
// backup.NoteFilesPrefix; "" (an older run, from before the files step) says nothing.
func filesNoteText(reason string) string {
	switch {
	case reason == "":
		return ""
	case reason == "no filesystem":
		return "settings only: no file system"
	case reason == "they need HTTP":
		return "files need HTTP"
	case strings.HasPrefix(reason, "firmware older than "):
		return "settings only: " + reason + " has no file access"
	}
	return backup.NoteFilesPrefix + reason
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
	f := &fleetBackups{store: a.svc.Store, health: health, apiOffSkip: a.apiOffSkip}
	f.pruneNext = func(context.Context, model.DeviceID) ([]model.Backup, error) { return nil, nil }
	f.files = func(context.Context, model.BackupID) (backup.RunFiles, error) { return backup.RunFiles{}, nil }
	if a.backups != nil {
		f.pruneNext = a.backups.PrunedAtNextRun
		f.files = a.backups.RunFiles
	}
	return f
}
