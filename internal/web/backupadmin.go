package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// BackupAdmin is what deleting backups needs on top of backup.Service
// (*backup.Manager implements it). Deps.Backups is asserted against it, so
// the backup.Service contract stays unchanged; without it the delete controls
// are not shown and the routes answer 404.
type BackupAdmin interface {
	PlanDelete(ctx context.Context, id model.BackupID) (backup.Deletion, error)
	DeleteRun(ctx context.Context, actor string, id model.BackupID) (backup.Deletion, error)
}

// BackupFileChecker tells whether a backup's file is gone from the data directory
// (deleted by hand). *backup.Manager implements it; without it no row is marked.
type BackupFileChecker interface {
	FileMissing(b model.Backup) bool
}

// fileMissing is true when the backup service can tell and the file is gone.
func (h *handler) fileMissing(b model.Backup) bool {
	c, ok := h.d.Backups.(BackupFileChecker)
	return ok && c.FileMissing(b)
}

// backupFileMissing answers a request for the content of a backup whose file was deleted
// by hand: it says why nothing can be downloaded, compared or restored, and where the row
// can be removed.
func (h *handler) backupFileMissing(w http.ResponseWriter, r *http.Request, b model.Backup) {
	where, label := "/devices/backups", "Backups of deleted devices"
	if b.DeviceID != 0 {
		where, label = fmt.Sprintf("/devices/%d/backups", b.DeviceID), "Backups of this device"
	}
	h.renderNotFound(w, r, "Backup file missing", "The file of this backup is missing",
		"The file was removed from the data directory, so it cannot be downloaded, compared or restored. Check that the data volume is mounted; if the file is really gone, remove the backup from the list.",
		ui.Btn{Label: label, Kind: "primary", Href: where}, ui.Btn{Label: "Go to dashboard", Href: "/"})
}

func (h *handler) backupAdmin() (BackupAdmin, bool) {
	a, ok := h.d.Backups.(BackupAdmin)
	return a, ok
}

func (h *handler) registerBackupAdmin(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if _, ok := h.backupAdmin(); !ok {
		return
	}
	route("GET /backups/{id}/delete", adm, h.backupDeleteConfirm)
	route("POST /backups/{id}/delete", adm, h.backupDelete)
}

// backupDelete removes the whole run (dump, snapshot, decoded copy) the backup
// belongs to. The confirmation is the browser dialog on the button, which
// names the extra risk when the newest backup of the device goes; the audit
// record says whether it did.
func (h *handler) backupDelete(w http.ResponseWriter, r *http.Request) {
	adm, _ := h.backupAdmin()
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	d, err := adm.DeleteRun(r.Context(), actor(r), model.BackupID(n))
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.backupDeleted(d)
		// A backup of a deleted device is listed on Devices -> Backups: back there.
		redirect(w, r, backupsHome(model.Backup{DeviceID: d.Device})+fmt.Sprintf("ok=deleted&n=%d", d.Runs))
	}
}

// BackupStorage is the storage totals and the backups of deleted devices on Devices -> Backups
// (design v2 N9; *backup.Manager implements it). Deps.Backups is asserted against it; without it
// the page shows no totals and no deleted devices' backups.
type BackupStorage interface {
	Usage(ctx context.Context) ([]store.BackupUsage, error)
	Orphans(ctx context.Context) ([]model.Backup, error)
}

// BackupUnreferenced lists 0.17-named backup files in old numeric folders that no
// backup entry points to (*backup.Manager implements it). Optional: without it the
// section is simply absent.
type BackupUnreferenced interface {
	Unreferenced(ctx context.Context) ([]backup.UnreferencedFile, error)
}

// withoutBackup is the one definition of "device without a backup": a live device (Device 0 is
// the deleted devices' row) with no backup run. The Backups page's tile counts with it and the
// device list's "No backup" chip filters with it, so the click shows what the tile counted (#381).
func withoutBackup(u store.BackupUsage) bool { return u.Device != 0 && u.Runs == 0 }

// annotateBackups sets deviceRow.NoBackup, the fact the "No backup" chip filters on. Without a
// backup service nothing is marked (and the chip is not offered, see devicesPageData).
func (h *handler) annotateBackups(ctx context.Context, rows []deviceRow) error {
	bs, ok := h.d.Backups.(BackupStorage)
	if !ok {
		return nil
	}
	usage, err := bs.Usage(ctx)
	if err != nil {
		return err
	}
	none := map[model.DeviceID]bool{}
	for _, u := range usage {
		if withoutBackup(u) {
			none[u.Device] = true
		}
	}
	for i := range rows {
		rows[i].NoBackup = none[rows[i].Device.ID]
	}
	return nil
}

