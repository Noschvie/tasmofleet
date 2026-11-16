package store

import (
	"context"
	"database/sql"
	"errors"
)

// GetKV reads one value of the settings table (migration 0001): TasmoFleet's own small state that is
// not a setting a person edits (those are in system_settings). ok is false when the key is not there.
func (s *SQLite) GetKV(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// PutKV stores one value of the settings table, replacing the one under the same key.
func (s *SQLite) PutKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}
