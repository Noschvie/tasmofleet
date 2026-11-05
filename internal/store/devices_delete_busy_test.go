package store

import (
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
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
