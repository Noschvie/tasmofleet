package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
)

// BackupBulk is what the bulk cleanup of Devices -> Backups needs (#622; *backup.Manager implements it):
// delete the backups of deleted devices, delete the unreferenced files, and Prune now. Deps.Backups is
// asserted against it; without it the page shows no bulk control and the routes do not exist.
type BackupBulk interface {
	PlanOrphanRuns(ctx context.Context, ids []model.BackupID) (backup.BulkResult, error)
	DeleteOrphanRuns(ctx context.Context, actor string, ids []model.BackupID) (backup.BulkResult, error)
	DeleteUnreferenced(ctx context.Context, actor string, paths []string) (backup.BulkResult, error)
	PruneNow(ctx context.Context, actor string, ids []model.BackupID) (backup.BulkResult, error)
}

// The bulk routes live below the page they act on. Each POST carries what the confirmation showed
// (ids or paths) and confirm=1; the backup service works out again what it may delete and removes only
// the overlap. The GET twins are the confirmation pages for browsers without JavaScript (the dialogs'
// text) and for "Delete selected", whose list depends on the boxes ticked.
const (
	orphansDeletePath = fleetPath + "/orphans/delete"
	unrefDeletePath   = fleetPath + "/unreferenced/delete"
	pruneNowPath      = fleetPath + "/prune"
)

func (h *handler) registerFleetBulk(route routeFunc, adm func(http.Handler) http.Handler) {
	if _, ok := h.d.Backups.(BackupBulk); !ok {
		return
	}
	route("GET "+orphansDeletePath, adm, h.orphansDeleteConfirm)
	route("POST "+orphansDeletePath, adm, h.orphansDelete)
	route("GET "+unrefDeletePath, adm, h.unrefDeleteConfirm)
	route("POST "+unrefDeletePath, adm, h.unrefDelete)
	route("GET "+pruneNowPath, adm, h.pruneNowConfirm)
	route("POST "+pruneNowPath, adm, h.pruneNow)
}

func (h *handler) bulk() BackupBulk {
	b, _ := h.d.Backups.(BackupBulk)
	return b
}

// bulkRefuse answers a POST that must not delete anything.
func (h *handler) bulkRefuse(w http.ResponseWriter, r *http.Request, text string) {
	h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Nothing deleted", "backups", messageData{
		Text: text, LinkText: "Back to the backups", Link: fleetPath, LinkPrimary: true}))
}

// bulkForm reads a bulk POST: the posted values of key and the confirmation. ok is false when the answer
// was written (not confirmed, nothing selected).
func (h *handler) bulkForm(w http.ResponseWriter, r *http.Request, key string) (vals []string, ok bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return nil, false
	}
	if r.PostFormValue("confirm") != "1" {
		h.bulkRefuse(w, r, "Nothing was deleted: the deletion was not confirmed.")
		return nil, false
	}
	vals = r.PostForm[key]
	if len(vals) == 0 {
		h.bulkRefuse(w, r, "Nothing was deleted: nothing was selected.")
		return nil, false
	}
	return vals, true
}

// bulkDone redirects to the page with the result as numbers; backupsNotice words it. An error after
// some files went still says how many went; the audit entry has the rest.
func (h *handler) bulkDone(w http.ResponseWriter, r *http.Request, key string, res backup.BulkResult, err error) {
	q := url.Values{"ok": {key}, "n": {strconv.Itoa(res.Runs)}, "f": {strconv.Itoa(res.Files)}, "b": {strconv.FormatInt(res.Bytes, 10)},
		"d": {strconv.Itoa(res.Devices)}, "l": {strconv.Itoa(res.Left)}}
	switch {
	case err == nil:
	case res.Files > 0:
		h.log.WarnContext(r.Context(), "bulk backup action stopped by an error", "action", key, "err", err)
		q.Set("ok", "bulk-partial")
	case errors.Is(err, backup.ErrNothingSelected):
		h.bulkRefuse(w, r, "Nothing was deleted: nothing was selected.")
		return
	case errors.Is(err, backup.ErrForeignBackup):
		h.notFound(w, r)
		return
	default:
		h.fail(w, r, err)
		return
	}
	redirect(w, r, fleetPath+"?"+q.Encode())
}

