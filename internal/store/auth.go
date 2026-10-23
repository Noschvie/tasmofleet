package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// MaxDeviceCookiesPerUser bounds the device_cookies table: the oldest
// cookies of a user are evicted beyond it.
const MaxDeviceCookiesPerUser = 50

// --- sessions ---

func (s *SQLite) sealToken(tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	return s.box.Seal(tok)
}

func (s *SQLite) openToken(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	return s.box.Open(sealed)
}

// CreateSession stores sess; a duplicate IDHash is ErrConflict, a local
// session for a changed or disabled account is ErrStaleActor.
func (s *SQLite) CreateSession(ctx context.Context, sess Session) error {
	p, err := json.Marshal(sess.Principal)
	if err != nil {
		return err
	}
	rt, err := s.sealToken(sess.RefreshToken)
	if err != nil {
		return fmt.Errorf("store: seal refresh token: %w", err)
	}
	// A local session is stored only while its account is enabled and still at
	// the password generation the login verified: the check and the insert are
	// one statement, so a session created concurrently with a password change
	// or disable can never exist.
	cond, condArgs := "1", []any(nil)
	if sess.Principal.Method == "local" {
		cond = "EXISTS (SELECT 1 FROM local_users u WHERE u.username=? AND u.password_gen=? AND u.disabled=0)"
		condArgs = []any{sess.Principal.Name, sess.PasswordGen}
	}
	const insert = `INSERT INTO sessions (id_hash, principal, subject, csrf_token, id_token, refresh_token, rechecked_at,
		password_gen, created_at, last_seen, expires_at) SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE `
	res, err := s.db.ExecContext(ctx, insert+cond, //nolint:gosec // G202: cond is a constant fragment; every value is bound
		append([]any{sess.IDHash, string(p), sess.Principal.Subject, sess.CSRFToken, sess.IDToken, rt, toMS(sess.RecheckedAt), sess.PasswordGen,
			toMS(sess.CreatedAt), toMS(sess.LastSeen), toMS(sess.ExpiresAt)}, condArgs...)...)
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrStaleActor
	}
	return nil
}

// GetSession looks a session up by the hash of its cookie. It does not check
// expiry: idle and absolute limits are policy in package auth.
func (s *SQLite) GetSession(ctx context.Context, idHash string) (Session, error) {
	var sess Session
	var p, rt string
	var created, seen, exp, rechecked int64
	err := s.db.QueryRowContext(ctx, `SELECT id_hash, principal, csrf_token, id_token, refresh_token, rechecked_at,
		password_gen, created_at, last_seen, expires_at FROM sessions WHERE id_hash=?`, idHash).
		Scan(&sess.IDHash, &p, &sess.CSRFToken, &sess.IDToken, &rt, &rechecked, &sess.PasswordGen, &created, &seen, &exp)
	if err != nil {
		return Session{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(p), &sess.Principal); err != nil {
		return Session{}, fmt.Errorf("store: session principal: %w", err)
	}
	if sess.RefreshToken, err = s.openToken(rt); err != nil {
		return Session{}, fmt.Errorf("store: open refresh token: %w", err)
	}
	sess.RecheckedAt, sess.CreatedAt, sess.LastSeen, sess.ExpiresAt = fromMS(rechecked), fromMS(created), fromMS(seen), fromMS(exp)
	return sess, nil
}

func (s *SQLite) TouchSession(ctx context.Context, idHash string, at time.Time) error {
	return affected(s.db.ExecContext(ctx, "UPDATE sessions SET last_seen=? WHERE id_hash=?", toMS(at), idHash))
}

// UpdateSessionRecheck stores the outcome of an OIDC re-check.
func (s *SQLite) UpdateSessionRecheck(ctx context.Context, sess Session) error {
	p, err := json.Marshal(sess.Principal)
	if err != nil {
		return err
	}
	rt, err := s.sealToken(sess.RefreshToken)
	if err != nil {
		return fmt.Errorf("store: seal refresh token: %w", err)
	}
	return affected(s.db.ExecContext(ctx, "UPDATE sessions SET principal=?, id_token=?, refresh_token=?, rechecked_at=? WHERE id_hash=?",
		string(p), sess.IDToken, rt, toMS(sess.RecheckedAt), sess.IDHash))
}

// DeleteSession is idempotent: deleting a missing session is not an error
// (double logout, expiry race).
func (s *SQLite) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id_hash=?", idHash)
	return err
}

