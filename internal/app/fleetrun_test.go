package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

var fleetT0 = time.Date(2026, 11, 5, 3, 0, 0, 0, time.Local)

type fleetEnv struct {
	st      *store.SQLite
	run     *scheduledRun
	b       *scheduledBackup
	health  map[model.DeviceID]model.Health
	locks   *devlock.Locks
	holds   *devlock.FirmwareHolds
	mu      sync.Mutex
	tried   []string         // devices a backup was started on
	fail    map[string]error // device name -> backup error
	notes   [][]string       // failed device names per notification
	notesN  []int            // tried count per notification
	devices map[string]model.DeviceID
}

func newFleetEnv(t *testing.T) *fleetEnv {
	t.Helper()
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
	e := &fleetEnv{st: st, health: map[model.DeviceID]model.Health{}, locks: devlock.New(), fail: map[string]error{}, devices: map[string]model.DeviceID{}}
	e.holds = devlock.NewFirmwareHolds(time.Now, firmwareHoldTimeout)
	e.run = newScheduledRun(events.NewHub(64))
	e.run.now = func() time.Time { return fleetT0.Add(time.Minute) }
	e.b = &scheduledBackup{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		run:     e.run,
		devices: st.ListDevices,
		health:  func() map[model.DeviceID]model.Health { return e.health },
		store:   st,
		enter:   backupGuard(deviceGuard{locks: e.locks, jobs: st, holds: e.holds}),
		now:     func() time.Time { return fleetT0 },
		backup: func(_ context.Context, id model.DeviceID) error {
			name := ""
			for n, d := range e.devices {
				if d == id {
					name = n
				}
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			e.tried = append(e.tried, name)
			return e.fail[name]
		},
		notifyFailed: func(tried int, failed []string, _ time.Time) {
			e.notes, e.notesN = append(e.notes, failed), append(e.notesN, tried)
		},
	}
	return e
}

// dev adds a device with its persisted last_seen_at (zero = never heard) and the tracker's current verdict.
func (e *fleetEnv) dev(t *testing.T, name string, seen time.Time, h model.Health) model.DeviceID {
	t.Helper()
	return e.devP(t, name, seen, h.State, h)
}

// devP is dev with the persisted health state apart from the tracker's verdict h (a restart: the
// database remembers, the tracker does not yet).
func (e *fleetEnv) devP(t *testing.T, name string, seen time.Time, persisted model.HealthState, h model.Health) model.DeviceID {
	t.Helper()
	id, err := e.st.CreateDevice(context.Background(), model.Device{Name: name, Address: "192.0.2." + string(rune('1'+len(e.devices)))})
	if err != nil {
		t.Fatal(err)
	}
	e.devices[name] = id
	if !seen.IsZero() {
		if err := e.st.SetLastSeen(context.Background(), id, persisted, seen); err != nil {
			t.Fatal(err)
		}
	}
	if h.State != "" {
		h.DeviceID = id
		e.health[id] = h
	}
	return id
}

func (e *fleetEnv) last(t *testing.T) model.BackupRun {
	t.Helper()
	r, ok, err := e.st.LastBackupRun(context.Background())
	if err != nil || !ok {
		t.Fatalf("no stored run: ok=%v err=%v", ok, err)
	}
	return r
}

func item(r model.BackupRun, name string) model.BackupRunItem {
	for _, it := range r.Items {
		if it.DeviceName == name {
			return it
		}
	}
	return model.BackupRunItem{}
}

// Why (#409): an offline device answers nothing, so trying it only produces a "failure" the person
// cannot act on. It is skipped, and the reason says since when (with the date: it may be days).
func TestScheduledBackupSkipsAnOfflineDeviceWithTheDate(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "up", fleetT0, model.Health{State: model.HealthOnline})
	since := time.Date(2026, 11, 3, 21, 14, 0, 0, time.Local)
	e.dev(t, "down", since, model.Health{State: model.HealthOffline, Since: since})
	e.b.Run(context.Background())
	if got := strings.Join(e.tried, ","); got != "up" {
		t.Errorf("backed up %q, want only the online device", got)
	}
	it := item(e.last(t), "down")
	if it.Outcome != model.BackupOutcomeSkipped || it.Reason != "offline since 3 Nov 21:14" {
		t.Errorf("item = %+v, want skipped, offline since 3 Nov 21:14", it)
	}
}

