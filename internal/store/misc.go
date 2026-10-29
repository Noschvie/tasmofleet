package store

import (
	"context"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// --- health events ---

func (s *SQLite) AppendHealthEvent(ctx context.Context, e HealthEvent) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO health_events (device_id, at, from_state, to_state, source, detail) VALUES (?,?,?,?,?,?)",
		e.DeviceID, toMS(s.orNow(e.At)), string(e.From), string(e.To), string(e.Source), e.Detail)
	return mapErr(err)
}

// ListHealthEvents returns the newest events first; limit <= 0 means all.
func (s *SQLite) ListHealthEvents(ctx context.Context, id model.DeviceID, limit int) ([]HealthEvent, error) {
	q, args := "SELECT device_id, at, from_state, to_state, source, detail FROM health_events WHERE device_id=? ORDER BY at DESC, id DESC", []any{id}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []HealthEvent
	for rows.Next() {
		var e HealthEvent
		var at int64
		var from, to, src string
		if err := rows.Scan(&e.DeviceID, &at, &from, &to, &src, &e.Detail); err != nil {
			return nil, err
		}
		e.At, e.From, e.To, e.Source = fromMS(at), model.HealthState(from), model.HealthState(to), model.HealthSource(src)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) PruneHealthEvents(ctx context.Context, before time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM health_events WHERE at < ?", toMS(before)))
}

func rowsDeleted(res interface{ RowsAffected() (int64, error) }, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- artifacts ---

const artifactCols = "id, release, filename, chip_family, variant, language, safeboot, size, sha256, source, source_url, fetched_at, custom"

func scanArtifact(r scanner) (model.Artifact, error) {
	var a model.Artifact
	var fam string
	var safe int
	var fetched int64
	err := r.Scan(&a.ID, &a.Release, &a.Filename, &fam, &a.Variant, &a.Language, &safe, &a.Size, &a.SHA256, &a.Source, &a.SourceURL, &fetched, &a.Custom)
	a.Family, a.Safeboot, a.FetchedAt = model.ChipFamily(fam), safe != 0, fromMS(fetched)
	a.Release = strings.TrimPrefix(a.Release, customPrefix(a.Custom))
	return a, err
}

// PutArtifact inserts a. Artifacts are immutable: the same (release,
// filename) again is ErrConflict.
func (s *SQLite) PutArtifact(ctx context.Context, a model.Artifact) (model.ArtifactID, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO firmware_artifacts (release, filename, chip_family, variant, language, safeboot, size,
		sha256, source, source_url, fetched_at, custom) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		customPrefix(a.Custom)+a.Release, a.Filename, string(a.Family), a.Variant, a.Language, b2i(a.Safeboot), a.Size, a.SHA256, a.Source, a.SourceURL,
		toMS(s.orNow(a.FetchedAt)), a.Custom)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	return model.ArtifactID(id), err
}

func (s *SQLite) GetArtifact(ctx context.Context, id model.ArtifactID) (model.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, "SELECT "+artifactCols+" FROM firmware_artifacts WHERE id=?", id))
	return a, notFound(err)
}

func (s *SQLite) FindArtifact(ctx context.Context, release, filename string) (model.Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, "SELECT "+artifactCols+" FROM firmware_artifacts WHERE release=? AND filename=? AND custom=''", release, filename))
	return a, notFound(err)
}

// FindCustomArtifact returns the file of an upload (key = Artifact.Custom).
func (s *SQLite) FindCustomArtifact(ctx context.Context, custom, release, filename string) (model.Artifact, error) {
	if custom == "" {
		return model.Artifact{}, ErrNotFound
	}
	a, err := scanArtifact(s.db.QueryRowContext(ctx, "SELECT "+artifactCols+" FROM firmware_artifacts WHERE custom=? AND release=? AND filename=?", custom, customPrefix(custom)+release, filename))
	return a, notFound(err)
}

