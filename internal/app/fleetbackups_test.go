package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
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
func (c *countingFleetStore) BackupRunTimes(context.Context) (time.Time, time.Time, error) {
	c.calls["times"]++
	return time.Time{}, time.Time{}, nil
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

// Why (#622): the retention prunes only after a successful backup, so a device whose last scheduled
// run failed or was skipped keeps its backups until it is backed up again; Prune now would thin
// exactly the devices that get no new ones. The plan marks them left out (the page shows a badge,
// the POST skips them) and passes everything else, the held runs included, through unchanged: the
// never-the-newest and restore rules are backup.PruneNowSet's, not repeated here.
func TestPrunePlanLeavesOutDevicesWhoseLastRunFailedOrWasSkipped(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	st := &countingFleetStore{calls: map[string]int{}, hasRn: true,
		usage: []store.BackupUsage{
			{Device: 0, Runs: 3},
			{Device: 1, Name: "ok", Runs: 5}, {Device: 2, Name: "failed", Runs: 5}, {Device: 3, Name: "skipped", Runs: 5},
			{Device: 4, Name: "new", Runs: 5}, {Device: 5, Name: "nothing", Runs: 1}, {Device: 6, Name: "empty"},
		},
		run: model.BackupRun{Items: []model.BackupRunItem{
			{DeviceID: 1, Outcome: model.BackupOutcomeOK}, {DeviceID: 2, Outcome: model.BackupOutcomeFailed}, {DeviceID: 3, Outcome: model.BackupOutcomeSkipped},
		}},
	}
	asked := map[model.DeviceID]bool{}
	f := &fleetBackups{store: st,
		prunePlan: func(_ context.Context, id model.DeviceID) ([]model.Backup, []backup.Held, error) {
			asked[id] = true
			if id == 5 {
				return nil, []backup.Held{{TakenAt: now, Why: backup.HeldNewest}}, nil // only the newest run was listed
			}
			return []model.Backup{{ID: model.BackupID(id), DeviceID: id, TakenAt: now.Add(-48 * time.Hour)}}, []backup.Held{{TakenAt: now, Why: backup.HeldNewest}}, nil
		}}
	plan, err := f.PrunePlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	left := map[model.DeviceID]string{}
	for _, d := range plan {
		left[d.Device] = d.LeftOut
		if len(d.Files) != 1 || d.Held != 1 {
			t.Errorf("device %d: files %d, held %d: the per-device rule's answer must pass through", d.Device, len(d.Files), d.Held)
		}
	}
	if len(plan) != 4 || left[1] != "" || left[4] != "" || left[2] == "" || left[3] == "" {
		t.Errorf("plan %+v: want devices 1 and 4 taking part, 2 (failed) and 3 (skipped) left out, 5 (nothing to prune) not listed", left)
	}
	if asked[0] || asked[6] {
		t.Error("the deleted devices' row and a device without backups must not be planned")
	}
}

// Why (#494, #622): Prune now must fail closed when it cannot tell which backups a restore is
// using. The guard is the restore_jobs table, read through the real store; a refactor that wires
// the manager to a store without it, or drops PruneNowPlan from the page, would silently let Prune
// now take the run an unfinished restore reads. With a real store, a running restore's run is held
// and the newest run is held, so only the run between them is offered.
func TestPrunePlanIsWiredToTheRestoreGuardOfTheRealStore(t *testing.T) {
	a, svc, _ := notifyRuntime(t, nil)
	a.backupRun = newScheduledRun(nil)
	if err := a.setupBackups(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.backups.SetRetention(1, 0)
	ctx := t.Context()
	dev, err := svc.Store.CreateDevice(ctx, model.Device{Name: "plug", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	var dumps []model.BackupID
	for i := range 3 {
		id, err := svc.Store.PutBackup(ctx, model.Backup{DeviceID: dev, DeviceName: "plug", Kind: model.BackupConfigDump, TakenAt: day.Add(time.Duration(i) * time.Hour),
			Path: "backups/plug/" + string(rune('a'+i)) + ".dmp", Size: 10, SHA256: "x"})
		if err != nil {
			t.Fatal(err)
		}
		dumps = append(dumps, id)
	}
	if _, err := svc.Store.CreateRestore(ctx, store.RestoreJob{DeviceID: dev, BackupID: dumps[0], Actor: "x"}); err != nil {
		t.Fatal(err)
	}

	plan, err := a.fleetBackups().PrunePlan(ctx)
	if errors.Is(err, backup.ErrNoRestoreGuard) {
		t.Fatal("Prune now is not wired to the restore jobs: it would refuse every time")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || len(plan[0].Files) != 1 || plan[0].Files[0].ID != dumps[1] || plan[0].Held != 2 {
		t.Errorf("plan %+v, want only the middle run offered: the oldest is a running restore's, the newest stays", plan)
	}
}

// Why (#924): a run whose files MQTT could not carry and HTTP did not either gives a long reason (the firmware and the
// HTTP failure); the fleet table has one short cell for it, the same "files need HTTP" as a run whose HTTP was known
// to fail. The device's own backups page shows the whole sentence.
func TestTheFleetCellShortensTheReasonThatTheFilesNeedHTTP(t *testing.T) {
	for _, reason := range []string{
		"they need HTTP",
		"they need HTTP (this firmware sends no files over MQTT), and HTTP could not be used: the device took too long to answer",
		"they need HTTP (an SD card is mounted), and HTTP could not be used: HTTP has not answered recently",
	} {
		if got := filesNoteText(reason); got != "files need HTTP" {
			t.Errorf("%q: cell %q", reason, got)
		}
	}
}
