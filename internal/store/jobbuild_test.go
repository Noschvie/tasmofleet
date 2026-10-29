package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the build the user picked for an ESP32-S2 (#88) must survive a restart and be copied by
// a retry; if it were lost, the resumed job could no longer select a file and would fail
// halfway through an update.
func TestJobTargetBuildRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "s2", "10.0.0.1")
	j := job(dev)
	j.TargetBuild = "s2cdc"
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{j})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, ids[0])
	if err != nil || got.TargetBuild != "s2cdc" {
		t.Fatalf("TargetBuild = %q, %v", got.TargetBuild, err)
	}
	// A state transition rewrites the row; the build is fixed at creation and must not be lost.
	prev := got.State
	got.State = model.JobFailed
	if err := s.SaveTransition(ctx, got, model.JobEvent{JobID: got.ID, From: prev, To: model.JobFailed, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetJob(ctx, ids[0]); err != nil || got.TargetBuild != "s2cdc" {
		t.Fatalf("after transition TargetBuild = %q, %v", got.TargetBuild, err)
	}
}

// Why: live databases upgrade from 0.17 (migration 0031) with finished and queued jobs in
// them; those jobs must load with "no build chosen" and behave as before.
func TestMigration0032KeepsExistingJobsWithoutBuild(t *testing.T) {
	db, path := rawDB(t)
	upTo, all := fstest.MapFS{}, fstest.MapFS{}
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		if p < "migrations/0032" {
			upTo[p] = &fstest.MapFile{Data: b}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, upTo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO devices (name, address, mac, topic, created_at, updated_at) VALUES ('old','10.0.0.1','aa:bb:cc:00:00:09','t',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ota_batches (created_by, created_at, retry_of) VALUES ('u',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, target_release, target_variant, identity_before, created_by, created_at, updated_at) VALUES (1,1,'succeeded','final','direct','only-newer','16.0.0','tasmota','{}','u',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var build string
	if err := db.QueryRowContext(ctx, `SELECT target_build FROM ota_jobs WHERE id=1`).Scan(&build); err != nil {
		t.Fatal(err)
	}
	if build != "" {
		t.Fatalf("existing job got build %q", build)
	}
}
