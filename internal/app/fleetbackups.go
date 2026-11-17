package app

import (
	"context"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web"
)

// fleetStore is what the Backups page reads (*store.SQLite).
type fleetStore interface {
	BackupUsage(ctx context.Context) ([]store.BackupUsage, error)
	LastBackupRun(ctx context.Context) (model.BackupRun, bool, error)
	DevicesSeen(ctx context.Context) (map[model.DeviceID]store.DeviceSeen, error)
	NewestRuns(ctx context.Context) (map[model.DeviceID]store.NewestRun, error)
	BackupRunTimes(ctx context.Context) (settled, missed time.Time, err error)
}

// fleetBackups is web.BackupFleet (#409): the last scheduled run, one row per device and what Prune
// now removes.
type fleetBackups struct {
	store  fleetStore
	health func() map[model.DeviceID]model.Health
	// apiOffSkip: see scheduledBackup; nil tries every device.
	apiOffSkip func(ctx context.Context, id model.DeviceID, h model.Health) string
	// files reads the manifest of a run (disk, no device I/O); *backup.Manager.RunFiles.
	files func(ctx context.Context, id model.BackupID) (backup.RunFiles, error)
	// prunePlan is what Prune now removes from one device and the runs it keeps back: the one rule
	// of backup.PruneNowSet (newest run, runs a restore uses); *backup.Manager.PruneNowPlan.
	prunePlan func(ctx context.Context, id model.DeviceID) ([]model.Backup, []backup.Held, error)
}

var _ web.BackupFleet = (*fleetBackups)(nil)

func (f *fleetBackups) LastRun(ctx context.Context) (model.BackupRun, bool, error) {
	return f.store.LastBackupRun(ctx)
}

func (f *fleetBackups) LastMissed(ctx context.Context) (time.Time, error) {
	_, missed, err := f.store.BackupRunTimes(ctx)
	return missed, err
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
	case reason == "they need HTTP", strings.HasPrefix(reason, "they need HTTP ("): // with why MQTT could not carry them (#924)
		return "files need HTTP"
	case strings.HasPrefix(reason, "firmware older than "):
		return "settings only: " + reason + " has no file access"
	}
	return backup.NoteFilesPrefix + reason
}

// leftOutText is why Prune now leaves a device alone: the retention prunes only after a successful
// backup, so a device whose last scheduled run failed or was skipped keeps its backups until it is
// backed up again; thinning it now would take the backups of exactly the devices that get no new ones.
const leftOutText = "its last run did not back it up"

// PrunePlan lists, over all devices, what Prune now removes. The rule for one device (never the
// newest run, never a run a restore uses) is backup.PruneNowSet and is not repeated here; this adds
// the one fleet rule, the left-out devices, which the page shows with a badge and the POST skips.
func (f *fleetBackups) PrunePlan(ctx context.Context) ([]web.PruneDevice, error) {
	usage, err := f.store.BackupUsage(ctx)
	if err != nil {
		return nil, err
	}
	run, _, err := f.store.LastBackupRun(ctx)
	if err != nil {
		return nil, err
	}
	lastRun := map[model.DeviceID]model.BackupOutcome{}
	for _, it := range run.Items {
		if it.DeviceID != 0 {
			lastRun[it.DeviceID] = it.Outcome
		}
	}
	var out []web.PruneDevice
	for _, u := range usage {
		if u.Device == 0 || u.Runs == 0 {
			continue
		}
		files, held, err := f.prunePlan(ctx, u.Device)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			continue
		}
		d := web.PruneDevice{Device: u.Device, Name: u.Name, Files: files, Held: len(held)}
		if o := lastRun[u.Device]; o == model.BackupOutcomeFailed || o == model.BackupOutcomeSkipped {
			d.LeftOut = leftOutText
		}
		out = append(out, d)
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
	f.prunePlan = func(context.Context, model.DeviceID) ([]model.Backup, []backup.Held, error) { return nil, nil, nil }
	f.files = func(context.Context, model.BackupID) (backup.RunFiles, error) { return backup.RunFiles{}, nil }
	if a.backups != nil {
		f.prunePlan = a.backups.PruneNowPlan
		f.files = a.backups.RunFiles
	}
	return f
}
