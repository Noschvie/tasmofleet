package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// deviceConfirmData is the confirmation page of a device restart: the no-JS fallback of the
// Restart dialogs on the device list and the device page, with the same wording.
type deviceConfirmData struct {
	Device model.Device
}

// devDeleteView is the delete page of a device (N2, screen "devdelete").
type devDeleteView struct {
	Name, Address, Meta string // Meta: " · ESP32 · Tasmota 14.3.0" after the address
	Runs                int    // backup runs, as the Backups tab counts them
	Summary             string // "2 kept, 16.1 KiB, newest 22 Oct 2026, 09:41"
	Rows                []devDeleteRow
	Choice              string // the posted choice on a refused post; "" = Keep (the reference's default)
	ChoiceErr, AckErr   bool
	Action, CSRF        string
	CancelHref          string
}

// devDeleteRow is one backup run on the delete page: when, and the Backups tab's detail line.
type devDeleteRow struct{ When, Detail string }

func (h *handler) registerDeviceConfirm(route routeFunc, adm func(http.Handler) http.Handler) {
	route("GET /devices/{id}/delete", adm, h.deviceDeleteConfirm)
	if h.d.DeviceCmd != nil {
		route("GET /devices/{id}/restart", adm, h.deviceRestartConfirm)
		route("POST /devices/{id}/restart", adm, h.deviceRestart)
	}
}

func (h *handler) deviceDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	h.renderDeviceDelete(w, r, http.StatusOK, d, devDeleteView{})
}

// renderDeviceDelete shows the delete page of d. p carries a refused post's choice and field
// errors; the rest is filled here: the backup runs newest first (what Keep leaves as orphans
// and Delete removes), their count and total size.
func (h *handler) renderDeviceDelete(w http.ResponseWriter, r *http.Request, status int, d model.Device, p devDeleteView) {
	list, err := h.d.Backups.List(r.Context(), d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, "Delete device", "devices", nil)
	devPath := "/devices/" + strconv.FormatInt(int64(d.ID), 10)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: d.Name, Path: devPath}, {Label: "Delete"}}
	p.Name, p.Address = d.Name, d.Address
	if d.Port != 0 {
		p.Address += ":" + strconv.Itoa(d.Port)
	}
	if d.Identity.Family != "" {
		p.Meta += " · " + chipText(d.Identity.Family)
	}
	if d.Identity.Version != "" {
		p.Meta += " · Tasmota " + d.Identity.Version
	}
	var total int64
	kept := 0
	for _, b := range list {
		total += b.Size // Delete removes every file, decoded copies included
	}
	runs := backupRuns(list)
	for _, run := range runs {
		if run.Kept {
			kept++
		}
		p.Rows = append(p.Rows, devDeleteRow{When: shortDate(run.TakenAt), Detail: deleteRowDetail(run)})
	}
	p.Runs = len(runs)
	if p.Runs > 0 {
		var parts []string
		if kept > 0 {
			parts = append(parts, strconv.Itoa(kept)+" kept")
		}
		parts = append(parts, humanBytes(total), "newest "+shortDate(runs[0].TakenAt))
		p.Summary = strings.Join(parts, ", ")
	}
	p.Action, p.CSRF, p.CancelHref = v.Base+devPath+"/delete", v.CSRF, v.Base+devPath
	h.show(w, r, status, "device_delete", v, deviceDeletePage(p))
}

func (h *handler) deviceRestartConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	h.renderDeviceConfirm(w, r, deviceConfirmData{Device: d})
}

// renderDeviceConfirm shows the confirmation page of a restart (ui.ConfirmPage) with the
// dialog's wording: Restart sends Restart 1.
func (h *handler) renderDeviceConfirm(w http.ResponseWriter, r *http.Request, c deviceConfirmData) {
	dev := c.Device
	title := "Restart " + dev.Name + "?"
	v := h.view(r, strings.TrimSuffix(title, "?"), "devices", nil)
	devPath := "/devices/" + strconv.FormatInt(int64(dev.ID), 10)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: dev.Name, Path: devPath}, {Label: "Restart"}}
	addr := dev.Address
	if dev.Port != 0 {
		addr += ":" + strconv.Itoa(dev.Port)
	}
	h.show(w, r, http.StatusOK, "device_confirm", v, ui.ConfirmPage(ui.ConfirmData{
		Title: title, Heading: "Restart device",
		Text:   "Sends Restart 1 to " + dev.Name + ". The device is offline for a few seconds while it reboots, and its health shows offline until it reports again.",
		Facts:  []ui.KVRow{{Key: "Address", Value: addr, Mono: true}},
		Action: v.Base + devPath + "/restart", CSRF: v.CSRF, Verb: "Restart device",
		CancelHref: v.Base + devPath, CancelLabel: "Cancel",
	}))
}

// deviceRestart sends "Restart 1" through the console service: the same
// policy check, device lock and audit record as a command typed in the
// console. The confirmation (dialog or page) is the caller's job; the
// server only accepts a POST, which the CSRF check already protects.
func (h *handler) deviceRestart(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	p, _ := auth.FromContext(r.Context())
	if _, err := h.d.DeviceCmd.Run(r.Context(), p, d.ID, "Restart 1"); err != nil {
		status, msg := consoleError(err)
		if status == http.StatusInternalServerError {
			h.log.ErrorContext(r.Context(), "device restart failed", "device", d.ID, "err", err)
		}
		h.message(w, r, status, h.view(r, "Restart failed", "devices", msg))
		return
	}
	back := "/devices/" + r.PathValue("id")
	if r.PostFormValue("from") == "list" {
		back = "/devices"
	}
	redirect(w, r, back+"?ok=restarted")
}

// deleteRowDetail is a backup's line on the delete page, as in the reference: what started it,
// the firmware and the size; Kept first, since Keep / Delete decides about those too. The
// snapshot notes of the Backups tab are left out: they do not matter for the decision.
func deleteRowDetail(r backupRun) string {
	var parts []string
	if r.Kept {
		parts = append(parts, "Kept")
	}
	if t := backupTriggerText(r.Trigger, r.TriggerRef); t != "" {
		parts = append(parts, t)
	}
	if r.Version != "" {
		parts = append(parts, r.Version)
	}
	return strings.Join(append(parts, humanBytes(r.Size)), " · ")
}
