package ota

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// flakyBackups takes a backup per device; the devices in fail return an error.
type flakyBackups struct {
	mu    sync.Mutex
	taken []model.DeviceID
	by    []string
	runs  []backup.Run
	fail  map[model.DeviceID]error
}

func (f *flakyBackups) Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken = append(f.taken, id)
	f.runs = append(f.runs, backup.RunFrom(ctx))
	f.by = append(f.by, actor)
	return nil, f.fail[id]
}

func backupHarness(t *testing.T, n int) (*harness, *flakyBackups, *fakePrincipals, *auditRec) {
	t.Helper()
	h, _, pr, aud := commandHarness(t, n)
	fb := &flakyBackups{fail: map[model.DeviceID]error{}}
	h.e.backups = fb
	return h, fb, pr, aud
}

// Why: "Back up" for many devices is a transaction like Send command: one job
// per device, each backed up once by the backup service, the start audited.
func TestBackupJobsBackUpEveryDeviceOnceAndAuditTheStart(t *testing.T) {
	h, fb, _, aud := backupHarness(t, 2)
	id, skipped, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1, 2})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("%v %v", err, skipped)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if len(jobs) != 2 || !jobs[0].IsBackup() || jobs[0].IsUpdate() || jobs[0].State != model.JobSucceeded || jobs[1].State != model.JobSucceeded {
		t.Fatalf("jobs %+v", jobs)
	}
	if len(fb.taken) != 2 || fb.by[0] != "alice" {
		t.Fatalf("taken %v by %v", fb.taken, fb.by)
	}
	var started bool
	for _, e := range aud.evs {
		started = started || (e.Action == "backup.bulk.start" && e.Actor == "alice")
	}
	if !started {
		t.Fatalf("the start must be audited: %+v", aud.evs)
	}
}

// Why: one device that cannot be backed up fails on its own job with the
// reason; the others still get their backup (per-device result).
func TestBackupJobFailureStaysOnItsDevice(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 2)
	fb.fail[1] = fmt.Errorf("backup: download config: %w", tasmota.ErrUnreachable)
	id, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if jobs[0].State != model.JobFailed || jobs[0].LastError != "The device did not answer" || jobs[1].State != model.JobSucceeded {
		t.Fatalf("jobs %+v", jobs)
	}
}

// Why (#997): the transaction's result row says why a device's backup failed in the same sentence as the scheduled
// run and the Backups table (backup.AttemptReason, one sentence everywhere): "Needs an MQTT password: ..." for a
// device only MQTT could back up, not a Go error chain ("backup: download config: tasmota: unreachable: ... (...)")
// that buries the reason and may carry the device's address.
func TestABackupJobStoresTheReasonTheBackupHistoryShows(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 1)
	blocked := &transport.BackupBlockedError{Err: fmt.Errorf("%w: GET http://192.0.2.1/dl", tasmota.ErrUnreachable), Reason: "needs an MQTT password: TasmoFleet cannot reach the device over HTTP, so only MQTT can carry the backup; enter the device's MQTT password to back up over MQTT"}
	fb.fail[1] = fmt.Errorf("backup: download config: %w", blocked)
	if _, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1}); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	j := h.job(t, 1)
	if j.State != model.JobFailed || j.LastError != backup.AttemptReason(fb.fail[1]) || !strings.HasPrefix(j.LastError, "Needs an MQTT password: ") {
		t.Fatalf("%+v: want the reason sentence", j)
	}
	if strings.Contains(j.LastError, "192.0.2.1") || strings.Contains(j.LastError, "backup: download config") {
		t.Fatalf("%q: the Go error chain reached the result", j.LastError)
	}
}

