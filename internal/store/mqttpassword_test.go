package store

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#609): a device whose MqttPassword is empty must be expressible. Before, the store knew only
// not set, broker and own (own never empty), so such a device could never be backed up or restored
// over MQTT. The empty mode stores no secret, and a typed save records its source and time.
func TestAnEmptyMQTTPasswordIsAModeWithoutASecret(t *testing.T) {
	s, _, _ := newStore(t)
	saved := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return saved }
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, "devicepw"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordEmpty, "ignored"); err != nil {
		t.Fatalf("the empty mode was refused: %v", err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.MQTTPasswordMode != model.MQTTPasswordEmpty || d.MQTTPassword != "" {
		t.Fatalf("stored %q/%q, %v", d.MQTTPasswordMode, d.MQTTPassword, err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT mqtt_password FROM devices WHERE id=?", id).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("a secret stayed at rest: %q, %v", raw, err)
	}
	if d.MQTTPasswordSource != model.MQTTPasswordTyped || !d.MQTTPasswordSetAt.Equal(saved) {
		t.Fatalf("source %q set at %v, want typed at %v", d.MQTTPasswordSource, d.MQTTPasswordSetAt, saved)
	}
}

// Why (#609, "the newer fact wins"): a backup replaces the stored password only when it was taken
// after the last save or the last backup it came from. A backup older than a typed password must not
// undo the person's save; a newer one must replace it, clear a refusal (it is a new password) and say
// where the password came from and when.
func TestABackupReplacesOnlyAnOlderPassword(t *testing.T) {
	s, _, _ := newStore(t)
	typed := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return typed }
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, "typedpw"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return typed.Add(2 * time.Hour) }
	applied, err := s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordOwn, "olderpw", typed.Add(-time.Minute))
	if err != nil || applied {
		t.Fatalf("an older backup: applied %v, %v", applied, err)
	}
	applied, err = s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordOwn, "samepw", typed)
	if err != nil || applied {
		t.Fatalf("a backup as old as the save: applied %v, %v", applied, err)
	}
	if d, _ := s.GetDevice(ctx, id); d.MQTTPassword != "typedpw" {
		t.Fatalf("an older backup replaced the typed password: %q", d.MQTTPassword)
	}
	if err := s.MarkMQTTPasswordBad(ctx, id, typed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	taken := typed.Add(90 * time.Minute)
	applied, err = s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordOwn, "newerpw", taken)
	if err != nil || !applied {
		t.Fatalf("a newer backup: applied %v, %v", applied, err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.MQTTPassword != "newerpw" || d.MQTTPasswordMode != model.MQTTPasswordOwn {
		t.Fatalf("after a newer backup: %q/%q, %v", d.MQTTPasswordMode, d.MQTTPassword, err)
	}
	if d.MQTTPasswordSource != model.MQTTPasswordFromBackup || !d.MQTTPasswordSetAt.Equal(taken) || !d.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("source %q set at %v bad %v", d.MQTTPasswordSource, d.MQTTPasswordSetAt, d.MQTTPasswordBadAt)
	}
	// Broker and empty store no secret; a typed save after the backup is newer again.
	if applied, err = s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordBroker, "brokerpw", taken.Add(time.Minute)); err != nil || !applied {
		t.Fatalf("broker: applied %v, %v", applied, err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT mqtt_password FROM devices WHERE id=?", id).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("broker mode kept a secret: %q, %v", raw, err)
	}
	if applied, err := s.ApplyMQTTPasswordFromBackup(ctx, 999, model.MQTTPasswordEmpty, "", taken.Add(time.Hour)); err != nil || applied {
		t.Fatalf("a deleted device: applied %v, %v", applied, err)
	}
	if _, err := s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordNone, "", taken.Add(time.Hour)); err == nil {
		t.Fatal("a backup cannot unset the password")
	}
}