// sysStorage is the storage totals and the backups of deleted devices on Devices -> Backups.
type sysStorage struct {
	Runs, Kept   int
	Size         string
	Orphans      []sysOrphan
	Unreferenced []sysUnreferenced // files no backup entry references (#205); read-only
	CanKeep      bool              // the Keep route exists
	CanDelete    bool              // the Delete route exists
}

// sysOrphan is one backup run of a deleted device (N2: deleted with backups=keep).
type sysOrphan struct {
	ID         model.BackupID // the run's dump, or its snapshot when it has none
	Name, MAC  string
	Taken      time.Time
	Size       string
	Kept       bool
	Missing    bool // the file was deleted by hand: no Download, the row can be deleted
	Download   string
	DeleteText string // empty: no Delete (no dump, or deleting is not possible)
}

// sysUnreferenced is one file of the "Unreferenced files" section.
type sysUnreferenced struct {
	Path, Size string
	Modified   time.Time
}

// sysStorageOf reads the storage summary and the orphans. nil when the backup service
// cannot give them; an error of the store fails the page (a wrong summary is worse).
func (h *handler) sysStorageOf(r *http.Request, base string) (*sysStorage, error) {
	bs, ok := h.d.Backups.(BackupStorage)
	if !ok {
		return nil, nil
	}
	usage, err := bs.Usage(r.Context())
	if err != nil {
		return nil, err
	}
	orphans, err := bs.Orphans(r.Context())
	if err != nil {
		return nil, err
	}
	_, canDelete := h.backupAdmin()
	_, canKeep := h.d.Backups.(BackupKeeper)
	st := &sysStorage{CanKeep: canKeep, CanDelete: canDelete}
	var total int64
	for _, u := range usage {
		st.Runs += u.Runs
		st.Kept += u.Kept
		total += u.Bytes // the deleted devices count in the totals; their runs are listed apart
	}
	st.Size = humanBytes(total)
	if ul, ok := h.d.Backups.(BackupUnreferenced); ok {
		files, err := ul.Unreferenced(r.Context())
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			st.Unreferenced = append(st.Unreferenced, sysUnreferenced{Path: f.Path, Size: humanBytes(f.Size), Modified: f.ModTime})
		}
	}
	st.Orphans = orphanRuns(orphans, base, canDelete, h.fileMissing)
	return st, nil
}

// orphanRuns groups the files of the deleted devices into runs: per device (MAC and name)
// and time, the way a device's Backups tab groups its own (backupRuns), newest first.
func orphanRuns(list []model.Backup, base string, canDelete bool, missing func(model.Backup) bool) []sysOrphan {
	type owner struct{ mac, name string }
	var order []owner
	byOwner := map[owner][]model.Backup{}
	for _, b := range list {
		o := owner{b.DeviceMAC, b.DeviceName}
		if _, seen := byOwner[o]; !seen {
			order = append(order, o)
		}
		byOwner[o] = append(byOwner[o], b)
	}
	var out []sysOrphan
	for _, o := range order {
		for _, run := range backupRuns(byOwner[o]) {
			b := run.Backup
			row := sysOrphan{ID: b.ID, Name: b.DeviceName, MAC: b.DeviceMAC, Taken: b.TakenAt, Size: humanBytes(b.Size),
				Kept: b.Kept, Download: fmt.Sprintf("%s/backups/%d", base, b.ID)}
			if missing(b) {
				row.Missing, row.Download = true, ""
			}
			if canDelete && b.Kind == model.BackupConfigDump {
				row.DeleteText = backupData{}.DeleteText(b)
			}
			out = append(out, row)
		}
	}
	slices.SortStableFunc(out, func(a, b sysOrphan) int { return b.Taken.Compare(a.Taken) })
	return out
}

// orphanName is the deleted device's name, or a placeholder for a backup from before names
// were copied (migration 0022 copied them; a device without a name had none).
func orphanName(o sysOrphan) string {
	if o.Name == "" {
		return "Deleted device"
	}
	return o.Name
}

// storeDate is a last backup short enough for the storage table's column: "23 Oct, 05:45"
// this year, "23 Oct 2025" before (the full time is in the cell's title).
func storeDate(now, t time.Time) string {
	t = t.Local()
	if t.Year() == now.Year() {
		return t.Format("2 Jan, 15:04")
	}
	return t.Format("2 Jan 2006")
}
