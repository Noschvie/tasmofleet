package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// firmwareState is the handler's firmware-page state: the one prefetcher, so
// that only one download run is active at a time across all visitors.
type firmwareState struct {
	once sync.Once
	pf   *firmware.Prefetcher
}

func (h *handler) prefetcher() *firmware.Prefetcher {
	h.fw.once.Do(func() { h.fw.pf = firmware.NewPrefetcher(h.d.Firmware, h.d.Clock) })
	return h.fw.pf
}

type routeFunc = func(string, func(http.Handler) http.Handler, http.HandlerFunc)

// registerFirmwareManage adds the admin-only cache management and prefetch
// routes. Deleting files and starting downloads change the cache the OTA
// engine relies on, so they need the admin role.
func (h *handler) registerFirmwareManage(route routeFunc, adm func(http.Handler) http.Handler) {
	route("GET /firmware/artifacts/{id}/delete", adm, h.firmwareDeleteConfirm)
	route("POST /firmware/artifacts/{id}/delete", adm, h.firmwareDelete)
	route("POST /firmware/delete-selected", adm, h.firmwareDeleteSelected)
	route("POST /firmware/prefetch", adm, h.firmwarePrefetch)
	route("GET /firmware/prefetch/{id}", adm, h.firmwarePrefetchPage)
	route("GET /firmware/prefetch/{id}/stream", adm, h.firmwarePrefetchStream)
}

// record writes an audit event (nil recorder = tests without audit).
func (h *handler) recordFirmware(r *http.Request, action, target string, detail map[string]any, result string) {
	if h.d.Recorder == nil {
		return
	}
	d, err := json.Marshal(detail)
	if err != nil {
		d = []byte("{}")
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{
		At: h.d.Clock.Now(), Actor: actor(r), Action: action, Target: target, Detail: string(d), Result: result,
		RemoteAddr: httpx.ClientIP(r.Context()).String(),
	})
}

func artifactDetail(a model.Artifact) map[string]any {
	return map[string]any{"release": a.Release, "file": a.Filename, "sha256": a.SHA256, "size": a.Size}
}

// ---- delete ----

type fwConfirm struct {
	Selected bool // delete the selected files instead of one
	Artifact artifactView
	List     []artifactView // selected: what would go
	Left     []artifactView // selected but left out: needed by a transaction, or an upload
	Size     int64
	Error    string
}

func (h *handler) renderFirmwareConfirm(w http.ResponseWriter, r *http.Request, status int, c fwConfirm) {
	title := "Delete cached file"
	if c.Selected {
		title = "Delete unused files"
	}
	v := h.view(r, title, "firmware", nil)
	v.Crumbs = []ui.Crumb{{Label: "Updates", Path: "/transactions"}, {Label: "Firmware", Path: "/firmware"}, {Label: "Delete"}}
	h.show(w, r, status, "firmware_confirm", v, fwConfirmContent(v, c))
}

func (h *handler) firmwareManager(w http.ResponseWriter) (firmware.Manager, bool) {
	m, ok := h.d.Firmware.(firmware.Manager)
	if !ok {
		http.Error(w, "deleting cached files is not supported", http.StatusNotImplemented)
	}
	return m, ok
}

func (h *handler) artifactFromPath(w http.ResponseWriter, r *http.Request) (artifactView, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		h.notFound(w, r)
		return artifactView{}, false
	}
	for _, a := range h.firmwareData(r).Artifacts {
		if a.ID == model.ArtifactID(id) {
			return a, true
		}
	}
	h.notFound(w, r)
	return artifactView{}, false
}

func (h *handler) firmwareDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.firmwareManager(w); !ok {
		return
	}
	a, ok := h.artifactFromPath(w, r)
	if !ok {
		return
	}
	c := fwConfirm{Artifact: a}
	if a.InUse {
		c.Error = h.inUseText(r.Context(), a.Jobs)
	}
	h.renderFirmwareConfirm(w, r, http.StatusOK, c)
}

// inUseText says which transactions need the file (the jobs when the batch is not known).
func (h *handler) inUseText(ctx context.Context, jobs []model.JobID) string {
	return "This file is needed by " + neededText(h.batchesOf(ctx, jobs), jobs) + " and cannot be deleted until it has ended or been cancelled."
}

