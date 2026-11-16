package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrUnreadable: a sealed value is stored but cannot be opened with this secret.key (it was replaced, or
// the database comes from another installation).
var ErrUnreadable = errors.New("store: sealed value cannot be opened (another secret.key?)")

// PushSubscription is one browser's Web Push subscription (#610). P256dh and Auth are the browser's keys
// for the payload encryption, base64url as the browser gives them. The owner is the admin who subscribed
// (sign-in method and subject). Label names the browser and system for the Settings list (#611; empty for a
// row made before migration 0050). LastOKAt and LastError are the outcome of the last push (zero and empty
// until then).
type PushSubscription struct {
	Endpoint                  string
	P256dh, Auth              string
	OwnerMethod, OwnerSubject string
	Label                     string
	CreatedAt, LastOKAt       time.Time
	LastError                 string
}

// PushStore keeps the VAPID key and the Web Push subscriptions (migrations 0049 and 0050).
type PushStore interface {
	// VAPIDKey returns the opened private key: ErrNotFound when none was made yet, ErrUnreadable when the
	// stored one cannot be opened.
	VAPIDKey(ctx context.Context) (string, error)
	// ReplaceVAPIDKey stores key sealed and deletes every subscription, in one transaction: they were made
	// for the previous key.
	ReplaceVAPIDKey(ctx context.Context, key string) error
	// SavePushSubscription inserts the subscription or replaces the row of the same endpoint (keys, owner,
	// label, creation time; the outcome of the last push starts empty again).
	SavePushSubscription(ctx context.Context, s PushSubscription) error
	// DeletePushSubscription removes the row of endpoint; false when there was none.
	DeletePushSubscription(ctx context.Context, endpoint string) (bool, error)
	ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error)
	// RecordPushResult stores the outcome of a push: failure "" is a delivered push at at. A row that is
	// gone meanwhile is no error.
	RecordPushResult(ctx context.Context, endpoint string, at time.Time, failure string) error
}

func (s *SQLite) VAPIDKey(ctx context.Context) (string, error) {
	var sealed string
	err := s.db.QueryRowContext(ctx, "SELECT private_key FROM push_vapid WHERE id = 1").Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	key, err := s.box.Open(sealed)
	if err != nil {
		return "", ErrUnreadable
	}
	return key, nil
}

func (s *SQLite) ReplaceVAPIDKey(ctx context.Context, key string) error {
	sealed, err := s.box.Seal(key)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO push_vapid (id, private_key, created_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET private_key = excluded.private_key, created_at = excluded.created_at`, sealed, toMS(s.now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM push_subscriptions"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) SavePushSubscription(ctx context.Context, p PushSubscription) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subscriptions (endpoint, p256dh, auth, owner_method, owner_subject, label, created_at, last_ok_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, '')
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, owner_method = excluded.owner_method,
			owner_subject = excluded.owner_subject, label = excluded.label, created_at = excluded.created_at, last_ok_at = 0, last_error = ''`,
		p.Endpoint, p.P256dh, p.Auth, p.OwnerMethod, p.OwnerSubject, p.Label, toMS(s.now()))
	return err
}

func (s *SQLite) DeletePushSubscription(ctx context.Context, endpoint string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQLite) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT endpoint, p256dh, auth, owner_method, owner_subject, label, created_at, last_ok_at, last_error
		FROM push_subscriptions ORDER BY endpoint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		var created, ok int64
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth, &p.OwnerMethod, &p.OwnerSubject, &p.Label, &created, &ok, &p.LastError); err != nil {
			return nil, err
		}
		p.CreatedAt, p.LastOKAt = fromMS(created), optMS(ok)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) RecordPushResult(ctx context.Context, endpoint string, at time.Time, failure string) error {
	if failure == "" {
		_, err := s.db.ExecContext(ctx, "UPDATE push_subscriptions SET last_ok_at = ?, last_error = '' WHERE endpoint = ?", toMS(at), endpoint)
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE push_subscriptions SET last_error = ? WHERE endpoint = ?", failure, endpoint)
	return err
}
