package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: the name ends up in a path below the data directory. Whatever a device
// reports as hostname or a user types as name must never leave the directory,
// hide itself or exceed common file-name limits.
func TestSanitizeName(t *testing.T) {
	long := strings.Repeat("a", 200)
	for _, c := range []struct{ in, want string }{
		{"office-monitor", "office-monitor"},
		{"a.b_c-D9", "a.b_c-D9"},
		{"../x", "_x"}, // no traversal: dots lead nowhere, the slash is replaced
		{"..", ""},
		{".", ""},
		{".hidden", "hidden"},
		{"my device", "my_device"},
		{"Küche/Spot", "K_che_Spot"},
		{"日本", "__"},
		{long, long[:63]},
		{"", ""},
		{"  ", ""},
		{"a/../b", "a_.._b"},
	} {
		if got := SanitizeName(c.in); got != c.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Why: the hostname is what the user sees on the device and on the network, so
// it wins; the inventory name and finally the ID keep the directory defined
// when a device never reported one.
func TestBaseNameFallbackOrder(t *testing.T) {
	d := model.Device{ID: 9, Name: "Wohnzimmer Lampe"}
	d.Net.Hostname = "lamp-wz"
	if got := baseName(d); got != "lamp-wz" {
		t.Errorf("hostname first: %q", got)
	}
	d.Net.Hostname = "///" // sanitises to "___": still usable, never empty
	if got := baseName(d); got != "___" {
		t.Errorf("odd hostname: %q", got)
	}
	d.Net.Hostname = "..."
	if got := baseName(d); got != "Wohnzimmer_Lampe" {
		t.Errorf("name when hostname unusable: %q", got)
	}
	d.Name = ""
	if got := baseName(d); got != "device-9" {
		t.Errorf("id last: %q", got)
	}
}

// fleet serves several devices to one Manager.
type fleet map[model.DeviceID]model.Device

func (f fleet) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	if d, ok := f[id]; ok {
		return d, nil
	}
	return model.Device{}, store.ErrNotFound
}

func host(id model.DeviceID, name, hostname string) model.Device {
	d := model.Device{ID: id, Name: name, Address: "172.31.13.28"}
	d.Net.Hostname = hostname
	return d
}

func fleetEnv(t *testing.T, devs ...model.Device) (*env, fleet) {
	t.Helper()
	f := fleet{}
	for _, d := range devs {
		f[d.ID] = d
	}
	return newEnv(t, func(o *Options) { o.Devices = f }), f
}

func backupOf(t *testing.T, e *env, id model.DeviceID) model.Backup {
	t.Helper()
	made, err := e.m.Backup(t.Context(), "tester", id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := byKindUnit(made, model.BackupConfigDump)
	return b
}

// Why (#129): files carry the hostname so a person looking into the data dir
// (or a restored share) knows whose file it is.
func TestBackupUsesHostnameForDirAndFiles(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "Kitchen light", "kitchen-spot"))
	made, err := e.m.Backup(t.Context(), "t", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"backups/kitchen-spot/20261018T120000Z_kitchen-spot.dmp",
		"backups/kitchen-spot/20261018T120000Z_kitchen-spot.snapshot.json",
		"backups/kitchen-spot/20261018T120000Z_kitchen-spot.decoded.json",
	}
	for i, b := range made {
		if b.Path != want[i] {
			t.Errorf("path %d = %s, want %s", i, b.Path, want[i])
		}
		if _, _, err := e.m.Open(t.Context(), b.ID); err != nil {
			t.Errorf("Open: %v", err)
		}
	}
}

// Why: a second run in the same second must not overwrite the dump: the free
// stamp check has to look at the NEW file name.
func TestFreeTimestampUsesNewName(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	a, b := backupOf(t, e, 7), backupOf(t, e, 7)
	if a.Path == b.Path || !strings.HasSuffix(b.Path, "T120001Z_kitchen.dmp") {
		t.Errorf("second run in the same second: %s then %s", a.Path, b.Path)
	}
}

