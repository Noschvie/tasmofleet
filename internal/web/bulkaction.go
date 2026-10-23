package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// Bulk "Back up" and "Restart": the devices selected in the list are the scope
// (never "all" by implication), offline devices are left out and listed, and the work
// runs as a transaction (per-device result, "Retry failed", audit) like Send
// command. A backup is harmless and starts at once; a restart takes the devices
// offline for a moment, so it has one confirmation that names that. The
// restart is the console's own "Restart 1": same command policy, device lock
// and audit as the single-device restart, because it goes through the same
// command transaction as Send command.

// BackupBatch starts a "Back up" transaction (ota.Engine.SubmitBackup, behind
// the device locks). Devices it cannot take are returned with the reason.
type BackupBatch interface {
	Submit(ctx context.Context, actor string, ids []model.DeviceID, opt ota.BackupOptions) (model.BatchID, []ota.Refusal, error)
}

func (h *handler) registerBulkAction(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.BackupBatch != nil {
		route("POST /devices/bulk/backup", adm, h.bulkBackup)
	}
	if h.d.CommandBatch != nil && h.d.DeviceCmd != nil {
		route("POST /devices/bulk/restart", adm, h.bulkRestartPage)
		route("POST /devices/bulk/restart/run", adm, h.bulkCommandRun) // same token, same transaction as Send command
	}
	if h.d.OTA != nil && (h.d.BackupBatch != nil || h.d.CommandBatch != nil) {
		route("GET /devices/bulk/result/{batch}", adm, h.bulkResult)
	}
}

const restartCommand = "Restart 1"

// leftOut lists the devices the engine refused, each with its reason.
func (h *handler) leftOut(r *http.Request, skipped []ota.Refusal) []leftItem {
	var out []leftItem
	for _, x := range skipped {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		out = append(out, leftItem{Name: d.Name, Reason: humanReason(x.Reason)})
	}
	return out
}

// bulkBackup shows the scope page (the bar's post, a "×" on a row), and with run=1 starts the
// backups of the online devices at once: a backup is harmless, so the scope page is the only
// step. The result page follows the transaction.
func (h *handler) bulkBackup(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	sv, ready, err := h.bulkScope(r, bulkBackup, "/devices/bulk/backup", "/devices/bulk/backup")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// "Keep these backups" and "Label" (N1); a form without them takes plain backups.
	opt := ota.BackupOptions{Keep: r.PostFormValue("keep") == "1", Label: r.PostFormValue("label")}
	sv.Keep, sv.Label = opt.Keep, opt.Label
	if r.PostFormValue("run") != "1" || dropping(r) || sv.Error != "" {
		h.showBulkScope(w, r, sv)
		return
	}
	p, _ := auth.FromContext(r.Context())
	batch, skipped, err := h.d.BackupBatch.Submit(ota.WithActorKind(r.Context(), p.Method), actor(r), ready, opt)
	switch {
	case errors.Is(err, ota.ErrNothingToFix):
		h.message(w, r, http.StatusConflict, h.view(r, "Nothing backed up", "devices", messageData{Text: "No backup was started.", Left: h.leftOut(r, skipped)}))
	case err != nil:
		h.fail(w, r, err)
	case len(skipped) == 0:
		redirect(w, r, resultURL(batch, h.formIDs(r)))
	default:
		// The transaction holds only the devices that were started, so the refused
		// ones are listed here, on the one page that knows their reason.
		no := strconv.FormatInt(int64(batch), 10)
		h.message(w, r, http.StatusOK, h.view(r, "Backup started", "devices", messageData{
			Text: "was started. These devices were left out and not backed up:", Left: h.leftOut(r, skipped),
			LinkText: "Transaction #" + no, Link: "/transactions/" + no, LinkLead: true,
		}))
	}
}

// bulkRestartPage shows the scope page of a restart. It is the confirmation: it names the
// consequence, and its run button posts a server-held token that carries "Restart 1" and the
// online devices, so the run restarts exactly what the page showed. Nothing is sent here.
func (h *handler) bulkRestartPage(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	sv, ready, err := h.bulkScope(r, bulkRestart, "/devices/bulk/restart", "/devices/bulk/restart/run")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sv.Parallel = 3 // the prototype's default "Devices at a time"; a redraw keeps the choice
	if n, err := strconv.Atoi(r.PostFormValue("parallel")); err == nil && slices.Contains(bulkParallels3, n) {
		sv.Parallel = n
	}
	p, _ := auth.FromContext(r.Context())
	if _, err := cmdpolicy.Authorize(restartCommand, p.Role); err != nil {
		_, sv.Error = consoleError(err)
	}
	if sv.Error == "" {
		sv.Token = h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), restartCommand, ready)
	}
	h.showBulkScope(w, r, sv)
}
