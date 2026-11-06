package store

import (
	"context"
	"time"
)

// Snapshot run triggers: what a row of snapshot_runs records (#602, migration 0046).
const (
	// SnapshotRunScheduled: a scheduled snapshot was attempted (it may have failed; the time of day is handled).
	SnapshotRunScheduled = "scheduled"
	// SnapshotRunMissed: the scheduled time passed while TasmoFleet was down and was too long ago to
	// start late; the snapshot was skipped (started_at is when that was noticed).
	SnapshotRunMissed = "missed"
	// SnapshotRunRescheduled: the schedule was changed (or created by a migration) at started_at; a time
	// of day that had passed before is not run late.
	SnapshotRunRescheduled = "rescheduled"
)

// SnapshotRunStore is implemented by *SQLite: the record the snapshot schedule reads (#602).
type SnapshotRunStore interface {
	// SaveSnapshotRun stores a row of the given trigger at the given time.
	SaveSnapshotRun(ctx context.Context, trigger string, at time.Time) error
	// SnapshotRunTimes says when the newest row of any trigger was written (what settled a scheduled
	// time) and when the newest missed row was. Zero when there is none.
	SnapshotRunTimes(ctx context.Context) (settled, missed time.Time, err error)
	// PruneSnapshotRuns deletes the rows written before the given time.
	PruneSnapshotRuns(ctx context.Context, before time.Time) (int64, error)
}

func (s *SQLite) SaveSnapshotRun(ctx context.Context, trigger string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO snapshot_runs (trigger, started_at) VALUES (?, ?)", trigger, toMS(at))
	return err
}

func (s *SQLite) SnapshotRunTimes(ctx context.Context) (settled, missed time.Time, err error) {
	var a, b int64
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(started_at) FROM snapshot_runs), 0),
		COALESCE((SELECT MAX(started_at) FROM snapshot_runs WHERE trigger = 'missed'), 0)`).Scan(&a, &b)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return fromMS(a), fromMS(b), nil
}

func (s *SQLite) PruneSnapshotRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM snapshot_runs WHERE started_at < ?", toMS(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

var _ SnapshotRunStore = (*SQLite)(nil)
