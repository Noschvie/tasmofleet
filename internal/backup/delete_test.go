package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// takeAt takes a backup with the clock at t and returns its dump record.
func (e *env) takeAt(t *testing.T, at time.Time) model.Backup {
	t.Helper()
	e.clk.t = at
	made, err := e.m.Backup(t.Context(), "tester", 7)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := byKindUnit(made, model.BackupConfigDump)
	if !ok {
		t.Fatal("no dump")
	}
	return b
}

func byKindUnit(bs []model.Backup, k model.BackupKind) (model.Backup, bool) {
	for _, b := range bs {
		if b.Kind == k {
			return b, true
		}
	}
	return model.Backup{}, false
}

func (e *env) files(t *testing.T) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(e.dir, "backups"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Why: a backup is three files (dump, snapshot, decoded twin) that the UI
// shows as one entry. Deleting it must take all three, on disk and in the
// index, or the next listing shows half a backup and the disk keeps files
// nothing can reach.
func TestDeleteRunRemovesTheWholeRun(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	old := e.takeAt(t, day)
	e.takeAt(t, day.Add(24*time.Hour))
	if got := e.files(t); got != 6 {
		t.Fatalf("setup: %d files", got)
	}

	d, err := e.m.DeleteRun(t.Context(), "admin", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Files != 3 || d.Runs != 1 || d.Newest || d.Remaining != 1 {
		t.Errorf("deletion = %+v, want 3 files of 1 run, not the newest, 1 run left", d)
	}
	if got := e.files(t); got != 3 {
		t.Errorf("%d files left on disk, want 3", got)
	}
	left, _ := e.m.List(t.Context(), 7)
	if len(left) != 3 {
		t.Errorf("%d index rows left, want 3", len(left))
	}
	for _, b := range left {
		if b.TakenAt.Equal(old.TakenAt) {
			t.Errorf("row of the deleted run is still indexed: %+v", b)
		}
	}
	last := e.audit.ev[len(e.audit.ev)-1]
	if last.Action != "backup.delete" || last.Actor != "admin" || !strings.Contains(last.Detail, `"files":3`) {
		t.Errorf("audit = %+v", last)
	}
}

// Why: deleting the newest backup is the one delete that can leave a device
// without a recent restore point. The caller must be able to tell, before and
// after, so it can warn.
func TestDeletingTheNewestRunIsFlagged(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	e.takeAt(t, day)
	newest := e.takeAt(t, day.Add(time.Hour))

	plan, err := e.m.PlanDelete(t.Context(), newest.ID)
	if err != nil || !plan.Newest || plan.Remaining != 1 {
		t.Fatalf("plan = %+v (%v)", plan, err)
	}
	if e.files(t) != 6 {
		t.Fatal("planning must not delete anything")
	}
	d, err := e.m.DeleteRun(t.Context(), "admin", newest.ID)
	if err != nil || !d.Newest {
		t.Fatalf("deletion = %+v (%v)", d, err)
	}
	if ev := e.audit.ev[len(e.audit.ev)-1]; !strings.Contains(ev.Detail, `"newest":true`) {
		t.Errorf("the audit record must say the newest backup went: %s", ev.Detail)
	}
}

// Why: "delete older than" is the bulk clean-up. It must keep what is not
// older (the boundary is exclusive), say whether it takes the newest backup,
// and a preview must match what the delete then does.
func TestDeleteOlderThan(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	e.takeAt(t, day)
	e.takeAt(t, day.Add(24*time.Hour))
	e.takeAt(t, day.Add(48*time.Hour))
	cut := day.Add(48 * time.Hour)

	plan, err := e.m.PlanDeleteOlderThan(t.Context(), 7, cut)
	if err != nil || plan.Runs != 2 || plan.Files != 6 || plan.Newest || plan.Remaining != 1 {
		t.Fatalf("plan = %+v (%v)", plan, err)
	}
	d, err := e.m.DeleteOlderThan(t.Context(), "admin", 7, cut)
	if err != nil || d != plan {
		t.Fatalf("deletion = %+v, plan was %+v (%v)", d, plan, err)
	}
	if got := e.files(t); got != 3 {
		t.Errorf("%d files left, want the 3 of the run at the boundary", got)
	}
	ev := e.audit.ev[len(e.audit.ev)-1]
	if ev.Action != "backup.prune" || !strings.Contains(ev.Detail, `"runs":2`) {
		t.Errorf("audit = %+v", ev)
	}

	all, err := e.m.DeleteOlderThan(t.Context(), "admin", 7, cut.Add(time.Hour))
	if err != nil || !all.Newest || all.Remaining != 0 {
		t.Fatalf("deleting everything: %+v (%v)", all, err)
	}
}

// Why: another device's backups must never be touched, even with a matching
// time; the delete is scoped to the device in the URL.
func TestDeleteOlderThanIsScopedToTheDevice(t *testing.T) {
	e := newEnv(t, nil)
	e.takeAt(t, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	d, err := e.m.DeleteOlderThan(t.Context(), "admin", 8, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || d.Runs != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	if e.files(t) != 3 {
		t.Error("another device's backup was deleted")
	}
}

// Why: Replace device backs up hardware that is not (yet) the inventory
// entry. The backup is filed under the entry but marked, so it is never
// mistaken for the broken device's newest configuration.
func TestBackupOfFilesUnderTheOwnerAndMarksIt(t *testing.T) {
	e := newEnv(t, nil)
	src := model.Device{Name: "new board", Address: "172.31.13.99"}
	made, err := e.m.BackupOf(t.Context(), "admin", 7, src, NoteReplacementHardware)
	if err != nil {
		t.Fatal(err)
	}
	dmp, _ := byKindUnit(made, model.BackupConfigDump)
	if dmp.DeviceID != 7 || !IsReplacementHardware(dmp) {
		t.Errorf("dump record = %+v", dmp)
	}
	if plain := e.takeAt(t, e.clk.t.Add(time.Hour)); IsReplacementHardware(plain) {
		t.Error("an ordinary backup must not carry the marker")
	}
}
