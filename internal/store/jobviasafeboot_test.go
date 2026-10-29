package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: whether a push job goes through safeboot is decided in the preview and must survive a
// restart of tasmofleet: a job resumed in the wrong mode would upload into the running app
// (refused for space) or, worse, skip the recovery it owes a device left in safeboot (#30).
func TestJobViaSafebootRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "esp32", "10.0.0.1")
	j := job(dev)
	j.Delivery, j.ViaSafeboot = model.DeliveryPush, true
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{j})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, ids[0])
	if err != nil || !got.ViaSafeboot {
		t.Fatalf("ViaSafeboot = %v, %v", got.ViaSafeboot, err)
	}
	// Every transition rewrites the row; the mode is fixed at creation and the new states must store.
	for _, st := range []model.JobState{model.JobSafebootSwitch, model.JobUpgradeIssued, model.JobSafebootRecover} {
		prev := got.State
		got.State, got.RecoveryStep = st, 2
		if err := s.SaveTransition(ctx, got, model.JobEvent{JobID: got.ID, From: prev, To: st, Event: "x"}); err != nil {
			t.Fatal(err)
		}
		if got, err = s.GetJob(ctx, ids[0]); err != nil || !got.ViaSafeboot || got.State != st || got.RecoveryStep != 2 {
			t.Fatalf("after %s: ViaSafeboot=%v state=%s err=%v", st, got.ViaSafeboot, got.State, err)
		}
	}
	// A plain job stays plain.
	_, ids, err = s.CreateBatch(ctx, "u", []model.Job{job(mustDevice(t, s, "other", "10.0.0.2"))})
	if err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetJob(ctx, ids[0]); err != nil || got.ViaSafeboot {
		t.Fatalf("plain job ViaSafeboot = %v, %v", got.ViaSafeboot, err)
	}
}

// Why: live databases upgrade with finished and queued jobs in them; those must load as "not via
// safeboot" and behave as before.
func TestMigration0034KeepsExistingJobsDirect(t *testing.T) {
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
		if p < "migrations/0034" {
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
	if _, err := db.ExecContext(ctx, `INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, target_release, target_variant, identity_before, delivery, created_by, created_at, updated_at) VALUES (1,1,'succeeded','final','direct','only-newer','16.0.0','tasmota','{}','push','u',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var via int
	if err := db.QueryRowContext(ctx, `SELECT via_safeboot FROM ota_jobs WHERE id=1`).Scan(&via); err != nil {
		t.Fatal(err)
	}
	if via != 0 {
		t.Fatalf("existing job got via_safeboot %d", via)
	}
}
