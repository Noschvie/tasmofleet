//go:build integration

package backup

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: the upgrade of a live 0.17 install. On the real SQLite store (LIKE
// escaping, compare-and-set, foreign keys) a 0.17 tree with two devices of one
// hostname and a deleted device's kept backup is migrated: every row opens
// with a matching checksum afterwards, paths stay relative below the data
// directory (a downgrade reads them by path), and a second start is a no-op.
func TestMigrateLayoutOnRealStore(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	ctx := t.Context()

	mk := func(name, host, addr string) model.Device {
		d := model.Device{Name: name, Address: addr}
		d.Net.Hostname = host
		id, err := st.CreateDevice(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		d.ID = id
		return d
	}
	d1, d2, gone := mk("one", "tasmota-A1", "10.0.0.1"), mk("two", "tasmota-A1", "10.0.0.2"), mk("gone", "gone", "10.0.0.3")

	type row struct {
		id      model.BackupID
		content string
	}
	var rows []row
	put := func(d model.Device, stamp string, kind model.BackupKind, ext, content string) {
		rel := "backups/" + strconv.FormatInt(int64(d.ID), 10) + "/" + stamp + "-" + string(kind) + "." + ext
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := st.PutBackup(ctx, model.Backup{DeviceID: d.ID, DeviceName: d.Name, Kind: kind, Path: rel, Size: int64(len(content)), SHA256: sha(content)})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row{id, content})
	}
	put(d1, "20261018T120000Z", model.BackupConfigDump, "dmp", "d1-dump")
	put(d1, "20261018T120000Z", model.BackupSnapshot, "json", "d1-snap")
	put(d2, "20261018T120000Z", model.BackupConfigDump, "dmp", "d2-dump")
	put(gone, "20261018T120000Z", model.BackupConfigDump, "dmp", "gone-dump")
	if err := st.DeleteDevice(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}

	m, err := New(Options{DataDir: dir, Devices: st, Store: st, Client: newFakeDevice(t), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.MigrateLayout(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 4 || res.Left != 0 {
		t.Fatalf("%+v", res)
	}
	for _, r := range rows {
		data, b, err := m.Open(ctx, r.id)
		if err != nil || string(data) != r.content {
			t.Errorf("row %d (%s): %v", r.id, b.Path, err)
		}
		if filepath.IsAbs(b.Path) || filepath.Clean(b.Path) != b.Path {
			t.Errorf("path must stay relative and clean: %s", b.Path)
		}
	}
	p1, _ := st.GetBackup(ctx, rows[0].id)
	p2, _ := st.GetBackup(ctx, rows[2].id)
	if p1.Path != "backups/tasmota-A1/20261018T120000Z_tasmota-A1.dmp" || p2.Path != "backups/tasmota-A1_"+strconv.FormatInt(int64(d2.ID), 10)+"/20261018T120000Z_tasmota-A1.dmp" {
		t.Errorf("%s / %s", p1.Path, p2.Path)
	}
	// The deleted device's kept backup moves too (#205), into a folder named after its stored name.
	if po, _ := st.GetBackup(ctx, rows[3].id); po.Path != "backups/gone/20261018T120000Z_gone.dmp" {
		t.Errorf("orphan path %s", po.Path)
	}
	if res2, _ := m.MigrateLayout(ctx); res2.Moved != 0 || res2.Left != 0 || res2.Repaired != 0 {
		t.Errorf("restart: %+v", res2)
	}
}
