package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The no-JavaScript pages of the dialog-only confirmations: delete a backup,
// delete a policy rule, cancel a transaction, cancel one update. Each is the
// GET twin of a POST route; opening the page changes nothing, the POST keeps
// its CSRF and role checks. The wording is the dialog's.

type confirmFact struct {
	Name, Value string
	Code        bool
	// At, when set, makes the value an instant (a <time> element time.js repaints; Value is then empty) followed by
	// Tail (" · 12.0 KiB").
	At   time.Time
	Tail string
}

type confirmField struct{ Name, Value string }

type actionConfirm struct {
	Title, Text, Warning    string
	TextStamp               ui.Stamped // when its instant is set, the text instead of Text
	Crumbs                  []ui.Crumb // steps between the page's sub-navigation entry (Trail) and the current page
	Last                    string     // the current page in the trail
	Facts                   []confirmFact
	Action                  string // POST path, without the base
	Fields                  []confirmField
	Ack                     string // a required checkbox with this label
	Verb                    string
	CancelHref, CancelLabel string
	NoBusy                  bool // the answer is a file download (no next page)
}

// words fills the title, text, verb and the dismissing button from the one builder dialog and page share.
func (c actionConfirm) words(w confirmWords) actionConfirm {
	c.Title, c.Text, c.Verb = w.Title, w.Text, w.Verb
	c.CancelLabel = "Cancel"
	if w.Dismiss != "" {
		c.CancelLabel = w.Dismiss
	}
	return c
}

// renderActionConfirm shows the confirmation page (ui.ConfirmPage): the dialog's look, the
// trail (its group and entry come from nav, see ui.ShellData.Trail) and the page itself as the last crumb. Every action here removes or stops
// something, so the verb is in the danger tone.
func (h *handler) renderActionConfirm(w http.ResponseWriter, r *http.Request, nav string, c actionConfirm) {
	v := h.view(r, c.Title, nav, nil)
	v.Crumbs = append(v.Crumbs, c.Crumbs...)
	v.Crumbs = append(v.Crumbs, ui.Crumb{Label: c.Last})
	d := ui.ConfirmData{
		Title: c.Title, Text: c.Text, TextStamp: c.TextStamp, Warning: c.Warning,
		Action: v.Base + c.Action, CSRF: v.CSRF, Verb: c.Verb, Danger: true, Ack: c.Ack,
		CancelHref: v.Base + c.CancelHref, CancelLabel: c.CancelLabel, NoBusy: c.NoBusy,
	}
	for _, f := range c.Facts {
		d.Facts = append(d.Facts, ui.KVRow{Key: f.Name, Value: f.Value, Mono: f.Code, At: f.At, Tail: f.Tail})
	}
	for _, f := range c.Fields {
		d.Fields = append(d.Fields, ui.KVRow{Key: f.Name, Value: f.Value})
	}
	h.show(w, r, http.StatusOK, "action_confirm", v, ui.ConfirmPage(d))
}

func (h *handler) backupDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	_, b, err := h.d.Backups.Open(r.Context(), model.BackupID(n))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	// A backup whose file is gone is exactly what the admin must be able to remove.
	if err != nil && !errors.Is(err, backup.ErrFileMissing) {
		h.fail(w, r, err)
		return
	}
	// Only a dump has the delete control (it takes the whole run with it).
	if b.Kind != "dmp" {
		h.notFound(w, r)
		return
	}
	back := strings.TrimRight(backupsHome(b), "?&") // a deleted device's backup: Devices -> Backups
	// A device's backup is a page of the device (Devices / kitchen / Backups / Delete); a deleted device's backup
	// lives on the fleet Backups tab (Devices / Backups / Delete), so the trail and the lit tab agree (#1000).
	nav, crumbs := "backups", []ui.Crumb(nil)
	if b.DeviceID != 0 {
		dev, err := h.d.Inventory.Get(r.Context(), b.DeviceID)
		switch {
		case err == nil:
			nav, crumbs = "device", []ui.Crumb{{Label: dev.Name, Path: devPath(dev.ID, "")}, {Label: "Backups", Path: back}}
		case errors.Is(err, store.ErrNotFound): // deleted since the backup was read: the page reads as a deleted device's backup
		default:
			h.fail(w, r, err)
			return
		}
	}
	c := actionConfirm{
		Title:     "Delete this backup?",
		TextStamp: backupData{}.DeleteWords(b),
		Facts: []confirmFact{
			{Name: "Taken", At: b.TakenAt},
			{Name: "Firmware", Value: b.Version},
		},
		Crumbs: crumbs,
		Last:   "Delete",
		Action: fmt.Sprintf("/backups/%d/delete", b.ID), Verb: "Delete backup",
		CancelHref: back, CancelLabel: "Cancel",
	}
	if b.DeviceID != 0 {
		if list, err := h.d.Backups.List(r.Context(), b.DeviceID); err == nil && backup.IsNewestRun(list, b) {
			c.Warning = "This is the newest backup of this device. Afterwards you can only restore the device to an older state."
		}
	}
	h.renderActionConfirm(w, r, nav, c)
}