// customPrefix namespaces the release column of an upload: UNIQUE(release,
// filename) of migration 0001 stays, and an upload of the same release and
// file name as an upstream file (or another upload) gets the row
// "<key>~<release>", which no upstream release can equal ('~' is not a name
// character). The prefix is added on write and removed on read; nothing
// outside this file sees it.
func customPrefix(custom string) string {
	if custom == "" {
		return ""
	}
	return custom + "~"
}

// ListArtifacts returns all artifacts ordered by release, filename.
func (s *SQLite) ListArtifacts(ctx context.Context) ([]model.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+artifactCols+" FROM firmware_artifacts ORDER BY release, filename")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteArtifact(ctx context.Context, id model.ArtifactID) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM firmware_artifacts WHERE id=?", id))
}

// --- backups ---

const backupCols = "id, COALESCE(device_id, 0), device_name, device_mac, kind, taken_at, fw_version, path, size, sha256, decoder_version, note, kept, trigger, trigger_ref"

func scanBackup(r scanner) (model.Backup, error) {
	var b model.Backup
	var kind, trigger string
	var taken int64
	var kept int
	err := r.Scan(&b.ID, &b.DeviceID, &b.DeviceName, &b.DeviceMAC, &kind, &taken, &b.Version, &b.Path, &b.Size, &b.SHA256,
		&b.DecoderVersion, &b.Note, &kept, &trigger, &b.TriggerRef)
	b.Kind, b.TakenAt, b.Kept, b.Trigger = model.BackupKind(kind), fromMS(taken), kept != 0, model.BackupTrigger(trigger)
	return b, err
}

// PutBackup indexes b; an unknown device yields ErrNotFound. A backup always
// belongs to a device when it is taken (DeviceID 0 is refused); it only loses
// the device when the device is deleted and its backups are kept.
func (s *SQLite) PutBackup(ctx context.Context, b model.Backup) (model.BackupID, error) {
	if b.DeviceID == 0 {
		return 0, ErrNotFound
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO backups (device_id, device_name, device_mac, kind, taken_at, fw_version, path, size,
		sha256, decoder_version, note, kept, trigger, trigger_ref)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, b.DeviceID, b.DeviceName, b.DeviceMAC, string(b.Kind), toMS(s.orNow(b.TakenAt)), b.Version,
		b.Path, b.Size, b.SHA256, b.DecoderVersion, b.Note, b2i(b.Kept), string(b.Trigger), b.TriggerRef)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	return model.BackupID(id), err
}

func (s *SQLite) GetBackup(ctx context.Context, id model.BackupID) (model.Backup, error) {
	b, err := scanBackup(s.db.QueryRowContext(ctx, "SELECT "+backupCols+" FROM backups WHERE id=?", id))
	return b, notFound(err)
}

// ListBackups returns the device's backups, newest first.
func (s *SQLite) ListBackups(ctx context.Context, device model.DeviceID) ([]model.Backup, error) {
	return s.listBackups(ctx, "SELECT "+backupCols+" FROM backups WHERE device_id=? ORDER BY taken_at DESC, id DESC", device)
}

// ListOrphanBackups returns the backups whose device was deleted while they
// were kept, newest first.
func (s *SQLite) ListOrphanBackups(ctx context.Context) ([]model.Backup, error) {
	return s.listBackups(ctx, "SELECT "+backupCols+" FROM backups WHERE device_id IS NULL ORDER BY taken_at DESC, id DESC")
}

// ListAllBackups returns every backup row, device or orphan, by ID.
func (s *SQLite) ListAllBackups(ctx context.Context) ([]model.Backup, error) {
	return s.listBackups(ctx, "SELECT "+backupCols+" FROM backups ORDER BY id")
}

