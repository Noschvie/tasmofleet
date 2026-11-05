package store

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (#551): a 0.27.3 database can hold a job waiting in restore_power and a stored
// OTA_POWER_MAX_AGE. The new state machine does not know that state, and a non-terminal job would
// block its device for ever through ota_jobs_one_active. The migration must end the job as
// succeeded (the update had worked), say why in its events, free the device for a new job, and
// drop the setting, while leaving every other job and setting alone.
func TestMigration0042EndsPowerRestoreJobsAndDropsTheSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0273.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0042" {
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
	for _, q := range []string{
		`INSERT INTO devices (id, name, address, mac, created_at, updated_at) VALUES (1, 'lamp', '10.0.0.1', '', 1, 1)`,
		`INSERT INTO devices (id, name, address, mac, created_at, updated_at) VALUES (2, 'plug', '10.0.0.2', '', 1, 1)`,
		`INSERT INTO ota_batches (id, created_by, created_at) VALUES (1, 'adam', 1)`,
		`INSERT INTO ota_jobs (id, batch_id, device_id, state, stage, target_release, target_variant, power_on, created_by, created_at, updated_at)
		 VALUES (10, 1, 1, 'restore_power', 'final', '15.0.0', 'tasmota', 1, 'adam', 1, 1700000000000)`,
		`INSERT INTO ota_jobs (id, batch_id, device_id, state, stage, target_release, target_variant, created_by, created_at, updated_at)
		 VALUES (11, 1, 2, 'failed', 'final', '15.0.0', 'tasmota', 'adam', 1, 1700000000000)`,
		`INSERT INTO system_settings (key, value, updated_by, updated_at) VALUES ('OTA_POWER_MAX_AGE', '30m', 'adam', 1)`,
		`INSERT INTO system_settings (key, value, updated_by, updated_at) VALUES ('OTA_PARALLEL', '4', 'adam', 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0041 -> current: %v", err)
	}
	s := &SQLite{db: db}

	j, err := s.GetJob(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != model.JobSucceeded {
		t.Errorf("job in restore_power is %q after the migration, want succeeded", j.State)
	}
	evs, err := s.JobEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].From != "restore_power" || evs[0].To != model.JobSucceeded || evs[0].Detail == "" {
		t.Errorf("events of the migrated job: %+v", evs)
	}
	if other, _ := s.GetJob(ctx, 11); other.State != model.JobFailed {
		t.Errorf("an unrelated job changed to %q", other.State)
	}
	if evs, _ := s.JobEvents(ctx, 11); len(evs) != 0 {
		t.Errorf("an unrelated job got events: %+v", evs)
	}
	// The device is free again: a new active job must not hit ota_jobs_one_active.
	if _, err := db.ExecContext(ctx, `INSERT INTO ota_jobs (batch_id, device_id, state, stage, target_release, target_variant, created_by, created_at, updated_at)
		VALUES (1, 1, 'queued', 'final', '15.1.0', 'tasmota', 'adam', 2, 2)`); err != nil {
		t.Errorf("new job on the migrated device: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM system_settings WHERE key='OTA_POWER_MAX_AGE'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("OTA_POWER_MAX_AGE rows: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM system_settings WHERE key='OTA_PARALLEL'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("another setting was touched: %d %v", n, err)
	}
}
