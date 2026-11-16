package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// guarded is the memory store with what *store.SQLite adds for the bulk actions: the source rows
// of unfinished restores. A test that does not set it up shows the fail-closed case.
type guarded struct {
	*memBackups
	active []model.BackupID
	err    error
}

func (g *guarded) ActiveRestoreBackups(context.Context) ([]model.BackupID, error) {
	return g.active, g.err
}

func guardedEnv(t *testing.T) (*env, *guarded) {
	t.Helper()
	var g *guarded
	e := newEnv(t, func(o *Options) {
		g = &guarded{memBackups: o.Store.(*memBackups)}
		o.Store = g
	})
	return e, g
}

// fiveRuns takes five runs of device 7, hourly, and returns their dump rows oldest first. The
// retention is then 2: PrunedAtNextRun lists the four oldest (the next backup would take a slot).
func fiveRuns(t *testing.T, e *env) []model.Backup {
	t.Helper()
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	var runs []model.Backup
	for i := range 5 {
		runs = append(runs, e.takeAt(t, day.Add(time.Duration(i)*time.Hour)))
	}
	e.m.SetRetention(2, 0)
	e.clk.t = e.clk.t.Add(time.Hour) // now is later than the newest run, as it is when an admin clicks
	return runs
}

func lastAudit(e *env) model.AuditEvent { return e.audit.ev[len(e.audit.ev)-1] }

func dumps(rows ...model.Backup) []model.BackupID {
	var out []model.BackupID
	for _, b := range rows {
		out = append(out, b.ID)
	}
	return out
}

// Why (#622): a Kept backup is something someone pinned on purpose. A bulk delete of the deleted
// devices' backups must leave it, even when its box is posted (the maintainer's rule; its own
// Delete button is still there). The rest of the selection goes in the same action.
func TestDeleteOrphanRunsNeverTakesAKeptRun(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	pinned := e.takeAt(t, day)
	plain := e.takeAt(t, day.Add(time.Hour))
	if _, err := e.m.SetKept(t.Context(), "op", pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	e.st.orphan(7)

	res, err := e.m.DeleteOrphanRuns(t.Context(), "adm", dumps(pinned, plain))
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 1 || res.Files != 3 || res.Left != 1 {
		t.Errorf("result %+v, want the plain run (3 files) gone and the kept one left", res)
	}
	left, _ := e.m.Orphans(t.Context())
	if len(left) != 3 || !left[0].Kept || !left[0].TakenAt.Equal(pinned.TakenAt) {
		t.Fatalf("left %+v, want the kept run whole", left)
	}
	if n := e.files(t); n != 3 {
		t.Errorf("%d files on disk, want the kept run's 3", n)
	}
	if ev := lastAudit(e); !strings.Contains(ev.Detail, `"why":"kept"`) {
		t.Errorf("the audit entry must say which run was left and why: %s", ev.Detail)
	}
}

// Why (#622): the form posts ids. An id of a current device's backup (a forged form, or a device
// that was restored from a backup meanwhile) must not delete anything: current devices have no
// free bulk delete, only the retention.
func TestDeleteOrphanRunsRefusesALiveDevicesBackup(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	live := e.takeAt(t, day)
	orph := e.takeAt(t, day.Add(time.Hour))
	e.st.orphan(7)
	e.st.mu.Lock()
	row := e.st.rows[live.ID]
	row.DeviceID = 9 // a current device
	e.st.rows[live.ID] = row
	e.st.mu.Unlock()

	if _, err := e.m.DeleteOrphanRuns(t.Context(), "adm", dumps(orph, live)); !errors.Is(err, ErrForeignBackup) {
		t.Fatalf("err = %v, want ErrForeignBackup", err)
	}
	if n := e.files(t); n != 6 {
		t.Errorf("%d files on disk, the refused action must delete nothing (6 before)", n)
	}
	if _, err := e.m.DeleteOrphanRuns(t.Context(), "adm", nil); !errors.Is(err, ErrNothingSelected) {
		t.Errorf("empty selection: %v", err)
	}
}

// Why (#622): the confirmation names a count and a size, so the deletion must be exactly that:
// the listed paths, still unreferenced. A path that became referenced since the page was drawn, a
// file of the current layout (a backup in progress writes there), a path that leaves the data
// directory and a name the scan never lists stay untouched.
func TestDeleteUnreferencedOnlyRemovesWhatTheScanStillListsAndWasConfirmed(t *testing.T) {
	e := newEnv(t, nil)
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(e.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stray := "backups/85/20261027T104345Z-dmp.dmp"
	onlyStray := "backups/86/20261027T104345Z-dmp.dmp" // alone in its folder: the folder goes with it
	unlisted := "backups/85/20261028T104345Z-dmp.dmp"  // unreferenced but not in the confirmation
	nowReferenced := "backups/85/20261029T104345Z-dmp.dmp"
	current := "backups/kitchen/20261027T104345Z_kitchen.dmp"
	outside := filepath.Join(filepath.Dir(e.dir), "outside-"+filepath.Base(e.dir))
	for _, p := range []string{stray, onlyStray, unlisted, nowReferenced, current} {
		write(p, "data")
	}
	if err := os.WriteFile(outside, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) }) // best effort: a file the test made beside its temp dir
	if _, err := e.st.PutBackup(t.Context(), model.Backup{DeviceID: 7, Kind: model.BackupConfigDump, Path: nowReferenced}); err != nil {
		t.Fatal(err)
	}

	confirmed := []string{stray, onlyStray, nowReferenced, current, "../" + filepath.Base(outside), "backups/85/nothing-here.dmp", stray}
	res, err := e.m.DeleteUnreferenced(t.Context(), "adm", confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Bytes != 8 || res.Left != 4 {
		t.Errorf("result %+v, want 2 files (8 bytes) removed and 4 paths left", res)
	}
	for _, rel := range []string{stray, onlyStray} {
		if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be gone (%v)", rel, err)
		}
	}
	for _, rel := range []string{unlisted, nowReferenced, current} {
		if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s must stay: %v", rel, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a path leaving the data directory must never be followed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "backups/86")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied old folder should go too (%v)", err)
	}
	if _, err := e.m.DeleteUnreferenced(t.Context(), "adm", nil); !errors.Is(err, ErrNothingSelected) {
		t.Errorf("empty selection: %v", err)
	}
}

