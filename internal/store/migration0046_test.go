package store

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

// migrate0046 builds a database as 0.33.x left it (up to 0045), stores the given settings rows,
// runs the migrations that follow and returns the opened store.
func migrate0046(t *testing.T, settings map[string]string) *SQLite {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v0330.db")
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
		if e.Name() >= "0046" {
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
		t.Fatalf("0045 -> current: %v", err)
	}
	return &SQLite{db: db}
}

// snapshotMarkers is how many "rescheduled" rows the snapshot run record holds.
func snapshotMarkers(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_runs WHERE trigger = 'rescheduled'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Why (#602): an install that took snapshots on an interval must keep taking them after the upgrade,
// as the new schedule can say it: every day at 03:00, whatever the interval was. The interval row goes
// (the setting no longer exists), and the other settings stay as they were.
func TestMigration0046TurnsASetIntervalIntoDaily0300(t *testing.T) {
	for _, interval := range []string{"24h", "6h30m", "1h", "168h"} {
		s := migrate0046(t, map[string]string{"SNAPSHOT_INTERVAL": interval, "SNAPSHOT_KEEP": "4"})
		if days, ok := setting(t, s, "SNAPSHOT_DAYS"); !ok || days != "mon,tue,wed,thu,fri,sat,sun" {
			t.Errorf("interval %s: SNAPSHOT_DAYS = %q, %v; want every day", interval, days, ok)
		}
		if at, ok := setting(t, s, "SNAPSHOT_TIME"); !ok || at != "03:00" {
			t.Errorf("interval %s: SNAPSHOT_TIME = %q, %v; want 03:00", interval, at, ok)
		}
		if _, ok := setting(t, s, "SNAPSHOT_INTERVAL"); ok {
			t.Errorf("interval %s: the removed SNAPSHOT_INTERVAL row is still stored", interval)
		}
		if keep, _ := setting(t, s, "SNAPSHOT_KEEP"); keep != "4" {
			t.Errorf("another setting was touched: SNAPSHOT_KEEP = %q", keep)
		}
	}
}

// Why (#602): "0" is how the old page stored "off" (a stored 0s too). It must stay off: turning it
// into a daily schedule would start snapshots the person switched off.
func TestMigration0046KeepsOffOff(t *testing.T) {
	for _, interval := range []string{"0", "0s", "0h", ""} {
		s := migrate0046(t, map[string]string{"SNAPSHOT_INTERVAL": interval})
		if days, ok := setting(t, s, "SNAPSHOT_DAYS"); ok {
			t.Errorf("interval %q: SNAPSHOT_DAYS = %q was created for a schedule that was off", interval, days)
		}
		if _, ok := setting(t, s, "SNAPSHOT_INTERVAL"); ok {
			t.Errorf("interval %q: the removed SNAPSHOT_INTERVAL row is still stored", interval)
		}
		if n := snapshotMarkers(t, s); n != 0 {
			t.Errorf("interval %q: %d marker(s) stored for a schedule that was off", interval, n)
		}
	}
	s := migrate0046(t, nil) // never set: the default 0
	if _, ok := setting(t, s, "SNAPSHOT_DAYS"); ok {
		t.Error("no stored interval: no schedule may appear")
	}
}

// Why (#602): the migrated schedule must not take a snapshot at the first start after the upgrade. If the
// upgrade happens within 60 minutes after 03:00 and no row is newer, the schedule would take it for a
// missed time and take one at once. The marker the migration stores says that the schedule begins now.
func TestMigration0046MarkerStopsAStartRunAfterTheUpgrade(t *testing.T) {
	s := migrate0046(t, map[string]string{"SNAPSHOT_INTERVAL": "24h"})
	before := time.Now().Add(-time.Minute)
	settled, missed, err := s.SnapshotRunTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Before(before) || !missed.IsZero() {
		t.Fatalf("settled %v, missed %v: the migration must settle everything up to now and skip nothing", settled, missed)
	}
	if n := snapshotMarkers(t, s); n != 1 {
		t.Errorf("%d rescheduled markers after the migration, want exactly 1", n)
	}
}

// Why (#602): the snapshot record and the device backup record are two records. A snapshot row that
// settled a backup time (or the other way round) would skip a fleet backup or a snapshot without
// a trace. The skip is told apart from the other rows, and an empty record is zero, not an error.
func TestSnapshotRunTimesAreTheirOwnRecord(t *testing.T) {
	s, _, _ := newStore(t)
	if settled, missed, err := s.SnapshotRunTimes(ctx); err != nil || !settled.IsZero() || !missed.IsZero() {
		t.Fatalf("empty record: %v %v %v", settled, missed, err)
	}
	ran := time.Date(2026, 11, 3, 3, 0, 5, 0, time.UTC)
	skipped := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	for trigger, at := range map[string]time.Time{SnapshotRunScheduled: ran, SnapshotRunMissed: skipped} {
		if err := s.SaveSnapshotRun(ctx, trigger, at); err != nil {
			t.Fatal(err)
		}
	}
	settled, missed, err := s.SnapshotRunTimes(ctx)
	if err != nil || !settled.Equal(skipped) || !missed.Equal(skipped) {
		t.Fatalf("settled %v, missed %v (%v); a skip settles its time and is told apart", settled, missed, err)
	}
	if bs, _, err := s.BackupRunTimes(ctx); err != nil || !bs.IsZero() {
		t.Fatalf("a snapshot row settled a device backup time: %v %v", bs, err)
	}
	if n, err := s.PruneSnapshotRuns(ctx, skipped); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want the older row only", n, err)
	}
}
