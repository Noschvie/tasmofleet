package snapshot

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type fakeSrc struct{ err error }

func (f fakeSrc) VacuumInto(_ context.Context, dst string) error {
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(dst, []byte("SQLite format 3\x00 not a real db"), 0o600)
}

// realStore opens a real migrated database: the snapshot must be a database TasmoFleet can open.
func realStore(t *testing.T) (*store.SQLite, string) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // test cleanup
	return st, dir
}

// A restore is only worth anything if the copy is a database that opens and carries the same
// schema version as the live one (a newer or older version would be refused or migrated).
func TestExportIsAValidDatabaseWithTheLiveSchemaVersion(t *testing.T) {
	st, dir := realStore(t)
	m := New(st, dir)
	ex, err := m.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close() //nolint:errcheck // test
	if ex.ContentType != "application/vnd.sqlite3" || filepath.Ext(ex.Name) != ".db" || !ValidName(ex.Name) {
		t.Fatalf("download %q (%s)", ex.Name, ex.ContentType)
	}
	tmp := filepath.Join(t.TempDir(), "copy.db")
	data, err := io.ReadAll(ex.File)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != ex.Size {
		t.Fatalf("size %d, announced %d", len(data), ex.Size)
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The live database is migrated to the newest file; the copy must say the same (the list shows
	// this number) and open as a TasmoFleet database without being migrated.
	migs, err := fs.Glob(store.Migrations, "migrations/*.sql")
	if err != nil || len(migs) == 0 {
		t.Fatalf("migrations: %v %d", err, len(migs))
	}
	if got := schemaOf(tmp); got != len(migs) {
		t.Fatalf("copy has schema %d, live database %d", got, len(migs))
	}
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.Open(tmp, box)
	if err != nil {
		t.Fatalf("the copy does not open: %v", err)
	}
	cp.Close() //nolint:errcheck // test
}

// The key is the second half of a restore, but it is also the thing that makes the file's
// secrets readable: it must only ever leave when the admin ticked the box.
func TestSecretKeyIsInTheDownloadOnlyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(dir, secretbox.KeyFile), key, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(fakeSrc{}, dir)

	plain, err := m.Export(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(plain.File)
	plain.Close() //nolint:errcheck // test
	if string(b[:15]) != "SQLite format 3" || filepath.Ext(plain.Name) != ".db" {
		t.Fatalf("without the option the download is the bare database, got %q %q", plain.Name, b[:15])
	}

	withKey, err := m.Export(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer withKey.Close() //nolint:errcheck // test
	if filepath.Ext(withKey.Name) != ".tar" || withKey.ContentType != "application/x-tar" {
		t.Fatalf("download %q (%s)", withKey.Name, withKey.ContentType)
	}
	got := map[string]string{}
	tr := tar.NewReader(withKey.File)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[h.Name] = string(body)
	}
	if got[secretbox.KeyFile] != string(key) || got["tasmofleet.db"] == "" || len(got) != 2 {
		t.Fatalf("tar holds %d entries: %v", len(got), keys(got))
	}
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	return k
}

// The copy holds the database (and maybe the key) in the clear; it must not outlive the download.
func TestExportRemovesItsScratchCopyOnClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretbox.KeyFile), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(fakeSrc{}, dir)
	ex, err := m.Export(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".snapshot-*")); len(left) != 1 {
		t.Fatalf("expected one scratch dir while downloading, got %v", left)
	}
	if err := ex.Close(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".snapshot-*")); len(left) != 0 {
		t.Fatalf("scratch copy left behind: %v", left)
	}
}

func TestExportFailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{err: errors.New("disk full")}, dir)
	if _, err := m.Export(context.Background(), false); err == nil {
		t.Fatal("expected the copy error")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".snapshot-*")); len(left) != 0 {
		t.Fatalf("scratch copy left behind: %v", left)
	}
}

func at(m *Manager, ts string) {
	t, err := time.ParseInLocation("2006-01-02 15:04", ts, time.Local)
	if err != nil {
		panic(err)
	}
	m.now = func() time.Time { return t }
}