// Why: a device with an active job is left out with the reason, and with
// nothing left the submit says so instead of creating an empty transaction.
func TestBackupSubmitSkipsBusyDevicesAndRefusesAnEmptyBatch(t *testing.T) {
	h, _, _, _ := backupHarness(t, 2)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindCommand, Commands: "Power1 ON", State: model.JobQueued, CreatedBy: "alice", CreatedKind: "local"})
	_, skipped, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1, 2})
	if err != nil || len(skipped) != 1 || skipped[0].DeviceID != 1 {
		t.Fatalf("%v %+v", err, skipped)
	}
	if _, _, err := h.e.SubmitBackup(as("local"), "alice", nil); !errors.Is(err, ErrNothingToFix) {
		t.Fatalf("empty batch: %v", err)
	}
}

// Why: like a command, the role is decided when the backup is taken: a creator
// who was demoted in between gets nothing out of the backup service.
func TestBackupStepFailsWhenTheCreatorIsDemoted(t *testing.T) {
	h, fb, pr, _ := backupHarness(t, 1)
	if _, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1}); err != nil {
		t.Fatal(err)
	}
	pr.roles["alice"] = model.RoleOperator
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobFailed || !strings.Contains(j.LastError, "admin") || len(fb.taken) != 0 {
		t.Fatalf("%+v taken %v", j, fb.taken)
	}
}

// Why: "Retry failed" repeats the backup for the failed devices only, as the
// retrying admin; an operator cannot retry.
func TestBackupRetryRepeatsFailedDevicesForAnAdminOnly(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 2)
	fb.fail[1] = errors.New("boom")
	orig, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	delete(fb.fail, 1)
	if _, err := h.e.SubmitRetry(as("local"), "bob", orig, nil); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("an operator's retry must be refused: %v", err)
	}
	res, err := h.e.SubmitRetry(as("local"), "alice", orig, nil)
	if err != nil || len(res.Devices) != 1 || res.Devices[0] != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: res.Batch})
	if len(jobs) != 1 || !jobs[0].IsBackup() || jobs[0].State != model.JobSucceeded {
		t.Fatalf("retry jobs %+v", jobs)
	}
}

// Why: a backup is harmless to repeat, but a job found mid-way after a restart
// is not silently assumed done: it runs again from the start.
func TestBackupJobFoundApplyingRunsAgain(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 1)
	h.st.put(model.Job{DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobApplying, CreatedBy: "alice", CreatedKind: "local"})
	h.run(t, h.e)
	if j := h.job(t, 1); j.State != model.JobSucceeded || len(fb.taken) != 1 {
		t.Fatalf("%+v taken %v", j, fb.taken)
	}
}

// Why (N1): "Keep these backups" and the label are chosen when the bulk backup
// starts but applied when each job runs, possibly after a restart or as a
// retry. They must reach the backup service with every job, marked as a bulk
// backup; without options the jobs take plain bulk backups.
func TestBackupJobsCarryKeepAndLabelToEveryRun(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 2)
	if _, _, err := h.e.SubmitBackupWith(as("local"), "alice", []model.DeviceID{1, 2}, BackupOptions{Keep: true, Label: "before\nthe move"}); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	if len(fb.runs) != 2 {
		t.Fatalf("runs %+v", fb.runs)
	}
	for i, r := range fb.runs {
		if r.Trigger != model.TriggerBulk || !r.Keep || r.Label != "before the move" {
			t.Errorf("run %d: %+v", i, r)
		}
	}
	if j := h.job(t, 1); !strings.HasPrefix(j.Commands, model.BackupMarker+"\n") {
		t.Errorf("options not stored with the job: %q", j.Commands)
	}
	if got := backupRun(model.BackupMarker); got.Keep || got.Label != "" || got.Trigger != model.TriggerBulk {
		t.Errorf("plain job: %+v", got)
	}
}

// batchDevices is the devices of a batch in the order their jobs were created, which is the order the engine
// starts them in (it takes queued jobs by job ID).
func batchDevices(t *testing.T, h *harness, id model.BatchID) []model.DeviceID {
	t.Helper()
	jobs, err := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].ID < jobs[k].ID })
	var out []model.DeviceID
	for _, j := range jobs {
		out = append(out, j.DeviceID)
	}
	return out
}

