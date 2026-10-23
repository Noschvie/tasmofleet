package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// frozenMigrations are the SHA-256 sums of migration files that have shipped.
// Why: a database records only its schema version, so editing a released
// file would leave existing installations silently on the old schema. A
// change is a new NNNN_*.sql file. Add a line here when a migration ships;
// never change an existing one.
var frozenMigrations = map[string]string{
	"0001_init.sql": "edef2fb653d9d81f449d3e091c6ce74f7a0986b2ac595e38bb414bf34fdbfabb",
	"0002_auth.sql": "fa895be00d16f0a004281d87744cf695a0d38199aea4588bd8c208930167e541",
}

func TestReleasedMigrationsAreUnchanged(t *testing.T) {
	for name, want := range frozenMigrations {
		b, err := fs.ReadFile(Migrations, "migrations/"+name)
		if err != nil {
			t.Fatalf("released migration %s is gone: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s changed (sha256 %s); released migrations are append-only, add a new migration instead", name, got)
		}
	}
}

// Why: installations of v0.0.1 hold a 0001 database with break-glass admins
// and live sessions; upgrading must keep them working.
func TestMigration0002KeepsExistingData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	first, err := fs.ReadFile(Migrations, "migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	only0001 := fstest.MapFS{"migrations/0001_init.sql": &fstest.MapFile{Data: first}}
	if err := migrate(db, path, only0001); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO local_users (username, password_hash, updated_at) VALUES ('alice', 'hash-a', 1700000000000)`,
		`INSERT INTO sessions (id_hash, principal, csrf_token, created_at, last_seen, expires_at)
		 VALUES ('s1', '{"Subject":"local:alice","Name":"alice","Method":"local"}', 'csrf', 1, 1, 9999999999999)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	upTo2 := fstest.MapFS{}
	for _, n := range []string{"0001_init.sql", "0002_auth.sql"} { // later migrations have their own tests
		b, err := fs.ReadFile(Migrations, "migrations/"+n)
		if err != nil {
			t.Fatal(err)
		}
		upTo2["migrations/"+n] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, upTo2); err != nil {
		t.Fatalf("0001 -> 0002: %v", err)
	}
	var role, hash, subject string
	var disabled, must, gen, sgen int64
	if err := db.QueryRowContext(ctx, "SELECT role, password_hash, disabled, must_change_password, password_gen FROM local_users WHERE username='alice'").
		Scan(&role, &hash, &disabled, &must, &gen); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || hash != "hash-a" || disabled != 0 || must != 0 || gen != 1 {
		t.Errorf("legacy user: role %q hash %q disabled %d must %d gen %d (break-glass users stay admins)", role, hash, disabled, must, gen)
	}
	if err := db.QueryRowContext(ctx, "SELECT subject, password_gen FROM sessions WHERE id_hash='s1'").Scan(&subject, &sgen); err != nil {
		t.Fatal(err)
	}
	if subject != "local:alice" {
		t.Errorf("session subject = %q, want it backfilled so the user's sessions can be revoked", subject)
	}
	if sgen == gen {
		t.Error("a pre-upgrade session must not match the current password generation")
	}
	if _, err := db.ExecContext(ctx, "SELECT id_hash, username, issued_at, password_gen FROM device_cookies"); err != nil {
		t.Errorf("device_cookies: %v", err)
	}
	if v := version(t, db); v != 2 {
		t.Errorf("schema version %d, want 2", v)
	}
}

// Why: sessions that exist when 0003 is applied were created on the only
// listener there was; they must stay valid there and nowhere else.
func TestMigration0003MarksExistingSessionsAsMainListener(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	upTo2 := fstest.MapFS{}
	for _, n := range []string{"0001_init.sql", "0002_auth.sql"} {
		b, err := fs.ReadFile(Migrations, "migrations/"+n)
		if err != nil {
			t.Fatal(err)
		}
		upTo2["migrations/"+n] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, upTo2); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id_hash, principal, csrf_token, created_at, last_seen, expires_at)
		VALUES ('s1', '{"Subject":"local:alice","Method":"local"}', 'csrf', 1, 1, 9999999999999)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0002 -> 0003: %v", err)
	}
	var l string
	if err := db.QueryRowContext(ctx, "SELECT listener FROM sessions WHERE id_hash='s1'").Scan(&l); err != nil {
		t.Fatal(err)
	}
	if l != "main" {
		t.Errorf("legacy session listener = %q, want main", l)
	}
}

// Why: the listener check is gone, so a session of the removed admin listener
// would be accepted on the main one. The migration must delete it, and keep
// the sessions of the main listener.
func TestMigration0011DropsAdminListenerSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // temp DB
	upTo10 := fstest.MapFS{}
	entries, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0011" {
			continue
		}
		b, err := fs.ReadFile(Migrations, "migrations/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		upTo10["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrate(db, path, upTo10); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for id, l := range map[string]string{"a": "admin", "m": "main"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id_hash, principal, csrf_token, created_at, last_seen, expires_at, listener)
			VALUES (?, '{"Subject":"local:alice","Method":"local"}', 'c', 1, 1, 9999999999999, ?)`, id, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, Migrations); err != nil {
		t.Fatalf("0010 -> 0011: %v", err)
	}
	var ids string
	if err := db.QueryRowContext(ctx, "SELECT group_concat(id_hash) FROM sessions").Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != "m" {
		t.Errorf("sessions left = %q, want only the main one", ids)
	}
}