// bulkNotice words the result a bulk action redirected with; "" for any other page state.
func bulkNotice(q url.Values) string {
	count := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return max(n, 0) }
	bytes, _ := strconv.ParseInt(q.Get("b"), 10, 64)
	bytes = max(bytes, 0)
	runs, files, devs, left := count("n"), count("f"), count("d"), count("l")
	var s string
	switch q.Get("ok") {
	case "orphans-deleted":
		s = "Deleted " + plural(runs, "backup") + " of deleted devices (" + humanBytes(bytes) + ")."
	case "unref-deleted":
		s = "Deleted " + plural(files, "unreferenced file") + " (" + humanBytes(bytes) + ")."
	case "pruned-now":
		s = "Pruned " + plural(runs, "backup") + " of " + plural(devs, "device") + " (" + humanBytes(bytes) + ")."
	case "bulk-partial":
		return "Stopped early because of an error: " + plural(files, "file") + " were deleted before it. The audit log lists them."
	default:
		return ""
	}
	if left > 0 {
		s += " " + strconv.Itoa(left) + " left alone: kept, the newest of a device, used by a restore, or already gone."
	}
	return s + " The audit log lists them."
}

// idsOf reads posted backup ids; ok is false for one that is not a positive number.
func idsOf(vals []string) ([]model.BackupID, bool) {
	ids, ok := backupIDsOf(vals)
	return ids, ok && len(ids) > 0
}

// orphansDelete deletes the runs of deleted devices the confirmation named.
func (h *handler) orphansDelete(w http.ResponseWriter, r *http.Request) {
	vals, ok := h.bulkForm(w, r, "id")
	if !ok {
		return
	}
	ids, ok := idsOf(vals)
	if !ok {
		h.notFound(w, r)
		return
	}
	res, err := h.bulk().DeleteOrphanRuns(r.Context(), actor(r), ids)
	h.bulkDone(w, r, "orphans-deleted", res, err)
}

// orphansDeleteConfirm is the confirmation of "Delete selected" for the backups of deleted devices.
func (h *handler) orphansDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	ids, ok := idsOf(r.URL.Query()["id"])
	if !ok {
		h.bulkRefuse(w, r, "Nothing to delete: no backup was selected.")
		return
	}
	plan, err := h.bulk().PlanOrphanRuns(r.Context(), ids)
	switch {
	case errors.Is(err, backup.ErrForeignBackup):
		h.notFound(w, r)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	case plan.Runs == 0:
		h.bulkRefuse(w, r, "Nothing to delete: the selected backups are kept or gone. A kept backup is released before it can be deleted.")
		return
	}
	text := plural(plan.Runs, "backup") + " of " + plural(plan.Devices, "deleted device") + ", " + humanBytes(plan.Bytes) +
		" in all, will be deleted with their dumps, snapshots and decoded copies."
	if plan.Left > 0 {
		text += " " + strconv.Itoa(plan.Left) + " of the selection stays: a kept backup is released before it can be deleted."
	}
	c := actionConfirm{
		Title: "Delete " + plural(plan.Runs, "backup") + " of deleted devices?",
		Text:  text + " This cannot be undone; the audit log lists every backup.",
		Last:  "Delete selected", Action: orphansDeletePath, Verb: "Delete " + plural(plan.Runs, "backup"),
		CancelHref: fleetPath, CancelLabel: "Cancel",
	}
	for _, id := range ids {
		c.Fields = append(c.Fields, confirmField{Name: "id", Value: strconv.FormatInt(int64(id), 10)})
	}
	c.Fields = append(c.Fields, confirmField{Name: "confirm", Value: "1"})
	h.renderActionConfirm(w, r, "backups", c)
}

// unreferencedNow reads the files that no backup entry points to.
func (h *handler) unreferencedNow(r *http.Request) ([]backup.UnreferencedFile, error) {
	ul, ok := h.d.Backups.(BackupUnreferenced)
	if !ok {
		return nil, nil
	}
	return ul.Unreferenced(r.Context())
}

// unrefDelete deletes the files the confirmation listed, as far as they are still unreferenced.
func (h *handler) unrefDelete(w http.ResponseWriter, r *http.Request) {
	paths, ok := h.bulkForm(w, r, "path")
	if !ok {
		return
	}
	res, err := h.bulk().DeleteUnreferenced(r.Context(), actor(r), paths)
	h.bulkDone(w, r, "unref-deleted", res, err)
}

