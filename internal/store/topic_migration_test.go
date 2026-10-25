package store

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
)

// Why (issue #52): devices stored before the topic was resolved hold "tasmota_%06X". Nothing on
// the bus matches that, so they showed "MQTT: Not connected" while online. The upgrade itself
// must repair what the stored MAC determines (Tasmota expands %06X to the last 6 hex digits of
// the Wi-Fi MAC, upper case), without anyone clicking "Refresh identity" on every device, and
// must leave alone what it cannot compute or what the user chose.
func TestMigration0028ResolvesTheTopicPlaceholderFromTheMAC(t *testing.T) {
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
		if p < "migrations/0028" {
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
	ctx := context.Background()
	for _, r := range []struct{ name, addr, mac, topic string }{
		{"resolved", "10.0.0.1", "08:d1:f9:2b:e9:70", "tasmota_%06X"},
		{"other-form", "10.0.0.2", "08:d1:f9:2b:e9:80", "tasmota_%08X"}, // not computed here: left to a refresh
		{"suffix", "10.0.0.3", "08:d1:f9:2b:e9:90", "a_%06X_b"},         // Tasmota drops the suffix: do not guess
		{"no-mac", "10.0.0.4", "", "nomac_%06X"},
		{"user", "10.0.0.5", "08:d1:f9:2b:e9:a0", "my-topic"},
		{"taken", "10.0.0.6", "08:d1:f9:2b:e9:b0", "dup_%06X"},
		{"holder", "10.0.0.7", "08:d1:f9:2b:e9:c0", "dup_2BE9B0"}, // already owns the topic "taken" would get
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO devices (name, address, mac, topic, created_at, updated_at) VALUES (?,?,?,?,0,0)`, r.name, r.addr, r.mac, r.topic); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	// The bus and the health tracker find a device by the topic of its messages: after the
	// upgrade the message of device "resolved" belongs to it, with no manual refresh.
	var id int
	if err := db.QueryRowContext(ctx, `SELECT id FROM devices WHERE topic = 'tasmota_2BE970'`).Scan(&id); err != nil {
		t.Fatalf("no device owns the real topic tasmota_2BE970: %v", err)
	}
	want := map[string]string{"resolved": "tasmota_2BE970", "other-form": "tasmota_%08X", "suffix": "a_%06X_b", "no-mac": "nomac_%06X", "user": "my-topic", "taken": "dup_%06X", "holder": "dup_2BE9B0"}
	rows, err := db.QueryContext(ctx, `SELECT name, topic FROM devices`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // read-only test query
	for rows.Next() {
		var name, topic string
		if err := rows.Scan(&name, &topic); err != nil {
			t.Fatal(err)
		}
		if topic != want[name] {
			t.Errorf("%s: topic %q, want %q", name, topic, want[name])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
