package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
)

// bulkBackups is the backup service with the bulk cleanup of Devices -> Backups (#622): it records
// what the handlers pass on and answers with canned results.
type bulkBackups struct {
	*unrefStorage
	orphanIDs, pruneIDs [][]model.BackupID
	paths               [][]string
	res, plan           backup.BulkResult
	err                 error
}

func (b *bulkBackups) PlanOrphanRuns(context.Context, []model.BackupID) (backup.BulkResult, error) {
	return b.plan, b.err
}
func (b *bulkBackups) DeleteOrphanRuns(_ context.Context, _ string, ids []model.BackupID) (backup.BulkResult, error) {
	b.orphanIDs = append(b.orphanIDs, ids)
	return b.res, b.err
}
func (b *bulkBackups) DeleteUnreferenced(_ context.Context, _ string, paths []string) (backup.BulkResult, error) {
	b.paths = append(b.paths, paths)
	return b.res, b.err
}
func (b *bulkBackups) PruneNow(_ context.Context, _ string, ids []model.BackupID) (backup.BulkResult, error) {
	b.pruneIDs = append(b.pruneIDs, ids)
	return b.res, b.err
}

// cleanupEnv is the Backups page with deleted devices' backups, unreferenced files and a prune plan
// of two devices: attic takes part with two runs (ids 1 and 3), cellar is left out (id 30).
func cleanupEnv(t *testing.T) (*env, *bulkBackups) {
	t.Helper()
	_, sb, _, _ := storageEnv(t)
	t1 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	f := []backup.UnreferencedFile{
		{Path: "backups/85/20261027T104345Z-dmp.dmp", Size: 2048, ModTime: t1},
		{Path: "backups/85/20261027T104345Z-snapshot.json", Size: 1024, ModTime: t1},
	}
	bb := &bulkBackups{unrefStorage: &unrefStorage{storageBackups: sb, files: f}}
	fleet := fleetFixture(time.Now())
	fleet.prune = []PruneDevice{
		{Device: 1, Name: "attic", Held: 1, Files: []model.Backup{
			{ID: 1, DeviceID: 1, Kind: model.BackupConfigDump, Size: 100, TakenAt: t1},
			{ID: 2, DeviceID: 1, Kind: model.BackupSnapshot, Size: 50, TakenAt: t1},
			{ID: 3, DeviceID: 1, Kind: model.BackupConfigDump, Size: 100, TakenAt: t1.Add(24 * time.Hour)},
		}},
		{Device: 3, Name: "cellar", LeftOut: "its last run did not back it up", Files: []model.Backup{
			{ID: 30, DeviceID: 3, Kind: model.BackupConfigDump, Size: 70, TakenAt: t1},
		}},
	}
	e, _, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Backups = bb; d.BackupFleet = fleet })
	return e, bb
}

var cleanupPaths = []string{"/devices/backups/orphans/delete", "/devices/backups/unreferenced/delete", "/devices/backups/prune"}