// Why (#622, #494): with a retention of 1 the list "Pruned at the next run" contains the current
// newest run, because the backup that would push it out is not taken yet. Prune now takes no
// backup, so deleting that run would leave the device with none at all. The newest run stays, and
// the device ends with exactly one.
func TestPruneNowNeverDeletesTheNewestRunWithKeepOne(t *testing.T) {
	e, _ := guardedEnv(t)
	runs := fiveRuns(t, e)
	e.m.SetRetention(1, 0)
	listed, _ := e.m.PrunedAtNextRun(t.Context(), 7)
	if !containsRun(listed, runs[4].TakenAt) {
		t.Fatal("setup: with a retention of 1 the next-run list must name the newest run, or this test proves nothing")
	}

	res, err := e.m.PruneNow(t.Context(), "adm", dumps(runs...))
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 4 || res.Left != 1 {
		t.Errorf("result %+v, want the four older runs gone and the newest left", res)
	}
	left, _ := e.m.List(t.Context(), 7)
	if len(left) != 3 || !left[0].TakenAt.Equal(runs[4].TakenAt) {
		t.Fatalf("left %+v, want only the newest run", left)
	}
	if ev := lastAudit(e); !strings.Contains(ev.Detail, HeldNewest) {
		t.Errorf("the audit entry must name why the newest run stayed: %s", ev.Detail)
	}
	// An admin reads the audit by device name, not by id: the left run is named like the removed ones.
	name := runs[4].DeviceName
	if name == "" {
		t.Fatal("setup: the dump row carries no device name, so the audit check proves nothing")
	}
	if ev := lastAudit(e); !strings.Contains(ev.Detail, `"left":[{"device":"`+name+`"`) {
		t.Errorf("the left run must carry the device name %q, not its id: %s", name, ev.Detail)
	}
}

