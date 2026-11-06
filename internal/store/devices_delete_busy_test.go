package store

import (
	"errors"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#466): a delete that cascades under a running job leaves the engine's goroutine holding a
// job id that SQLite reuses for the next job (ota_jobs.id has no AUTOINCREMENT). The store must
// refuse the delete while a job holds the device, and tell "busy" apart from "no such device".

func TestDeleteDeviceRefusedWhileAJobRuns(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteDevice(ctx, dev)
	if !errors.Is(err, ErrDeviceBusy) || !errors.Is(err, ErrConflict) {
		t.Fatalf("delete under a queued job: %v (want ErrDeviceBusy, which wraps ErrConflict)", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("busy must not look like not found")
	}
	if _, err := s.GetDevice(ctx, dev); err != nil {
		t.Fatalf("the refused delete removed the device: %v", err)
	}
	if _, err := s.GetJob(ctx, ids[0]); err != nil {
		t.Fatalf("the refused delete removed the job: %v", err)
	}
}

func TestDeleteDeviceAllowedAfterTheJobEnded(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := s.GetJob(ctx, ids[0])
	done := j
	done.State = model.JobSucceeded
	if err := s.SaveTransition(ctx, done, model.JobEvent{JobID: j.ID, From: model.JobQueued, To: model.JobSucceeded, Event: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, dev); err != nil {
		t.Fatalf("a finished job keeps its history and must not block the delete: %v", err)
	}
	if _, err := s.GetJob(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished job history survived the cascade: %v", err)
	}
}

func TestDeleteDeviceRefusedWhileARestoreRuns(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	rid, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 3, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, dev); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("delete under a running restore: %v", err)
	}
	r, _ := s.GetRestore(ctx, rid)
	r.State, r.Detail = RestoreDone, "ok"
	if err := s.UpdateRestore(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(ctx, dev); err != nil {
		t.Fatalf("a finished restore must not block the delete: %v", err)
	}
}

func TestDeleteDeviceUnknownIsNotFoundNotBusy(t *testing.T) {
	s, _, _ := newStore(t)
	err := s.DeleteDevice(ctx, 99)
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("unknown device: %v", err)
	}
}

// A job created for a device that was deleted must fail on the foreign key, so a stale submit can
// never attach a job to a device that is gone.
func TestJobForADeletedDeviceFails(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.DeleteDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateBatch(ctx, "u", []model.Job{job(dev)}); err == nil {
		t.Fatal("a job was created for a deleted device")
	}
}

// Why: the guard lists the terminal states in SQL, and the store is not mutation-tested. A typo in
// 'failed' or 'cancelled' would make devices undeletable for ever (or a running state deletable), so
// every state is tried, terminal ones must allow the delete and the others must refuse it.
func TestDeleteDeviceGuardCoversEveryJobState(t *testing.T) {
	otaStates := map[model.JobState]bool{ // true = holds the device
		model.JobQueued: true, model.JobPreflight: true, model.JobBackup: true, model.JobSetOtaURL: true,
		model.JobSafebootSwitch: true, model.JobSafebootRecover: true, model.JobUpgradeIssued: true,
		model.JobAwaitReboot: true, model.JobVerify: true, model.JobRestoreOtaURL: true, model.JobApplying: true,
		model.JobSucceeded: false, model.JobFailed: false, model.JobCancelled: false,
	}
	for st, holds := range otaStates {
		s, _, _ := newStore(t)
		dev := mustDevice(t, s, "a", "10.0.0.1")
		j := job(dev)
		j.State = st
		if _, _, err := s.CreateBatch(ctx, "u", []model.Job{j}); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		err := s.DeleteDevice(ctx, dev)
		if holds != errors.Is(err, ErrDeviceBusy) || (!holds && err != nil) {
			t.Errorf("job state %q: holds=%v but DeleteDevice returned %v", st, holds, err)
		}
		if st.Terminal() == holds {
			t.Errorf("job state %q: the test table disagrees with model.JobState.Terminal", st)
		}
	}
	restoreStates := map[string]bool{
		RestorePending: true, RestoreBackup: true, RestoreFiles: true, RestoreSending: true,
		RestoreWaiting: true, RestoreVerifying: true, RestoreDone: false, RestoreFailed: false,
	}
	for st, holds := range restoreStates {
		s, _, _ := newStore(t)
		dev := mustDevice(t, s, "a", "10.0.0.1")
		id, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 1, Actor: "u"})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := s.GetRestore(ctx, id)
		r.State = st
		if err := s.UpdateRestore(ctx, r); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		err = s.DeleteDevice(ctx, dev)
		if holds != errors.Is(err, ErrDeviceBusy) || (!holds && err != nil) {
			t.Errorf("restore state %q: holds=%v but DeleteDevice returned %v", st, holds, err)
		}
		if r.Terminal() == holds {
			t.Errorf("restore state %q: the test table disagrees with RestoreJob.Terminal", st)
		}
	}
}