// Why (#993): a person who clicks "Back up all now" or "Back up selected" expects the devices to start in name
// order, whatever order the page posted them in (device ID order, or the list's current sort). The engine starts
// queued jobs by job ID, so the order the jobs are created in IS the start order; it comes from the device store's
// own list (the same rule as the scheduled run), not from the request. A duplicate stays next to its first
// occurrence and is still refused; a device the list no longer holds goes last and keeps its posted place.
func TestBackupSubmitCreatesJobsInTheDeviceStoresNameOrder(t *testing.T) {
	h, _, _, _ := backupHarness(t, 6)
	h.st.listOrder = []model.DeviceID{3, 1, 4, 2} // the store's name order; the test does not repeat its rule
	id, skipped, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{9, 1, 2, 3, 4, 1, 8})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := batchDevices(t, h, id), []model.DeviceID{3, 1, 4, 2, 9, 8}; !slices.Equal(got, want) {
		t.Fatalf("jobs created for %v, want the store's order %v (then the unknown devices as posted)", got, want)
	}
	if len(skipped) != 1 || skipped[0].DeviceID != 1 {
		t.Fatalf("the duplicate must still be refused: %+v", skipped)
	}
	// Only backups take the store's order: a command batch keeps the order it was posted in, and a failing device
	// list is no new way for it to fail.
	h.st.listErr = errors.New("disk gone")
	cid, _, err := h.e.SubmitCommand(as("local"), "alice", []PolicyRequest{
		{DeviceID: 6, Commands: []string{"Power1 ON"}}, {DeviceID: 5, Commands: []string{"Power1 ON"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := batchDevices(t, h, cid), []model.DeviceID{6, 5}; !slices.Equal(got, want) {
		t.Fatalf("command jobs created for %v, want the posted order %v", got, want)
	}
}

// Why (#993): a retry creates its jobs through the same path, so a retry of a batch that was started out of order
// (older versions, or any other path) runs A to Z again instead of keeping the old batch's order.
func TestBackupRetryStartsDevicesByName(t *testing.T) {
	h, fb, _, _ := backupHarness(t, 3)
	h.st.listOrder = []model.DeviceID{2, 3, 1}
	fb.fail[1], fb.fail[2], fb.fail[3] = errors.New("boom"), errors.New("boom"), errors.New("boom")
	now := t0
	orig, _, err := h.st.CreateBatch(context.Background(), "alice", []model.Job{ // created out of name order
		{DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobQueued, CreatedBy: "alice", CreatedKind: "local", CreatedAt: now, UpdatedAt: now},
		{DeviceID: 3, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobQueued, CreatedBy: "alice", CreatedKind: "local", CreatedAt: now, UpdatedAt: now},
		{DeviceID: 2, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobQueued, CreatedBy: "alice", CreatedKind: "local", CreatedAt: now, UpdatedAt: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	delete(fb.fail, 1)
	delete(fb.fail, 2)
	delete(fb.fail, 3)
	res, err := h.e.SubmitRetry(as("local"), "alice", orig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := batchDevices(t, h, res.Batch), []model.DeviceID{2, 3, 1}; !slices.Equal(got, want) {
		t.Fatalf("retry jobs created for %v, want %v", got, want)
	}
}

// Why (#993): without the device list there is no order to take; creating the jobs anyway would start them in
// whatever order was posted, the very thing this rule exists to prevent. Nothing is created, so nothing starts.
func TestBackupSubmitFailsWhenTheDeviceListCannotBeRead(t *testing.T) {
	h, _, _, aud := backupHarness(t, 2)
	boom := errors.New("disk gone")
	h.st.listErr = boom
	_, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{2, 1})
	if !errors.Is(err, boom) {
		t.Fatalf("want the list error, got %v", err)
	}
	if h.st.batches != 0 {
		t.Fatalf("%d batches were created although the order is unknown", h.st.batches)
	}
	for _, e := range aud.evs {
		if e.Action == "backup.bulk.start" {
			t.Fatalf("a start was audited for nothing: %+v", e)
		}
	}
}