func (h *handler) firmwareDelete(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	m, ok := h.firmwareManager(w)
	if !ok {
		return
	}
	view, ok := h.artifactFromPath(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != "yes" {
		h.renderFirmwareConfirm(w, r, http.StatusUnprocessableEntity, fwConfirm{Artifact: view, Error: "Confirm the deletion first."})
		return
	}
	target := fmt.Sprintf("artifact:%d", view.ID)
	a, err := m.Delete(r.Context(), view.ID)
	var busy *firmware.InUseError
	switch {
	case errors.As(err, &busy):
		h.recordFirmware(r, "firmware.delete", target, artifactDetail(view.Artifact), "refused: "+err.Error())
		view.InUse, view.Jobs = true, busy.Jobs
		h.renderFirmwareConfirm(w, r, http.StatusConflict, fwConfirm{Artifact: view, Error: h.inUseText(r.Context(), busy.Jobs)})
	case errors.Is(err, store.ErrNotFound):
		h.recordFirmware(r, "firmware.delete", target, artifactDetail(view.Artifact), "not found (already deleted)")
		h.notFound(w, r)
	case err != nil:
		h.recordFirmware(r, "firmware.delete", target, artifactDetail(view.Artifact), "error: "+err.Error())
		h.log.ErrorContext(r.Context(), "firmware delete failed", "artifact", view.ID, "err", err)
		p := h.firmwareData(r)
		p.Error = "The file could not be deleted. See the server log."
		h.showFirmware(w, r, http.StatusInternalServerError, p)
	default:
		h.recordFirmware(r, "firmware.delete", target, artifactDetail(a), "ok")
		p := h.firmwareData(r)
		p.Notice = "Deleted " + a.Release + "/" + a.Filename + " (" + humanBytes(a.Size) + ")"
		h.showFirmware(w, r, http.StatusOK, p)
	}
}

// selectedIDs reads the posted artifact ids (no duplicates, only positive numbers).
func selectedIDs(r *http.Request) []model.ArtifactID {
	var ids []model.ArtifactID
	seen := map[model.ArtifactID]bool{}
	for _, v := range r.PostForm["id"] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !seen[model.ArtifactID(n)] {
			seen[model.ArtifactID(n)] = true
			ids = append(ids, model.ArtifactID(n))
		}
	}
	return ids
}

// selectedConfirm lists what deleting the posted ids would remove, and which of
// them are left out: a file a pending or running transaction needs, and
// uploads, which are deleted one by one because they cannot be downloaded again.
func (h *handler) selectedConfirm(r *http.Request, ids []model.ArtifactID) fwConfirm {
	c := fwConfirm{Selected: true}
	want := map[model.ArtifactID]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, a := range h.firmwareData(r).Artifacts {
		switch {
		case !want[a.ID]:
		case a.Selectable:
			c.List = append(c.List, a)
			c.Size += a.Size
		default:
			c.Left = append(c.Left, a)
		}
	}
	return c
}

// firmwareDeleteSelected is the bulk delete of the cached-files table. Without
// confirm=yes it renders the confirmation (what goes, what is left out); with
// it, it deletes the confirmed ids, not whatever is unused at POST time. Each
// delete is checked again by the cache, so a file a job started to need
// meanwhile is kept, and an upload is refused here as well.
func (h *handler) firmwareDeleteSelected(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	m, ok := h.firmwareManager(w)
	if !ok {
		return
	}
	_ = r.ParseForm() // a malformed or oversized body leaves no ids, which is refused below
	ids := selectedIDs(r)
	c := h.selectedConfirm(r, ids)
	switch {
	case len(ids) == 0:
		c.Error = "Select at least one file."
		h.renderFirmwareConfirm(w, r, http.StatusUnprocessableEntity, c)
		return
	case r.PostFormValue("confirm") != "yes":
		h.renderFirmwareConfirm(w, r, http.StatusOK, c)
		return
	}
	// The confirmed list: the posted ids that are still selectable. Everything
	// else posted (needed by a transaction now, an upload, already gone) is
	// answered below without calling Delete for uploads.
	var deleted, kept, gone int
	var freed int64
	var failure error
	for _, id := range ids {
		target := fmt.Sprintf("artifact:%d", id)
		if up := slices.IndexFunc(c.Left, func(a artifactView) bool { return a.ID == id && firmware.IsCustomSource(a.Source) }); up >= 0 {
			d := artifactDetail(c.Left[up].Artifact)
			d["via"] = "delete-selected"
			h.recordFirmware(r, "firmware.delete", target, d, "refused: an uploaded image is deleted on its own")
			kept++
			continue
		}
		a, err := m.Delete(r.Context(), id)
		var busy *firmware.InUseError
		switch {
		case err == nil:
			d := artifactDetail(a)
			d["via"] = "delete-selected"
			h.recordFirmware(r, "firmware.delete", target, d, "ok")
			deleted++
			freed += a.Size
		case errors.As(err, &busy):
			d := artifactDetail(busy.Artifact)
			d["via"] = "delete-selected"
			h.recordFirmware(r, "firmware.delete", target, d, "refused: "+err.Error())
			kept++
		case errors.Is(err, store.ErrNotFound):
			h.recordFirmware(r, "firmware.delete", target, map[string]any{"via": "delete-selected"}, "not found (already deleted)")
			gone++
		default:
			h.recordFirmware(r, "firmware.delete", target, map[string]any{"via": "delete-selected"}, "error: "+err.Error())
			failure = err
		}
		if failure != nil {
			break
		}
	}
	p := h.firmwareData(r)
	status := http.StatusOK
	if failure != nil {
		h.log.ErrorContext(r.Context(), "firmware delete-selected failed", "err", failure)
		p.Error, status = "Not everything could be deleted. See the server log.", http.StatusInternalServerError
	}
	p.Notice = fmt.Sprintf("Deleted %d files (%s).", deleted, humanBytes(freed))
	if kept > 0 {
		p.Notice += fmt.Sprintf(" %d files that are needed were kept.", kept)
	}
	if gone > 0 {
		p.Notice += fmt.Sprintf(" %d were already gone.", gone)
	}
	h.showFirmware(w, r, status, p)
}

