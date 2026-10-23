package store

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (N2): deleting a device must not take its backups with it unless the
// user says so. The store keeps them with device_id NULL, still naming the
// device and MAC they were taken for, so Settings -> Backups can list them and
// a replacement can be restored from them.
func TestDeleteDeviceKeepsBackupsAsOrphans(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "garage", Address: "10.0.0.9", MAC: "AA:BB:CC:00:11:22"})
	if err != nil {
		t.Fatal(err)
	}
	other := mustDevice(t, s, "other", "10.0.0.10")
	b := model.Backup{DeviceID: id, DeviceName: "garage", DeviceMAC: "AA:BB:CC:00:11:22", Kind: model.BackupConfigDump,
		TakenAt: ms(1_700_000_000_000), Path: "backups/1/a.dmp", SHA256: "aa", Kept: true, Trigger: model.TriggerManual}
	bid, err := s.PutBackup(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBackup(ctx, model.Backup{DeviceID: other, Kind: model.BackupConfigDump, Path: "backups/2/a.dmp", SHA256: "bb"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackup(ctx, bid)
	if err != nil {
		t.Fatalf("backup went with the device: %v", err)
	}
	b.ID, b.DeviceID = bid, 0
	if !reflect.DeepEqual(got, b) {
		t.Fatalf("orphan changed:\n got %+v\nwant %+v", got, b)
	}
	orphans, err := s.ListOrphanBackups(ctx)
	if err != nil || len(orphans) != 1 || orphans[0].ID != bid {
		t.Fatalf("orphans: %v %+v (the other device's backup is not an orphan)", err, orphans)
	}
	// A new backup always belongs to a device.
	if _, err := s.PutBackup(ctx, model.Backup{Kind: model.BackupConfigDump, Path: "x", SHA256: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("backup without device stored: %v", err)
	}
}

// Why (N1): the files of one run are kept or released together; a partial
// change (one id unknown) must change nothing, or a run would be half pruned.
func TestSetBackupsKeptIsAllOrNothing(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	a, _ := s.PutBackup(ctx, model.Backup{DeviceID: dev, Kind: model.BackupConfigDump, Path: "a", SHA256: "a"})
	b, _ := s.PutBackup(ctx, model.Backup{DeviceID: dev, Kind: model.BackupSnapshot, Path: "b", SHA256: "b"})
	if err := s.SetBackupsKept(ctx, []model.BackupID{a, b}, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []model.BackupID{a, b} {
		if g, _ := s.GetBackup(ctx, id); !g.Kept {
			t.Errorf("backup %d not kept", id)
		}
	}
	if err := s.SetBackupsKept(ctx, []model.BackupID{a, 999}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if g, _ := s.GetBackup(ctx, a); !g.Kept {
		t.Error("a was released although the change as a whole failed")
	}
}

// Why (N1): failed attempts are history shown as "Failed" rows; they go with
// the audit retention and with their device.
func TestBackupAttempts(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	base := ms(1_700_000_000_000)
	for i, r := range []string{"old", "mid", "new"} {
		if _, err := s.AddBackupAttempt(ctx, model.BackupAttempt{DeviceID: dev, At: base.Add(time.Duration(i) * time.Hour),
			Trigger: model.TriggerScheduled, Version: "14.3.0", Reason: r}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.ListBackupAttempts(ctx, dev, 2)
	if err != nil || len(l) != 2 || l[0].Reason != "new" || l[1].Reason != "mid" || l[0].Trigger != model.TriggerScheduled || l[0].Version != "14.3.0" {
		t.Fatalf("newest first, limited: %v %+v", err, l)
	}
	if n, err := s.PruneBackupAttempts(ctx, base.Add(30*time.Minute)); err != nil || n != 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if l, _ := s.ListBackupAttempts(ctx, dev, 0); len(l) != 2 {
		t.Fatalf("after prune: %+v", l)
	}
	if _, err := s.AddBackupAttempt(ctx, model.BackupAttempt{DeviceID: 99, Reason: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("attempt for unknown device: %v", err)
	}
	if err := s.DeleteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListBackupAttempts(ctx, dev, 0); len(l) != 0 {
		t.Error("attempts survived their device")
	}
}

// Why (N2): 0022 rebuilds the backups table of every installation. The rows
// of a v0.10.x database (schema 0020) must survive with their ids (restore
// jobs refer to them without a foreign key), gain the name and MAC of their
// device, and afterwards outlive a deleted device instead of cascading.
func TestMigrationsFromV010KeepBackupsAndStopTheCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v010.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	v010 := fstest.MapFS{}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0021" {
			continue
		}
		b, err := fs.ReadFile(Migrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		v010["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, v010); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO devices (id, name, address, mac, created_at, updated_at) VALUES (3, 'garage', '10.0.0.3', 'AA:BB:CC:00:00:03', 1, 1)`,
		`INSERT INTO devices (id, name, address, mac, created_at, updated_at) VALUES (4, 'porch', '10.0.0.4', '', 1, 1)`,
		`INSERT INTO backups (id, device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note)
		 VALUES (11, 3, 'dmp', 1700000000000, '14.2.0', 'backups/3/a-dmp.dmp', 4096, 'aa', '', 'decoder outdated')`,
		`INSERT INTO backups (id, device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note)
		 VALUES (12, 3, 'decoded', 1700000000000, '14.2.0', 'backups/3/a-decoded.json', 100, 'bb', '14.3.0', '')`,
		`INSERT INTO backups (id, device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note)
		 VALUES (20, 4, 'dmp', 1700000100000, '15.0.0', 'backups/4/b-dmp.dmp', 4096, 'cc', '', '')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0020 -> current: %v", err)
	}
	s := &SQLite{db: db, now: time.Now}
	got, err := s.GetBackup(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	want := model.Backup{ID: 11, DeviceID: 3, DeviceName: "garage", DeviceMAC: "AA:BB:CC:00:00:03", Kind: model.BackupConfigDump,
		TakenAt: ms(1_700_000_000_000), Version: "14.2.0", Path: "backups/3/a-dmp.dmp", Size: 4096, SHA256: "aa", Note: "decoder outdated"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated row:\n got %+v\nwant %+v", got, want)
	}
	if l, _ := s.ListBackups(ctx, 3); len(l) != 2 || l[0].ID != 12 || l[0].DecoderVersion != "14.3.0" {
		t.Fatalf("device 3 rows: %+v", l)
	}
	var idx int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='backups_device' AND tbl_name='backups'").Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index backups_device: %d %v", idx, err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM devices WHERE id=3"); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.ListOrphanBackups(ctx); len(o) != 2 || o[0].DeviceName != "garage" {
		t.Fatalf("orphans after deleting the device: %+v", o)
	}
	var violations int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		t.Errorf("foreign key violations after the rebuild: %d %v", violations, err)
	}
}