// Why: a device that never answered since it was added is skipped with that reason, decided from
// the persisted last_seen_at.
func TestScheduledBackupSkipsADeviceThatNeverAnswered(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "ghost", time.Time{}, model.Health{State: model.HealthUnknown})
	e.b.Run(context.Background())
	if it := item(e.last(t), "ghost"); it.Outcome != model.BackupOutcomeSkipped || it.Reason != "never answered" {
		t.Errorf("item = %+v, want skipped, never answered", it)
	}
	if len(e.tried) != 0 {
		t.Errorf("tried %v, want none", e.tried)
	}
}

// Why (the case that would silently skip the fleet): right after a start every device is "unknown"
// to the tracker until it is heard. A run then must still try the devices that were seen before.
func TestScheduledBackupAfterARestartTriesDevicesSeenBefore(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "a", fleetT0.Add(-time.Hour), model.Health{State: model.HealthUnknown})
	e.dev(t, "b", fleetT0.Add(-48*time.Hour), model.Health{}) // not in the tracker at all
	e.b.Run(context.Background())
	if got := strings.Join(e.tried, ","); got != "a,b" {
		t.Errorf("tried %q, want both devices", got)
	}
	r := e.last(t)
	if r.OK != 2 || r.Skipped != 0 {
		t.Errorf("run = %+v, want 2 ok, none skipped", r)
	}
}

// Why: a device that answers but refuses our password is reachable and needs the person to act: it
// is tried and reported as failed, not hidden among the skipped.
func TestScheduledBackupTriesAnAuthFailedDeviceAndReportsTheFailure(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "locked", fleetT0, model.Health{State: model.HealthAuthFailed})
	e.fail["locked"] = tasmota.ErrAuth
	e.b.Run(context.Background())
	it := item(e.last(t), "locked")
	if it.Outcome != model.BackupOutcomeFailed || it.Reason != backup.AttemptReason(tasmota.ErrAuth) {
		t.Errorf("item = %+v, want failed with the auth reason", it)
	}
}

// Why: a device somebody else is working on (an update, a restore) must not be touched, and
// is not a failure either.
func TestScheduledBackupSkipsABusyDevice(t *testing.T) {
	e := newFleetEnv(t)
	id := e.dev(t, "busy", fleetT0, model.Health{State: model.HealthOnline})
	release, _, _ := e.locks.TryAcquire(id, "restore")
	defer release()
	e.b.Run(context.Background())
	if it := item(e.last(t), "busy"); it.Outcome != model.BackupOutcomeSkipped || it.Reason != "another operation was running" {
		t.Errorf("item = %+v", it)
	}
	if len(e.tried) != 0 {
		t.Errorf("tried %v, want none", e.tried)
	}
	if _, _, ok := e.locks.TryAcquire(id, "x"); ok {
		t.Error("the scheduled run released a lock it did not take")
	}
}

// Why: the run takes the device's lock while it backs up and gives it back, or the device would
// refuse every later operation.
func TestScheduledBackupReleasesTheDeviceLock(t *testing.T) {
	e := newFleetEnv(t)
	id := e.dev(t, "a", fleetT0, model.Health{State: model.HealthOnline})
	e.b.backup = func(context.Context, model.DeviceID) error {
		if _, holder, ok := e.locks.TryAcquire(id, "x"); ok || holder != "backup" {
			t.Errorf("during the backup the lock must be held by %q, got ok=%v holder=%q", "backup", ok, holder)
		}
		return nil
	}
	e.b.Run(context.Background())
	release, _, ok := e.locks.TryAcquire(id, "x")
	if !ok {
		t.Fatal("lock still held after the run")
	}
	release()
}

