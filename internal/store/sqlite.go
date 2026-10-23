package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nofuturekid/tasmofleet/internal/secretbox"
)

// Migrations are applied in lexical file order; PRAGMA user_version holds
// the number of the last applied file. Files are never edited after they
// are merged; a change is a new file.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// ErrSchemaTooNew is returned by Open when the database was written by a
// newer tasmofleet than this binary. Downgrades are not supported.
var ErrSchemaTooNew = errors.New("store: database schema is newer than this binary")

// keepPreMigrationCopies is how many <db>.pre-<N> files survive.
const keepPreMigrationCopies = 3

// SQLite implements Store. Open configures WAL, foreign_keys=ON,
// busy_timeout=5000 and a single writer connection.
type SQLite struct {
	db  *sql.DB
	box secretbox.Box
	now func() time.Time
}

// Open opens (creating if needed) the database at path, applies pending
// migrations and returns the store. box encrypts device passwords.
func Open(path string, box secretbox.Box) (*SQLite, error) {
	if box == nil {
		return nil, errors.New("store: secretbox is required")
	}
	if err := prepareFiles(path); err != nil {
		return nil, err
	}
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := migrate(db, path, Migrations); err != nil {
		db.Close() //nolint:errcheck // the migration error is the one to report
		return nil, err
	}
	// -wal and -shm exist now; SQLite derives their mode from the DB file,
	// but verify instead of trusting that.
	if err := repairModes(path); err != nil {
		db.Close() //nolint:errcheck // the permission error is the one to report
		return nil, err
	}
	return &SQLite{db: db, box: box, now: time.Now}, nil
}

// prepareFiles makes sure the database never exists with the process umask:
// the parent directory is created 0700 if missing (an existing one is left
// alone, so a stricter mode is never loosened), the database file is created
// 0600 before SQLite sees it, and existing files with group/other bits are
// repaired.
func prepareFiles(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return fmt.Errorf("store: create data directory: %w", err)
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // operator-configured database path
	switch {
	case err == nil:
		if err := f.Close(); err != nil { // an empty file is a valid new SQLite database
			return fmt.Errorf("store: create database file: %w", err)
		}
	case errors.Is(err, fs.ErrExist):
	default:
		return fmt.Errorf("store: create database file: %w", err)
	}
	return repairModes(file)
}

// repairModes removes group/other permissions from the database and its
// -wal/-shm files (they hold session hashes, sealed device passwords and the
// audit log) and logs a warning for every file it had to fix.
func repairModes(file string) error {
	for _, p := range []string{file, file + "-wal", file + "-shm"} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("store: stat %s: %w", p, err)
		}
		if fi.Mode().Perm()&0o077 == 0 {
			continue
		}
		slog.Warn("store: database file was accessible to group/other, restricting to owner", "path", p, "mode", fi.Mode().Perm().String())
		if err := os.Chmod(p, fi.Mode().Perm()&^0o077); err != nil {
			return fmt.Errorf("store: restrict %s: %w", p, err)
		}
	}
	return nil
}

func openDB(file string) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: file}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", file, err)
	}
	// One connection: a single writer removes SQLITE_BUSY handling entirely.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(context.Background()); err != nil {
		db.Close() //nolint:errcheck // the ping error is the one to report
		return nil, fmt.Errorf("store: open %s: %w", file, err)
	}
	return db, nil
}

type migration struct {
	n    int
	name string
	sql  string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []migration
	for i, f := range files {
		base := path.Base(f)
		num, _, ok := strings.Cut(base, "_")
		n, err := strconv.Atoi(num)
		if !ok || err != nil {
			return nil, fmt.Errorf("store: migration %q must be named NNNN_name.sql", base)
		}
		if n != i+1 {
			return nil, fmt.Errorf("store: migration %q has number %d, expected %d (gap or duplicate)", base, n, i+1)
		}
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{n: n, name: base, sql: string(b)})
	}
	return out, nil
}

func migrate(db *sql.DB, file string, fsys fs.FS) error {
	ctx := context.Background() // Open has no context; migrations run once at start
	migs, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	var cur int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if cur > len(migs) {
		return fmt.Errorf("%w (database has version %d, this binary knows %d); upgrade tasmofleet or restore a .pre-N copy", ErrSchemaTooNew, cur, len(migs))
	}
	if cur == len(migs) {
		return nil
	}
	// A brand-new database has nothing worth preserving.
	if cur > 0 {
		if err := preMigrationCopy(ctx, db, file, len(migs)); err != nil {
			return err
		}
	}
	for _, m := range migs[cur:] {
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("store: apply %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	// PRAGMA does not take bound parameters; m.n is an int we parsed ourselves.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.n)); err != nil {
		return err
	}
	return tx.Commit()
}

// preMigrationCopy writes <db>.pre-<target> (the state before migrating to
// version target) with VACUUM INTO, which is consistent under WAL, and
// prunes all but the newest copies.
func preMigrationCopy(ctx context.Context, db *sql.DB, file string, target int) error {
	dst := file + ".pre-" + strconv.Itoa(target)
	// VACUUM INTO refuses an existing target, so the copy is written inside a
	// fresh 0700 directory (MkdirTemp) and renamed into place; it is never
	// visible with a looser mode.
	tmp, err := os.MkdirTemp(filepath.Dir(file), ".premigrate-")
	if err != nil {
		return fmt.Errorf("store: pre-migration copy: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // scratch dir, best effort
	part := filepath.Join(tmp, "copy.db")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", part); err != nil {
		return fmt.Errorf("store: pre-migration copy: %w", err)
	}
	if err := os.Chmod(part, 0o600); err != nil {
		return fmt.Errorf("store: pre-migration copy: %w", err)
	}
	if err := os.Rename(part, dst); err != nil {
		return fmt.Errorf("store: pre-migration copy: %w", err)
	}
	matches, err := filepath.Glob(file + ".pre-*")
	if err != nil {
		return err
	}
	type cp struct {
		n    int
		path string
	}
	var cps []cp
	for _, m := range matches {
		n, err := strconv.Atoi(strings.TrimPrefix(m, file+".pre-"))
		if err != nil {
			continue // not ours
		}
		cps = append(cps, cp{n, m})
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].n > cps[j].n })
	for _, c := range cps[min(len(cps), keepPreMigrationCopies):] {
		if err := os.Remove(c.path); err != nil {
			return fmt.Errorf("store: prune pre-migration copies: %w", err)
		}
	}
	return nil
}

// Ping checks the database connection.
func (s *SQLite) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

// --- helpers ---

func toMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// orNow returns t, or the store clock when t is zero.
func (s *SQLite) orNow(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// mapErr translates SQLite constraint violations into the package errors.
func mapErr(err error) error {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	switch se.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return fmt.Errorf("%w: referenced row does not exist: %w", ErrNotFound, err)
	}
	return err
}

// notFound turns sql.ErrNoRows into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// affected returns ErrNotFound when res touched no row.
func affected(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
