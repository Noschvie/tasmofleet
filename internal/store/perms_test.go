//go:build !windows

// POSIX file modes only: tasmofleet ships for Linux, so the file is not built on Windows.

package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/secretbox"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// The DB holds session hashes, sealed device passwords and the audit log, so
// it must never be readable by other local users, whatever the umask is.
func TestOpenCreatesPrivateFiles(t *testing.T) {
	box, _ := secretbox.LoadOrCreate(t.TempDir())
	path := filepath.Join(t.TempDir(), "new", "data", "tasmofleet.db")
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()                                                            //nolint:errcheck // temp DB
	if _, err := s.CreateDevice(ctx, deviceFor("a", "10.0.0.1")); err != nil { // forces WAL/SHM into existence
		t.Fatal(err)
	}
	if m := modeOf(t, filepath.Dir(path)); m != 0o700 {
		t.Errorf("data dir mode %v, want 0700", m)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if m := modeOf(t, p); m&0o077 != 0 {
			t.Errorf("%s mode %v exposes group/other", p, m)
		}
	}
}

func TestOpenRepairsLooseDatabaseMode(t *testing.T) {
	box, _ := secretbox.LoadOrCreate(t.TempDir())
	path := filepath.Join(t.TempDir(), "tasmofleet.db")
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	s.Close() //nolint:errcheck // temp DB
	// Simulate a database from an older version or a restore with a loose mode.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close() //nolint:errcheck // temp DB
	if m := modeOf(t, path); m&0o077 != 0 {
		t.Errorf("db mode %v not repaired", m)
	}
}

func TestOpenDoesNotLoosenExistingDir(t *testing.T) {
	box, _ := secretbox.LoadOrCreate(t.TempDir())
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	s.Close() //nolint:errcheck // temp DB
	if m := modeOf(t, dir); m != 0o700 {
		t.Fatalf("dir mode %v", m)
	}
}

func TestRepairModesFixesWalAndShm(t *testing.T) {
	base := filepath.Join(t.TempDir(), "x.db")
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.WriteFile(p, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o664); err != nil {
			t.Fatal(err)
		}
	}
	if err := repairModes(base); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if m := modeOf(t, p); m != 0o600 {
			t.Errorf("%s mode %v, want 0600", p, m)
		}
	}
}

// The copy must never be visible with a looser mode, not even briefly, and no
// scratch directory may be left behind.
func TestPreMigrationCopyIsPrivateAndLeavesNoScratch(t *testing.T) {
	db, path := rawDB(t)
	for _, files := range [][]string{{"0001_a"}, {"0001_a", "0002_b"}} { // a fresh DB gets no copy
		if err := migrate(db, path, mig(files...)); err != nil {
			t.Fatal(err)
		}
	}
	if m := modeOf(t, path+".pre-2"); m != 0o600 {
		t.Fatalf("copy mode %v", m)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".premigrate-*"))
	if len(left) != 0 {
		t.Fatalf("scratch dirs left: %v", left)
	}
}
