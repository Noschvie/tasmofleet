package store

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// migrate0045 builds a database as 0.32.x left it (up to 0044), stores the given settings rows,
// runs the migrations that follow and returns the opened store.
func migrate0045(t *testing.T, settings map[string]string) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v0320.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // temp DB
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0045" {
			continue
		}
		b, err := fs.ReadFile(Migrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		old["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, old); err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if _, err := db.ExecContext(ctx, `INSERT INTO system_settings (key, value, updated_by, updated_at) VALUES (?, ?, 'adam', 1)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0044 -> current: %v", err)
	}
	return &SQLite{db: db}
}

func setting(t *testing.T, s *SQLite, key string) (string, bool) {
	t.Helper()
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM system_settings WHERE key = ?`, key).Scan(&v)
	return v, err == nil
}

// Why (#601): an install that backed up on an interval must keep backing up after the upgrade, as
// the new schedule can say it: every day at 03:00, whatever the interval was. The interval row goes
// (the setting no longer exists), and the other settings stay as they were.
func TestMigration0045TurnsASetIntervalIntoDaily0300(t *testing.T) {
	for _, interval := range []string{"24h", "6h30m", "1m", "168h"} {
		s := migrate0045(t, map[string]string{"BACKUP_INTERVAL": interval, "BACKUP_KEEP": "4"})
		if days, ok := setting(t, s, "BACKUP_DAYS"); !ok || days != "mon,tue,wed,thu,fri,sat,sun" {
			t.Errorf("interval %s: BACKUP_DAYS = %q, %v; want every day", interval, days, ok)
		}
		if at, ok := setting(t, s, "BACKUP_TIME"); !ok || at != "03:00" {
			t.Errorf("interval %s: BACKUP_TIME = %q, %v; want 03:00", interval, at, ok)
		}
		if _, ok := setting(t, s, "BACKUP_INTERVAL"); ok {
			t.Errorf("interval %s: the removed BACKUP_INTERVAL row is still stored", interval)
		}
		if keep, _ := setting(t, s, "BACKUP_KEEP"); keep != "4" {
			t.Errorf("another setting was touched: BACKUP_KEEP = %q", keep)
		}
	}
}

// Why (#601): "0" is how the old page stored "manual only" (a stored 0s too). It must stay manual only:
// turning it into a daily schedule would start backups the person switched off.
func TestMigration0045KeepsManualOnlyManualOnly(t *testing.T) {
	for _, interval := range []string{"0", "0s", "0h", ""} {
		s := migrate0045(t, map[string]string{"BACKUP_INTERVAL": interval})
		if days, ok := setting(t, s, "BACKUP_DAYS"); ok {
			t.Errorf("interval %q: BACKUP_DAYS = %q was created for a schedule that was off", interval, days)
		}
		if _, ok := setting(t, s, "BACKUP_INTERVAL"); ok {
			t.Errorf("interval %q: the removed BACKUP_INTERVAL row is still stored", interval)
		}
		if n := markerCount(t, s); n != 0 {
			t.Errorf("interval %q: %d marker(s) stored for a schedule that was off", interval, n)
		}
	}
	s := migrate0045(t, nil) // never set: the default 0
	if _, ok := setting(t, s, "BACKUP_DAYS"); ok {
		t.Error("no stored interval: no schedule may appear")
	}
}

// markerCount is how many "rescheduled" markers the record holds.
func markerCount(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_runs WHERE trigger = 'rescheduled' AND completed = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Why (#601, #612): the migrated schedule must not run the fleet at the first start after the upgrade.
// If the upgrade happens within 60 minutes after 03:00 and the last run is older, the schedule would
// take it for a missed run and start one at once. The marker the migration stores says that the
// schedule begins now, so a restart does not start a run.
func TestMigration0045MarkerStopsAStartRunAfterTheUpgrade(t *testing.T) {
	s := migrate0045(t, map[string]string{"BACKUP_INTERVAL": "24h"})
	before := time.Now().Add(-time.Minute)
	settled, missed, err := s.BackupRunTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Before(before) || !missed.IsZero() {
		t.Fatalf("settled %v, missed %v: the migration must settle everything up to now and skip nothing", settled, missed)
	}
	if _, ok, _ := s.LastBackupRun(ctx); ok {
		t.Error("the marker is no run: the Last run card must not show it")
	}
	if n := markerCount(t, s); n != 1 {
		t.Errorf("%d rescheduled markers after the migration, want exactly 1", n)
	}
}

// Why (#601): markers settle a scheduled time but are no runs and no skips unless they say so: the
// status bar and the Last run card must keep showing the last real run, and the page must show
// "skipped" only for a missed marker.
func TestBackupRunTimesAndLastRunSeparateRunsFromMarkers(t *testing.T) {
	s, _, _ := newStore(t)
	at := time.Date(2026, 11, 3, 3, 0, 5, 0, time.UTC)
	settled, missed, err := s.BackupRunTimes(ctx)
	if err != nil || !settled.IsZero() || !missed.IsZero() {
		t.Fatalf("empty record: %v %v %v", settled, missed, err)
	}
	if _, err := s.SaveBackupRun(ctx, model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: at, FinishedAt: at.Add(time.Minute), Completed: true, OK: 1}); err != nil {
		t.Fatal(err)
	}
	later := at.Add(24 * time.Hour)
	if _, err := s.SaveBackupRun(ctx, model.BackupRun{Trigger: model.BackupRunMissed, StartedAt: later, FinishedAt: later, Completed: true}); err != nil {
		t.Fatal(err)
	}
	later2 := later.Add(time.Hour)
	if _, err := s.SaveBackupRun(ctx, model.BackupRun{Trigger: model.BackupRunRescheduled, StartedAt: later2, FinishedAt: later2, Completed: true}); err != nil {
		t.Fatal(err)
	}
	// A run that was cut short settles nothing: it is started again after a restart.
	cut := later2.Add(time.Hour)
	if _, err := s.SaveBackupRun(ctx, model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: cut}); err != nil {
		t.Fatal(err)
	}
	settled, missed, err = s.BackupRunTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !settled.Equal(later2) || !missed.Equal(later) {
		t.Errorf("settled %v (want %v, the newest completed row), missed %v (want %v)", settled, later2, missed, later)
	}
	run, ok, err := s.LastBackupRun(ctx)
	if err != nil || !ok || !run.StartedAt.Equal(at) || run.Trigger != model.BackupRunScheduled {
		t.Errorf("LastBackupRun = %+v %v %v; want the real run of %v, not a marker", run, ok, err, at)
	}
}
