package app

import (
	"context"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// countingFleetStore counts how often each read happens: the page must not grow a query per device.
type countingFleetStore struct {
	usage  []store.BackupUsage
	run    model.BackupRun
	hasRn  bool
	seen   map[model.DeviceID]store.DeviceSeen
	newest map[model.DeviceID]store.NewestRun
	calls  map[string]int
}

func (c *countingFleetStore) BackupUsage(context.Context) ([]store.BackupUsage, error) {
	c.calls["usage"]++
	return c.usage, nil
}
func (c *countingFleetStore) LastBackupRun(context.Context) (model.BackupRun, bool, error) {
	c.calls["run"]++
	return c.run, c.hasRn, nil
}
func (c *countingFleetStore) DevicesSeen(context.Context) (map[model.DeviceID]store.DeviceSeen, error) {
	c.calls["seen"]++
	return c.seen, nil
}
func (c *countingFleetStore) NewestRuns(context.Context) (map[model.DeviceID]store.NewestRun, error) {
	c.calls["newest"]++
	return c.newest, nil
}

// Why (#409): the page lists the whole fleet at once and must stay cheap however big it is: a
// fixed number of reads, none of them a device request, and every device gets its row, with its
// result of the last run and what the next run would do with it.
func TestFleetOverviewIsABoundedNumberOfReadsWithoutDeviceIO(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	st := &countingFleetStore{calls: map[string]int{}, hasRn: true,
		usage: []store.BackupUsage{
			{Device: 0, Runs: 3, Bytes: 9}, // orphans: not a device row
			{Device: 1, Name: "kitchen", Runs: 4, Kept: 1, Bytes: 400, Last: now.Add(-2 * time.Hour)},
			{Device: 2, Name: "garage"},
			{Device: 3, Name: "attic", Runs: 1, Bytes: 10, Last: now.Add(-72 * time.Hour)},
		},
		run: model.BackupRun{Items: []model.BackupRunItem{
			{DeviceID: 1, DeviceName: "kitchen", Outcome: model.BackupOutcomeOK, At: now.Add(-2 * time.Hour)},
			{DeviceID: 3, DeviceName: "attic", Outcome: model.BackupOutcomeFailed, Reason: "The device did not answer", At: now.Add(-2 * time.Hour)},
		}},
		seen: map[model.DeviceID]store.DeviceSeen{1: {At: now, State: model.HealthOnline}, 3: {At: now.Add(-24 * time.Hour), State: model.HealthOffline}},
		newest: map[model.DeviceID]store.NewestRun{
			1: {Device: 1, DumpID: 10, FilesID: 11},
			3: {Device: 3, DumpID: 30, DumpNote: "files not backed up: no filesystem"},
		},
	}
	manifestReads := 0
	f := &fleetBackups{store: st,
		files: func(_ context.Context, id model.BackupID) (backup.RunFiles, error) {
			manifestReads++
			return backup.RunFiles{Manifest: &backup.Manifest{Files: []backup.ManifestFile{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}}}}, nil
		},
		health: func() map[model.DeviceID]model.Health {
			return map[model.DeviceID]model.Health{3: {State: model.HealthOffline, Since: now.Add(-24 * time.Hour)}}
		}}
	rows, err := f.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for k, n := range st.calls {
		if n != 1 {
			t.Errorf("%s read %d times, want once however many devices there are", k, n)
		}
	}
	if manifestReads != 1 {
		t.Errorf("manifests read %d times, want once: only the device with a files archive", manifestReads)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want one per device (not the orphans)", len(rows))
	}
	k, g, a := rows[0], rows[1], rows[2]
	if k.Name != "kitchen" || k.Runs != 4 || k.Files != 3 || k.FilesNote != "" || k.Kept != 1 || k.Bytes != 400 || k.LastOutcome != model.BackupOutcomeOK || k.SkipReason != "" || !k.NewestAt.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("kitchen = %+v", k)
	}
	if g.Name != "garage" || !g.NewestAt.IsZero() || g.LastOutcome != "" || g.SkipReason != "never answered" {
		t.Errorf("garage = %+v, want no backup, not in the last run, would be skipped as never answered", g)
	}
	if a.FilesNote != "settings only: no file system" || a.Files != 0 || a.LastOutcome != model.BackupOutcomeFailed || a.LastReason != "The device did not answer" || a.SkipReason != "offline" || !a.SkipSince.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("attic = %+v", a)
	}
}

// Why: an archive that cannot be read (damaged, gone from disk) must not take the page down: the row
// stays and says so.
func TestFleetOverviewSaysWhenAFilesArchiveCannotBeRead(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	st := &countingFleetStore{calls: map[string]int{},
		usage:  []store.BackupUsage{{Device: 1, Name: "kitchen", Runs: 1, Last: now}},
		seen:   map[model.DeviceID]store.DeviceSeen{1: {At: now, State: model.HealthOnline}},
		newest: map[model.DeviceID]store.NewestRun{1: {Device: 1, DumpID: 10, FilesID: 11}},
	}
	f := &fleetBackups{store: st,
		health: func() map[model.DeviceID]model.Health { return nil },
		files: func(context.Context, model.BackupID) (backup.RunFiles, error) {
			return backup.RunFiles{}, backup.ErrBadArchive
		}}
	rows, err := f.Overview(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, err = %v", rows, err)
	}
	if rows[0].Files != 0 || rows[0].FilesNote != "files archive could not be read" {
		t.Errorf("row = %+v, want no count and the note", rows[0])
	}
}