// Why (#609): a passed password test says the device accepts the stored password. It must end with
// the next change of that password (a save, a backup) and with a refusal, or the device would show a
// password as working that was never tried; and a test that finishes after the password changed is
// about the old one and must not be recorded.
func TestATestResultEndsWithTheNextChange(t *testing.T) {
	s, _, _ := newStore(t)
	saved := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return saved }
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordBroker, ""); err != nil {
		t.Fatal(err)
	}
	ok := saved.Add(time.Minute)
	if err := s.MarkMQTTPasswordBad(ctx, id, saved); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMQTTPasswordOK(ctx, id, saved, ok); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if !d.MQTTPasswordOKAt.Equal(ok) || !d.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("ok %v bad %v after an accepted test", d.MQTTPasswordOKAt, d.MQTTPasswordBadAt)
	}
	if err := s.MarkMQTTPasswordBad(ctx, id, ok.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordOKAt.IsZero() {
		t.Fatalf("a refusal kept the passed test %v", d.MQTTPasswordOKAt)
	}
	if err := s.MarkMQTTPasswordOK(ctx, id, saved, ok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordEmpty, "", ok.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordOKAt.IsZero() {
		t.Fatalf("a new password from a backup kept the old test %v", d.MQTTPasswordOKAt)
	}
	if err := s.MarkMQTTPasswordOK(ctx, id, saved, ok.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a test of the replaced password was recorded: %v", err)
	}
	if err := s.MarkMQTTPasswordOK(ctx, id, ok.Add(time.Hour), ok.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordEmpty, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordOKAt.IsZero() {
		t.Fatalf("a save kept the old test %v", d.MQTTPasswordOKAt)
	}
}

// Why (#609): a refusal is about the password that was sent. The learner now replaces the password on its
// own (after a backup, at every start), so a device can refuse the old password after the new one was stored.
// Marking the new one would block every MQTT transfer of the device for a password it was never sent, and the
// learner would not lift the mark while later backups show the same value. A refusal of the stored password
// is still recorded.
func TestARefusalOfTheOldPasswordLeavesTheNewOne(t *testing.T) {
	s, _, _ := newStore(t)
	t0 := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return t0 }
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, "oldpw"); err != nil {
		t.Fatal(err)
	}
	sent, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := s.ApplyMQTTPasswordFromBackup(ctx, id, model.MQTTPasswordOwn, "newpw", t0.Add(time.Minute)); err != nil || !applied {
		t.Fatalf("applied %v, %v", applied, err)
	}
	if err := s.MarkMQTTPasswordBadFor(ctx, id, sent.MQTTPasswordSetAt, t0.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refusal of the replaced password was recorded: %v", err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.MQTTPassword != "newpw" || !d.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("stored %q bad %v, %v", d.MQTTPassword, d.MQTTPasswordBadAt, err)
	}
	refused := t0.Add(3 * time.Minute)
	if err := s.MarkMQTTPasswordOK(ctx, id, d.MQTTPasswordSetAt, refused.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMQTTPasswordBadFor(ctx, id, d.MQTTPasswordSetAt, refused); err != nil {
		t.Fatalf("the refusal of the stored password: %v", err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordBadAt.Equal(refused) || !d.MQTTPasswordOKAt.IsZero() {
		t.Fatalf("bad %v ok %v after the refusal of the stored password", d.MQTTPasswordBadAt, d.MQTTPasswordOKAt)
	}
	if err := s.MarkMQTTPasswordBadFor(ctx, id, d.MQTTPasswordSetAt, time.Time{}); err == nil {
		t.Fatal("a refusal without a time was recorded")
	}
}

// Why (#609): after the upgrade, a password typed before it must count as newer than every backup that
// is older than its last save, or the first pass over the existing backups would replace a password the
// person typed after that backup. A device without a password has no date, so the pass may fill it.
func TestTheMigrationDatesTypedPasswords(t *testing.T) {
	db, path := rawDB(t)
	upTo, all := fstest.MapFS{}, fstest.MapFS{}
	var this string
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, "_device_mqtt_password_source.sql") {
			this = p
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil || this == "" {
		t.Fatalf("migration not found: %q, %v", this, err)
	}
	for p, f := range all {
		if p < this {
			upTo[p] = f
		}
	}
	if err := migrate(db, path, upTo); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO devices (name, address, created_at, updated_at, mqtt_password_mode) VALUES ('typed','10.0.0.1',0,1700000000000,'broker')`,
		`INSERT INTO devices (name, address, created_at, updated_at) VALUES ('none','10.0.0.2',0,1700000000000)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		want *int64
	}{{"typed", ptr(int64(1700000000000))}, {"none", nil}} {
		var got *int64
		var src string
		if err := db.QueryRowContext(ctx, `SELECT mqtt_password_set_at, mqtt_password_source FROM devices WHERE name=?`, tt.name).Scan(&got, &src); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) || src != "" {
			t.Errorf("%s: set at %v source %q, want %v typed", tt.name, got, src, tt.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// Why (#609, "Use the broker password for these…"): the dialog lists the devices that are unset when it opens,
// and the confirm can come minutes later. A password that a backup taught or a person typed in between is a
// newer fact about a device; the bulk write must leave it alone (a read-then-write loop would overwrite it), and
// it must report which devices it really wrote, so the audit and the follow-up tests name only those.
func TestSetMQTTPasswordsWhereUnsetLeavesAStoredPasswordAlone(t *testing.T) {
	s, _, _ := newStore(t)
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	unset := mustDevice(t, s, "unset", "10.0.0.1")
	learned := mustDevice(t, s, "learned", "10.0.0.2")
	typed := mustDevice(t, s, "typed", "10.0.0.3")
	if ok, err := s.ApplyMQTTPasswordFromBackup(ctx, learned, model.MQTTPasswordOwn, "taught-pw", at.Add(-time.Hour)); err != nil || !ok {
		t.Fatalf("learn: %v %v", ok, err)
	}
	if err := s.SetMQTTPassword(ctx, typed, model.MQTTPasswordEmpty, ""); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return at.Add(time.Minute) }
	applied, err := s.SetMQTTPasswordsWhereUnset(ctx, []model.DeviceID{unset, learned, typed, 9999}, model.MQTTPasswordBroker)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != unset {
		t.Fatalf("applied %v, want only the unset device %d", applied, unset)
	}
	d, _ := s.GetDevice(ctx, unset)
	if d.MQTTPasswordMode != model.MQTTPasswordBroker || d.MQTTPasswordSource != model.MQTTPasswordTyped || !d.MQTTPasswordSetAt.Equal(at.Add(time.Minute)) || !d.MQTTPasswordOKAt.IsZero() {
		t.Fatalf("unset device: %q source %q set %v ok %v", d.MQTTPasswordMode, d.MQTTPasswordSource, d.MQTTPasswordSetAt, d.MQTTPasswordOKAt)
	}
	if d, _ = s.GetDevice(ctx, learned); d.MQTTPasswordMode != model.MQTTPasswordOwn || d.MQTTPassword != "taught-pw" || d.MQTTPasswordSource != model.MQTTPasswordFromBackup {
		t.Fatalf("a learned password was overwritten: %q %q %q", d.MQTTPasswordMode, d.MQTTPassword, d.MQTTPasswordSource)
	}
	if d, _ = s.GetDevice(ctx, typed); d.MQTTPasswordMode != model.MQTTPasswordEmpty {
		t.Fatalf("a typed password was overwritten: %q", d.MQTTPasswordMode)
	}
	if _, err := s.SetMQTTPasswordsWhereUnset(ctx, []model.DeviceID{unset}, model.MQTTPasswordOwn); err == nil {
		t.Fatal("the own mode needs a secret: a bulk write must refuse it")
	}
}
