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
	t, err := time.ParseInLocation("2006-01-02 15:04", ts, time.UTC)
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
		if _, err := m.Run(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	at(m, "2026-10-30 03:00")
	res, err := m.Run(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	in, pruned := res.Info, res.Pruned
	if pruned != 3 || in.Name != "tasmofleet-20261030-0300Z.db" {
		t.Fatalf("pruned %d, took %s", pruned, in.Name)
	}
	list, _ := m.List()
	var names []string
	for _, l := range list {
		names = append(names, l.Name)
	}
	want := []string{"tasmofleet-20261030-0300Z.db", "tasmofleet-20261029-0300Z.db", "tasmofleet-20261028-0300Z.db"}
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
	if _, err := m.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	m.src = fakeSrc{err: &os.PathError{Op: "write", Path: "x", Err: errors.New("no space left on device")}}
	at(m, "2026-10-30 03:00")
	if _, err := m.Run(context.Background(), 1); err == nil {
		t.Fatal("expected the failure")
	}
	st := m.Status()
	if !st.Failed || st.Reason != "no space left on device" || st.File != "tasmofleet-20261030-0300Z.db" {
		t.Fatalf("status %+v", st)
	}
	if list, _ := m.List(); len(list) != 1 || list[0].Name != "tasmofleet-20261029-0300Z.db" {
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
	if _, err := m.Run(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasmofleet-20261001-0000Z.db"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret.key"), filepath.Join(m.Dir(), "tasmofleet-20260101-0000Z.db")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../secret.key", "..%2fsecret.key", "../tasmofleet-20261001-0000Z.db", "/etc/passwd", "tasmofleet-20261029-0300Z.db/../../secret.key",
		"tasmofleet-20261029-0300Z.db\x00", "secret.key", "", "tasmofleet-20261029-0300Z.dbx", "tasmofleet-1-2.db",
		"tasmofleet-20260101-0000Z.db", // a symlink in the folder: not a regular file
		"tasmofleet-20269999-9999Z.db", // well-formed but not there
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
	if _, err := os.Stat(filepath.Join(dir, "tasmofleet-20261001-0000Z.db")); err != nil {
		t.Fatalf("a file outside the folder was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("the key was touched: %v", err)
	}
	if list, _ := m.List(); len(list) != 1 {
		t.Fatalf("the symlink must not be listed, got %v", list)
	}
	f, in, err := m.Open("tasmofleet-20261029-0300Z.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck // test
	if in.Size == 0 {
		t.Fatal("size")
	}
	if err := m.Delete("tasmofleet-20261029-0300Z.db"); err != nil {
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
	if _, err := m.Run(context.Background(), 7); err != nil {
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

// Why: a run whose snapshot is written but whose old files cannot be deleted is a good run. The
// card and the audit must not call it a failure, or the admin would chase a problem that is only
// a stuck retention.
func TestPruneFailureDoesNotMakeTheRunFail(t *testing.T) {
	m := New(fakeSrc{}, t.TempDir())
	at(m, "2026-10-28 03:00")
	if _, err := m.Run(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	m.remove = func(string) error { return errors.New("read-only file system") }
	at(m, "2026-10-29 03:00")
	res, err := m.Run(context.Background(), 1)
	if err != nil || res.PruneErr == nil || res.Info.Name != "tasmofleet-20261029-0300Z.db" {
		t.Fatalf("run %+v err %v: the snapshot is good, only the prune failed", res, err)
	}
	if st := m.Status(); st.Failed {
		t.Fatalf("status must stay good: %+v", st)
	}
}

// Why: names carry the time in UTC and order, age and pruning follow the name. The mtime of a
// file changes with copies and restores of the data directory and must not decide what is
// "newest" or gets deleted.
func TestOrderAndPruneFollowTheNameNotTheModificationTime(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	for _, ts := range []string{"2026-10-27 03:00", "2026-10-28 03:00", "2026-10-29 03:00"} {
		at(m, ts)
		if _, err := m.Run(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) // the oldest snapshot looks like the newest by mtime
	if err := os.Chtimes(filepath.Join(m.Dir(), "tasmofleet-20261027-0300Z.db"), old, old); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List()
	if list[0].Name != "tasmofleet-20261029-0300Z.db" || !list[0].Taken.Equal(time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("newest by name expected, got %s %v", list[0].Name, list[0].Taken)
	}
	if n, err := m.Prune(2); err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if _, _, err := m.Open("tasmofleet-20261027-0300Z.db"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the oldest by name goes, whatever its mtime says")
	}
}

// Why: a clock that jumps back must not make the run delete the snapshot it just wrote.
func TestPruneNeverDeletesTheFileJustWritten(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	for _, ts := range []string{"2026-10-28 03:00", "2026-10-29 03:00"} {
		at(m, ts)
		if _, err := m.Run(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	at(m, "2026-10-01 03:00") // the clock went back
	res, err := m.Run(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Open(res.Info.Name); err != nil {
		t.Fatalf("the snapshot just written was pruned: %v", err)
	}
}

// Why: after a restart the next run counts from the newest stored snapshot (not from the start of
// the process), so the card's "Next" is right; an overdue schedule runs at once; a failing disk is
// retried one interval after the failure, not in a loop.
func TestNextRunCountsFromTheLastSnapshotOrFailure(t *testing.T) {
	dir := t.TempDir()
	m := New(fakeSrc{}, dir)
	at(m, "2026-10-29 03:00")
	if !m.NextRun(24 * time.Hour).Equal(m.now()) {
		t.Fatal("nothing stored: now")
	}
	if _, err := m.Run(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	fresh := New(fakeSrc{}, dir) // a restarted process: no memory of the run
	at(fresh, "2026-10-29 20:00")
	want := time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC)
	if got := fresh.NextRun(24 * time.Hour); !got.Equal(want) || fresh.Due(24*time.Hour) {
		t.Fatalf("next %v, want %v (not due yet)", got, want)
	}
	at(fresh, "2026-11-02 12:00")
	if !fresh.Due(24 * time.Hour) {
		t.Fatal("overdue after the restart: runs at once")
	}
	fresh.src = failSrc{}
	if _, err := fresh.Run(context.Background(), 7); err == nil {
		t.Fatal("expected failure")
	}
	if got, want := fresh.NextRun(24*time.Hour), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC); !got.Equal(want) || fresh.Due(24*time.Hour) {
		t.Fatalf("after a failure the retry is one interval later: %v", got)
	}
}

type failSrc struct{}

func (failSrc) VacuumInto(context.Context, string) error { return errors.New("disk full") }
