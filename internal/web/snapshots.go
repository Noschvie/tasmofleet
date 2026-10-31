package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
)

// SnapshotService is the database snapshots of Settings -> Backups (*snapshot.Manager implements
// it). Optional: without it the "TasmoFleet database" card and its routes are absent.
type SnapshotService interface {
	Export(ctx context.Context, withKey bool) (*snapshot.Export, error)
	List() ([]snapshot.Info, error)
	Open(name string) (*os.File, snapshot.Info, error)
	Delete(name string) error
	Status() snapshot.Status
	Dir() string
}

const snapPath = sysPath + "/snapshots"

func (h *handler) registerSnapshots(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Snapshots == nil {
		return
	}
	route("POST "+sysPath+"/snapshot", adm, h.snapDownload)
	route("GET "+snapPath+"/{name}/download", adm, h.snapStoredDownload)
	route("GET "+snapPath+"/{name}/delete", adm, h.snapDeleteConfirm)
	route("POST "+snapPath+"/{name}/delete", adm, h.snapDelete)
}

// snapStepUp asks for the password (local account) or a new sign-in at the identity provider
// before a snapshot leaves the server: it holds every user's password hash and the encrypted
// device and broker passwords. It is the same confirmation as for the sign-in settings (five
// minutes per session, package auth); a proxy-header principal, who has nothing to confirm with,
// passes as everywhere else (needsStepUp). It answers the page with the dialog open and returns
// false when the confirmation is due.
func (h *handler) snapStepUp(w http.ResponseWriter, r *http.Request, what string) bool {
	if !h.needsStepUp(r) {
		return true
	}
	p := h.sysData(r, nil, nil)
	p.Active, p.NeedStepUp, p.StepUpWhat = "backups", true, what
	h.sysRender(w, r, http.StatusForbidden, p)
	return false
}

// snapDownload makes a consistent copy of the live database for one download; with include_key
// the response is a .tar of the database and secret.key. The copy exists only for the response.
func (h *handler) snapDownload(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k := range r.PostForm {
		if k != "csrf" && k != "include_key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}
	withKey := r.PostForm.Get("include_key") == "1"
	if !h.snapStepUp(w, r, "Downloading a snapshot of the database") {
		return
	}
	ex, err := h.d.Snapshots.Export(r.Context(), withKey)
	if err != nil {
		h.snapAudit(r, "snapshot.download", "failed: "+snapReason(err), map[string]any{"with_key": withKey})
		h.fail(w, r, err)
		return
	}
	defer ex.Close() //nolint:errcheck // the scratch copy is removed best effort; the response is out
	h.snapAudit(r, "snapshot.download", "ok", map[string]any{"with_key": withKey, "file": ex.Name, "bytes": ex.Size})
	snapHeaders(w, ex.Name, ex.ContentType)
	http.ServeContent(w, r, ex.Name, ex.Taken, ex.File)
}

// snapStoredDownload sends a stored snapshot. The name is looked up in the snapshots folder; it
// is never used as a path (package snapshot).
func (h *handler) snapStoredDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !snapshot.ValidName(name) {
		h.notFound(w, r)
		return
	}
	if !h.snapStepUp(w, r, "Downloading a snapshot of the database") {
		return
	}
	f, in, err := h.d.Snapshots.Open(name)
	if errors.Is(err, snapshot.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close() //nolint:errcheck // read-only handle
	h.snapAudit(r, "snapshot.download", "ok", map[string]any{"with_key": false, "file": in.Name, "bytes": in.Size, "stored": true})
	snapHeaders(w, in.Name, "application/vnd.sqlite3")
	http.ServeContent(w, r, in.Name, in.Taken, f)
}

// snapExists says whether name is a stored snapshot.
func (h *handler) snapExists(name string) bool {
	list, err := h.d.Snapshots.List()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(list, func(in snapshot.Info) bool { return in.Name == name })
}

// snapDeleteConfirm is the confirmation of Delete as a page (no JavaScript).
func (h *handler) snapDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.snapExists(name) {
		h.notFound(w, r)
		return
	}
	h.renderActionConfirm(w, r, "system", actionConfirm{
		Title: "Delete this snapshot?", Heading: "Delete this snapshot", Text: snapDeleteText(name),
		Crumbs: []confirmLink{{"/settings/system", "Settings"}, {sysPath + "?tab=backups", "Backups"}}, Last: "Delete snapshot",
		Action: snapPath + "/" + name + "/delete", Verb: "Delete snapshot", CancelHref: sysPath + "?tab=backups", CancelLabel: "Cancel",
	})
}

