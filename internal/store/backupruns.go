package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// BackupRunStore is implemented by *SQLite next to BackupStore: the record of the scheduled
// backup runs (#409, migration 0043).
type BackupRunStore interface {
	// SaveBackupRun stores a run and its items in one transaction and returns the run's id.
	SaveBackupRun(ctx context.Context, r model.BackupRun) (int64, error)
	// LastBackupRun is the newest run that was completed (a run cut short is history only).
	LastBackupRun(ctx context.Context) (model.BackupRun, bool, error)
	// PruneBackupRuns deletes the runs started before the given time, with their items.
	PruneBackupRuns(ctx context.Context, before time.Time) (int64, error)
	// DevicesSeen maps every device that was ever heard to its persisted last_seen_at and health
	// state. A device that never answered is absent.
	DevicesSeen(ctx context.Context) (map[model.DeviceID]DeviceSeen, error)
}

// DeviceSeen is what is persisted of a device's health: when it was last heard and the verdict
// that was stored with it. Used after a start, before the tracker has heard anything.
type DeviceSeen struct {
	At    time.Time
	State model.HealthState
}

var _ BackupRunStore = (*SQLite)(nil)

func (s *SQLite) SaveBackupRun(ctx context.Context, r model.BackupRun) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	res, err := tx.ExecContext(ctx, `INSERT INTO backup_runs (trigger, started_at, finished_at, completed, tried, ok, failed, skipped)
		VALUES (?,?,?,?,?,?,?,?)`, string(r.Trigger), toMS(r.StartedAt), toMS(r.FinishedAt), b2i(r.Completed), r.Tried, r.OK, r.Failed, r.Skipped)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, it := range r.Items {
		// The device id is looked up, so a device deleted while the run was under way becomes NULL
		// (its name stays) instead of failing the foreign key and losing the whole run.
		if _, err := tx.ExecContext(ctx, `INSERT INTO backup_run_items (run_id, device_id, device_name, outcome, reason, at)
			VALUES (?, (SELECT id FROM devices WHERE id = ?), ?,?,?,?)`, id, int64(it.DeviceID), it.DeviceName, string(it.Outcome), it.Reason, toMS(it.At)); err != nil {
			return 0, mapErr(err)
		}
	}
	return id, tx.Commit()
}

func (s *SQLite) LastBackupRun(ctx context.Context) (model.BackupRun, bool, error) {
	var r model.BackupRun
	var trig string
	var started, finished int64
	var completed int
	err := s.db.QueryRowContext(ctx, `SELECT id, trigger, started_at, finished_at, completed, tried, ok, failed, skipped
		FROM backup_runs WHERE completed <> 0 ORDER BY started_at DESC, id DESC LIMIT 1`).
		Scan(&r.ID, &trig, &started, &finished, &completed, &r.Tried, &r.OK, &r.Failed, &r.Skipped)
	if errors.Is(err, sql.ErrNoRows) {
		return model.BackupRun{}, false, nil
	}
	if err != nil {
		return model.BackupRun{}, false, err
	}
	r.Trigger, r.StartedAt, r.Completed = model.BackupRunTrigger(trig), fromMS(started), completed != 0
	if finished != 0 {
		r.FinishedAt = fromMS(finished)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(device_id, 0), device_name, outcome, reason, at
		FROM backup_run_items WHERE run_id = ? ORDER BY id`, r.ID)
	if err != nil {
		return model.BackupRun{}, false, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	for rows.Next() {
		var it model.BackupRunItem
		var out string
		var at int64
		if err := rows.Scan(&it.DeviceID, &it.DeviceName, &out, &it.Reason, &at); err != nil {
			return model.BackupRun{}, false, err
		}
		it.Outcome, it.At = model.BackupOutcome(out), fromMS(at)
		r.Items = append(r.Items, it)
	}
	return r, true, rows.Err()
}

func (s *SQLite) PruneBackupRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backup_runs WHERE started_at < ?", toMS(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLite) DevicesSeen(ctx context.Context) (map[model.DeviceID]DeviceSeen, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, last_seen_at, health_state FROM devices WHERE last_seen_at <> 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[model.DeviceID]DeviceSeen{}
	for rows.Next() {
		var id model.DeviceID
		var at int64
		var st string
		if err := rows.Scan(&id, &at, &st); err != nil {
			return nil, err
		}
		out[id] = DeviceSeen{At: fromMS(at), State: model.HealthState(st)}
	}
	return out, rows.Err()
}
