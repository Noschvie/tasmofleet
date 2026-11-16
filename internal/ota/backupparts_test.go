package ota

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// partsBackups is a backup service that hands back the given kinds, as one run, with the dump coming
// through a real router when there is one.
type partsBackups struct {
	r     *transport.Router
	kinds []model.BackupKind
}

func (b partsBackups) Backup(ctx context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	dctx, dump := transport.WithTrace(ctx)
	if b.r != nil {
		if _, err := b.r.DownloadConfig(dctx, tasmota.TargetFor(model.Device{ID: id, Address: "192.0.2.1"})); err != nil {
			return nil, err
		}
	}
	files := ""
	if slices.Contains(b.kinds, model.BackupFiles) {
		files = "http" // the files cannot come over MQTT: the Manager reads them over HTTP whatever carried the dump
	}
	backup.Record(ctx, dump.Via(), files) // what the Manager reports: the dump's way and the files' way
	out := make([]model.Backup, 0, len(b.kinds))
	for i, k := range b.kinds {
		out = append(out, model.Backup{ID: model.BackupID(i + 1), DeviceID: id, Kind: k})
	}
	return out, nil
}

func jobNote(t *testing.T, h *harness) string {
	t.Helper()
	var note string
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	for _, ev := range h.st.events {
		if ev.To == model.JobSucceeded {
			note = ev.Detail
		}
	}
	return note
}

// Why (#838): one backup of one device is one thing, however many files it is stored as. The result
// used to count the stored files ("3 backup files taken over HTTP"), which looked like three backups
// after "Back up all now" on 20 devices. The note names the parts of the one backup instead, in a
// fixed order (backup.Note), so the same run reads the same on the bulk result, the transaction page
// and the device page's own Back up (#882), and no number is left to misread.
func TestBulkBackupNoteNamesThePartsOfOneBackupNotAFileCount(t *testing.T) {
	cfg, snap, dec, files := model.BackupConfigDump, model.BackupSnapshot, model.BackupDecoded, model.BackupFiles
	for _, tc := range []struct {
		name  string
		kinds []model.BackupKind
		want  string
	}{
		{"config and status", []model.BackupKind{cfg, snap}, "(config, status)"},
		{"with the decoded copy", []model.BackupKind{cfg, snap, dec}, "(config, status, decoded)"},
		{"with the files archive", []model.BackupKind{cfg, snap, files}, "(config, status, files)"},
		{"every part", []model.BackupKind{cfg, snap, dec, files}, "(config, status, decoded, files)"},
		{"stored in another order", []model.BackupKind{files, dec, snap, cfg}, "(config, status, decoded, files)"},
	} {
		for _, via := range []string{"mqtt", "http"} {
			t.Run(tc.name+" over "+via, func(t *testing.T) {
				h, _, _, _ := backupHarness(t, 1)
				b := partsBackups{kinds: tc.kinds}
				if via == "mqtt" {
					r := transport.NewRouter(transport.Options{HTTP: viaHTTP{}, Devices: viaDevices{}, Facts: viaFacts{}})
					r.SetMQTT(viaMQTT{})
					b.r = r
				}
				h.e.backups = b
				if _, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1}); err != nil {
					t.Fatal(err)
				}
				h.run(t, h.e)
				want := "Backup taken over " + strings.ToUpper(via) + " " + tc.want
				if via == "mqtt" && slices.Contains(tc.kinds, files) { // #926: the files left MQTT, the note says so
					want = "Backup taken over MQTT, files over HTTP " + tc.want
				}
				if got := jobNote(t, h); got != want {
					t.Errorf("note %q, want %q", got, want)
				}
			})
		}
	}
}