// unrefDeleteConfirm is the confirmation of "Delete all" as a page: every file listed now.
func (h *handler) unrefDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	files, err := h.unreferencedNow(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(files) == 0 {
		h.bulkRefuse(w, r, "Nothing to delete: no file is left that no backup entry points to.")
		return
	}
	var size int64
	c := actionConfirm{Last: "Delete unreferenced files",
		Action: unrefDeletePath, CancelHref: fleetPath, CancelLabel: "Cancel"}
	for _, f := range files {
		size += f.Size
		c.Facts = append(c.Facts, confirmFact{Name: f.Path, Value: humanBytes(f.Size), Code: true})
		c.Fields = append(c.Fields, confirmField{Name: "path", Value: f.Path})
	}
	c.Fields = append(c.Fields, confirmField{Name: "confirm", Value: "1"})
	c.Title = "Delete " + plural(len(files), "unreferenced file") + "?"
	c.Verb = "Delete " + plural(len(files), "file")
	c.Text = plural(len(files), "file") + ", " + humanBytes(size) + " in all, in old backup folders that no backup entry points to. " +
		"No backup and no restore point uses them. This cannot be undone; the audit log lists every file."
	h.renderActionConfirm(w, r, "backups", c)
}

// prunePlanNow works out again what Prune now may take at this moment: the dump ids of the runs of the
// devices that take part. The POST and the confirmation page both start from it, so neither can offer
// what the page's list would not.
func (h *handler) prunePlanNow(r *http.Request) (runs []fleetPruneRun, devices int, err error) {
	plan, err := h.d.BackupFleet.PrunePlan(r.Context())
	if err != nil {
		return nil, 0, err
	}
	for _, d := range plan {
		if d.LeftOut == "" {
			runs = append(runs, pruneRunsOf(d)...)
			devices++
		}
	}
	return runs, devices, nil
}

// pruneNow deletes the runs the confirmation named that Prune now still takes: the ids are cut to the
// current plan first (a device that failed its last run since is left out), then the backup service
// checks each device's runs once more under its lock.
func (h *handler) pruneNow(w http.ResponseWriter, r *http.Request) {
	vals, ok := h.bulkForm(w, r, "id")
	if !ok {
		return
	}
	ids, ok := idsOf(vals)
	if !ok {
		h.notFound(w, r)
		return
	}
	runs, _, err := h.prunePlanNow(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	allowed := map[model.BackupID]bool{}
	for _, run := range runs {
		allowed[run.ID] = true
	}
	var take []model.BackupID
	for _, id := range ids {
		if allowed[id] {
			take = append(take, id)
		}
	}
	skipped := len(ids) - len(take)
	if len(take) == 0 {
		h.bulkDone(w, r, "pruned-now", backup.BulkResult{Left: skipped}, nil)
		return
	}
	res, err := h.bulk().PruneNow(r.Context(), actor(r), take)
	res.Left += skipped
	h.bulkDone(w, r, "pruned-now", res, err)
}

// pruneNowConfirm is "Prune now" as a page: what the Storage card's list would take now.
func (h *handler) pruneNowConfirm(w http.ResponseWriter, r *http.Request) {
	runs, devices, err := h.prunePlanNow(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(runs) == 0 {
		v := h.view(r, "Nothing to prune", "backups", messageData{
			Text: "Nothing to prune: no device has a backup beyond the retention that Prune now may take.", LinkText: "Back to the backups", Link: fleetPath, LinkPrimary: true})
		h.message(w, r, http.StatusOK, v)
		return
	}
	var size int64
	c := actionConfirm{Last: "Prune now",
		Action: pruneNowPath, CancelHref: fleetPath, CancelLabel: "Cancel"}
	for _, run := range runs {
		size += run.Bytes
		c.Facts = append(c.Facts, confirmFact{Name: run.Name, At: run.Taken, Tail: " · " + humanBytes(run.Bytes)})
		c.Fields = append(c.Fields, confirmField{Name: "id", Value: strconv.FormatInt(int64(run.ID), 10)})
	}
	c.Fields = append(c.Fields, confirmField{Name: "confirm", Value: "1"})
	c.Title, c.Verb = "Prune "+plural(len(runs), "backup")+" now?", "Prune "+plural(len(runs), "backup")
	c.Text = plural(len(runs), "backup") + " of " + plural(devices, "device") + ", " + humanBytes(size) + " in all, are deleted now instead of at their next backup. " +
		pruneNowRules + " This cannot be undone; the audit log lists every backup."
	h.renderActionConfirm(w, r, "backups", c)
}

// pruneNowRules is the sentence that says what Prune now never takes, in the dialog and on the page.
const pruneNowRules = "Kept backups, the newest backup of a device and a backup a restore is using are never pruned."

// pruneLeftOutText names the devices Prune now leaves alone.
func pruneLeftOutText(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "Left out: " + strings.Join(names, ", ") + ", whose last run did not back them up."
}
