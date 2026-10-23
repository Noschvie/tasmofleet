package store

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// BackupUsage is the backup storage of one device (design v2 N9, Settings ->
// Backups). Device 0 is the sum of the backups of deleted devices (orphans);
// that row is only present when there are any.
type BackupUsage struct {
	Device model.DeviceID
	Name   string
	Runs   int   // backups as the device's Backups tab counts them: one per run, decoded copies not counted
	Kept   int   // runs that are kept (never pruned)
	Bytes  int64 // every file of every run, decoded copies included: what the backups take on disk
	Last   time.Time
}

// BackupUsageStore is implemented by *SQLite next to BackupStore.
type BackupUsageStore interface {
	// BackupUsage lists every device (with or without backups) and, when there
	// are orphans, one row with Device 0 for them. Order: device id.
	BackupUsage(ctx context.Context) ([]BackupUsage, error)
}

// A run is the files taken at the same time for the same device (dump, snapshot,
// decoded copy); the run key of an orphan includes the MAC it was taken for, so
// two deleted devices backed up in the same millisecond still count twice.
const backupUsageQuery = `
SELECT d.id, d.name,
       COUNT(DISTINCT CASE WHEN b.kind <> 'decoded' THEN b.taken_at END),
       COUNT(DISTINCT CASE WHEN b.kind <> 'decoded' AND b.kept <> 0 THEN b.taken_at END),
       COALESCE(SUM(b.size), 0), COALESCE(MAX(b.taken_at), 0)
  FROM devices d LEFT JOIN backups b ON b.device_id = d.id
 GROUP BY d.id
UNION ALL
SELECT 0, '',
       COUNT(DISTINCT CASE WHEN b.kind <> 'decoded' THEN b.device_mac || '|' || b.device_name || '|' || b.taken_at END),
       COUNT(DISTINCT CASE WHEN b.kind <> 'decoded' AND b.kept <> 0 THEN b.device_mac || '|' || b.device_name || '|' || b.taken_at END),
       COALESCE(SUM(b.size), 0), COALESCE(MAX(b.taken_at), 0)
  FROM backups b WHERE b.device_id IS NULL
HAVING COUNT(*) > 0
ORDER BY 1`

// BackupUsage aggregates the backups table in one statement (no per-device
// queries; the table holds a few thousand rows at most, no index needed).
func (s *SQLite) BackupUsage(ctx context.Context) ([]BackupUsage, error) {
	rows, err := s.db.QueryContext(ctx, backupUsageQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []BackupUsage
	for rows.Next() {
		var u BackupUsage
		var last int64
		if err := rows.Scan(&u.Device, &u.Name, &u.Runs, &u.Kept, &u.Bytes, &last); err != nil {
			return nil, err
		}
		if last != 0 {
			u.Last = fromMS(last)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
