package store

import (
	"context"
	"errors"
	"time"
)

// PrincipalRecord is the display name of a subject. Kind is "sso", "local" or "header".
//
// Writing (UpsertPrincipal, RecordLogin): DisplayName is the name captured at the sign-in and replaces
// the stored one; Custom and Provider are ignored, the chosen name has its own writer (SetCustomName).
// Reading (ListPrincipals): DisplayName is the name to show (Custom when the person chose one, else
// Provider), Provider the sign-in name, Custom the chosen name (empty: none).
type PrincipalRecord struct {
	Subject     string
	DisplayName string
	Provider    string
	Custom      string
	Kind        string
	UpdatedAt   time.Time
	// Role is the role of the last sign-in ("admin", "operator"; empty when
	// unknown) and LastLogin that sign-in (zero when unknown). Only
	// RecordLogin writes them.
	Role      string
	LastLogin time.Time
	// Email is the address of the last sign-in (empty when unknown); only
	// RecordLogin writes it.
	Email string
}

// PrincipalStore maps the stable subject that audit rows and jobs store to a
// name a human recognises. The subject stays the key; the name may change.
type PrincipalStore interface {
	// UpsertPrincipal inserts or updates the record of p.Subject.
	UpsertPrincipal(ctx context.Context, p PrincipalRecord) error
	ListPrincipals(ctx context.Context) ([]PrincipalRecord, error)
}

// UserDirectory is what the Users page reads and the sign-in writes.
type UserDirectory interface {
	// RecordLogin upserts p like UpsertPrincipal and also stores p.Role and
	// sets the last login to p.UpdatedAt (now when zero).
	RecordLogin(ctx context.Context, p PrincipalRecord) error
	ListPrincipals(ctx context.Context) ([]PrincipalRecord, error)
	// CountSessions returns the live web sessions per subject: not expired at
	// now and seen within idle.
	CountSessions(ctx context.Context, now time.Time, idle time.Duration) (map[string]int, error)
	// SetCustomName stores the display name a person chose for themselves (empty clears it, so the
	// sign-in name shows again). ErrNotFound when the subject was never recorded. Sign-ins never touch it.
	SetCustomName(ctx context.Context, subject, name string) error
	// ListSubjectSessions returns the live sessions of one subject (not
	// expired at now, seen within idle), most recently seen first.
	ListSubjectSessions(ctx context.Context, subject string, now time.Time, idle time.Duration) ([]Session, error)
	// DeleteSubjectSession ends the one session idHash if it belongs to
	// subject; false when no such session exists.
	DeleteSubjectSession(ctx context.Context, subject, idHash string) (bool, error)
}

func (s *SQLite) RecordLogin(ctx context.Context, p PrincipalRecord) error {
	at := toMS(s.orNow(p.UpdatedAt))
	_, err := s.db.ExecContext(ctx, `INSERT INTO principals (subject, display_name, kind, updated_at, role, last_login_at, email) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(subject) DO UPDATE SET display_name = excluded.display_name, kind = excluded.kind, updated_at = excluded.updated_at,
		role = excluded.role, last_login_at = excluded.last_login_at, email = excluded.email`,
		p.Subject, p.DisplayName, p.Kind, at, p.Role, at, p.Email)
	return err
}

func (s *SQLite) CountSessions(ctx context.Context, now time.Time, idle time.Duration) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, COUNT(*) FROM sessions WHERE expires_at > ? AND last_seen > ? GROUP BY subject`,
		toMS(now), toMS(now.Add(-idle)))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[string]int{}
	for rows.Next() {
		var sub string
		var n int
		if err := rows.Scan(&sub, &n); err != nil {
			return nil, err
		}
		out[sub] = n
	}
	return out, rows.Err()
}

func (s *SQLite) SetCustomName(ctx context.Context, subject, name string) error {
	n, err := rowsDeleted(s.db.ExecContext(ctx, `UPDATE principals SET custom_name = NULLIF(?, '') WHERE subject = ?`, name, subject))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) UpsertPrincipal(ctx context.Context, p PrincipalRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO principals (subject, display_name, kind, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(subject) DO UPDATE SET display_name = excluded.display_name, kind = excluded.kind, updated_at = excluded.updated_at`,
		p.Subject, p.DisplayName, p.Kind, toMS(s.orNow(p.UpdatedAt)))
	return err
}

func (s *SQLite) ListPrincipals(ctx context.Context) ([]PrincipalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject, COALESCE(NULLIF(custom_name, ''), display_name), display_name, COALESCE(custom_name, ''), kind, updated_at, role, last_login_at, email FROM principals ORDER BY subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []PrincipalRecord
	for rows.Next() {
		var p PrincipalRecord
		var at, login int64
		if err := rows.Scan(&p.Subject, &p.DisplayName, &p.Provider, &p.Custom, &p.Kind, &at, &p.Role, &login, &p.Email); err != nil {
			return nil, err
		}
		p.UpdatedAt = fromMS(at)
		if login > 0 {
			p.LastLogin = fromMS(login)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) ListSubjectSessions(ctx context.Context, subject string, now time.Time, idle time.Duration) ([]Session, error) {
	ids, err := s.subjectSessionIDs(ctx, subject, now, idle)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(ids))
	for _, id := range ids {
		sess, err := s.GetSession(ctx, id)
		if errors.Is(err, ErrNotFound) {
			continue // ended meanwhile
		}
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// subjectSessionIDs returns the IDs of the live sessions of subject, most
// recently seen first. GetSession then reads each (it opens the sealed token).
func (s *SQLite) subjectSessionIDs(ctx context.Context, subject string, now time.Time, idle time.Duration) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id_hash FROM sessions WHERE subject = ? AND expires_at > ? AND last_seen > ?
		ORDER BY last_seen DESC, created_at DESC`, subject, toMS(now), toMS(now.Add(-idle)))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *SQLite) DeleteSubjectSession(ctx context.Context, subject, idHash string) (bool, error) {
	n, err := rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM sessions WHERE subject = ? AND id_hash = ?", subject, idHash))
	return n > 0, err
}