// Why: skipping is not failing. A run that skipped everything colours nothing and tells nobody.
func TestScheduledBackupAllSkippedIsCalmAndSilent(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "ghost", time.Time{}, model.Health{})
	e.dev(t, "gone", fleetT0.Add(-72*time.Hour), model.Health{State: model.HealthOffline, Since: fleetT0.Add(-72 * time.Hour)})
	e.b.Run(context.Background())
	s := e.run.Status()
	if s.LastFailed != 0 || s.LastTotal != 0 || s.LastSkipped != 2 || s.Finished.IsZero() {
		t.Errorf("status = %+v, want 0 tried, 0 failed, 2 skipped", s)
	}
	if len(e.notes) != 0 {
		t.Errorf("notified %v, want nothing", e.notes)
	}
}

// Why: only real failures warn. 2 failed and 3 skipped: the bar's numbers and the notification
// speak of the 2 (of the 4 devices that were tried), never of the skipped ones.
func TestScheduledBackupTwoFailedThreeSkippedWarnsOnTheFailuresOnly(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "ok1", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "ok2", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "bad1", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "bad2", fleetT0, model.Health{State: model.HealthOnline})
	e.fail["bad1"], e.fail["bad2"] = tasmota.ErrUnreachable, tasmota.ErrTimeout
	e.dev(t, "ghost1", time.Time{}, model.Health{})
	e.dev(t, "ghost2", time.Time{}, model.Health{})
	e.dev(t, "gone", fleetT0.Add(-time.Hour), model.Health{State: model.HealthOffline, Since: fleetT0.Add(-time.Hour)})
	e.b.Run(context.Background())
	s := e.run.Status()
	if s.LastTotal != 4 || s.LastFailed != 2 || s.LastSkipped != 3 {
		t.Errorf("status = %+v, want 4 tried, 2 failed, 3 skipped", s)
	}
	if len(e.notes) != 1 || strings.Join(e.notes[0], ",") != "bad1,bad2" || e.notesN[0] != 4 {
		t.Errorf("notifications = %v (tried %v), want one naming bad1 and bad2 of 4", e.notes, e.notesN)
	}
	r := e.last(t)
	if r.Tried != 4 || r.OK != 2 || r.Failed != 2 || r.Skipped != 3 || !r.Completed {
		t.Errorf("stored run = %+v", r)
	}
}

// Why: after a restart the bar must not forget the last run (it used to start empty).
func TestScheduledRunSurvivesARestart(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "a", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "ghost", time.Time{}, model.Health{})
	e.b.Run(context.Background())
	want := e.run.Status()

	again := newScheduledRun(nil)
	r, ok, err := e.st.LastBackupRun(context.Background())
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	again.Restore(r)
	got := again.Status()
	if got.LastTotal != want.LastTotal || got.LastFailed != want.LastFailed || got.LastSkipped != want.LastSkipped || got.Finished.IsZero() {
		t.Errorf("after restart %+v, want %+v", got, want)
	}
}

// Why: a run that shutdown cut short is not a result: it is stored as history only, the last
// finished run stays the one the bar shows, and the device the shutdown interrupted is not
// counted as a failure.
func TestScheduledBackupCutShortByShutdownIsNoResult(t *testing.T) {
	e := newFleetEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.dev(t, "a", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "b", fleetT0, model.Health{State: model.HealthOnline})
	e.b.backup = func(context.Context, model.DeviceID) error { cancel(); return context.Canceled }
	e.b.Run(ctx)
	if s := e.run.Status(); !s.Finished.IsZero() || s.LastFailed != 0 {
		t.Errorf("status = %+v, want no finished run", s)
	}
	if _, ok, _ := e.st.LastBackupRun(context.Background()); ok {
		t.Error("a cut-short run must not be the last run")
	}
	if len(e.notes) != 0 {
		t.Errorf("notified %v for a shutdown", e.notes)
	}
	if n, err := e.st.PruneBackupRuns(context.Background(), fleetT0.Add(24*time.Hour)); err != nil || n != 1 {
		t.Errorf("stored runs = %d (%v), want the cut-short one kept as history", n, err)
	}
}

