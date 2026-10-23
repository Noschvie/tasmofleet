package store

import (
	"context"
	"time"
)

// SystemSetting is one stored operational setting. Secret values are
// encrypted at rest; LoadSystemSettings returns them opened.
type SystemSetting struct {
	Key    string
	Value  string
	Secret bool
	// Unreadable: a sealed value that could not be decrypted; Value is empty.
	Unreadable bool
	// Target is what a secret was entered for (scheme://host:port); empty for plain settings.
	Target    string
	UpdatedBy string
	UpdatedAt time.Time
}

// SystemSettingChange is one write of SaveSystemSettings.
type SystemSettingChange struct {
	Key    string
	Value  string
	Secret bool   // seal the value
	Target string // secrets: the target the value was entered for
	Delete bool   // remove the stored value (back to default)
}

// PolicyRuleVersion is one saved state of the managed policy rules.
type PolicyRuleVersion struct {
	ID       int64
	Rules    string // JSON array, POLICY_RULES format
	Note     string
	Actor    string
	RevertOf int64
	At       time.Time
}

// SystemSettingsStore persists the settings edited in the web UI and the
// versions of the managed policy rules.
type SystemSettingsStore interface {
	LoadSystemSettings(ctx context.Context) (map[string]SystemSetting, error)
	// SaveSystemSettings applies all changes in one transaction.
	SaveSystemSettings(ctx context.Context, actor string, changes []SystemSettingChange) error

	AddPolicyRuleVersion(ctx context.Context, v PolicyRuleVersion) (int64, error)
	// CurrentPolicyRules returns the newest version, ErrNotFound if none.
	CurrentPolicyRules(ctx context.Context) (PolicyRuleVersion, error)
	GetPolicyRuleVersion(ctx context.Context, id int64) (PolicyRuleVersion, error)
	// ListPolicyRuleVersions returns the newest versions first.
	ListPolicyRuleVersions(ctx context.Context, limit int) ([]PolicyRuleVersion, error)
}

func (s *SQLite) LoadSystemSettings(ctx context.Context) (map[string]SystemSetting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, sealed, target, updated_by, updated_at FROM system_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[string]SystemSetting{}
	for rows.Next() {
		var st SystemSetting
		var sealed int
		var at int64
		if err := rows.Scan(&st.Key, &st.Value, &sealed, &st.Target, &st.UpdatedBy, &at); err != nil {
			return nil, err
		}
		st.Secret, st.UpdatedAt = sealed != 0, fromMS(at)
		if st.Secret {
			if opened, oerr := s.box.Open(st.Value); oerr != nil {
				st.Value, st.Unreadable = "", true
			} else {
				st.Value = opened
			}
		}
		out[st.Key] = st
	}
	return out, rows.Err()
}

func (s *SQLite) SaveSystemSettings(ctx context.Context, actor string, changes []SystemSettingChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	now := toMS(s.now())
	for _, c := range changes {
		if c.Delete {
			if _, err := tx.ExecContext(ctx, `DELETE FROM system_settings WHERE key=?`, c.Key); err != nil {
				return err
			}
			continue
		}
		val := c.Value
		if c.Secret {
			if val, err = s.box.Seal(val); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_settings (key, value, sealed, target, updated_by, updated_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, sealed=excluded.sealed, target=excluded.target, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
			c.Key, val, b2i(c.Secret), c.Target, actor, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const policyVersionCols = "id, rules, note, actor, revert_of, created_at"

func scanPolicyVersion(r scanner) (PolicyRuleVersion, error) {
	var v PolicyRuleVersion
	var at int64
	err := r.Scan(&v.ID, &v.Rules, &v.Note, &v.Actor, &v.RevertOf, &at)
	v.At = fromMS(at)
	return v, err
}

func (s *SQLite) AddPolicyRuleVersion(ctx context.Context, v PolicyRuleVersion) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO policy_rule_versions (rules, note, actor, revert_of, created_at) VALUES (?,?,?,?,?)`,
		v.Rules, v.Note, v.Actor, v.RevertOf, toMS(s.now()))
	if err != nil {
		return 0, mapErr(err)
	}
	return res.LastInsertId()
}

func (s *SQLite) CurrentPolicyRules(ctx context.Context) (PolicyRuleVersion, error) {
	v, err := scanPolicyVersion(s.db.QueryRowContext(ctx, "SELECT "+policyVersionCols+" FROM policy_rule_versions ORDER BY id DESC LIMIT 1"))
	return v, notFound(err)
}

func (s *SQLite) GetPolicyRuleVersion(ctx context.Context, id int64) (PolicyRuleVersion, error) {
	v, err := scanPolicyVersion(s.db.QueryRowContext(ctx, "SELECT "+policyVersionCols+" FROM policy_rule_versions WHERE id=?", id))
	return v, notFound(err)
}

func (s *SQLite) ListPolicyRuleVersions(ctx context.Context, limit int) ([]PolicyRuleVersion, error) {
	q, args := "SELECT "+policyVersionCols+" FROM policy_rule_versions ORDER BY id DESC", []any(nil)
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []PolicyRuleVersion
	for rows.Next() {
		v, err := scanPolicyVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
