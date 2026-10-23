package store

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: live databases hold devices from before the MAC set existed. The
// migration must seed the set from the old column, or every existing device
// would stop matching by MAC and show up as "new" in discovery.
func TestMigrationSeedsMACSetFromExistingColumn(t *testing.T) {
	db, path := rawDB(t)
	upTo := fstest.MapFS{}
	all := fstest.MapFS{}
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		// everything before 0013_device_macs: later migrations need it first
		if p < "migrations/0013_device_macs" {
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
		`INSERT INTO devices (name, address, mac, created_at, updated_at) VALUES ('a','10.0.0.1','aa:bb:cc:00:00:01',0,0)`,
		`INSERT INTO devices (name, address, mac, http_user, created_at, updated_at) VALUES ('nomac','10.0.0.2','','admin',0,0)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_macs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("device_macs rows = %d (%v), want 1 (the device without MAC has none)", n, err)
	}
}

// Why: Ethernet + Wi-Fi are one device; both MACs must find it, and a MAC can
// belong to only one device.
func TestMACSetMatchesEveryMACAndIsUnique(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "e", Address: "10.0.0.1", MAC: "AA:BB:CC:00:00:01", MACs: []string{"aa:bb:cc:00:00:02"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"aa:bb:cc:00:00:01", "AA:BB:CC:00:00:02"} {
		if d, err := s.GetDeviceByMAC(ctx, m); err != nil || d.ID != id || len(d.MACs) != 2 {
			t.Errorf("lookup %s: %+v %v", m, d, err)
		}
	}
	_, err = s.CreateDevice(ctx, model.Device{Name: "f", Address: "10.0.0.2", MAC: "aa:bb:cc:00:00:03", MACs: []string{"aa:bb:cc:00:00:02"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second device claiming a MAC of the set: %v", err)
	}
	if _, err := s.GetDeviceByMAC(ctx, "aa:bb:cc:00:00:03"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create left a MAC behind: %v", err)
	}
}

// Why: the MAC set changes only through an explicit action. An ordinary
// update (full replacement from a possibly stale copy) must not drop or add
// MACs.
func TestUpdateDeviceKeepsMACSet(t *testing.T) {
	s, _, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "e", Address: "10.0.0.1", MAC: "aa:bb:cc:00:00:01", MACs: []string{"aa:bb:cc:00:00:02"}})
	d, _ := s.GetDevice(ctx, id)
	d.Name = "renamed"
	d.MACs = nil // a caller that does not know the set
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDevice(ctx, id)
	if len(got.MACs) != 2 {
		t.Fatalf("MACs = %v, want both kept", got.MACs)
	}
}

// Why: all live devices have a web user but no password (TasmoAdmin import).
// "No password" means no credentials; the user must not make a device look
// protected. The stored row is left untouched.
func TestUserWithoutPasswordLoadsAsNoCredentials(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO devices (name, address, http_user, created_at, updated_at) VALUES ('a','10.0.0.1','admin',0,0)`); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDevice(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.HTTPUser != "" || d.HasCredentials() {
		t.Fatalf("device = %+v", d)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT http_user FROM devices WHERE id=1`).Scan(&raw); err != nil || raw != "admin" {
		t.Fatalf("stored user = %q (%v): loading must not delete data", raw, err)
	}
}

// Why: a deleted device must not keep its MACs, or a new device with the
// same hardware could never be added (and lookups would find a ghost).
func TestDeletingDeviceRemovesItsMACs(t *testing.T) {
	s, _, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "e", Address: "10.0.0.1", MAC: "aa:bb:cc:00:00:01", MACs: []string{"aa:bb:cc:00:00:02"}})
	if err := s.DeleteDevice(ctx, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_macs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("device_macs rows after delete = %d (%v)", n, err)
	}
}

// Why: MACs arrive in many spellings; lookup and write must agree on one form
// or the same hardware would exist twice.
func TestMACsAreNormalisedOnWriteAndLookup(t *testing.T) {
	s, _, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "e", Address: "10.0.0.1", MAC: "AA:BB:CC:00:00:01", MACs: []string{"AA:BB:CC:00:00:02"}})
	d, err := s.GetDeviceByMAC(ctx, "AA:BB:CC:00:00:02")
	if err != nil || d.ID != id || d.MACs[0] != "aa:bb:cc:00:00:01" {
		t.Fatalf("%+v %v", d, err)
	}
}
