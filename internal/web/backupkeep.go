package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// BackupKeeper is the N1 part of the backup service: keeping (pinning) runs,
// the retention in force, what the next pruning removes, and failed attempts
// (*backup.Manager implements it). Deps.Backups is asserted against it like
// BackupAdmin; without it the Keep route answers 404.
type BackupKeeper interface {
	SetKept(ctx context.Context, actor string, id model.BackupID, kept bool) (model.Backup, error)
	Retention() backup.Retention
	PrunedAtNextRun(ctx context.Context, device model.DeviceID) ([]model.Backup, error)
	Attempts(ctx context.Context, device model.DeviceID, limit int) ([]model.BackupAttempt, error)
}

// BackupBulkDeleter deletes several runs at once ("Delete selected", N1) and
// every backup of a device ("Delete the backups too", N2).
type BackupBulkDeleter interface {
	PlanDeleteRuns(ctx context.Context, device model.DeviceID, ids []model.BackupID) (backup.Deletion, error)
	DeleteRuns(ctx context.Context, actor string, device model.DeviceID, ids []model.BackupID) (backup.Deletion, error)
	DeleteAll(ctx context.Context, actor string, device model.DeviceID) (backup.Deletion, error)
}

func (h *handler) backupKeeper() (BackupKeeper, bool) {
	k, ok := h.d.Backups.(BackupKeeper)
	return k, ok
}

func (h *handler) backupBulkDeleter() (BackupBulkDeleter, bool) {
	k, ok := h.d.Backups.(BackupBulkDeleter)
	return k, ok
}

func (h *handler) registerBackupKeep(route routeFunc, op, adm func(http.Handler) http.Handler) {
	if _, ok := h.backupKeeper(); ok {
		route("POST /backups/{id}/keep", op, h.backupKeep)
	}
	if _, ok := h.backupBulkDeleter(); ok {
		route("POST /devices/{id}/backups/delete", adm, h.backupDeleteSelected)
		route("GET /devices/{id}/backups/delete", adm, h.backupDeleteSelectedConfirm)
		if _, ok := h.backupKeeper(); ok {
			route("GET /devices/{id}/backups/prune", adm, h.backupPruneConfirm)
		}
	}
}

// backupKeep keeps (kept=1) or releases (kept=0) the run the backup belongs
// to: the Keep toggle posts the state it switches to, so a double submit
// cannot flip it back. Operators may do it: it only protects a backup from
// the retention, it deletes nothing.
func (h *handler) backupKeep(w http.ResponseWriter, r *http.Request) {
	k, _ := h.backupKeeper()
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	limitBody(w, r)
	var kept bool
	switch r.PostFormValue("kept") {
	case "1":
		kept = true
	case "0":
	default:
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Nothing changed", "devices", messageData{Text: "Nothing changed: say whether to keep the backup."}))
		return
	}
	b, err := k.SetKept(r.Context(), actor(r), model.BackupID(n), kept)
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	ok := "kept"
	if !kept {
		ok = "released"
	}
	redirect(w, r, backupsHome(b)+"ok="+ok)
}

// backupsHome is the page that lists b, ready for a query: the device's
// Backups tab, or Settings -> Backups for a backup of a deleted device.
func backupsHome(b model.Backup) string {
	if b.DeviceID == 0 {
		return "/settings/system?tab=backups&"
	}
	return fmt.Sprintf("/devices/%d/backups?", b.DeviceID)
}

// backupDeleteSelected deletes the runs of the selected backups ("Delete
// selected"). The confirmation that names the count and whether the newest
// goes is the caller's dialog (PlanDeleteRuns gives both); the server insists
// on confirm=1 and on every id belonging to the device in the URL.
func (h *handler) backupDeleteSelected(w http.ResponseWriter, r *http.Request) {
	id, ok := h.deviceID(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	var ids []model.BackupID
	for _, s := range r.PostForm["id"] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			h.notFound(w, r)
			return
		}
		ids = append(ids, model.BackupID(n))
	}
	refuse := func(text string) {
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Nothing deleted", "devices", messageData{
			Text: text, LinkText: "Back to the backups", Link: fmt.Sprintf("/devices/%d/backups", id)}))
	}
	if r.PostFormValue("confirm") != "1" {
		refuse("Nothing was deleted: the deletion was not confirmed.")
		return
	}
	del, _ := h.backupBulkDeleter()
	d, err := del.DeleteRuns(r.Context(), actor(r), id, ids)
	switch {
	case errors.Is(err, backup.ErrNothingSelected):
		refuse("Nothing was deleted: no backup was selected.")
	case errors.Is(err, backup.ErrForeignBackup), errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.releaseBackupWait(id)
		redirect(w, r, fmt.Sprintf("/devices/%d/backups?ok=deleted&n=%d", id, d.Runs))
	}
}

// backupRetentionView is the view data of the retention line and the row
// labels of the Backups tab (N1 UI hook): "Keeping the last 5 backups and
// anything kept · 3 will be pruned at the next run", "Pruned at next run",
// "Kept", and the "Failed" rows.
type backupRetentionView struct {
	Keep     int
	MaxAge   time.Duration
	Pruned   map[time.Time]bool // runs (by TakenAt) the next pruning removes
	Runs     int                // len(Pruned)
	Attempts []model.BackupAttempt
}

// Line is the retention sentence above the list.
func (v backupRetentionView) Line() string {
	s := "Keeping the last " + plural(v.Keep, "backup")
	if v.MaxAge > 0 {
		s += " and everything from the last " + longDuration(v.MaxAge)
	}
	s += " and anything kept"
	if v.Runs > 0 {
		s += fmt.Sprintf(" · %d will be pruned at the next run", v.Runs)
	}
	return s
}

// IsPruned reports whether the run of b goes with the next pruning.
func (v backupRetentionView) IsPruned(b model.Backup) bool { return v.Pruned[b.TakenAt] }

// backupRetention collects the view data; ok is false when the backup service
// has no N1 support (tests with a plain fake).
func (h *handler) backupRetention(ctx context.Context, device model.DeviceID) (backupRetentionView, bool, error) {
	k, ok := h.backupKeeper()
	if !ok {
		return backupRetentionView{}, false, nil
	}
	ret := k.Retention()
	v := backupRetentionView{Keep: ret.Keep, MaxAge: ret.MaxAge, Pruned: map[time.Time]bool{}}
	pruned, err := k.PrunedAtNextRun(ctx, device)
	if err != nil {
		return v, true, err
	}
	for _, b := range pruned {
		v.Pruned[b.TakenAt] = true
	}
	v.Runs = len(v.Pruned)
	if v.Attempts, err = k.Attempts(ctx, device, 20); err != nil {
		return v, true, err
	}
	return v, true, nil
}

// backupTriggerText is what started a backup, for the row's second line.
// Backups from before v0.11 have no trigger and show none.
func backupTriggerText(t model.BackupTrigger, ref string) string {
	switch t {
	case model.TriggerManual:
		return "Manual"
	case model.TriggerScheduled:
		return "Scheduled"
	case model.TriggerUpdate:
		if ref != "" {
			return "Automatic, before update to " + ref
		}
		return "Automatic, before update"
	case model.TriggerRestore:
		return "Automatic, before restore"
	case model.TriggerReplace:
		return "Automatic, during replace"
	case model.TriggerBulk:
		return "Bulk backup"
	}
	return ""
}