// UpdateBackupPath changes a row's path only if it still is oldPath, so a
// concurrent change is never overwritten.
func (s *SQLite) UpdateBackupPath(ctx context.Context, id model.BackupID, oldPath, newPath string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET path=? WHERE id=? AND path=?", newPath, id, oldPath)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// likeEscaper escapes the LIKE wildcards of a literal (ESCAPE '\').
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// BackupDirOwners lists the devices with backups below backups/<dir>/. SQLite's
// LIKE ignores ASCII case, which is what a directory collision needs.
func (s *SQLite) BackupDirOwners(ctx context.Context, dir string) ([]model.DeviceID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT COALESCE(device_id, 0) FROM backups WHERE path LIKE ? ESCAPE '\'`,
		"backups/"+likeEscaper.Replace(dir)+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.DeviceID
	for rows.Next() {
		var id model.DeviceID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *SQLite) listBackups(ctx context.Context, q string, args ...any) ([]model.Backup, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteBackup(ctx context.Context, id model.BackupID) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM backups WHERE id=?", id))
}

// SetBackupsKept sets or clears "kept" on the given rows in one transaction
// (the files of one run change together). An id that does not exist is
// ErrNotFound and nothing changes.
func (s *SQLite) SetBackupsKept(ctx context.Context, ids []model.BackupID, kept bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	for _, id := range ids {
		if err := affected(tx.ExecContext(ctx, "UPDATE backups SET kept=? WHERE id=?", b2i(kept), id)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddBackupAttempt records a failed backup.
func (s *SQLite) AddBackupAttempt(ctx context.Context, a model.BackupAttempt) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO backup_attempts (device_id, at, kind, ref, fw_version, reason) VALUES (?,?,?,?,?,?)",
		a.DeviceID, toMS(s.orNow(a.At)), string(a.Trigger), a.TriggerRef, a.Version, a.Reason)
	if err != nil {
		return 0, mapErr(err)
	}
	return res.LastInsertId()
}

// ListBackupAttempts returns the device's failed backups, newest first;
// limit <= 0 means all.
func (s *SQLite) ListBackupAttempts(ctx context.Context, device model.DeviceID, limit int) ([]model.BackupAttempt, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, device_id, at, kind, ref, fw_version, reason FROM backup_attempts WHERE device_id=? ORDER BY at DESC, id DESC LIMIT ?", device, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.BackupAttempt
	for rows.Next() {
		var a model.BackupAttempt
		var at int64
		var kind string
		if err := rows.Scan(&a.ID, &a.DeviceID, &at, &kind, &a.TriggerRef, &a.Version, &a.Reason); err != nil {
			return nil, err
		}
		a.At, a.Trigger = fromMS(at), model.BackupTrigger(kind)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneBackupAttempts deletes failed attempts older than before (they share
// the audit retention).
func (s *SQLite) PruneBackupAttempts(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backup_attempts WHERE at < ?", toMS(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- audit ---

// AppendAudit adds one row. An empty Detail is stored as "{}".
func (s *SQLite) AppendAudit(ctx context.Context, e model.AuditEvent) error {
	detail := e.Detail
	if detail == "" {
		detail = "{}"
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO audit_log (at, actor, action, target, detail, result, remote_addr) VALUES (?,?,?,?,?,?,?)",
		toMS(s.orNow(e.At)), e.Actor, e.Action, e.Target, detail, e.Result, e.RemoteAddr)
	return err
}

// auditTargetAlias maps "batch:N" to "transaction:N" and back; any other
// target is its own alias.
func auditTargetAlias(t string) string {
	if id, ok := strings.CutPrefix(t, "batch:"); ok {
		return "transaction:" + id
	}
	if id, ok := strings.CutPrefix(t, "transaction:"); ok {
		return "batch:" + id
	}
	return t
}

// ListAudit returns matching rows newest first; Limit 0 means no limit.
func (s *SQLite) ListAudit(ctx context.Context, f AuditFilter) ([]model.AuditEvent, error) {
	var where []string
	var args []any
	if f.Actor != "" {
		where = append(where, "actor=?")
		args = append(args, f.Actor)
	}
	if len(f.Actors) > 0 {
		where = append(where, "actor IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Actors)), ",")+")")
		for _, a := range f.Actors {
			args = append(args, a)
		}
	}
	if f.Target != "" {
		// Audit history is never rewritten: rows written before the rename say
		// "batch:N", new ones "transaction:N". Both spellings are one subject.
		where = append(where, "target IN (?,?)")
		args = append(args, f.Target, auditTargetAlias(f.Target))
	}
	if f.ActionPrefix != "" {
		// substr instead of LIKE: "_" and "%" in an action name are not wildcards.
		where = append(where, "substr(action, 1, length(?)) = ?")
		args = append(args, f.ActionPrefix, f.ActionPrefix)
	}
	if f.Search != "" || len(f.SearchTargets) > 0 || len(f.SearchActors) > 0 {
		var or []string
		if f.Search != "" {
			// instr instead of LIKE for the same reason; lower() folds ASCII,
			// which is what actors, actions and targets are made of.
			or = append(or, "instr(lower(actor), lower(?)) > 0", "instr(lower(action), lower(?)) > 0", "instr(lower(target), lower(?)) > 0")
			args = append(args, f.Search, f.Search, f.Search)
		}
		if len(f.SearchTargets) > 0 {
			or = append(or, "target IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.SearchTargets)), ",")+")")
			for _, t := range f.SearchTargets {
				args = append(args, t)
			}
		}
		if len(f.SearchActors) > 0 {
			or = append(or, "actor IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.SearchActors)), ",")+")")
			for _, a := range f.SearchActors {
				args = append(args, a)
			}
		}
		where = append(where, "("+strings.Join(or, " OR ")+")")
	}
	if !f.Since.IsZero() {
		where = append(where, "at >= ?")
		args = append(args, toMS(f.Since))
	}
	q := "SELECT id, at, actor, action, target, detail, result, remote_addr FROM audit_log"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // fragments are constants; values are bound
	}
	q += " ORDER BY at DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.AuditEvent
	for rows.Next() {
		var e model.AuditEvent
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.Result, &e.RemoteAddr); err != nil {
			return nil, err
		}
		e.At = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditFacets returns the distinct actors and actions since the given time.
func (s *SQLite) AuditFacets(ctx context.Context, since time.Time) (actors, actions []string, err error) {
	distinct := func(col string) ([]string, error) {
		rows, err := s.db.QueryContext(ctx, "SELECT DISTINCT "+col+" FROM audit_log WHERE at >= ? ORDER BY "+col, toMS(since)) //nolint:gosec // col is one of two constants
		if err != nil {
			return nil, err
		}
		defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	if actors, err = distinct("actor"); err != nil {
		return nil, nil, err
	}
	if actions, err = distinct("action"); err != nil {
		return nil, nil, err
	}
	return actors, actions, nil
}

func (s *SQLite) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, "DELETE FROM audit_log WHERE at < ?", toMS(before)))
}

// --- custom firmware ---

// PutCustomMeta records who uploaded the artifact m.ArtifactID and under which
// label. The artifact must exist (foreign key).
func (s *SQLite) PutCustomMeta(ctx context.Context, m CustomMeta) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO firmware_custom (artifact_id, label, original_name, uploaded_by, uploaded_at)
		VALUES (?,?,?,?,?)`, m.ArtifactID, m.Label, m.OriginalName, m.UploadedBy, toMS(s.orNow(m.UploadedAt)))
	return mapErr(err)
}

// ListCustomMeta returns the provenance of every uploaded image, newest first.
func (s *SQLite) ListCustomMeta(ctx context.Context) ([]CustomMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT artifact_id, label, original_name, uploaded_by, uploaded_at
		FROM firmware_custom ORDER BY uploaded_at DESC, artifact_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []CustomMeta
	for rows.Next() {
		var m CustomMeta
		var at int64
		if err := rows.Scan(&m.ArtifactID, &m.Label, &m.OriginalName, &m.UploadedBy, &at); err != nil {
			return nil, err
		}
		m.UploadedAt = fromMS(at)
		out = append(out, m)
	}
	return out, rows.Err()
}
