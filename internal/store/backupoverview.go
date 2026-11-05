package store

import (
	"context"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// NewestRun is the newest backup run of a device as the Backups page needs it: the settings dump's
// id and note (the note says why the run has no files) and the files archive row of the same run.
type NewestRun struct {
	Device   model.DeviceID
	DumpID   model.BackupID
	DumpNote string
	FilesID  model.BackupID // 0 = the run has no files archive
}

// NewestRunStore is implemented by *SQLite next to BackupUsageStore.
type NewestRunStore interface {
	// NewestRuns has one entry per device that has a backup, from one statement.
	NewestRuns(ctx context.Context) (map[model.DeviceID]NewestRun, error)
}

var _ NewestRunStore = (*SQLite)(nil)

const newestRunsQuery = `
SELECT d.device_id, d.id, d.note,
       COALESCE((SELECT f.id FROM backups f WHERE f.device_id = d.device_id AND f.kind = 'files' AND f.taken_at = d.taken_at ORDER BY f.id LIMIT 1), 0)
  FROM backups d
 WHERE d.kind = 'dmp' AND d.device_id IS NOT NULL
   AND d.id = (SELECT m.id FROM backups m WHERE m.device_id = d.device_id AND m.kind = 'dmp' ORDER BY m.taken_at DESC, m.id DESC LIMIT 1)`

func (s *SQLite) NewestRuns(ctx context.Context) (map[model.DeviceID]NewestRun, error) {
	rows, err := s.db.QueryContext(ctx, newestRunsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[model.DeviceID]NewestRun{}
	for rows.Next() {
		var r NewestRun
		if err := rows.Scan(&r.Device, &r.DumpID, &r.DumpNote, &r.FilesID); err != nil {
			return nil, err
		}
		out[r.Device] = r
	}
	return out, rows.Err()
}
