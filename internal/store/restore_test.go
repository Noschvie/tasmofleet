package store

import (
	"context"
	"errors"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: two restores racing on one device would overwrite each other's
// config; the database, not the service, is what makes "one at a time" hold.
func TestRestoreAllowsOneUnfinishedJobPerDevice(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	dev, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	id, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 3, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 4, Actor: "bob"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active job: %v", err)
	}
	j, _ := s.GetRestore(ctx, id)
	j.State, j.Detail, j.Verified = RestoreDone, "ok", true
	if err := s.UpdateRestore(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 4, Actor: "bob"}); err != nil {
		t.Fatalf("after a terminal job a new one must be allowed: %v", err)
	}
	list, _ := s.ListRestores(ctx, dev, 0)
	if len(list) != 2 || list[0].BackupID != 4 || !list[1].Verified || list[1].State != RestoreDone {
		t.Errorf("list: %+v", list)
	}
}

// Why: a crash mid-restore must not block the device forever; start-up
// recovery fails the orphaned job, and finished jobs stay untouched.
func TestFailActiveRestores(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	d1, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	d2, _ := s.CreateDevice(ctx, model.Device{Name: "b", Address: "10.0.0.2"})
	a, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d1, BackupID: 1, Actor: "x"})
	b, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d2, BackupID: 2, Actor: "x"})
	jb, _ := s.GetRestore(ctx, b)
	jb.State = RestoreDone
	if err := s.UpdateRestore(ctx, jb); err != nil {
		t.Fatal(err)
	}
	n, err := s.FailActiveRestores(ctx, "interrupted by restart", nil)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	ja, _ := s.GetRestore(ctx, a)
	if ja.State != RestoreFailed || ja.Detail != "interrupted by restart" {
		t.Errorf("%+v", ja)
	}
	jb, _ = s.GetRestore(ctx, b)
	if jb.State != RestoreDone {
		t.Errorf("finished job changed: %+v", jb)
	}
	if _, err := s.GetRestore(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job: %v", err)
	}
}

// Why (#324): a restore interrupted while it writes files (state "files") is an unfinished job
// like any other: it still blocks a second restore of the device (partial uploads must not be
// raced), and after a restart of TasmoFleet start-up recovery fails it instead of leaving it
// stuck. The state column is free text, so this proves no schema list excludes the new state.
func TestRestoreInFilesStateIsActiveAndRecoveredAfterARestart(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	d, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	id, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d, BackupID: 1, Actor: "x"})
	j, _ := s.GetRestore(ctx, id)
	j.State, j.Detail = RestoreFiles, "writing lib/x.be (2 of 3)"
	if err := s.UpdateRestore(ctx, j); err != nil {
		t.Fatal(err)
	}
	if j.Terminal() {
		t.Error("a job in the files state must not count as finished")
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: d, BackupID: 2, Actor: "x"}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second restore while files are written: %v, want ErrConflict", err)
	}
	if n, err := s.FailActiveRestores(ctx, "interrupted by restart", nil); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got, _ := s.GetRestore(ctx, id); got.State != RestoreFailed {
		t.Errorf("state after recovery: %q", got.State)
	}
}

// Why (#622): "Prune now" deletes backups in bulk and must never take the one a restore or a
// replace is reading from; without it a failed restore could not be retried (#494). The set is
// read from the job rows that already exist, so it ends with the job and nothing can go stale: a
// finished job protects nothing, and start-up recovery fails the jobs of a process that is gone.
func TestActiveRestoreBackupsListsOnlyUnfinishedJobs(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	d1, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	d2, _ := s.CreateDevice(ctx, model.Device{Name: "b", Address: "10.0.0.2"})
	d3, _ := s.CreateDevice(ctx, model.Device{Name: "c", Address: "10.0.0.3"})
	running, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d1, BackupID: 11, Actor: "x"})
	if j, _ := s.GetRestore(ctx, running); j.State != RestorePending {
		t.Fatalf("setup: %+v", j)
	}
	done, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d2, BackupID: 22, Actor: "x"})
	jd, _ := s.GetRestore(ctx, done)
	jd.State = RestoreDone
	if err := s.UpdateRestore(ctx, jd); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d3, BackupID: 33, Actor: "x"})
	jf, _ := s.GetRestore(ctx, failed)
	jf.State = RestoreFailed
	if err := s.UpdateRestore(ctx, jf); err != nil {
		t.Fatal(err)
	}

	got, err := s.ActiveRestoreBackups(ctx)
	if err != nil || len(got) != 1 || got[0] != 11 {
		t.Fatalf("active sources = %v (%v), want only the backup of the unfinished job", got, err)
	}

	if _, err := s.FailActiveRestores(ctx, "interrupted by restart", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ActiveRestoreBackups(ctx); err != nil || len(got) != 0 {
		t.Errorf("after the restart recovery %v (%v), want none: a job that is gone must protect nothing", got, err)
	}
}