// Why (#622): the bulk deletes are admin-only and the page must not offer what answers 403. An
// operator sees the counts and the folded lists, no box and no button that deletes; an admin
// sees all three controls.
func TestFleetBulkRoutesAreAdminOnly(t *testing.T) {
	e, bb := cleanupEnv(t)
	_, op := e.do(t, "GET", "/devices/backups", "operator", nil)
	for _, bad := range []string{`type="checkbox"`, "Prune now…", "Delete all…", "Delete selected…", "bk-orph-sel", `id="dlg-fprune"`, `id="dlg-unref"`} {
		if strings.Contains(op, bad) {
			t.Errorf("operator page must not contain %q", bad)
		}
	}
	for _, want := range []string{"Pruned at the next run", "Unreferenced files", "Backups of deleted devices"} {
		if !strings.Contains(op, want) {
			t.Errorf("operator page lacks the counts: %q", want)
		}
	}
	_, adm := e.do(t, "GET", "/devices/backups", "admin", nil)
	for _, want := range []string{"Prune now…", "Delete all…", "Delete selected…", `id="dlg-fprune"`, `id="dlg-unref"`, `id="bk-orph-sel"`} {
		if !strings.Contains(adm, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
	for _, p := range cleanupPaths {
		for _, m := range []string{"GET", "POST"} {
			if r, _ := e.do(t, m, p, "operator", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "id": {"1"}, "path": {"x"}}); r.StatusCode != 403 {
				t.Errorf("operator %s %s: %d, want 403", m, p, r.StatusCode)
			}
		}
	}
	if len(bb.orphanIDs)+len(bb.pruneIDs)+len(bb.paths) != 0 {
		t.Error("an operator request reached the backup service")
	}
}

// Why (#622): the confirmation is what the admin agrees to, so it must name the count and the
// size, list what goes, name the devices left out and say what is never taken. The dialog and the
// page without JavaScript say the same; the form carries the ids of exactly the listed runs.
func TestPruneNowConfirmationNamesCountAndSize(t *testing.T) {
	e, _ := cleanupEnv(t)
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	dlg := page[strings.Index(page, `id="dlg-fprune"`):]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	for _, want := range []string{"Prune 2 backups now?", "2 backups of 1 device, 250 B in all", "Left out: cellar", "never pruned", "audit log lists every backup",
		`name="id" value="1"`, `name="id" value="3"`, "attic", "Prune 2 backups"} {
		if !strings.Contains(dlg, want) {
			t.Errorf("dialog lacks %q: %s", want, dlg)
		}
	}
	if strings.Contains(dlg, `value="30"`) || strings.Contains(dlg, `value="2"`) {
		t.Error("the form must carry the dump of each taken run only: not the left-out device, not the snapshot row")
	}
	storage := cardOf(t, page, "storage")
	if !strings.Contains(storage, "Left out") || !strings.Contains(storage, "cellar") {
		t.Errorf("the Storage list must show the left-out device with its badge: %s", storage)
	}
	r, body := e.do(t, "GET", "/devices/backups/prune", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("confirmation page: %d", r.StatusCode)
	}
	for _, want := range []string{"Prune 2 backups now?", "250 B in all", `name="id" value="1"`, `name="confirm" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation page lacks %q", want)
		}
	}
}

// Why (#622): a POST without the confirmation (a crawler, a replayed link, a forged form) must
// delete nothing. The three actions are destructive and cannot be undone.
func TestBulkPostWithoutConfirmDeletesNothing(t *testing.T) {
	e, bb := cleanupEnv(t)
	for _, p := range cleanupPaths {
		r, _ := e.do(t, "POST", p, "admin", url.Values{"csrf": {"tok"}, "id": {"1"}, "path": {"backups/85/20261027T104345Z-dmp.dmp"}})
		if r.StatusCode != 422 {
			t.Errorf("POST %s without confirm: %d, want 422", p, r.StatusCode)
		}
		r, _ = e.do(t, "POST", p, "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}})
		if r.StatusCode != 422 {
			t.Errorf("POST %s with nothing selected: %d, want 422", p, r.StatusCode)
		}
	}
	if len(bb.orphanIDs)+len(bb.pruneIDs)+len(bb.paths) != 0 {
		t.Error("an unconfirmed or empty POST reached the backup service")
	}
}

// Why (#622): the page may be minutes old. Between the dialog and the click a device can fail its
// run (and so be left out) or a run can vanish from the plan. The POST cuts the posted ids to what
// the plan allows now, so it never deletes more than the confirmation named and never what the
// plan no longer offers, and it says how many it left alone.
func TestPruneNowPostIsCutToTheCurrentPlan(t *testing.T) {
	e, bb := cleanupEnv(t)
	bb.res = backup.BulkResult{Devices: 1, Runs: 1, Files: 2, Bytes: 150, Left: 1}
	r, _ := e.do(t, "POST", "/devices/backups/prune", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "id": {"1", "30", "999"}})
	if r.StatusCode != 303 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if len(bb.pruneIDs) != 1 || len(bb.pruneIDs[0]) != 1 || bb.pruneIDs[0][0] != 1 {
		t.Fatalf("the service got %v, want only run 1: 30 is a left-out device's, 999 is not in the plan", bb.pruneIDs)
	}
	loc := r.Header.Get("Location")
	q, _ := url.Parse(loc)
	if q.Query().Get("ok") != "pruned-now" || q.Query().Get("l") != "3" {
		t.Errorf("redirect %q: want the result with 3 left alone (1 by the service, 2 cut here)", loc)
	}
	_, page := e.do(t, "GET", loc[strings.Index(loc, "/devices"):], "admin", nil)
	if !strings.Contains(page, "Pruned 1 backup of 1 device") || !strings.Contains(page, "3 left alone") || !strings.Contains(page, "audit log lists them") {
		t.Errorf("the page does not word the result: %s", page[:min(len(page), 400)])
	}

	// nothing allowed: the service is not called at all
	bb.pruneIDs = nil
	if r, _ := e.do(t, "POST", "/devices/backups/prune", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "id": {"30"}}); r.StatusCode != 303 || len(bb.pruneIDs) != 0 {
		t.Errorf("a left-out device's run: status %d, service calls %v", r.StatusCode, bb.pruneIDs)
	}
}

// Why (#622): "Delete all" and "Delete selected" pass exactly what the form carried to the service,
// which works out what is still allowed; a service refusal for a current device's backup answers
// 404 like the other backup routes, and an error after some went still tells how many.
func TestBulkDeletesPassTheFormOnAndWordTheResult(t *testing.T) {
	e, bb := cleanupEnv(t)
	bb.res = backup.BulkResult{Runs: 2, Files: 6, Bytes: 4096, Left: 1}
	r, _ := e.do(t, "POST", "/devices/backups/orphans/delete", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "id": {"72", "70"}})
	if r.StatusCode != 303 || len(bb.orphanIDs) != 1 || len(bb.orphanIDs[0]) != 2 {
		t.Fatalf("orphans: %d %v", r.StatusCode, bb.orphanIDs)
	}
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "ok=orphans-deleted") || !strings.Contains(loc, "b=4096") {
		t.Errorf("redirect %q", loc)
	}
	if got := bulkNotice(url.Values{"ok": {"orphans-deleted"}, "n": {"2"}, "b": {"4096"}, "l": {"1"}}); !strings.HasPrefix(got, "Deleted 2 backups of deleted devices (4.0 KiB).") || !strings.Contains(got, "1 left alone") {
		t.Errorf("notice %q", got)
	}

	r, _ = e.do(t, "POST", "/devices/backups/unreferenced/delete", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "path": {"backups/85/20261027T104345Z-dmp.dmp"}})
	if r.StatusCode != 303 || len(bb.paths) != 1 || bb.paths[0][0] != "backups/85/20261027T104345Z-dmp.dmp" {
		t.Fatalf("unreferenced: %d %v", r.StatusCode, bb.paths)
	}

	bb.err = backup.ErrForeignBackup
	bb.res = backup.BulkResult{}
	if r, _ = e.do(t, "POST", "/devices/backups/orphans/delete", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "id": {"1"}}); r.StatusCode != 404 {
		t.Errorf("a current device's backup: %d, want 404", r.StatusCode)
	}
	bb.err, bb.res = errors.New("disk full"), backup.BulkResult{Files: 3}
	r, _ = e.do(t, "POST", "/devices/backups/unreferenced/delete", "admin", url.Values{"csrf": {"tok"}, "confirm": {"1"}, "path": {"x"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.Contains(loc, "ok=bulk-partial") {
		t.Errorf("an error after 3 files: %d %q, want the partial notice", r.StatusCode, loc)
	}
	if got := bulkNotice(url.Values{"ok": {"bulk-partial"}, "f": {"3"}}); !strings.Contains(got, "3 files were deleted before it") || strings.Contains(got, "disk full") {
		t.Errorf("notice %q: it names the count, never the error text", got)
	}
}

// Why (#622): "Delete selected" for the deleted devices' backups lists what the ticked boxes
// stand for: the count, the size and what stays (a kept run), so the admin confirms the real set.
func TestOrphansDeleteConfirmationNamesWhatGoesAndWhatStays(t *testing.T) {
	e, bb := cleanupEnv(t)
	bb.plan = backup.BulkResult{Devices: 2, Runs: 3, Files: 9, Bytes: 5120, Left: 1}
	r, body := e.do(t, "GET", "/devices/backups/orphans/delete?id=70&id=72", "admin", nil)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, want := range []string{"Delete 3 backups of deleted devices?", "3 backups of 2 deleted devices, 5.0 KiB in all", "1 of the selection stays", `name="id" value="70"`, `name="confirm" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	bb.plan = backup.BulkResult{Left: 2}
	if r, _ := e.do(t, "GET", "/devices/backups/orphans/delete?id=70", "admin", nil); r.StatusCode != 422 {
		t.Errorf("nothing left to delete: %d, want 422 and no confirmation of nothing", r.StatusCode)
	}
	bb.err = backup.ErrForeignBackup
	if r, _ := e.do(t, "GET", "/devices/backups/orphans/delete?id=1", "admin", nil); r.StatusCode != 404 {
		t.Errorf("a current device's backup: %d", r.StatusCode)
	}
}

// Why (#622): the unreferenced files are the user's data; "Delete all" must show every file and
// the total before it deletes, in the dialog and on the page without JavaScript.
func TestUnreferencedDeleteAllNamesEveryFile(t *testing.T) {
	e, _ := cleanupEnv(t)
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	dlg := page[strings.Index(page, `id="dlg-unref"`):]
	dlg = dlg[:strings.Index(dlg, "</dialog>")]
	for _, want := range []string{"Delete 2 unreferenced files?", "2 files, 3.0 KiB in all", "backups/85/20261027T104345Z-dmp.dmp", "backups/85/20261027T104345Z-snapshot.json",
		`name="path" value="backups/85/20261027T104345Z-dmp.dmp"`, "audit log lists every file"} {
		if !strings.Contains(dlg, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	r, body := e.do(t, "GET", "/devices/backups/unreferenced/delete", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "3.0 KiB in all") || !strings.Contains(body, `name="path" value="backups/85/20261027T104345Z-snapshot.json"`) {
		t.Errorf("confirmation page: %d", r.StatusCode)
	}
}

// Why (#622): a kept backup of a deleted device is pinned on purpose: its box is disabled with the
// reason and Select all does not count it, so the bar never promises more than the delete does.
func TestKeptOrphanHasNoBoxAndSelectAllLeavesItOut(t *testing.T) {
	e, _ := cleanupEnv(t)
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	orph := cardOf(t, page, "orphans")
	if !strings.Contains(orph, "Kept: release it to delete it") || !strings.Contains(orph, "Select all that can go (1; 1 kept stay)") {
		t.Errorf("kept run or Select all text wrong: %s", orph)
	}
	if n := strings.Count(orph, `form="bk-orph-sel" name="id"`); n != 1 {
		t.Errorf("%d boxes, want 1: the kept run (id 70) has none", n)
	}
	if !strings.Contains(orph, "data-select-scope") || !strings.Contains(cardOf(t, page, "devices"), "data-select-scope") {
		t.Error("the device table and the deleted devices' list must each be a selection of their own (select.js)")
	}
}
