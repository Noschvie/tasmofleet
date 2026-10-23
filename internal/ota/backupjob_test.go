package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
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
	fb.fail[1] = errors.New("the device is unreachable")
	id, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{BatchID: id})
	if jobs[0].State != model.JobFailed || !strings.Contains(jobs[0].LastError, "unreachable") || jobs[1].State != model.JobSucceeded {
		t.Fatalf("jobs %+v", jobs)
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