// Why: two devices may report the same hostname (default "tasmota-ABC123"
// style names, cloned configs). Their backups must never share a directory;
// the loser gets <host>_<id>. Orphans (deleted device, kept backups) and
// case differences count as owners too, because the file system may be
// case-insensitive.
func TestDirCollisions(t *testing.T) {
	t.Run("other device", func(t *testing.T) {
		e, _ := fleetEnv(t, host(1, "a", "plug"), host(2, "b", "plug"))
		a, b := backupOf(t, e, 1), backupOf(t, e, 2)
		if !strings.HasPrefix(a.Path, "backups/plug/") || !strings.HasPrefix(b.Path, "backups/plug_2/") {
			t.Errorf("%s / %s", a.Path, b.Path)
		}
		if !strings.Contains(b.Path, "_plug.dmp") {
			t.Errorf("file name keeps the plain hostname: %s", b.Path)
		}
		if c := backupOf(t, e, 2); !strings.HasPrefix(c.Path, "backups/plug_2/") {
			t.Errorf("device 2 stays in its own directory: %s", c.Path)
		}
	})
	t.Run("orphan", func(t *testing.T) {
		e, f := fleetEnv(t, host(1, "a", "plug"))
		backupOf(t, e, 1)
		e.st.orphan(1)
		f[3] = host(3, "c", "plug")
		if b := backupOf(t, e, 3); !strings.HasPrefix(b.Path, "backups/plug_3/") {
			t.Errorf("a deleted device's kept backups own the directory: %s", b.Path)
		}
	})
	t.Run("case-insensitive", func(t *testing.T) {
		e, _ := fleetEnv(t, host(1, "a", "Plug"), host(2, "b", "plug"))
		backupOf(t, e, 1)
		if b := backupOf(t, e, 2); !strings.HasPrefix(b.Path, "backups/plug_2/") {
			t.Errorf("Plug and plug are one directory on some file systems: %s", b.Path)
		}
	})
	t.Run("wildcard characters are literal", func(t *testing.T) {
		e, _ := fleetEnv(t, host(1, "a", "ab"), host(2, "b", "a_"))
		backupOf(t, e, 1)
		if b := backupOf(t, e, 2); !strings.HasPrefix(b.Path, "backups/a_/") {
			t.Errorf("'a_' is not a prefix collision with 'ab': %s", b.Path)
		}
	})
}

// Why: concurrent first backups of two devices with one hostname must not
// both pick the plain directory (the mutex spans choice to first index row).
func TestConcurrentSameHostnameNeverShareDir(t *testing.T) {
	e, _ := fleetEnv(t, host(1, "a", "plug"), host(2, "b", "plug"))
	e.m.o.Decoder = nil // the fake decoder counts calls without a lock
	type result struct {
		b   model.Backup
		err error
	}
	res := make(chan result, 2)
	for _, id := range []model.DeviceID{1, 2} {
		go func() {
			made, err := e.m.Backup(context.Background(), "t", id)
			r := result{err: err}
			if err == nil {
				r.b, _ = byKindUnit(made, model.BackupConfigDump)
			}
			res <- r
		}()
	}
	a, b := <-res, <-res
	if a.err != nil || b.err != nil {
		t.Fatal(a.err, b.err)
	}
	if filepath.Dir(a.b.Path) == filepath.Dir(b.b.Path) {
		t.Fatalf("both in %s", filepath.Dir(a.b.Path))
	}
}

// Why: the DB is the index. A hostname change gives new backups the new
// directory; the old files are never moved, and stay readable.
func TestHostnameChangeKeepsOldFiles(t *testing.T) {
	e, f := fleetEnv(t, host(7, "k", "old-name"))
	oldB := backupOf(t, e, 7)
	f[7] = host(7, "k", "new-name")
	e.clk.t = e.clk.t.Add(time.Hour)
	newB := backupOf(t, e, 7)
	if !strings.HasPrefix(oldB.Path, "backups/old-name/") || !strings.HasPrefix(newB.Path, "backups/new-name/") {
		t.Fatalf("%s / %s", oldB.Path, newB.Path)
	}
	for _, b := range []model.Backup{oldB, newB} {
		if _, _, err := e.m.Open(t.Context(), b.ID); err != nil {
			t.Errorf("%s: %v", b.Path, err)
		}
	}
}