func (s *SQLite) DeleteUserSessions(ctx context.Context, subject string) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM sessions WHERE subject=?", subject))
}

// DeleteExpiredSessions removes sessions with expires_at <= now.
func (s *SQLite) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", toMS(now)))
}

// --- local users ---

var errBadRole = errors.New("store: local user role must be operator or admin")

func roleToDB(r model.Role) (string, error) {
	switch r {
	case model.RoleOperator:
		return "operator", nil
	case model.RoleAdmin:
		return "admin", nil
	case model.RoleNone:
	}
	return "", errBadRole
}

func roleFromDB(s string) model.Role {
	switch s {
	case "admin":
		return model.RoleAdmin
	case "operator":
		return model.RoleOperator
	}
	return model.RoleNone // unreachable: the column has a CHECK constraint
}

const localUserCols = "username, password_hash, updated_at, role, disabled, must_change_password, password_gen, " +
	"display_name, email, otp_expires_at, created_at, created_by, initial_setup"

// optMS maps the "unknown/none" value 0 to the zero time.
func optMS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return fromMS(ms)
}

// optToMS is optMS reversed.
func optToMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return toMS(t)
}

func scanLocalUser(r scanner) (LocalUser, error) {
	var u LocalUser
	var at, otp, created int64
	var role string
	var dis, must, setup int
	err := r.Scan(&u.Username, &u.PasswordHash, &at, &role, &dis, &must, &u.PasswordGen,
		&u.DisplayName, &u.Email, &otp, &created, &u.CreatedBy, &setup)
	u.InitialSetup = setup != 0
	u.UpdatedAt, u.Role, u.Disabled, u.MustChangePassword = fromMS(at), roleFromDB(role), dis != 0, must != 0
	u.OTPExpiresAt, u.CreatedAt = optMS(otp), optMS(created)
	return u, err
}

func (s *SQLite) GetLocalUser(ctx context.Context, username string) (LocalUser, error) {
	u, err := scanLocalUser(s.db.QueryRowContext(ctx, "SELECT "+localUserCols+" FROM local_users WHERE username=?", username))
	return u, notFound(err)
}

func (s *SQLite) ListLocalUsers(ctx context.Context) ([]LocalUser, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+localUserCols+" FROM local_users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []LocalUser
	for rows.Next() {
		u, err := scanLocalUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// HasLocalUsers reports whether any local account exists.
func (s *SQLite) HasLocalUsers(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM local_users)").Scan(&n)
	return n == 1, err
}

// actorCond is the SQL condition "the acting admin is still valid" and its
// arguments; the zero Actor (host CLI) imposes none.
func actorCond(by Actor) (string, []any) {
	if by.Username == "" {
		return "1", nil
	}
	return `EXISTS (SELECT 1 FROM local_users act WHERE act.username=? AND act.role='admin' AND act.disabled=0 AND act.password_gen=?)`,
		[]any{by.Username, by.PasswordGen}
}

// actorStale reports whether a statement that matched nothing did so because
// of the actor.
func (s *SQLite) actorStale(ctx context.Context, by Actor) (bool, error) {
	if by.Username == "" {
		return false, nil
	}
	cond, args := actorCond(by)
	var ok int
	if err := s.db.QueryRowContext(ctx, "SELECT "+cond, args...).Scan(&ok); err != nil {
		return false, err
	}
	return ok == 0, nil
}

// AddLocalUser inserts u with PasswordGen 1. A plain INSERT: an existing
// name is ErrConflict and is never overwritten.
func (s *SQLite) AddLocalUser(ctx context.Context, u LocalUser, by Actor) error {
	role, err := roleToDB(u.Role)
	if err != nil {
		return err
	}
	cond, cargs := actorCond(by)
	args := append([]any{u.Username, u.PasswordHash, toMS(s.orNow(u.UpdatedAt)), role, b2i(u.Disabled), b2i(u.MustChangePassword),
		u.DisplayName, u.Email, optToMS(u.OTPExpiresAt), toMS(s.orNow(u.CreatedAt)), u.CreatedBy, b2i(u.InitialSetup)}, cargs...)
	const insert = `INSERT INTO local_users (username, password_hash, updated_at, role, disabled, must_change_password, password_gen,
		display_name, email, otp_expires_at, created_at, created_by, initial_setup)
		SELECT ?,?,?,?,?,?,1,?,?,?,?,?,? WHERE `
	res, err := s.db.ExecContext(ctx, insert+cond, args...) //nolint:gosec // G202: cond is a constant fragment; every value is bound
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return ErrStaleActor
	}
	return nil
}

