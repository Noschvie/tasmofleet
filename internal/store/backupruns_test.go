package store

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func sampleRun(start int64, dev model.DeviceID, completed bool) model.BackupRun {
	return model.BackupRun{Trigger: model.BackupRunScheduled, StartedAt: ms(start), FinishedAt: ms(start + 500), Completed: completed,
		Tried: 2, OK: 1, Failed: 1, Skipped: 1, Items: []model.BackupRunItem{
			{DeviceID: dev, DeviceName: "kitchen", Outcome: model.BackupOutcomeOK, At: ms(start + 100)},
			{DeviceID: dev, DeviceName: "kitchen2", Outcome: model.BackupOutcomeFailed, Reason: "the device refused the password", At: ms(start + 200)},
			{DeviceID: 0, DeviceName: "garage", Outcome: model.BackupOutcomeSkipped, Reason: "never answered", At: ms(start + 300)},
		}}
}

// Why (#409): the bar, the dashboard and the Backups page show the last scheduled run after a
// restart, so the run and every device's result must come back exactly as written.
func TestBackupRunRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	d := mustDevice(t, s, "kitchen", "10.0.0.1")
	if _, ok, err := s.LastBackupRun(ctx); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v, want no run", ok, err)
	}
	id, err := s.SaveBackupRun(ctx, sampleRun(1000, d, true))
	if err != nil || id == 0 {
		t.Fatalf("save: %d %v", id, err)
	}
	got, ok, err := s.LastBackupRun(ctx)
	if err != nil || !ok {
		t.Fatalf("last: ok=%v err=%v", ok, err)
	}
	want := sampleRun(1000, d, true)
	want.ID = id
	want.Items[2].DeviceID = 0
	if got.ID != id || got.Trigger != want.Trigger || !got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) ||
		!got.Completed || got.Tried != 2 || got.OK != 1 || got.Failed != 1 || got.Skipped != 1 || len(got.Items) != 3 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i, it := range got.Items {
		w := want.Items[i]
		if it.DeviceID != w.DeviceID || it.DeviceName != w.DeviceName || it.Outcome != w.Outcome || it.Reason != w.Reason || !it.At.Equal(w.At) {
			t.Errorf("item %d = %+v, want %+v", i, it, w)
		}
	}
}

// Why: a run that shutdown cut short is history, not "the last run": the bar must keep showing the
// last run that went through (the rule the in-memory bar already had).
func TestLastBackupRunIsTheNewestCompletedOne(t *testing.T) {
	s, _, _ := newStore(t)
	d := mustDevice(t, s, "kitchen", "10.0.0.1")
	older, _ := s.SaveBackupRun(ctx, sampleRun(1000, d, true))
	if _, err := s.SaveBackupRun(ctx, sampleRun(2000, d, false)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LastBackupRun(ctx)
	if err != nil || !ok || got.ID != older {
		t.Fatalf("last = %+v ok=%v err=%v, want the completed run %d", got, ok, err, older)
	}
	newer, _ := s.SaveBackupRun(ctx, sampleRun(3000, d, true))
	if got, _, _ := s.LastBackupRun(ctx); got.ID != newer {
		t.Errorf("last = %d, want the newest completed run %d", got.ID, newer)
	}
}

// Why: the record is history, not a reason to keep a deleted device's row. The item keeps the
// name it had (the page still says whose it was); the device id goes.
func TestBackupRunItemKeepsTheNameWhenTheDeviceIsDeleted(t *testing.T) {
	s, _, _ := newStore(t)
	d := mustDevice(t, s, "kitchen", "10.0.0.1")
	if _, err := s.SaveBackupRun(ctx, sampleRun(1000, d, true)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LastBackupRun(ctx)
	if err != nil || !ok || len(got.Items) != 3 {
		t.Fatalf("after delete: %+v ok=%v err=%v, want the run with its items", got, ok, err)
	}
	if got.Items[0].DeviceID != 0 || got.Items[0].DeviceName != "kitchen" {
		t.Errorf("item = %+v, want the name kept and no device id", got.Items[0])
	}
}

// Why (#409): the run record follows the audit retention (one knob for how long history is kept);
// its items go with the run.
func TestPruneBackupRunsByAge(t *testing.T) {
	s, _, _ := newStore(t)
	d := mustDevice(t, s, "kitchen", "10.0.0.1")
	if _, err := s.SaveBackupRun(ctx, sampleRun(1000, d, true)); err != nil {
		t.Fatal(err)
	}
	keep, _ := s.SaveBackupRun(ctx, sampleRun(5000, d, true))
	n, err := s.PruneBackupRuns(ctx, ms(3000))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want 1", n, err)
	}
	if got, ok, _ := s.LastBackupRun(ctx); !ok || got.ID != keep {
		t.Errorf("the newer run must stay: %+v", got)
	}
	var items int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM backup_run_items").Scan(&items); err != nil || items != 3 {
		t.Errorf("items left = %d (%v), want only the kept run's 3", items, err)
	}
}

// Why (#409): "never answered" is decided from the persisted last_seen_at, not from the health
// state of this run of the program: a device seen last week is not "never answered" after a restart.
func TestDevicesSeenReturnsOnlyDevicesThatWereHeard(t *testing.T) {
	s, _, _ := newStore(t)
	heard := mustDevice(t, s, "heard", "10.0.0.1")
	mustDevice(t, s, "silent", "10.0.0.2")
	at := time.Date(2026, 11, 3, 21, 14, 0, 0, time.UTC)
	if err := s.SetLastSeen(ctx, heard, model.HealthOffline, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.DevicesSeen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[heard].Equal(at) {
		t.Errorf("DevicesSeen = %v, want only the heard device at %v", got, at)
	}
}

// Why (#409): 0043 is the upgrade of every 0.28.0 database. The devices and backups an installation
// already holds must survive it untouched, and the new tables must be usable at once.
func TestMigration0043KeepsA0280DatabaseAndAddsTheRunRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	upTo42 := fstest.MapFS{}
	all, err := fs.Glob(Migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if strings.Contains(f, "0043_") {
			continue
		}
		b, err := fs.ReadFile(Migrations, f)
		if err != nil {
			t.Fatal(err)
		}
		upTo42[f] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, upTo42); err != nil {
		t.Fatalf("up to 0042: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO devices (name, address, last_seen_at, created_at, updated_at) VALUES ('kitchen', '10.0.0.1', 1700000000000, 1, 1)"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0042 -> 0043: %v", err)
	}
	var name string
	var seen int64
	if err := db.QueryRowContext(ctx, "SELECT name, last_seen_at FROM devices").Scan(&name, &seen); err != nil || name != "kitchen" || seen != 1700000000000 {
		t.Errorf("device after 0043: %q %d %v", name, seen, err)
	}
	var runs, items int
	if err := db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM backup_runs), (SELECT COUNT(*) FROM backup_run_items)").Scan(&runs, &items); err != nil || runs != 0 || items != 0 {
		t.Errorf("run record after 0043: %d runs, %d items, %v; want empty tables", runs, items, err)
	}
}