// ---- prefetch ----

type prefetchView struct {
	Base, CSRF          string
	Run                 firmware.PrefetchRun
	Running             bool
	Done, Failed, Total int
	Percent             int
}

func (h *handler) prefetchView(r *http.Request, run firmware.PrefetchRun) prefetchView {
	v := prefetchView{Base: httpBase(r), CSRF: h.d.Guard.CSRFToken(r), Run: run, Running: run.Running()}
	v.Done, v.Failed, v.Total = run.Counts()
	if v.Total > 0 {
		v.Percent = (v.Done + v.Failed) * 100 / v.Total
	}
	return v
}

// firmwarePrefetch starts a download of exactly the files the current fleet
// needs for one release. The file list is computed here from the inventory,
// never taken from the form, and every file goes through Cache.Ensure, so
// the size limit, the cross-check and the image-header check apply.
func (h *handler) firmwarePrefetch(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	release := r.PostFormValue("release")
	fail := func(status int, msg string) {
		p := h.firmwareData(r)
		p.Error = msg
		h.showFirmware(w, r, status, p)
	}
	rels, err := h.d.Firmware.Releases(r.Context())
	if err != nil {
		fail(http.StatusBadGateway, "The upstream index could not be loaded: "+safeErr(err.Error()))
		return
	}
	var rel *firmware.Release
	for i := range rels {
		if rels[i].Version == release {
			rel = &rels[i]
		}
	}
	if rel == nil {
		fail(http.StatusUnprocessableEntity, "Unknown release.")
		return
	}
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	files := firmware.FleetNeeds(*rel, devs).Files
	if len(files) == 0 {
		fail(http.StatusUnprocessableEntity, "No device in the fleet needs a file from release "+rel.Version+".")
		return
	}
	run, started, err := h.prefetcher().Start(r.Context(), actor(r), rel.Version, files)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if started {
		h.recordFirmware(r, "firmware.prefetch", "release:"+rel.Version, map[string]any{"files": files}, "ok")
	}
	redirect(w, r, "/firmware/prefetch/"+run.ID)
}

func (h *handler) firmwarePrefetchPage(w http.ResponseWriter, r *http.Request) {
	run, ok := h.prefetcher().Get(r.PathValue("id"))
	if !ok {
		h.notFound(w, r)
		return
	}
	v := h.view(r, "Prefetch "+run.Release, "firmware", nil)
	v.Crumbs = []ui.Crumb{{Label: "Updates", Path: "/transactions"}, {Label: "Firmware", Path: "/firmware"}, {Label: "Prefetch"}}
	h.show(w, r, http.StatusOK, "firmware_prefetch", v, fwPrefetchContent(h.prefetchView(r, run), h.pr.Text(run.Actor)))
}

// firmwarePrefetchStream pushes the run as an HTML fragment whenever it
// changes. The last fragment has no stream attributes, so the browser drops
// the connection instead of reconnecting.
func (h *handler) firmwarePrefetchStream(w http.ResponseWriter, r *http.Request) {
	pf := h.prefetcher()
	id := r.PathValue("id")
	if _, ok := pf.Get(id); !ok {
		h.notFound(w, r)
		return
	}
	user := actor(r)
	if !h.acquireStream(user) {
		http.Error(w, "too many open event streams", http.StatusTooManyRequests)
		return
	}
	defer h.releaseStream(user)
	checker, _ := h.d.Guard.(sessionChecker)
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	poll := time.NewTicker(300 * time.Millisecond)
	defer poll.Stop()
	lastCheck := h.d.Clock.Now()
	lastKey := ""
	prefs := h.userPrefs(r) // read once; a column change shows from the next page load on
	for {
		run, ok := pf.Get(id)
		if !ok {
			return
		}
		done, failed, _ := run.Counts()
		if key := fmt.Sprint(run.Running(), done, failed, itemStates(run)); key != lastKey {
			lastKey = key
			var frag bytes.Buffer
			if err := fwPrefetchStatus(h.prefetchView(r, run)).Render(r.Context(), &frag); err != nil {
				return
			}
			var buf bytes.Buffer
			writeSSE(&buf, "prefetch", string(applyTablePrefs(frag.Bytes(), prefs)))
			if _, err := buf.WriteTo(w); err != nil || rc.Flush() != nil {
				return
			}
			if !run.Running() {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
		}
		if now := h.d.Clock.Now(); now.Sub(lastCheck) >= h.d.SSERecheck {
			lastCheck = now
			if checker != nil {
				if p, err := checker.Principal(r.Clone(httpx.Detach(r.Context()))); err != nil || p.Role < model.RoleAdmin {
					return
				}
			}
		}
	}
}

func itemStates(run firmware.PrefetchRun) string {
	var b bytes.Buffer
	for _, it := range run.Items {
		b.WriteString(it.State[:1])
	}
	return b.String()
}