// SetLocalUser creates or replaces the user, incrementing PasswordGen of an
// existing one. It does not check the last-admin rule and is meant for
// seeding data in tests; the application uses the narrower methods above.
func (s *SQLite) SetLocalUser(ctx context.Context, u LocalUser) error {
	role, err := roleToDB(u.Role)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_users (username, password_hash, updated_at, role, disabled, must_change_password)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash, updated_at=excluded.updated_at,
		role=excluded.role, disabled=excluded.disabled, must_change_password=excluded.must_change_password,
		password_gen=password_gen+1, initial_setup=0`,
		u.Username, u.PasswordHash, toMS(s.orNow(u.UpdatedAt)), role, b2i(u.Disabled), b2i(u.MustChangePassword))
	return err
}

// guarded runs one conditional statement; if it matched nothing, the actor
// lost its rights (ErrStaleActor), or the account is missing (ErrNotFound),
// or the last-admin rule protects it (ErrLastAdmin).
func (s *SQLite) guarded(ctx context.Context, username string, by Actor, res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if stale, err := s.actorStale(ctx, by); err != nil {
		return err
	} else if stale {
		return ErrStaleActor
	}
	if _, err := s.GetLocalUser(ctx, username); err != nil {
		return err
	}
	return ErrLastAdmin
}

// keepsAnAdmin is the SQL condition "the row stays valid for the last-admin
// rule": it is not an enabled admin, or another enabled admin exists.
const keepsAnAdmin = `(role <> 'admin' OR disabled = 1 OR EXISTS
	(SELECT 1 FROM local_users o WHERE o.username <> local_users.username AND o.role = 'admin' AND o.disabled = 0))`

func (s *SQLite) SetLocalPassword(ctx context.Context, username, hash string, at time.Time, mustChange bool, by Actor) error {
	return s.setLocalPassword(ctx, username, hash, at, mustChange, time.Time{}, by)
}

func (s *SQLite) SetLocalOneTimePassword(ctx context.Context, username, hash string, at, expires time.Time, by Actor) error {
	if expires.IsZero() {
		return errors.New("store: a one-time password needs an expiry")
	}
	return s.setLocalPassword(ctx, username, hash, at, true, expires, by)
}

func (s *SQLite) setLocalPassword(ctx context.Context, username, hash string, at time.Time, mustChange bool, otpExpires time.Time, by Actor) error {
	cond, cargs := actorCond(by)
	const update = `UPDATE local_users SET password_hash=?, updated_at=?, must_change_password=?, otp_expires_at=?,
		password_gen=password_gen+1, initial_setup=0 WHERE username=? AND `
	res, err := s.db.ExecContext(ctx, update+cond, append([]any{hash, toMS(s.orNow(at)), b2i(mustChange), optToMS(otpExpires), username}, cargs...)...) //nolint:gosec // G202: cond is a constant fragment; every value is bound
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 { //nolint:errcheck // sqlite always knows
		return nil
	}
	if stale, err := s.actorStale(ctx, by); err != nil {
		return err
	} else if stale {
		return ErrStaleActor
	}
	_, err = s.GetLocalUser(ctx, username)
	if err == nil {
		err = ErrNotFound // matched nothing although it exists: it changed under us
	}
	return err
}

func (s *SQLite) ChangeOwnPassword(ctx context.Context, username string, expectGen int64, hash string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE local_users SET password_hash=?, updated_at=?, must_change_password=0, otp_expires_at=0,
		password_gen=password_gen+1, initial_setup=0 WHERE username=? AND password_gen=? AND disabled=0`, hash, toMS(s.orNow(at)), username, expectGen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // sqlite always knows
		return ErrStaleActor
	}
	return nil
}

func (s *SQLite) SetLocalProfile(ctx context.Context, username, displayName, email string, by Actor) error {
	cond, cargs := actorCond(by)
	q := "UPDATE local_users SET display_name=?, email=? WHERE username=? AND " + cond //nolint:gosec // G202: cond is a constant fragment; every value is bound
	res, err := s.db.ExecContext(ctx, q, append([]any{displayName, email, username}, cargs...)...)
	return s.guardedNoLast(ctx, username, by, res, err)
}

