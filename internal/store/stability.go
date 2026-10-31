package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// --- restarts and Wi-Fi samples (#325) ---

func (s *SQLite) AppendBoot(ctx context.Context, b model.DeviceBoot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	if _, err := tx.ExecContext(ctx, "INSERT INTO device_boots (device_id, at, boot_count, reason, uptime_before) VALUES (?,?,?,?,?)",
		b.DeviceID, toMS(s.orNow(b.At)), b.BootCount, b.Reason, int64(b.UptimeBefore/time.Second)); err != nil {
		return mapErr(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_boots WHERE device_id=? AND id NOT IN
		(SELECT id FROM device_boots WHERE device_id=? ORDER BY at DESC, id DESC LIMIT ?)`, b.DeviceID, b.DeviceID, MaxBootsPerDevice); err != nil {
		return err
	}
	return tx.Commit()
}

const bootCols = "device_id, at, boot_count, reason, uptime_before"

func scanBoots(rows *sql.Rows, err error) ([]model.DeviceBoot, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.DeviceBoot
	for rows.Next() {
		var b model.DeviceBoot
		var at, up int64
		if err := rows.Scan(&b.DeviceID, &at, &b.BootCount, &b.Reason, &up); err != nil {
			return nil, err
		}
		b.At, b.UptimeBefore = fromMS(at), time.Duration(up)*time.Second
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *SQLite) ListBoots(ctx context.Context, id model.DeviceID, limit int) ([]model.DeviceBoot, error) {
	q, args := "SELECT "+bootCols+" FROM device_boots WHERE device_id=? ORDER BY at DESC, id DESC", []any{id}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	return scanBoots(s.db.QueryContext(ctx, q, args...))
}

func (s *SQLite) BootsSince(ctx context.Context, since time.Time) (map[model.DeviceID][]model.DeviceBoot, error) {
	list, err := scanBoots(s.db.QueryContext(ctx, "SELECT "+bootCols+" FROM device_boots WHERE at>=? ORDER BY at DESC, id DESC", toMS(since)))
	if err != nil {
		return nil, err
	}
	out := map[model.DeviceID][]model.DeviceBoot{}
	for _, b := range list {
		out[b.DeviceID] = append(out[b.DeviceID], b)
	}
	return out, nil
}

func (s *SQLite) AppendWifiSample(ctx context.Context, w model.WifiSample) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO device_wifi_samples (device_id, at, dbm) VALUES (?,?,?)",
		w.DeviceID, toMS(s.orNow(w.At)), w.DBm)
	return mapErr(err)
}

func (s *SQLite) WifiSamplesSince(ctx context.Context, since time.Time) (map[model.DeviceID][]model.WifiSample, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT device_id, at, dbm FROM device_wifi_samples WHERE at>=? ORDER BY at", toMS(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[model.DeviceID][]model.WifiSample{}
	for rows.Next() {
		var w model.WifiSample
		var at int64
		if err := rows.Scan(&w.DeviceID, &at, &w.DBm); err != nil {
			return nil, err
		}
		w.At = fromMS(at)
		out[w.DeviceID] = append(out[w.DeviceID], w)
	}
	return out, rows.Err()
}

func (s *SQLite) PruneWifiSamples(ctx context.Context, before time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM device_wifi_samples WHERE at < ?", toMS(before)))
}
