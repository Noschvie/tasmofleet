package store

import (
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

// Why: before 0.20 the language was written only by an HTTP probe, so those identities are facts.
// Migration 0036 added the flag with default 0, and without this backfill every existing device and
// every running job would be refused by Select (D10) until its next probe. An identity read over MQTT
// (never stamped identity_at) must stay unknown: its empty language proves nothing.
func TestMigration0037BackfillsLanguageKnownForHTTPIdentitiesOnly(t *testing.T) {
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
		if p < "migrations/0037" {
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
	for _, q := range []string{
		`INSERT INTO devices (name, address, mac, topic, identity_at, created_at, updated_at) VALUES ('http','10.0.0.1','aa:bb:cc:00:00:01','t1',1700000000000,0,0)`,
		`INSERT INTO devices (name, address, mac, topic, created_at, updated_at) VALUES ('mqtt','10.0.0.2','aa:bb:cc:00:00:02','t2',0,0)`,
		`INSERT INTO devices (name, address, mac, topic, identity_at, created_at, updated_at) VALUES ('zero','10.0.0.3','aa:bb:cc:00:00:03','t3',0,0,0)`,
		`INSERT INTO ota_batches (created_by, created_at, retry_of) VALUES ('u',0,0)`,
		// A job as 0.19 stored it: json.Marshal(Identity) without the flag.
		`INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, target_release, target_variant, identity_before, created_by, created_at, updated_at) VALUES (1,1,'await_reboot','minimal','staged','only-newer','16.0.0','tasmota','{"Family":"esp8266","Version":"15.0.0","Variant":"tasmota","Language":"DE"}','u',0,0)`,
		`INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, target_release, target_variant, identity_before, created_by, created_at, updated_at) VALUES (1,2,'queued','final','direct','only-newer','16.0.0','tasmota','','u',0,0)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"http": 1, "mqtt": 0, "zero": 0} {
		var got int
		if err := db.QueryRowContext(ctx, `SELECT identity_lang_known FROM devices WHERE name=?`, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("device %s: identity_lang_known = %d, want %d", name, got, want)
		}
	}
	s := &SQLite{db: db, now: time.Now}
	j, err := s.GetJob(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !j.IdentityBefore.LanguageKnown || j.IdentityBefore.Language != "DE" {
		t.Errorf("running job identity = %+v: a resumed stage 2 would be refused by Select", j.IdentityBefore)
	}
}