func (s *SQLite) SetOwnProfile(ctx context.Context, username string, expectGen int64, displayName, email string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE local_users SET display_name=?, email=? WHERE username=? AND password_gen=? AND disabled=0`,
		displayName, email, username, expectGen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // sqlite always knows
		return ErrStaleActor
	}
	return nil
}

func (s *SQLite) SetLocalRole(ctx context.Context, username string, role model.Role, by Actor) error {
	r, err := roleToDB(role)
	if err != nil {
		return err
	}
	cond, cargs := actorCond(by)
	q := `UPDATE local_users SET role=? WHERE username=? AND ` + cond + ` AND (?='admin' OR ` + keepsAnAdmin + `)` //nolint:gosec // G202: cond and keepsAnAdmin are constant fragments; every value is bound
	res, err := s.db.ExecContext(ctx, q,
		append(append([]any{r, username}, cargs...), r)...)
	return s.guarded(ctx, username, by, res, err)
}

func (s *SQLite) SetLocalDisabled(ctx context.Context, username string, disabled bool, by Actor) error {
	cond, cargs := actorCond(by)
	if !disabled {
		q := "UPDATE local_users SET disabled=0 WHERE username=? AND " + cond //nolint:gosec // G202: cond and keepsAnAdmin are constant fragments; every value is bound
		res, err := s.db.ExecContext(ctx, q, append([]any{username}, cargs...)...)
		return s.guardedNoLast(ctx, username, by, res, err)
	}
	q := `UPDATE local_users SET disabled=1, password_gen=password_gen+1, initial_setup=0 WHERE username=? AND ` + cond + ` AND ` + keepsAnAdmin //nolint:gosec // G202: cond and keepsAnAdmin are constant fragments; every value is bound
	res, err := s.db.ExecContext(ctx, q,
		append([]any{username}, cargs...)...)
	return s.guarded(ctx, username, by, res, err)
}

// guardedNoLast is guarded for statements the last-admin rule does not cover.
func (s *SQLite) guardedNoLast(ctx context.Context, username string, by Actor, res sql.Result, err error) error {
	if err := s.guarded(ctx, username, by, res, err); !errors.Is(err, ErrLastAdmin) {
		return err
	}
	return ErrNotFound // unreachable in practice: the account vanished between statement and diagnosis
}

func (s *SQLite) DeleteLocalUser(ctx context.Context, username string, by Actor) error {
	cond, cargs := actorCond(by)
	q := `DELETE FROM local_users WHERE username=? AND ` + cond + ` AND ` + keepsAnAdmin //nolint:gosec // G202: cond and keepsAnAdmin are constant fragments; every value is bound
	res, err := s.db.ExecContext(ctx, q, append([]any{username}, cargs...)...)
	if err := s.guarded(ctx, username, by, res, err); err != nil {
		return err
	}
	// The interface preferences of a removed account go with it (a later account
	// of the same name must not inherit them). Subject = "local:" + username.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_prefs WHERE user_key = ?`, "local:"+username); err != nil {
		return fmt.Errorf("account removed, its preferences were not: %w", err)
	}
	return nil
}

// --- device cookies ---

func (s *SQLite) PutDeviceCookie(ctx context.Context, idHash, username string, issued time.Time, passwordGen int64) error {
	// Same rule as for sessions: recorded only for the generation the login
	// verified, in one statement with the check.
	res, err := s.db.ExecContext(ctx, `INSERT INTO device_cookies (id_hash, username, issued_at, password_gen)
		SELECT ?,?,?,? WHERE EXISTS (SELECT 1 FROM local_users u WHERE u.username=? AND u.password_gen=? AND u.disabled=0)`,
		idHash, username, toMS(s.orNow(issued)), passwordGen, username, passwordGen)
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrStaleActor
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM device_cookies WHERE username=? AND id_hash NOT IN
		(SELECT id_hash FROM device_cookies WHERE username=? ORDER BY issued_at DESC, rowid DESC LIMIT ?)`,
		username, username, MaxDeviceCookiesPerUser)
	return err
}

func (s *SQLite) HasDeviceCookie(ctx context.Context, idHash, username string, passwordGen int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM device_cookies WHERE id_hash=? AND username=? AND password_gen=?)", idHash, username, passwordGen).Scan(&n)
	return n == 1, err
}

func (s *SQLite) DeleteDeviceCookie(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM device_cookies WHERE id_hash=?", idHash)
	return err
}

func (s *SQLite) DeleteUserDeviceCookies(ctx context.Context, username string) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM device_cookies WHERE username=?", username))
}

func (s *SQLite) DeleteDeviceCookiesBefore(ctx context.Context, issued time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM device_cookies WHERE issued_at < ?", toMS(issued)))
}
