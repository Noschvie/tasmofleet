package app

import (
	"context"
	"encoding/json"
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

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
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
	tried   []string                // devices a backup was started on
	fail    map[string]error        // device name -> backup error
	notes   [][]notify.FailedDevice // failed devices per notification
	notesN  []int                   // tried count per notification
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
		notifyFailed: func(tried int, failed []notify.FailedDevice, _ time.Time) {
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

func failedNames(fs []notify.FailedDevice) []string {
	var names []string
	for _, f := range fs {
		names = append(names, f.Name)
	}
	return names
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
	if it.Outcome != model.BackupOutcomeSkipped || it.Reason != "offline" || !it.Since.Equal(since) {
		t.Errorf("item = %+v, want skipped, offline, since the tracker's time (a plain word and a time, not a hand-formatted text)", it)
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
	if len(e.notes) != 1 || strings.Join(failedNames(e.notes[0]), ",") != "bad1,bad2" || e.notesN[0] != 4 {
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
	// the bar, the page and the dashboard tile show the same time: when the run began
	if got.Started.IsZero() || !got.Started.Equal(r.StartedAt) || !want.Started.Equal(r.StartedAt) {
		t.Errorf("run started %v (before restart %v, after %v), want the stored start %v", r.StartedAt, want.Started, got.Started, r.StartedAt)
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
	if it.Outcome != model.BackupOutcomeSkipped || it.Reason != "offline" || !it.Since.Equal(since) {
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

// Why (decision 2026-11-05): a device that is online but has its HTTP API off and no MQTT path cannot be backed up.
// The run skips it with the reason (like an offline one: no failure, no notification), and a device that MQTT can
// serve is still backed up.
func TestScheduledBackupSkipsADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	e := newFleetEnv(t)
	at := fleetT0.Add(-time.Hour)
	// The refusal was first seen an hour ago and again a minute ago: the skip is "since" the first one (#645).
	latest := fleetT0.Add(-time.Minute)
	e.dev(t, "api-off", fleetT0, model.Health{State: model.HealthOnline, HTTPRefusedSince: at, HTTPRefusedAt: latest})
	e.dev(t, "api-off-but-mqtt", fleetT0, model.Health{State: model.HealthOnline, HTTPRefusedSince: at, HTTPRefusedAt: latest})
	e.dev(t, "fine", fleetT0, model.Health{State: model.HealthOnline})
	e.b.apiOffSkip = func(_ context.Context, id model.DeviceID, h model.Health) string {
		if id == e.devices["api-off"] && h.APIOff() {
			return skipAPIOff
		}
		return ""
	}
	e.b.Run(context.Background())
	if got := strings.Join(e.tried, ","); got != "api-off-but-mqtt,fine" && got != "fine,api-off-but-mqtt" {
		t.Errorf("backed up %q, want the two others", got)
	}
	it := item(e.last(t), "api-off")
	if it.Outcome != model.BackupOutcomeSkipped || it.Reason != "HTTP API off (SetOption128 0)" || !it.Since.Equal(at) {
		t.Errorf("item = %+v, want skipped, HTTP API off, since the refusal", it)
	}
	if len(e.notes) != 0 {
		t.Errorf("a skipped device raised a failure notification: %v", e.notes)
	}
}

// Why: the real rule behind the hook: a refusal the router no longer remembers still means no HTTP path, and
// only a device that MQTT can back up is left to the run, which the router then backs up over MQTT (it decides
// on the same facts; tried over HTTP first, Tasmota's silent /dl would fail the run and notify).
func TestAPIOffSkipUsesTheTrackersFindingNotTheRoutersMemory(t *testing.T) {
	id := model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", FreeKB: 700}
	httpOnly := model.Device{ID: 1, Name: "plug", Identity: id}
	onBroker := model.Device{ID: 2, Name: "mqtt-plug", Identity: id, MQTT: model.MQTTOurs, Topic: "mqtt-plug", FullTopic: "%prefix%/%topic%/",
		GroupTopic: "tasmotas", GroupTopicReadAt: factsNow, MoreGroupTopicsReadAt: factsNow, MQTTPasswordMode: model.MQTTPasswordOwn}
	refused := model.Health{State: model.HealthOnline, HTTPRefusedAt: factsNow}
	for _, tc := range []struct {
		name string
		id   model.DeviceID
		h    model.Health
		want string
		path transport.Path // the router's backup path
	}{
		{"HTTP-only device with its API off", 1, refused, skipAPIOff, transport.None},
		{"API answers", 1, model.Health{State: model.HealthOnline, SeenHTTP: factsNow}, "", transport.HTTP},
		{"API off but MQTT can back it up", 2, refused, "", transport.MQTT},
	} {
		h := tc.h
		if tc.id == 2 {
			h.LWT = "Online"
		}
		// The memory is empty: the router forgot the refusal. The facts read the same tracker as the run.
		a := &runtime{facts: newFacts([]model.Device{httpOnly, onBroker}, fakeFactsHealth{tc.id: h}, nil)}
		if got := a.apiOffSkip(context.Background(), tc.id, h); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		f, err := a.facts.Facts(context.Background(), tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got := transport.Choose(f).Backup; got != tc.path {
			t.Errorf("%s: the router backs up over %q, want %q", tc.name, got, tc.path)
		}
	}
}

// apiOffRuntime: device 1 is HTTP only, device 2 is on our broker with its own reads over MQTT but a strict
// write rule that fails: device 3, also on our broker, has a GroupTopic nobody has read yet.
func apiOffRuntime() (*runtime, map[model.DeviceID]model.Health) {
	id := model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", FreeKB: 700}
	httpOnly := model.Device{ID: 1, Name: "plug", Identity: id}
	onBroker := model.Device{ID: 2, Name: "mqtt-plug", Identity: id, MQTT: model.MQTTOurs, Topic: "mqtt-plug", FullTopic: "%prefix%/%topic%/",
		GroupTopic: "tasmotas", GroupTopicReadAt: factsNow, MoreGroupTopicsReadAt: factsNow, MQTTPasswordMode: model.MQTTPasswordOwn}
	unread := model.Device{ID: 3, Name: "other-plug", Identity: id, MQTT: model.MQTTOurs, Topic: "other-plug", FullTopic: "%prefix%/%topic%/"}
	refused := model.Health{State: model.HealthOnline, HTTPRefusedAt: factsNow}
	h2 := refused
	h2.LWT = "Online"
	hs := map[model.DeviceID]model.Health{1: refused, 2: h2, 3: {State: model.HealthOnline, LWT: "Online"}}
	return &runtime{svc: &Services{}, facts: newFacts([]model.Device{httpOnly, onBroker, unread}, fakeFactsHealth(hs), nil)}, hs
}

// Why (#645): the silent policy read every ten minutes is skipped (no read, no error) for a device whose HTTP API is
// off and which MQTT cannot read, like an offline one; a device MQTT can read, and one whose API answers, are read.
func TestPolicyReadSkipsADeviceWhoseHTTPAPIIsOffAndNothingElseReads(t *testing.T) {
	a, hs := apiOffRuntime()
	ctx := context.Background()
	if f, err := a.facts.Facts(ctx, 2); err != nil || transport.Choose(f).Commands != transport.MQTT || transport.Choose(f).Writes != transport.None {
		t.Fatalf("fixture: device 2 must be read over MQTT and have no write path (%v)", err)
	}
	if !a.readSkipped(ctx, 1, hs[1]) {
		t.Error("an HTTP-only device with its API off is read")
	}
	if a.readSkipped(ctx, 2, hs[2]) {
		t.Error("a device MQTT reads was skipped")
	}
	ok := model.Health{State: model.HealthOnline, SeenHTTP: factsNow}
	if a.readSkipped(ctx, 1, ok) {
		t.Error("a device whose API answers was skipped")
	}
	if !a.readSkipped(ctx, 1, model.Health{State: model.HealthOffline}) {
		t.Error("an offline device is read")
	}
}

// Why: the scheduled run and the Backups page must use the same rule as the manual Back up (the hooks are the
// wiring a refactor can lose silently): both skip an HTTP-only device whose API is off, with the one reason.
func TestScheduledBackupAndBackupsPageAreWiredToTheAPIOffRule(t *testing.T) {
	a, hs := apiOffRuntime()
	for name, hook := range map[string]func(context.Context, model.DeviceID, model.Health) string{
		"scheduled run": a.scheduledBackup().apiOffSkip,
		"Backups page":  a.fleetBackups().apiOffSkip,
	} {
		if hook == nil {
			t.Fatalf("%s: no API-off hook", name)
		}
		if got := hook(context.Background(), 1, hs[1]); got != skipAPIOff {
			t.Errorf("%s: %q, want %q", name, got, skipAPIOff)
		}
	}
}

// Why (#685): the message must let the person find the device and tell whether they must act, so
// each failed device reaches the notifier with its address and the reason the history shows. The
// reason comes from backup.AttemptReason: fixed sentences, never the raw error, which can carry a
// URL with a password (CLAUDE.md safety rule 4).
func TestScheduledBackupNotificationCarriesAddressAndReasonNotTheRawError(t *testing.T) {
	e := newFleetEnv(t)
	e.dev(t, "ok", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "mute", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "blocked", fleetT0, model.Health{State: model.HealthOnline})
	e.dev(t, "leaky", fleetT0, model.Health{State: model.HealthOnline})
	e.fail["mute"] = fmt.Errorf("GET http://admin:hunter2@192.0.2.2/dl: %w", tasmota.ErrUnreachable)
	e.fail["blocked"] = &transport.BackupBlockedError{Err: tasmota.ErrHTTPAPIRefused, Reason: "the web API is off and the device has no MQTT password stored"}
	e.fail["leaky"] = errors.New("login admin:hunter2 failed")
	e.b.Run(context.Background())
	if len(e.notes) != 1 || len(e.notes[0]) != 3 {
		t.Fatalf("notifications = %+v, want one naming the three failed devices", e.notes)
	}
	by := map[string]notify.FailedDevice{}
	for _, f := range e.notes[0] {
		by[f.Name] = f
	}
	if f := by["mute"]; f.Address != "192.0.2.2" || f.Reason != "The device did not answer" {
		t.Errorf("mute = %+v, want its address and the unreachable reason", f)
	}
	if f := by["blocked"]; f.Address != "192.0.2.3" || f.Reason != "The web API is off and the device has no MQTT password stored" {
		t.Errorf("blocked = %+v, want the sentence the transport wrote for the person", f)
	}
	if f := by["leaky"]; f.Reason != "The device did not hand over its configuration" {
		t.Errorf("leaky = %+v, want the generic reason", f)
	}
	text := notify.BackupFailed(config.Config{}, 4, e.notes[0], fleetT0).Text
	if strings.Contains(text, "hunter2") || strings.Contains(text, "admin") {
		t.Errorf("message leaks a credential from an error: %q", text)
	}
}

// Why (#685): the address is what the person types to reach the device, so a non-default HTTP
// port is part of it, and the hostname the device reported helps when the address is a lease.
func TestScheduledBackupNotificationShowsPortAndReportedHostname(t *testing.T) {
	e := newFleetEnv(t)
	id, err := e.st.CreateDevice(context.Background(), model.Device{Name: "porty", Address: "192.0.2.1", Port: 8080, Net: model.NetFacts{Hostname: "tasmota-porty"}})
	if err != nil {
		t.Fatal(err)
	}
	e.devices["porty"] = id
	if err := e.st.SetLastSeen(context.Background(), id, model.HealthOnline, fleetT0); err != nil {
		t.Fatal(err)
	}
	e.health[id] = model.Health{DeviceID: id, State: model.HealthOnline}
	e.fail["porty"] = tasmota.ErrTimeout
	e.b.Run(context.Background())
	if len(e.notes) != 1 || len(e.notes[0]) != 1 {
		t.Fatalf("notifications = %+v", e.notes)
	}
	if f := e.notes[0][0]; f.Address != "192.0.2.1:8080" || f.Hostname != "tasmota-porty" || f.Reason != "The device took too long to answer" {
		t.Errorf("failed device = %+v", f)
	}
}

// Why (#747): the notification is one line per failed device. Rows stored by older versions may
// hold a hostname (or device name) with a line break from an uncleaned probe; listed as it is,
// it would forge an extra failed device the person then goes looking for. The text is cleaned
// where it enters the message, whatever the row holds.
func TestScheduledBackupNotificationHasOneLinePerFailedDeviceWhateverTheRowHolds(t *testing.T) {
	e := newFleetEnv(t)
	forged := "x\n- Heater (192.0.2.9): The device did not answer"
	for i, d := range []model.Device{
		{Name: "porty", Address: "192.0.2.1", Net: model.NetFacts{Hostname: forged}},
		{Name: "bad\r\n- Lamp (192.0.2.8): The device did not answer", Address: "192.0.2.2", Net: model.NetFacts{Hostname: strings.Repeat("h", 500)}},
		// U+2028 is no control character (Unicode Zl) but a receiver can render it as a line break
		{Name: "sep", Address: "192.0.2.3", Net: model.NetFacts{Hostname: "y\u2028- Fan (192.0.2.7): The device did not answer"}},
	} {
		id, err := e.st.CreateDevice(context.Background(), d)
		if err != nil {
			t.Fatal(i, err)
		}
		e.devices[d.Name] = id
		if err := e.st.SetLastSeen(context.Background(), id, model.HealthOnline, fleetT0); err != nil {
			t.Fatal(err)
		}
		e.health[id] = model.Health{DeviceID: id, State: model.HealthOnline}
		e.fail[d.Name] = tasmota.ErrTimeout
	}
	e.b.Run(context.Background())
	if len(e.notes) != 1 || len(e.notes[0]) != 3 {
		t.Fatalf("notifications = %+v", e.notes)
	}
	text := notify.BackupFailed(config.Config{}, 3, e.notes[0], fleetT0).Text
	if lines := strings.Split(text, "\n"); len(lines) != 4 { // "Failed:" and one line per device
		t.Fatalf("message has %d lines, want 4:\n%s", len(lines), text)
	}
	if strings.ContainsAny(text, "\u2028\u2029") {
		t.Errorf("message keeps a line or paragraph separator: %q", text)
	}
	for _, f := range e.notes[0] {
		if len([]rune(f.Hostname)) > 100 || len([]rune(f.Name)) > 100 {
			t.Errorf("name or hostname not capped: %d, %d runes", len([]rune(f.Name)), len([]rune(f.Hostname)))
		}
	}
}

// needsPWHTTP is the device's HTTP side for TestScheduledBackupCountsADeviceThatNeedsAnMQTTPasswordAsFailed: it never
// answers (another LAN), and records what reached it.
type needsPWHTTP struct {
	mu    sync.Mutex
	calls []string
}

func (h *needsPWHTTP) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
	return nil, tasmota.ErrTimeout
}

func (h *needsPWHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, "/dl")
	return nil, tasmota.ErrTimeout
}

func (h *needsPWHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, tasmota.ErrTimeout
}

// needsPWMQTT is the command channel: the device is live on TasmoFleet's broker; it records every publish.
type needsPWMQTT struct {
	mu   sync.Mutex
	sent []string
}

func (m *needsPWMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, c)
	return json.RawMessage(`{}`), nil
}
func (m *needsPWMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m *needsPWMQTT) Connected() bool { return true }
func (m *needsPWMQTT) Download(_ context.Context, _ model.Device, _ string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, "FileDownload")
	return []byte("DUMP"), nil
}

// needsPWFacts: usable on TasmoFleet's broker in every way but the MQTT password, which is not set; nothing is known
// of HTTP (a device live on MQTT is not polled over HTTP, and a timeout is never recorded as a failure).
type needsPWFacts struct{}

func (needsPWFacts) HTTPState(model.DeviceID) transport.HTTPState { return transport.HTTPState{} }
func (needsPWFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: d.Topic, TopicExclusive: true,
		LWTOnline: true, GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour)},
		Password: transport.Password{Mode: model.MQTTPasswordNone}}, nil
}

