package web

import (
	"context"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// BackupFleet is what the Devices, Backups page (#409) reads: the last scheduled run, one row per
// device, and what the next prune removes. Optional: without it the page and the dashboard hint
// say nothing about runs.
type BackupFleet interface {
	// LastRun is the last finished scheduled run; ok is false when there is none yet.
	LastRun(ctx context.Context) (run model.BackupRun, ok bool, err error)
	// Overview has one row per inventory device and does no device I/O: it reads the database and,
	// for the files columns, the manifest of each device's newest archive on disk. Files is the number
	// of stored files in that archive; FilesNote says why the newest run has none ("settings only: no
	// file system", "files need HTTP") and is empty otherwise.
	Overview(ctx context.Context) ([]model.BackupDeviceRow, error)
	// PruneNext lists the backup runs the next backup would delete.
	PruneNext(ctx context.Context) ([]model.Backup, error)
}