// Retention is the promise that makes a schedule safe to leave on: exactly N stay, the newest.
func TestPruneKeepsTheNewestN(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	for _, ts := range []string{"2026-10-25 03:00", "2026-10-26 03:00", "2026-10-27 03:00", "2026-10-28 03:00", "2026-10-29 03:00"} {
		at(m, ts)
		if _, _, err := m.Run(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	at(m, "2026-10-30 03:00")
	in, pruned, err := m.Run(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 3 || in.Name != "tasmofleet-20261030-0300.db" {
		t.Fatalf("pruned %d, took %s", pruned, in.Name)
	}
	list, _ := m.List()
	var names []string
	for _, l := range list {
		names = append(names, l.Name)
	}
	want := []string{"tasmofleet-20261030-0300.db", "tasmofleet-20261029-0300.db", "tasmofleet-20261028-0300.db"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("kept %v, want %v", names, want)
	}
}

// A failed run must report why, and must not cost a stored snapshot: deleting old ones when the
// new one could not be written would leave less protection than before.
func TestFailedRunIsReportedAndDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	at(m, "2026-10-29 03:00")
	if _, _, err := m.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	m.src = fakeSrc{err: &os.PathError{Op: "write", Path: "x", Err: errors.New("no space left on device")}}
	at(m, "2026-10-30 03:00")
	if _, _, err := m.Run(context.Background(), 1); err == nil {
		t.Fatal("expected the failure")
	}
	st := m.Status()
	if !st.Failed || st.Reason != "no space left on device" || st.File != "tasmofleet-20261030-0300.db" {
		t.Fatalf("status %+v", st)
	}
	if list, _ := m.List(); len(list) != 1 || list[0].Name != "tasmofleet-20261029-0300.db" {
		t.Fatalf("stored after the failure: %v", list)
	}
	if st.LastGood.IsZero() {
		t.Fatal("the last good snapshot is still known")
	}
	if left, _ := filepath.Glob(filepath.Join(m.Dir(), ".snapshot-*")); len(left) != 0 {
		t.Fatalf("scratch left: %v", left)
	}
}

// A name comes from a URL. Nothing but a snapshot's own file name may reach the file system.
func TestOpenAndDeleteRefuseNamesOutsideTheSnapshotsFolder(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	at(m, "2026-10-29 03:00")
	if _, _, err := m.Run(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasmofleet-20261001-0000.db"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.key"), filepath.Join(m.Dir(), "tasmofleet-20260101-0000.db")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../secret.key", "..%2fsecret.key", "../tasmofleet-20261001-0000.db", "/etc/passwd", "tasmofleet-20261029-0300.db/../../secret.key",
		"tasmofleet-20261029-0300.db\x00", "secret.key", "", "tasmofleet-20261029-0300.dbx", "tasmofleet-1-2.db",
		"tasmofleet-20260101-0000.db", // a symlink in the folder: not a regular file
		"tasmofleet-20269999-9999.db", // well-formed but not there
	} {
		if f, _, err := m.Open(bad); !errors.Is(err, ErrNotFound) {
			if f != nil {
				f.Close() //nolint:errcheck // test
			}
			t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
		}
		if err := m.Delete(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "tasmofleet-20261001-0000.db")); err != nil {
		t.Fatalf("a file outside the folder was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("the key was touched: %v", err)
	}
	if list, _ := m.List(); len(list) != 1 {
		t.Fatalf("the symlink must not be listed, got %v", list)
	}
	f, in, err := m.Open("tasmofleet-20261029-0300.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck // test
	if in.Size == 0 {
		t.Fatal("size")
	}
	if err := m.Delete("tasmofleet-20261029-0300.db"); err != nil {
		t.Fatal(err)
	}
}

// A restart must not take an extra snapshot of its own accord, or a container that restarts
// often would push the daily copies out of the retention.
func TestDueWaitsForTheInterval(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	if !m.Due(24 * time.Hour) {
		t.Fatal("nothing stored: due")
	}
	at(m, "2026-10-29 03:00")
	if _, _, err := m.Run(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(m.Dir(), "tasmofleet-20261029-0300.db"), m.now(), m.now()); err != nil {
		t.Fatal(err)
	}
	at(m, "2026-10-29 12:00")
	if m.Due(24 * time.Hour) {
		t.Fatal("9 hours old, interval 24h: not due")
	}
	at(m, "2026-10-30 03:00")
	if !m.Due(24 * time.Hour) {
		t.Fatal("24 hours old: due")
	}
}

func TestCleanTempRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	for _, p := range []string{filepath.Join(dir, ".snapshot-a"), filepath.Join(m.Dir(), ".snapshot-b")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m.CleanTemp()
	for _, pat := range []string{filepath.Join(dir, ".snapshot-*"), filepath.Join(m.Dir(), ".snapshot-*")} {
		if left, _ := filepath.Glob(pat); len(left) != 0 {
			t.Fatalf("left %v", left)
		}
	}
}