type auditCapture struct {
	mu  sync.Mutex
	evs []model.AuditEvent
}

func (a *auditCapture) Record(_ context.Context, e model.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, e)
}

// Why (#997, plan-0420 question 1): a device in another LAN that only MQTT could back up, without its MQTT password,
// used to fail the scheduled run after the 20 s download timeout with "The device took too long to answer", which
// hides the cause. It still counts as Failed (it stays in the failure notification, as with the timeout), but with
// the reason "needs an MQTT password", decided before anything is sent: one bounded HTTP probe, no /dl, nothing
// published. The run goes through the real backup service and router, so the service adds no Status 11 of its own
// (its failure evidence reads only a device that answered), and the audit entry carries the same reason.
func TestScheduledBackupCountsADeviceThatNeedsAnMQTTPasswordAsFailed(t *testing.T) {
	e := newFleetEnv(t)
	id, err := e.st.CreateDevice(context.Background(), model.Device{Name: "remote", Address: "192.0.2.1", Topic: "remote", MQTT: model.MQTTOurs})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetLastSeen(context.Background(), id, model.HealthOnline, fleetT0); err != nil {
		t.Fatal(err)
	}
	e.health[id] = model.Health{DeviceID: id, State: model.HealthOnline}
	h, mq, aud := &needsPWHTTP{}, &needsPWMQTT{}, &auditCapture{}
	r := transport.NewRouter(transport.Options{HTTP: h, Devices: e.st, Facts: needsPWFacts{}})
	r.SetMQTT(mq)
	m, err := backup.New(backup.Options{DataDir: t.TempDir(), Devices: e.st, Store: e.st, Client: r, Audit: aud})
	if err != nil {
		t.Fatal(err)
	}
	e.b.backup = func(ctx context.Context, id model.DeviceID) error { _, err := m.Backup(ctx, "system", id); return err }
	e.b.Run(context.Background())

	it := item(e.last(t), "remote")
	if it.Outcome != model.BackupOutcomeFailed || !strings.HasPrefix(it.Reason, "Needs an MQTT password: ") {
		t.Fatalf("item %+v: want Failed with the reason", it)
	}
	if len(e.notes) != 1 || len(e.notes[0]) != 1 || e.notes[0][0].Reason != it.Reason {
		t.Fatalf("notifications %+v: the device stays in the failure notification with the reason", e.notes)
	}
	h.mu.Lock()
	calls := strings.Join(h.calls, "|")
	h.mu.Unlock()
	if calls != "Status 11" || len(mq.sent) != 0 {
		t.Fatalf("HTTP %q, MQTT %v: want one Status 11 probe, no /dl, nothing published", calls, mq.sent)
	}
	if len(aud.evs) != 1 || !strings.Contains(aud.evs[0].Result, "needs an MQTT password") {
		t.Fatalf("audit %+v: the entry carries the reason", aud.evs)
	}
}