// Why (#409): the run record follows the audit retention like the other history: the daily
// retention deletes runs older than AuditKeep.
func TestRetentionPrunesBackupRuns(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "a", fleetT0, model.Health{State: model.HealthOnline})
	e.b.now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	e.b.Run(context.Background())
	a := &runtime{svc: &Services{Sys: auditKeepSys(t, 24*time.Hour), Store: e.st}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.prune(context.Background())
	if _, ok, _ := e.st.LastBackupRun(context.Background()); ok {
		t.Error("a run older than the audit retention is still stored")
	}
}

func auditKeepSys(t *testing.T, keep time.Duration) *sysconfig.Service {
	t.Helper()
	cfg, err := config.Load([]string{"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=header", "TASMOFLEET_TRUSTED_PROXIES=172.31.12.20/32", "TASMOFLEET_OPERATOR_GROUPS=ops"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuditKeep = keep
	sys, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: &memSettings{rows: map[string]store.SystemSetting{}}, Audit: nopAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return sys
}

// Why (#409, the restart): the tracker says "unknown" for every device until it hears it. A device
// that was offline before the restart and still does not answer must be skipped from what the
// database remembers, not tried and counted as a failure (a message after every restart).
func TestScheduledBackupAfterARestartSkipsADevicePersistedAsOffline(t *testing.T) {
	e := newFleetEnv(t)
	since := fleetT0.Add(-30 * time.Hour)
	e.devP(t, "down", since, model.HealthOffline, model.Health{State: model.HealthUnknown})
	e.devP(t, "fine", fleetT0.Add(-time.Hour), model.HealthOnline, model.Health{State: model.HealthUnknown})
	e.fail["down"] = tasmota.ErrUnreachable // would fail if it were tried
	e.b.Run(context.Background())
	if got := strings.Join(e.tried, ","); got != "fine" {
		t.Errorf("tried %q, want only the device that was not offline before the restart", got)
	}
	it := item(e.last(t), "down")
	if it.Outcome != model.BackupOutcomeSkipped || it.Reason != "offline since 3 Nov 21:00" {
		t.Errorf("item = %+v, want skipped, offline since the persisted last_seen_at", it)
	}
	if s := e.run.Status(); s.LastFailed != 0 || s.LastSkipped != 1 || len(e.notes) != 0 {
		t.Errorf("status = %+v, notes %v: a skipped device must not fail the run or send a message", s, e.notes)
	}
}

// Why: the tracker's own verdict wins over what was stored: a device that is online now is tried
// even if the database still says offline from before.
func TestScheduledBackupTrackerVerdictBeatsThePersistedState(t *testing.T) {
	e := newFleetEnv(t)
	e.devP(t, "back", fleetT0.Add(-time.Hour), model.HealthOffline, model.Health{State: model.HealthOnline})
	e.b.Run(context.Background())
	if got := strings.Join(e.tried, ","); got != "back" {
		t.Errorf("tried %q, want the device that is online now", got)
	}
}

// Why: "busy" means the device guard says so: a running update transaction excludes the backup
// even while nobody holds the lock (the engine's job is the exclusion between its own steps).
func TestScheduledBackupSkipsADeviceWithARunningTransaction(t *testing.T) {
	e := newFleetEnv(t)
	id := e.dev(t, "updating", fleetT0, model.Health{State: model.HealthOnline})
	j := model.Job{DeviceID: id, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
	_, ids, err := e.st.CreateBatch(context.Background(), "u", []model.Job{j})
	if err != nil {
		t.Fatal(err)
	}
	j.ID, j.State = ids[0], model.JobAwaitReboot
	if err := e.st.SaveTransition(context.Background(), j, model.JobEvent{JobID: ids[0], From: model.JobQueued, To: model.JobAwaitReboot, Event: "test"}); err != nil {
		t.Fatal(err)
	}
	e.b.Run(context.Background())
	if len(e.tried) != 0 {
		t.Errorf("tried %v, want none: a transaction is running on the device", e.tried)
	}
	if it := item(e.last(t), "updating"); it.Outcome != model.BackupOutcomeSkipped || it.Reason != skipBusy {
		t.Errorf("item = %+v", it)
	}
}

// Why: a device deleted before its turn is gone, not failed: no item, no failure, no message. One
// deleted after its turn must not cost the run either (store test); here the run goes on.
func TestScheduledBackupDeviceDeletedBeforeItsTurnIsNeitherFailedNorListed(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "gone", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "stays", fleetT0, model.Health{State: model.HealthOnline})
	e.fail["gone"] = fmt.Errorf("backup: %w", store.ErrNotFound)
	e.b.Run(context.Background())
	r := e.last(t)
	if item(r, "gone").DeviceName != "" || r.Failed != 0 || r.OK != 1 || len(e.notes) != 0 {
		t.Errorf("run = %+v, notes %v: the deleted device must leave no trace, the other is backed up", r, e.notes)
	}
}

// Why: a firmware update typed in the console (or sent by another tool) keeps the device flashing after
// its command ended; a backup then gets no answer and would count as failed. The device is busy.
func TestScheduledBackupSkipsADeviceUnderAFirmwareHold(t *testing.T) {
	e := newFleetEnv(t)
	id := e.dev(t, "flashing", fleetT0, model.Health{State: model.HealthOnline})
	e.holds.Start(id, 100)
	e.b.Run(context.Background())
	if len(e.tried) != 0 {
		t.Errorf("tried %v, want none: the device is still flashing", e.tried)
	}
	if it := item(e.last(t), "flashing"); it.Outcome != model.BackupOutcomeSkipped || it.Reason != skipBusy {
		t.Errorf("item = %+v", it)
	}
	release, _, ok := e.locks.TryAcquire(id, "x")
	if !ok {
		t.Fatal("the run kept the lock of a held device")
	}
	release()
}

// Why: when the guard cannot tell whether the device is busy (its database read failed), the device is
// left alone: skipped, never tried, never failed.
func TestScheduledBackupSkipsADeviceWhenTheGuardCannotTell(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "a", fleetT0, model.Health{State: model.HealthOnline})
	e.b.enter = func(context.Context, model.DeviceID, string) (func(), error) {
		return nil, errors.New("database is locked")
	}
	e.b.Run(context.Background())
	if len(e.tried) != 0 {
		t.Errorf("tried %v, want none", e.tried)
	}
	if it := item(e.last(t), "a"); it.Outcome != model.BackupOutcomeSkipped || it.Reason != skipBusy {
		t.Errorf("item = %+v", it)
	}
}

// Why: the run the server starts (backupAll) must use the same guard as the rules above: a device with
// a running transaction or under a firmware hold gets no request at all.
func TestBackupAllLeavesBusyDevicesAlone(t *testing.T) {
	a, svc, sent := notifyRuntime(t, nil)
	var hits atomic.Int32
	add := func(name string) model.DeviceID {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
		if err != nil {
			t.Fatal(err)
		}
		id, err := svc.Store.CreateDevice(t.Context(), model.Device{Name: name, Address: "127.0.0.1", Port: port})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Store.SetLastSeen(t.Context(), id, model.HealthOnline, time.Now()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	updating, flashing := add("updating"), add("flashing")
	j := model.Job{DeviceID: updating, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
	_, ids, err := svc.Store.CreateBatch(t.Context(), "u", []model.Job{j})
	if err != nil {
		t.Fatal(err)
	}
	j.ID, j.State = ids[0], model.JobAwaitReboot
	if err := svc.Store.SaveTransition(t.Context(), j, model.JobEvent{JobID: ids[0], From: model.JobQueued, To: model.JobAwaitReboot, Event: "test"}); err != nil {
		t.Fatal(err)
	}
	a.fwHolds = devlock.NewFirmwareHolds(time.Now, firmwareHoldTimeout)
	a.fwHolds.Start(flashing, 0)
	a.backupRun = newScheduledRun(nil)
	if err := a.setupBackups(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.backupAll(t.Context())
	if n := hits.Load(); n != 0 {
		t.Errorf("busy devices got %d requests, want none", n)
	}
	if got := sent(); len(got) != 0 {
		t.Errorf("sent %v for busy devices", got)
	}
	r, ok, err := svc.Store.LastBackupRun(t.Context())
	if err != nil || !ok || r.Tried != 0 || r.Skipped != 2 {
		t.Errorf("run = %+v ok=%v err=%v, want 2 skipped, none tried", r, ok, err)
	}
}