func (h *handler) rulesDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !policy.RuleIDRe.MatchString(id) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n := indexOfRule(st.Specs, id)
	if n < 0 {
		h.notFound(w, r)
		return
	}
	s := st.Specs[n]
	// the dialog's words: the device count comes from the same evaluation as the rule page's dialog
	stat, err := h.ruleStats(r.Context(), ruleList{Specs: st.Specs, Version: st.VersionID}, s, n)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.renderActionConfirm(w, r, "policy", actionConfirm{
		Facts: []confirmFact{
			{Name: "Rule", Value: ruleName(s)},
			{Name: "Expected", Value: wantText(s.Command, s.Want), Code: true},
		},
		Last:       "Delete",
		Action:     rulesPath + "/delete",
		Fields:     []confirmField{{"id", id}, {"version", strconv.FormatInt(st.VersionID, 10)}},
		CancelHref: rulesPath + "/" + id,
	}.words(ruleDeleteWords(ruleName(s), s.Command, stat.Applies(), s.Disabled)))
}

func (h *handler) batchCancelConfirm(w http.ResponseWriter, r *http.Request) {
	p, ok := h.batchData(w, r)
	if !ok {
		return
	}
	if len(p.Jobs) > 0 && !h.needAdminFor(w, r, p.Jobs[0].Job) {
		return
	}
	back := "/transactions/" + strconv.FormatInt(int64(p.Batch), 10)
	if !p.Active {
		http.Error(w, "transaction is not running", http.StatusConflict)
		return
	}
	if p.Planned > 0 && p.Running+p.Queued+p.Done == 0 { // a plan that has not started: nothing runs, nothing changes
		first := p.Jobs[0].Job
		h.renderActionConfirm(w, r, "transactions", actionConfirm{
			Crumbs: []ui.Crumb{{Label: p.Batch.TxID(), Path: back}},
			Last:   "Cancel",
			Action: back + "/cancel", CancelHref: back,
		}.words(cancelScheduleWords(p.Batch.TxID(), planCancelText(txTitle(first, p.Planned)+" ("+plural(p.Planned, "device")+")", first.StartAt))))
		return
	}
	// the same words as the page's dialog (txnBody)
	h.renderActionConfirm(w, r, "transactions", actionConfirm{
		Crumbs: []ui.Crumb{{Label: p.Batch.TxID(), Path: back}},
		Last:   "Cancel",
		Action: back + "/cancel", CancelHref: back,
	}.words(cancelRemainingWords(p.Batch.TxID(), cancelText(p.Queued))))
}

func (h *handler) jobCancelConfirm(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	j, err := h.d.Jobs.GetJob(r.Context(), model.JobID(n))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.needAdminFor(w, r, j) {
		return
	}
	if j.State.Terminal() {
		http.Error(w, "job is not active", http.StatusConflict)
		return
	}
	name := fmt.Sprintf("device %d", j.DeviceID)
	if d, err := h.d.Inventory.Get(r.Context(), j.DeviceID); err == nil {
		name = d.Name
	}
	back := "/transactions/" + strconv.FormatInt(int64(j.BatchID), 10)
	if j.State == model.JobScheduled {
		h.renderActionConfirm(w, r, "transactions", actionConfirm{
			Crumbs: []ui.Crumb{{Label: j.BatchID.TxID(), Path: back}},
			Last:   "Cancel update",
			Action: fmt.Sprintf("/jobs/%d/cancel", j.ID), CancelHref: back,
		}.words(cancelUpdateWords(name, planCancelText("The update of "+name, j.StartAt), true)))
		return
	}
	h.renderActionConfirm(w, r, "transactions", actionConfirm{
		Crumbs: []ui.Crumb{{Label: j.BatchID.TxID(), Path: back}},
		Last:   "Cancel update",
		Action: fmt.Sprintf("/jobs/%d/cancel", j.ID), CancelHref: back,
	}.words(cancelUpdateWords(name, jobRunningCancelText, false)))
}
