package store

import (
	"context"
	"time"
)

// PrefsStore keeps per-user interface preferences. user is the principal's
// stable subject; key and value are validated by the caller (package web).
type PrefsStore interface {
	// UserPrefs returns all preferences of user (key -> JSON value).
	UserPrefs(ctx context.Context, user string) (map[string]string, error)
	SetUserPref(ctx context.Context, user, key, value string) error
	// DeleteUserPref removes one preference; a missing one is not an error.
	DeleteUserPref(ctx context.Context, user, key string) error
}

func (s *SQLite) UserPrefs(ctx context.Context, user string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pref_key, value FROM user_prefs WHERE user_key = ?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *SQLite) SetUserPref(ctx context.Context, user, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_prefs (user_key, pref_key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_key, pref_key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		user, key, value, toMS(time.Now()))
	return err
}

func (s *SQLite) DeleteUserPref(ctx context.Context, user, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_prefs WHERE user_key = ? AND pref_key = ?`, user, key)
	return err
}