// Why: a failed "Replace device" files a newer run of replacement hardware under the dead device.
// NewestRun then returns that run, so only the newest-own-run rule keeps the device's real
// configuration: with a retention of 1 the own run is listed for pruning (the replacement run is
// counted apart), and without the rule Prune now would leave only the other hardware's dump.
func TestPruneNowNeverDeletesTheDevicesNewestOwnRun(t *testing.T) {
	t1 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	list := []model.Backup{
		{ID: 1, DeviceID: 7, Kind: model.BackupConfigDump, TakenAt: t1, Trigger: model.TriggerScheduled},
		{ID: 2, DeviceID: 7, Kind: model.BackupConfigDump, TakenAt: t2, Trigger: model.TriggerManual, Note: NoteReplacementHardware},
	}
	now := t2.Add(time.Hour)
	if !containsRun(PrunedAtNextRun(list, 1, 0, now), t1) {
		t.Fatal("setup: with a retention of 1 the own run must be listed for the next pruning, or this test proves nothing")
	}
	prune, held := PruneNowSet(list, 1, 0, now, nil)
	if containsRun(prune, t1) {
		t.Fatalf("Prune now would remove the device's only own run: %+v", prune)
	}
	if len(held) != 1 || !held[0].TakenAt.Equal(t1) || held[0].Why != HeldNewest {
		t.Errorf("held %+v, want the own run held as the newest", held)
	}
}

func containsRun(files []model.Backup, taken time.Time) bool {
	for _, b := range files {
		if b.TakenAt.Equal(taken) {
			return true
		}
	}
	return false
}

// Why (#494): a restore reads its backup after the safety backup, and a failed restore is retried
// from the same backup. If Prune now took the run an unfinished restore or replace uses, the retry
// would find nothing. The whole run stays, the others go.
func TestPruneNowSkipsTheRunAnActiveRestoreUses(t *testing.T) {
	e, g := guardedEnv(t)
	runs := fiveRuns(t, e)
	g.active = []model.BackupID{runs[1].ID}

	res, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[0], runs[1], runs[2]))
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 2 || res.Left != 1 {
		t.Errorf("result %+v, want runs 0 and 2 gone and the restore's run left", res)
	}
	list, _ := e.m.List(t.Context(), 7)
	if got := len(runsOf(list, runs[1].TakenAt)); got != 3 {
		t.Errorf("the restore's run has %d files left, want all 3", got)
	}
	if _, _, err := e.m.Open(t.Context(), runs[1].ID); err != nil {
		t.Errorf("the restore's source cannot be opened: %v", err)
	}
	if ev := lastAudit(e); !strings.Contains(ev.Detail, HeldInUse) {
		t.Errorf("the audit entry must name the run left for the restore: %s", ev.Detail)
	}

	g.active = nil // the restore finished: nothing protects the run any more
	if _, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[1])); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.m.List(t.Context(), 7); len(runsOf(list, runs[1].TakenAt)) != 0 {
		t.Error("a finished restore must protect nothing")
	}
}

// Why (#494): if the store cannot say which backups a restore uses, Prune now cannot know a run
// is safe to delete. It refuses and deletes nothing, rather than guess.
func TestPruneNowRefusesWithoutRestoreSource(t *testing.T) {
	e := newEnv(t, nil) // memBackups has no ActiveRestoreBackups
	runs := fiveRuns(t, e)
	if _, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[0])); !errors.Is(err, ErrNoRestoreGuard) {
		t.Fatalf("err = %v, want ErrNoRestoreGuard", err)
	}
	if _, _, err := e.m.PruneNowPlan(t.Context(), 7); !errors.Is(err, ErrNoRestoreGuard) {
		t.Errorf("plan err = %v, want ErrNoRestoreGuard", err)
	}
	e2, g := guardedEnv(t)
	runs = fiveRuns(t, e2)
	g.err = errors.New("database is locked")
	if _, err := e2.m.PruneNow(t.Context(), "adm", dumps(runs[0])); err == nil {
		t.Error("a failed read of the restore jobs must stop the prune")
	}
	if n := e.files(t) + e2.files(t); n != 30 {
		t.Errorf("%d files on disk, a refused prune must delete nothing (30 before)", n)
	}
}