// Why: no empty directory litter after retention or manual delete, but a
// directory that still holds a file must stay.
func TestEmptyDirRemovedAfterDelete(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	first := backupOf(t, e, 7)
	e.clk.t = e.clk.t.Add(time.Hour)
	second := backupOf(t, e, 7)
	dir := filepath.Join(e.dir, "backups", "kitchen")
	if _, err := e.m.DeleteRun(t.Context(), "t", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory with files left was removed: %v", err)
	}
	if _, err := e.m.DeleteRun(t.Context(), "t", second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty directory stays: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "backups")); err != nil {
		t.Errorf("the backups root must never be removed: %v", err)
	}
}

func TestEmptyDirRemovedAfterPrune(t *testing.T) {
	e, f := fleetEnv(t, host(7, "k", "old-name"))
	e.m.SetRetention(1, 0)
	backupOf(t, e, 7)
	f[7] = host(7, "k", "new-name")
	e.clk.t = e.clk.t.Add(time.Hour)
	backupOf(t, e, 7) // keeps 1 run: the old run is pruned and its directory is empty
	if _, err := os.Stat(filepath.Join(e.dir, "backups", "old-name")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pruned run left its empty directory: %v", err)
	}
}

// --- migration ---

type legacyFile struct {
	dev      model.DeviceID
	stamp    string
	kind     model.BackupKind
	content  string
	orphan   bool
	name     string // DeviceName / DeviceMAC stored with the row (orphans keep them)
	mac      string
	skipDisk bool
}

// legacy writes a 0.17 tree + rows and returns the rows.
func legacy(t *testing.T, e *env, files ...legacyFile) []model.Backup {
	t.Helper()
	var out []model.Backup
	for _, lf := range files {
		ext := "json"
		if lf.kind == model.BackupConfigDump {
			ext = "dmp"
		}
		rel := fmt.Sprintf("backups/%d/%s-%s.%s", lf.dev, lf.stamp, lf.kind, ext)
		if !lf.skipDisk {
			full := filepath.Join(e.dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(lf.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		sum := sha256.Sum256([]byte(lf.content))
		b := model.Backup{DeviceID: lf.dev, DeviceName: lf.name, DeviceMAC: lf.mac, Kind: lf.kind, TakenAt: time.Now(), Path: rel, Size: int64(len(lf.content)), SHA256: hex.EncodeToString(sum[:])}
		id, err := e.st.PutBackup(t.Context(), b)
		if err != nil {
			t.Fatal(err)
		}
		b.ID = id
		out = append(out, b)
		if lf.orphan {
			e.st.mu.Lock()
			r := e.st.rows[id]
			r.DeviceID = 0
			e.st.rows[id] = r
			e.st.mu.Unlock()
		}
	}
	return out
}

func sha(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }

// snapshotDisk lists every regular file with its content hash.
func snapshotDisk(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = sha(string(b))
		}
		return nil
	})
	return out
}

func contentsOnDisk(t *testing.T, e *env) map[string]bool {
	t.Helper()
	have := map[string]bool{}
	for _, h := range snapshotDisk(t, e.dir) {
		have[h] = true
	}
	return have
}