// snapDelete removes one stored snapshot. Like deleting a device backup it acts at once; the
// confirmation is the dialog on the button.
func (h *handler) snapDelete(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	err := h.d.Snapshots.Delete(name)
	switch {
	case errors.Is(err, snapshot.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.snapAudit(r, "snapshot.delete", "failed: "+snapReason(err), map[string]any{"file": name})
		h.fail(w, r, err)
	default:
		h.snapAudit(r, "snapshot.delete", "ok", map[string]any{"file": name})
		redirect(w, r, sysPath+"?tab=backups&ok=snapshot-deleted")
	}
}

// snapHeaders marks the response as a file the browser saves and never caches or sniffs.
func snapHeaders(w http.ResponseWriter, name, ctype string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

// snapAudit records a download or delete. Nothing of the file's content, and no key, is in it.
func (h *handler) snapAudit(r *http.Request, action, result string, detail map[string]any) {
	if h.d.Recorder == nil {
		return
	}
	d := ""
	if b, err := json.Marshal(detail); err == nil {
		d = string(b)
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{
		At: h.d.Clock.Now(), Actor: actor(r), Action: action, Target: "database", Detail: d, Result: result,
		RemoteAddr: httpx.ClientIP(r.Context()).String(),
	})
}

func snapReason(err error) string {
	s := err.Error()
	return s[:min(len(s), 120)]
}

// sysDB is the "TasmoFleet database" card and the status of the snapshot schedule card.
type sysDB struct {
	LiveSize  string // about how big a download is; empty when unknown
	Hint      string // the line next to the download button
	Dir       string
	Files     []sysSnapshot
	Kept      int // SNAPSHOT_KEEP
	On        bool
	Status    sysSnapStatus
	CanDelete bool
}

// sysSnapshot is one stored snapshot.
type sysSnapshot struct {
	Name, Size string
	Taken      time.Time
	Schema     int
	Latest     bool
	Download   string
	Delete     string
}

// sysSnapStatus is the last scheduled attempt as the cards show it. State is "none" (schedule on,
// nothing yet), "ok" or "fail".
type sysSnapStatus struct {
	State   string
	Taken   time.Time // the last good snapshot
	Attempt time.Time // the last failed attempt
	Next    time.Time // the next run; zero when it cannot be told
	File    string
	Reason  string
	Count   int
}

// sysDBOf reads the stored snapshots and the schedule's state for the Backups tab.
func (h *handler) sysDBOf(r *http.Request, base string) (*sysDB, error) {
	s := h.d.Snapshots
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	db := &sysDB{Dir: s.Dir(), CanDelete: true}
	if h.d.Database != nil {
		if info, err := h.d.Database.DBInfo(r.Context()); err == nil && info.Size > 0 {
			db.LiveSize = humanBytes(info.Size)
		}
	}
	db.Hint = "A consistent copy of the live database, made while TasmoFleet runs"
	if db.LiveSize != "" {
		db.Hint += " (about " + db.LiveSize + ")"
	}
	db.Hint += ". You are asked for your password first."
	db.Kept, _ = strconv.Atoi(h.storedValue("SNAPSHOT_KEEP"))
	every, _ := time.ParseDuration(h.storedValue("SNAPSHOT_INTERVAL"))
	db.On = every > 0
	for i, in := range list {
		db.Files = append(db.Files, sysSnapshot{
			Name: in.Name, Size: humanBytes(in.Size), Taken: in.Taken, Schema: in.Schema, Latest: i == 0,
			Download: base + snapPath + "/" + in.Name + "/download",
			Delete:   base + snapPath + "/" + in.Name + "/delete",
		})
	}
	st := s.Status()
	db.Status = sysSnapStatus{State: "none", Taken: st.LastGood, Count: len(list)}
	switch {
	case st.Failed:
		db.Status.State, db.Status.Attempt, db.Status.File, db.Status.Reason = "fail", st.Attempted, st.File, st.Reason
		if every > 0 {
			db.Status.Next = st.Attempted.Add(every)
		}
	case !st.LastGood.IsZero():
		db.Status.State = "ok"
		if every > 0 {
			db.Status.Next = st.LastGood.Add(every)
		}
	}
	return db, nil
}
