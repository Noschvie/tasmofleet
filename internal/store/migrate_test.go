package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func rawDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // temp DB
	return db, path
}

func mig(names ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for i, n := range names {
		sqlText := "CREATE TABLE t_" + n + " (id INTEGER);"
		if i == 0 {
			sqlText = "CREATE TABLE t_" + n + " (id INTEGER PRIMARY KEY);"
		}
		m["migrations/"+n+".sql"] = &fstest.MapFile{Data: []byte(sqlText)}
	}
	return m
}

func version(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrateAppliesInOrderAndKeepsNewestThreeCopies(t *testing.T) {
	db, path := rawDB(t)
	steps := []string{"0001_a", "0002_b", "0003_c", "0004_d", "0005_e", "0006_f"}
	for n := 1; n <= len(steps); n++ {
		if err := migrate(db, path, mig(steps[:n]...)); err != nil {
			t.Fatal(err)
		}
		if v := version(t, db); v != n {
			t.Fatalf("version %d, want %d", v, n)
		}
	}
	// First migration runs on an empty database: nothing to preserve, so no copy.
	// Later ones copy the database as it was before: pre-2 .. pre-6, newest 3 kept.
	got, _ := filepath.Glob(path + ".pre-*")
	want := []string{path + ".pre-4", path + ".pre-5", path + ".pre-6"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("copies %v, want %v", got, want)
	}
	// The copy holds the state BEFORE the migration: pre-6 has table 5 but not 6.
	cp, err := sql.Open("sqlite", "file:"+path+".pre-6?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close() //nolint:errcheck // temp DB
	if err := cp.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t_0005_e").Scan(new(int)); err != nil {
		t.Fatalf("pre-6 copy lacks table e: %v", err)
	}
	if err := cp.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t_0006_f").Scan(new(int)); err == nil {
		t.Fatal("pre-6 copy already contains table f")
	}
	fi, _ := os.Stat(path + ".pre-6")
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("copy mode %v exposes device data", fi.Mode().Perm())
	}
}

// A broken migration must leave the schema and version exactly as before,
// otherwise the next start would try to re-apply half of it.
func TestMigrateFailureRollsBack(t *testing.T) {
	db, path := rawDB(t)
	if err := migrate(db, path, mig("0001_a")); err != nil {
		t.Fatal(err)
	}
	bad := mig("0001_a", "0002_b")
	bad["migrations/0002_b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE t_0002_b (id INTEGER); THIS IS NOT SQL;")}
	if err := migrate(db, path, bad); err == nil {
		t.Fatal("expected error")
	}
	if v := version(t, db); v != 1 {
		t.Fatalf("version %d after failed migration", v)
	}
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t_0002_b").Scan(new(int)); err == nil {
		t.Fatal("table b survived the rolled-back migration")
	}
}

func TestMigrateRejectsNewerSchemaWithoutTouchingIt(t *testing.T) {
	db, path := rawDB(t)
	if err := migrate(db, path, mig("0001_a", "0002_b")); err != nil {
		t.Fatal(err)
	}
	err := migrate(db, path, mig("0001_a"))
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err %v", err)
	}
	if v := version(t, db); v != 2 {
		t.Fatalf("version changed to %d", v)
	}
}

// A gap or a duplicate number means a merge went wrong; applying around it
// would build a schema nobody tested.
func TestMigrateRejectsBadNumbering(t *testing.T) {
	for name, files := range map[string][]string{
		"gap":       {"0001_a", "0003_c"},
		"no number": {"0001_a", "init"},
		"duplicate": {"0001_a", "0001_b"},
		"not first": {"0002_b"},
	} {
		t.Run(name, func(t *testing.T) {
			db, path := rawDB(t)
			if err := migrate(db, path, mig(files...)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
