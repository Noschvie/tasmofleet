package store

import (
	"reflect"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why (N9): Settings -> Backups answers "how much space do the backups take and
// which devices have none" from one query. The counts must match what the
// device's Backups tab shows (one backup per run, the decoded copy is not a
// backup of its own), the size must be what is on disk (every file), a device
// without backups must still appear (it is what "Devices without a backup"
// counts), and backups of deleted devices must not vanish from the totals.
func TestBackupUsageAggregatesRunsSizesKeptAndOrphans(t *testing.T) {
	s, _, _ := newStore(t)
	big := mustDevice(t, s, "big", "10.0.0.1")
	none := mustDevice(t, s, "none", "10.0.0.2")
	gone, err := s.CreateDevice(ctx, model.Device{Name: "gone", Address: "10.0.0.3", MAC: "AA:BB:CC:00:00:03"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(dev model.DeviceID, name, mac string, kind model.BackupKind, at int64, size int64, kept bool) {
		t.Helper()
		if _, err := s.PutBackup(ctx, model.Backup{DeviceID: dev, DeviceName: name, DeviceMAC: mac, Kind: kind,
			TakenAt: ms(at), Path: "p", SHA256: "x", Size: size, Kept: kept}); err != nil {
			t.Fatal(err)
		}
	}
	// big: two runs, the first with dump, snapshot and decoded copy, the second kept.
	put(big, "big", "", model.BackupConfigDump, 1000, 100, false)
	put(big, "big", "", model.BackupSnapshot, 1000, 10, false)
	put(big, "big", "", model.BackupDecoded, 1000, 50, false)
	put(big, "big", "", model.BackupConfigDump, 2000, 200, true)
	put(big, "big", "", model.BackupSnapshot, 2000, 20, true)
	// gone: one kept run, then the device is deleted with backups=keep.
	put(gone, "gone", "AA:BB:CC:00:00:03", model.BackupConfigDump, 1500, 70, true)
	put(gone, "gone", "AA:BB:CC:00:00:03", model.BackupSnapshot, 1500, 7, true)
	if err := s.DeleteDevice(ctx, gone); err != nil {
		t.Fatal(err)
	}

	got, err := s.BackupUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []BackupUsage{
		{Device: 0, Runs: 1, Kept: 1, Bytes: 77, Last: ms(1500)},
		{Device: big, Name: "big", Runs: 2, Kept: 1, Bytes: 380, Last: ms(2000)},
		{Device: none, Name: "none"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage:\n got %+v\nwant %+v", got, want)
	}
}

// Why: without orphans there is no "deleted devices" row, so the page does not
// show an empty line, and an empty fleet answers with nothing (no error).
func TestBackupUsageWithoutOrphansHasNoDeviceZeroRow(t *testing.T) {
	s, _, _ := newStore(t)
	got, err := s.BackupUsage(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty fleet: %v %+v", err, got)
	}
	d := mustDevice(t, s, "a", "10.0.0.1")
	got, err = s.BackupUsage(ctx)
	if err != nil || len(got) != 1 || got[0].Device != d {
		t.Fatalf("one device, no backups: %v %+v", err, got)
	}
}