// Why: THE data-safety property. After migrating a 0.17 tree every backup row
// still opens with a matching checksum, no content disappeared, the numeric
// directories are gone, and a second run changes nothing.
func TestMigrateLayoutMovesEverythingAndIsIdempotent(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "Kitchen", "kitchen-spot"), host(8, "Hall", ""))
	rows := legacy(t, e,
		legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"},
		legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupSnapshot, content: "snap-a"},
		legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupDecoded, content: "dec-a"},
		legacyFile{dev: 8, stamp: "20261017T080000Z", kind: model.BackupConfigDump, content: "dump-b"},
		legacyFile{dev: 5, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "dump-orphan", orphan: true},
	)
	before := contentsOnDisk(t, e)

	res, err := e.m.MigrateLayout(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 5 || res.Left != 0 || res.Repaired != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, r := range rows {
		got, err := e.st.GetBackup(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		data, _, err := e.m.Open(t.Context(), r.ID)
		if err != nil {
			t.Errorf("backup %d (%s) unreadable after migration: %v", r.ID, got.Path, err)
			continue
		}
		if sha(string(data)) != r.SHA256 {
			t.Errorf("backup %d content changed", r.ID)
		}
		if strings.Contains(r.Path, "-dmp.") && got.Path == r.Path {
			t.Errorf("backup %d not moved: %s", r.ID, got.Path)
		}
	}
	for h := range before {
		if !contentsOnDisk(t, e)[h] {
			t.Errorf("content %s lost", h)
		}
	}
	for path, want := range map[model.BackupID]string{
		rows[0].ID: "backups/kitchen-spot/20261018T120000Z_kitchen-spot.dmp",
		rows[1].ID: "backups/kitchen-spot/20261018T120000Z_kitchen-spot.snapshot.json",
		rows[2].ID: "backups/kitchen-spot/20261018T120000Z_kitchen-spot.decoded.json",
		rows[3].ID: "backups/Hall/20261017T080000Z_Hall.dmp",         // no hostname: inventory name
		rows[4].ID: "backups/orphan-5/20261001T080000Z_orphan-5.dmp", // an orphan without a stored name
	} {
		if g, _ := e.st.GetBackup(t.Context(), path); g.Path != want {
			t.Errorf("row %d path %s, want %s", path, g.Path, want)
		}
	}
	for _, n := range []string{"5", "7", "8"} {
		if _, err := os.Stat(filepath.Join(e.dir, "backups", n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("empty legacy dir %s stays: %v", n, err)
		}
	}

	disk, rowsNow := snapshotDisk(t, e.dir), pathsOf(t, e)
	res2, err := e.m.MigrateLayout(t.Context())
	if err != nil || res2.Moved != 0 || res2.Left != 0 || res2.Repaired != 0 {
		t.Errorf("second run must be a no-op: %+v %v", res2, err)
	}
	if fmt.Sprint(snapshotDisk(t, e.dir)) != fmt.Sprint(disk) || fmt.Sprint(pathsOf(t, e)) != fmt.Sprint(rowsNow) {
		t.Error("second run changed disk or index")
	}
}

func pathsOf(t *testing.T, e *env) map[model.BackupID]string {
	t.Helper()
	all, _ := e.st.ListAllBackups(t.Context())
	m := map[model.BackupID]string{}
	for _, b := range all {
		m[b.ID] = b.Path
	}
	return m
}

// failOnce makes the first UpdateBackupPath fail like a crash right after the
// rename (the process dies before the row is changed).
type failOnce struct {
	*memBackups
	failed bool
}

func (f *failOnce) UpdateBackupPath(ctx context.Context, id model.BackupID, o, n string) (bool, error) {
	if !f.failed {
		f.failed = true
		return false, errors.New("simulated crash")
	}
	return f.memBackups.UpdateBackupPath(ctx, id, o, n)
}

// Why: a crash between rename and row update leaves the row pointing at a
// missing file. The next start must find the moved file by checksum and fix
// the row; the backup is readable again and nothing was deleted.
func TestMigrateLayoutRepairsCrashAfterRename(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	fo := &failOnce{memBackups: e.st}
	e.m.o.Store = fo
	rows := legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"})

	res, _ := e.m.MigrateLayout(t.Context())
	if res.Left != 1 || res.Moved != 0 {
		t.Fatalf("crash run %+v", res)
	}
	if _, _, err := e.m.Open(t.Context(), rows[0].ID); err == nil {
		t.Fatal("precondition: the row should point to the missing old file")
	}
	if got := snapshotDisk(t, e.dir); len(got) != 1 {
		t.Fatalf("exactly one file expected on disk: %v", got)
	}

	res, _ = e.m.MigrateLayout(t.Context())
	if res.Repaired != 1 || res.Left != 0 {
		t.Fatalf("restart run %+v", res)
	}
	if data, _, err := e.m.Open(t.Context(), rows[0].ID); err != nil || string(data) != "dump-a" {
		t.Errorf("not readable after repair: %v", err)
	}
}

// Why: a file that does not match its checksum may be the only copy of
// something; it is never moved, replaced or deleted, only reported.
func TestMigrateLayoutLeavesUnclearCases(t *testing.T) {
	type tc struct {
		name  string
		setup func(t *testing.T, e *env, r model.Backup)
	}
	for _, c := range []tc{
		{"old file corrupt", func(t *testing.T, e *env, r model.Backup) {
			must(t, os.WriteFile(filepath.Join(e.dir, r.Path), []byte("tampered"), 0o600))
		}},
		{"target exists with other content", func(t *testing.T, e *env, r model.Backup) {
			must(t, os.MkdirAll(filepath.Join(e.dir, "backups/kitchen"), 0o700))
			must(t, os.WriteFile(filepath.Join(e.dir, "backups/kitchen/20261018T120000Z_kitchen.dmp"), []byte("other"), 0o600))
		}},
		{"old and new both match", func(t *testing.T, e *env, r model.Backup) {
			must(t, os.MkdirAll(filepath.Join(e.dir, "backups/kitchen"), 0o700))
			must(t, os.WriteFile(filepath.Join(e.dir, "backups/kitchen/20261018T120000Z_kitchen.dmp"), []byte("dump-a"), 0o600))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := fleetEnv(t, host(7, "k", "kitchen"))
			rows := legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"})
			c.setup(t, e, rows[0])
			before := snapshotDisk(t, e.dir)
			res, err := e.m.MigrateLayout(t.Context())
			if err != nil || res.Left != 1 || res.Moved != 0 || res.Repaired != 0 {
				t.Fatalf("%+v %v", res, err)
			}
			if fmt.Sprint(snapshotDisk(t, e.dir)) != fmt.Sprint(before) {
				t.Error("a file was moved, changed or deleted")
			}
			if g, _ := e.st.GetBackup(t.Context(), rows[0].ID); g.Path != rows[0].Path {
				t.Errorf("row changed to %s", g.Path)
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Why: two devices with one hostname must end up in different directories
// during the migration too, never merged into one.
func TestMigrateLayoutHostnameCollision(t *testing.T) {
	e, _ := fleetEnv(t, host(1, "a", "plug"), host(2, "b", "plug"))
	rows := legacy(t, e,
		legacyFile{dev: 1, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "one"},
		legacyFile{dev: 2, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "two"},
	)
	if res, err := e.m.MigrateLayout(t.Context()); err != nil || res.Moved != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	a, _ := e.st.GetBackup(t.Context(), rows[0].ID)
	b, _ := e.st.GetBackup(t.Context(), rows[1].ID)
	if a.Path != "backups/plug/20261018T120000Z_plug.dmp" || b.Path != "backups/plug_2/20261018T120000Z_plug.dmp" {
		t.Errorf("%s / %s", a.Path, b.Path)
	}
}

// Why: a path outside the 0.17 pattern (hand-edited, imported, odd kind) is
// not ours to touch.
func TestMigrateLayoutIgnoresForeignPaths(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	for _, p := range []string{"backups/7/notes.txt", "backups/7/../7/x-dmp.dmp", "other/7/20261018T120000Z-dmp.dmp", "backups/7/20261018T120000Z-dmp.json"} {
		sum := sha("x")
		if _, err := e.st.PutBackup(t.Context(), model.Backup{DeviceID: 7, Kind: model.BackupConfigDump, Path: p, SHA256: sum}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

// Why: a numeric directory that still holds anything (unindexed leftovers,
// orphans) must survive the cleanup: the cleanup only ever uses os.Remove.
func TestMigrateLayoutKeepsNonEmptyNumericDirs(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "d"})
	stray := filepath.Join(e.dir, "backups/7/stray.bin")
	must(t, os.WriteFile(stray, []byte("unindexed"), 0o600))
	if _, err := e.m.MigrateLayout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("unindexed file in legacy dir lost: %v", err)
	}
}

// Why (M1): after a power loss the row may already name the hostname path
// while the rename never reached the disk. If the 0.17 file is still there and
// matches, the row is pointed back and the file migrated: the backup is not
// orphaned.
func TestMigrateLayoutRelinksRowWhoseRenameWasLost(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	rows := legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"})
	newRel := "backups/kitchen/20261018T120000Z_kitchen.dmp"
	if ok, err := e.st.UpdateBackupPath(t.Context(), rows[0].ID, rows[0].Path, newRel); !ok || err != nil {
		t.Fatal(ok, err)
	}
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 1 || res.Left != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if data, b, err := e.m.Open(t.Context(), rows[0].ID); err != nil || string(data) != "dump-a" || b.Path != newRel {
		t.Errorf("not readable at the new path: %v %s", err, b.Path)
	}
}

// Why (M1): the safety net only trusts a checksum match; a different file at
// the legacy name is left alone and the row is not touched.
func TestMigrateLayoutRelinkRequiresChecksum(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	rows := legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"})
	must(t, os.WriteFile(filepath.Join(e.dir, rows[0].Path), []byte("other"), 0o600))
	newRel := "backups/kitchen/20261018T120000Z_kitchen.dmp"
	_, _ = e.st.UpdateBackupPath(t.Context(), rows[0].ID, rows[0].Path, newRel)
	if _, err := e.m.MigrateLayout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.st.GetBackup(t.Context(), rows[0].ID); g.Path != newRel {
		t.Errorf("row changed to %s", g.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, rows[0].Path)); string(b) != "other" {
		t.Error("legacy file touched")
	}
}

// Why (M1): the rename is only trusted after the directories are synced; a
// directory that cannot be synced makes the sync fail loudly, not silently.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("existing dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing dir must fail")
	}
}

// Why (M2): device A's only legacy row is left in place (corrupt), so the index
// does not show A in the hostname directory. B, with the same hostname, must
// still not take the directory A is entitled to.
func TestMigrateLayoutSameHostnameFirstRowLeft(t *testing.T) {
	e, _ := fleetEnv(t, host(1, "a", "plug"), host(2, "b", "plug"))
	rows := legacy(t, e,
		legacyFile{dev: 1, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "one"},
		legacyFile{dev: 2, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "two"},
	)
	must(t, os.WriteFile(filepath.Join(e.dir, rows[0].Path), []byte("tampered"), 0o600))
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 1 || res.Left != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := e.st.GetBackup(t.Context(), rows[1].ID); b.Path != "backups/plug_2/20261018T120000Z_plug.dmp" {
		t.Errorf("B must not share A's directory: %s", b.Path)
	}
}

// Why (M3): if two rows name one legacy file, moving it for the first would
// leave the second dangling. Neither moves.
func TestMigrateLayoutSkipsSharedPath(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	rows := legacy(t, e, legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "dump-a"})
	dup := rows[0]
	if _, err := e.st.PutBackup(t.Context(), dup); err != nil {
		t.Fatal(err)
	}
	before := snapshotDisk(t, e.dir)
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 0 || res.Left != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if fmt.Sprint(snapshotDisk(t, e.dir)) != fmt.Sprint(before) {
		t.Error("shared file moved")
	}
}

// --- orphans (#205) ---

func rowOf(t *testing.T, e *env, id model.BackupID) model.Backup {
	t.Helper()
	b, err := e.st.GetBackup(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Why (#205): 0.18 moved only the live devices' files, so a user was left with
// numeric directories next to the hostname ones and the migration looked half
// done. A kept backup of a deleted device carries the device's name; that is
// what a person browsing the data directory recognises.
func TestMigrateLayoutMovesOrphanByStoredName(t *testing.T) {
	e, _ := fleetEnv(t)
	rows := legacy(t, e,
		legacyFile{dev: 4, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "dump", orphan: true, name: "Old plug", mac: "AA:BB"},
		legacyFile{dev: 4, stamp: "20261001T080000Z", kind: model.BackupSnapshot, content: "snap", orphan: true, name: "Old plug", mac: "AA:BB"},
	)
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 2 || res.Left != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if g := rowOf(t, e, rows[0].ID); g.Path != "backups/Old_plug/20261001T080000Z_Old_plug.dmp" || g.DeviceID != 0 {
		t.Errorf("dump row: %+v", g)
	}
	if g := rowOf(t, e, rows[1].ID); g.Path != "backups/Old_plug/20261001T080000Z_Old_plug.snapshot.json" {
		t.Errorf("snapshot row: %s", g.Path)
	}
	if data, _, err := e.m.Open(t.Context(), rows[0].ID); err != nil || string(data) != "dump" {
		t.Errorf("orphan not readable after the move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "backups", "4")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied numeric directory must go: %v", err)
	}
}

// Why: an orphan without a stored name must still end up somewhere a person can
// recognise as "not a live device" and that cannot clash with one.
func TestMigrateLayoutOrphanWithoutNameUsesFormerID(t *testing.T) {
	e, _ := fleetEnv(t)
	rows := legacy(t, e, legacyFile{dev: 19, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "d", orphan: true})
	if _, err := e.m.MigrateLayout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if g := rowOf(t, e, rows[0].ID); g.Path != "backups/orphan-19/20261001T080000Z_orphan-19.dmp" {
		t.Errorf("path %s", g.Path)
	}
}

// Why: the directory rule of 0.18 (a directory never mixes two devices'
// backups) holds for orphans too. A live device whose hostname equals the
// deleted device's name keeps the plain directory only if it is free; the
// orphan then gets <name>_<former id>, so restoring or pruning one device can
// never touch the other's files.
func TestMigrateLayoutOrphanNeverSharesLiveDeviceDir(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	rows := legacy(t, e,
		legacyFile{dev: 7, stamp: "20261018T120000Z", kind: model.BackupConfigDump, content: "live"},
		legacyFile{dev: 3, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "dead", orphan: true, name: "kitchen", mac: "M1"},
	)
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Moved != 2 || res.Left != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	live, orph := rowOf(t, e, rows[0].ID).Path, rowOf(t, e, rows[1].ID).Path
	if path.Dir(live) == path.Dir(orph) {
		t.Fatalf("live and orphan share %s", path.Dir(live))
	}
	if !strings.HasPrefix(path.Dir(orph), "backups/kitchen") || !strings.HasPrefix(path.Dir(live), "backups/kitchen") {
		t.Errorf("both keep the name: %s / %s", live, orph)
	}
	// a later start finds the live device's directory occupied by the orphan's
	// neighbour only through the index: nothing moves again.
	if res, _ = e.m.MigrateLayout(t.Context()); res.Moved != 0 || res.Left != 0 {
		t.Errorf("second run %+v", res)
	}
}

// Why: two deleted devices with the same name are different devices (different
// ids); they must not be merged into one directory.
func TestMigrateLayoutTwoOrphansSameNameStaySeparate(t *testing.T) {
	e, _ := fleetEnv(t)
	rows := legacy(t, e,
		legacyFile{dev: 3, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "a", orphan: true, name: "plug", mac: "M1"},
		legacyFile{dev: 9, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "b", orphan: true, name: "plug", mac: "M2"},
	)
	if res, err := e.m.MigrateLayout(t.Context()); err != nil || res.Moved != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	a, b := rowOf(t, e, rows[0].ID).Path, rowOf(t, e, rows[1].ID).Path
	if path.Dir(a) == path.Dir(b) {
		t.Errorf("same directory %s", path.Dir(a))
	}
	if path.Dir(b) != "backups/plug_9" {
		t.Errorf("second orphan: %s", b)
	}
}

// Why: a crash between rename and row update must also heal for an orphan,
// otherwise the users already on 0.18 would see a dangling kept backup.
func TestMigrateLayoutOrphanRepairsCrashAfterRename(t *testing.T) {
	e, _ := fleetEnv(t)
	e.m.o.Store = &failOnce{memBackups: e.st}
	rows := legacy(t, e, legacyFile{dev: 4, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "dump", orphan: true, name: "Old plug"})
	if res, _ := e.m.MigrateLayout(t.Context()); res.Left != 1 || res.Moved != 0 {
		t.Fatalf("crash run %+v", res)
	}
	res, _ := e.m.MigrateLayout(t.Context())
	if res.Repaired != 1 || res.Left != 0 {
		t.Fatalf("restart run %+v", res)
	}
	if data, _, err := e.m.Open(t.Context(), rows[0].ID); err != nil || string(data) != "dump" {
		t.Errorf("not readable after repair: %v", err)
	}
}

// Why: a user who deleted the numeric directory by hand (as the reporter did)
// leaves rows without files. The migration must not guess or delete them: it
// counts them, and Open says plainly why nothing can be restored or downloaded.
func TestMissingFileRowIsCountedListedAndRefused(t *testing.T) {
	e, _ := fleetEnv(t)
	rows := legacy(t, e,
		legacyFile{dev: 85, stamp: "20261027T104345Z", kind: model.BackupConfigDump, content: "x", orphan: true, name: "gone", skipDisk: true},
		legacyFile{dev: 4, stamp: "20261001T080000Z", kind: model.BackupConfigDump, content: "ok", orphan: true, name: "here"},
	)
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Missing != 1 || res.Moved != 1 || res.Left != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if g := rowOf(t, e, rows[0].ID); g.Path != rows[0].Path {
		t.Errorf("the missing row must stay untouched: %s", g.Path)
	}
	if !e.m.FileMissing(rowOf(t, e, rows[0].ID)) || e.m.FileMissing(rowOf(t, e, rows[1].ID)) {
		t.Error("FileMissing must tell the two apart")
	}
	_, b, err := e.m.Open(t.Context(), rows[0].ID)
	if !errors.Is(err, ErrFileMissing) || b.ID != rows[0].ID {
		t.Fatalf("Open: %v (row %d)", err, b.ID)
	}
	// the kept-backups list still shows the row, and an admin can remove it
	if orph, _ := e.m.Orphans(t.Context()); len(orph) != 2 {
		t.Errorf("orphans listed: %d", len(orph))
	}
	if _, err := e.m.DeleteRun(t.Context(), "admin", rows[0].ID); err != nil {
		t.Fatalf("delete the missing row: %v", err)
	}
	if _, err := e.st.GetBackup(t.Context(), rows[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row still there: %v", err)
	}
	// A second start is quiet: nothing left to move, nothing missing.
	if res, _ = e.m.MigrateLayout(t.Context()); res.Moved != 0 || res.Missing != 0 || res.Left != 0 {
		t.Errorf("second run %+v", res)
	}
}

// Why: files deleted from a hostname-layout directory (not just a 0.17 one)
// are missing too; the startup line must count them so the user learns of them.
func TestMigrateLayoutCountsMissingFilesInNewLayout(t *testing.T) {
	e, _ := fleetEnv(t, host(7, "k", "kitchen"))
	b := backupOf(t, e, 7)
	full := filepath.Join(e.dir, filepath.FromSlash(b.Path))
	must(t, os.Remove(full))
	res, err := e.m.MigrateLayout(t.Context())
	if err != nil || res.Missing != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}