// Why (#622): the confirmation named a set; a backup taken after it, a changed retention or a
// forged form must not make the action delete more. Only the overlap of what was confirmed and what
// the rule allows now goes, and a row of a deleted device is refused outright.
func TestPruneNowDeletesNothingTheConfirmationDidNotName(t *testing.T) {
	e, _ := guardedEnv(t)
	runs := fiveRuns(t, e)

	res, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[0]))
	if err != nil || res.Runs != 1 || res.Files != 3 {
		t.Fatalf("result %+v (%v), want only the confirmed run", res, err)
	}
	list, _ := e.m.List(t.Context(), 7)
	if countRuns(list) != 4 {
		t.Errorf("%d runs left, want 4: the other runs the rule lists were not confirmed", countRuns(list))
	}

	// a run that the rule does not list (inside the retention) and one that is gone
	res, err = e.m.PruneNow(t.Context(), "adm", append(dumps(runs[4]), 9999, runs[0].ID))
	if err != nil || res.Runs != 0 || res.Left != 3 {
		t.Errorf("result %+v (%v), want nothing removed and 3 left", res, err)
	}
	if countRuns(mustList(t, e)) != 4 {
		t.Error("a run the rule does not list must stay")
	}

	// a pinned run is not listed either
	if _, err := e.m.SetKept(t.Context(), "op", runs[1].ID, true); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.m.PruneNow(t.Context(), "adm", dumps(runs[1])); res.Runs != 0 || res.Left != 1 {
		t.Errorf("kept run: %+v", res)
	}

	e.st.orphan(7)
	if _, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[2])); !errors.Is(err, ErrForeignBackup) {
		t.Errorf("a deleted device's backup: %v, want ErrForeignBackup", err)
	}
	if _, err := e.m.PruneNow(t.Context(), "adm", nil); !errors.Is(err, ErrNothingSelected) {
		t.Errorf("empty selection: %v", err)
	}
}

func mustList(t *testing.T, e *env) []model.Backup {
	t.Helper()
	l, err := e.m.List(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Why (#622): "one audit entry per bulk action, listing every backup". Without the entry an admin
// cannot tell afterwards what a bulk delete took; the entry must name each run and each file the
// action removed, and carry the error when it stopped early.
func TestBulkActionsWriteOneAuditEntryListingEveryRun(t *testing.T) {
	e, _ := guardedEnv(t)
	runs := fiveRuns(t, e)
	before := len(e.audit.ev)
	if _, err := e.m.PruneNow(t.Context(), "adm", dumps(runs[0], runs[1], runs[2])); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.ev) != before+1 {
		t.Fatalf("%d audit entries for one prune, want 1", len(e.audit.ev)-before)
	}
	ev := lastAudit(e)
	if ev.Action != "backup.prune_now" || ev.Actor != "adm" || ev.Result != "ok" {
		t.Errorf("entry %+v", ev)
	}
	for _, r := range runs[:3] {
		if !strings.Contains(ev.Detail, r.TakenAt.UTC().Format(time.RFC3339)) {
			t.Errorf("run %s missing from %s", r.TakenAt, ev.Detail)
		}
	}
	if !strings.Contains(ev.Detail, `"runs":3`) || !strings.Contains(ev.Detail, `"files":9`) {
		t.Errorf("totals missing: %s", ev.Detail)
	}

	e.st.orphan(7)
	before = len(e.audit.ev)
	orph, _ := e.m.Orphans(t.Context())
	var pick []model.BackupID
	for _, b := range orph {
		if b.Kind == model.BackupConfigDump {
			pick = append(pick, b.ID)
		}
	}
	if _, err := e.m.DeleteOrphanRuns(t.Context(), "adm", pick); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.ev) != before+1 || lastAudit(e).Action != "backup.delete_orphans" || !strings.Contains(lastAudit(e).Detail, `"runs":2`) {
		t.Errorf("orphan delete audit: %+v", e.audit.ev[before:])
	}
}

// Why (#622): the confirmation page names a count and a size; they must be what the delete then
// removes, kept run left out in both, or the admin agrees to one thing and gets another.
func TestPlanOrphanRunsIsWhatTheDeleteThenRemoves(t *testing.T) {
	e := newEnv(t, nil)
	day := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	pinned := e.takeAt(t, day)
	a := e.takeAt(t, day.Add(time.Hour))
	b := e.takeAt(t, day.Add(2*time.Hour))
	if _, err := e.m.SetKept(t.Context(), "op", pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	e.st.orphan(7)
	pick := dumps(pinned, a, b)

	plan, err := e.m.PlanOrphanRuns(t.Context(), pick)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.files(t); n != 9 {
		t.Fatalf("planning removed files: %d left", n)
	}
	done, err := e.m.DeleteOrphanRuns(t.Context(), "adm", pick)
	if err != nil {
		t.Fatal(err)
	}
	if plan != done || plan.Runs != 2 || plan.Files != 6 || plan.Left != 1 || plan.Devices != 1 || plan.Bytes == 0 {
		t.Errorf("plan %+v, delete %+v: they must agree (2 runs, 6 files, the kept run left)", plan, done)
	}
}
