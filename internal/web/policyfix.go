package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// PolicyFix starts a "Make compliant" transaction (ota.Engine.SubmitPolicy,
// behind the device locks). Devices it cannot take are returned with the
// reason.
type PolicyFix interface {
	Submit(ctx context.Context, actor string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error)
}

func (h *handler) registerPolicyFix(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Policy == nil || h.d.PolicyFix == nil {
		return
	}
	route("POST /policy/fix", adm, h.policyFixPreview)
	route("POST /policy/fix/apply", adm, h.policyFixApply)
}

type fixCmd struct {
	Label, Text, Have, Want, Note string
}

type fixSkipView struct{ Label, Reason string }

type fixRow struct {
	Device  model.Device
	Cmds    []fixCmd
	Skipped []fixSkipView
	// Blocked is why the device is not touched at all ("" = it is).
	Blocked string
}

type fixPage struct {
	Rows     []fixRow
	Ready    int // devices that get commands
	Commands int
	IDs      []model.DeviceID
	Digest   string
	Stale    bool // the values changed since the preview the admin saw
}

type fixStarted struct {
	Batch   model.BatchID
	Skipped []fixSkipView // device name and reason
}

// activeJobStates are the states in which a job holds its device.
var activeJobStates = []model.JobState{
	model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobUpgradeIssued,
	model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower, model.JobApplying,
}

// fixSelection reads which devices the admin picked: one ("only"), every
// deviating one ("all") or the ticked ones ("device").
func fixSelection(r *http.Request, rep PolicyReport) []model.DeviceID {
	parse := func(vals []string) []model.DeviceID {
		var out []model.DeviceID
		seen := map[model.DeviceID]bool{}
		for _, v := range vals {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !seen[model.DeviceID(n)] {
				seen[model.DeviceID(n)] = true
				out = append(out, model.DeviceID(n))
			}
		}
		return out
	}
	switch {
	case r.PostForm.Get("only") != "":
		return parse(r.PostForm["only"][:1])
	case r.PostForm.Get("all") != "":
		var out []model.DeviceID
		seen := map[model.DeviceID]bool{}
		for _, f := range rep.Findings {
			if !f.Unknown && !advisory(f) && !seen[f.DeviceID] {
				seen[f.DeviceID] = true
				out = append(out, f.DeviceID)
			}
		}
		return out
	}
	return parse(r.PostForm["device"])
}

// fixRows turns the report's findings for the picked devices into the plan the
// admin reviews. Only Required rules are included (an Advisory rule is a hint, design v2
// N3) and only values read live count (see policy.PlanFix); a device with an active job is
// listed as not touched.
func (h *handler) fixRows(ctx context.Context, rep PolicyReport, ids []model.DeviceID) []fixRow {
	live := func(id model.DeviceID, key string) bool { return rep.Provenance[id][key].Kind == "live" }
	pick := map[model.DeviceID]bool{}
	for _, id := range ids {
		pick[id] = true
	}
	var findings []policy.Finding
	for _, f := range rep.Findings {
		if pick[f.DeviceID] && !advisory(f) {
			findings = append(findings, f)
		}
	}
	plans := map[model.DeviceID]policy.FixPlan{}
	for _, p := range policy.PlanFix(findings, live) {
		plans[p.DeviceID] = p
	}
	now := h.d.Clock.Now()
	var rows []fixRow
	for _, d := range rep.Devices {
		if !pick[d.ID] {
			continue
		}
		row := fixRow{Device: d}
		p := plans[d.ID]
		for _, c := range p.Commands {
			cmd := fixCmd{Label: settings.Label(c.Command), Text: c.Text, Have: c.Have, Want: c.Want}
			if src, ok := rep.Provenance[d.ID][strings.ToLower(c.Command)]; ok {
				cmd.Note = sourceNote(src, now)
			}
			row.Cmds = append(row.Cmds, cmd)
		}
		// Two rules for one setting (a different expected value each) skip with the same words:
		// say it once, with the number of rules.
		var skipped []fixSkipView
		counts := map[fixSkipView]int{}
		for _, s := range p.Skipped {
			v := fixSkipView{Label: settings.Label(s.Command), Reason: s.Reason}
			if counts[v]++; counts[v] == 1 {
				skipped = append(skipped, v)
			}
		}
		for _, v := range skipped {
			if n := counts[v]; n > 1 {
				v.Label = fmt.Sprintf("%s (%d rules)", v.Label, n)
			}
			row.Skipped = append(row.Skipped, v)
		}
		switch {
		case len(row.Cmds) == 0 && len(row.Skipped) == 0:
			row.Blocked = "Already compliant, nothing to change."
		case len(row.Cmds) == 0:
			row.Blocked = "Nothing can be changed: every deviation is unknown or not read live."
		case h.d.Jobs != nil:
			if act, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{DeviceID: d.ID, States: activeJobStates, Limit: 1}); err == nil && len(act) > 0 {
				row.Blocked = "Another transaction is running on this device (#" + strconv.FormatInt(int64(act[0].BatchID), 10) + ")."
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Device.Name) < strings.ToLower(rows[j].Device.Name)
	})
	return rows
}

func fixPageOf(rows []fixRow) fixPage {
	p := fixPage{Rows: rows}
	sum := sha256.New()
	for _, row := range rows {
		if row.Blocked != "" {
			continue
		}
		p.Ready++
		p.IDs = append(p.IDs, row.Device.ID)
		for _, c := range row.Cmds {
			p.Commands++
			sum.Write([]byte(strconv.FormatInt(int64(row.Device.ID), 10) + "\x00" + c.Text + "\n"))
		}
	}
	p.Digest = hex.EncodeToString(sum.Sum(nil))
	return p
}

func (h *handler) fixForm(w http.ResponseWriter, r *http.Request) (PolicyReport, bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return PolicyReport{}, false
	}
	rep, err := h.d.Policy.Report(r.Context(), actor(r), false)
	if err != nil {
		h.fail(w, r, err)
		return rep, false
	}
	return rep, true
}

// policyFixPreview shows the exact commands per device. Nothing is sent.
func (h *handler) policyFixPreview(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.fixForm(w, r)
	if !ok {
		return
	}
	p := fixPageOf(h.fixRows(r.Context(), rep, fixSelection(r, rep)))
	h.fixRender(w, r, http.StatusOK, p)
}

// policyFixApply starts the transaction. The plan is built again from the
// current values; if it differs from the one the admin reviewed, the preview is
// shown again instead of applying something nobody looked at.
func (h *handler) policyFixApply(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.fixForm(w, r)
	if !ok {
		return
	}
	rows := h.fixRows(r.Context(), rep, fixSelection(r, rep))
	p := fixPageOf(rows)
	if p.Digest != r.PostForm.Get("digest") {
		p.Stale = true
		h.fixRender(w, r, http.StatusConflict, p)
		return
	}
	var reqs []ota.PolicyRequest
	for _, row := range rows {
		if row.Blocked != "" {
			continue
		}
		req := ota.PolicyRequest{DeviceID: row.Device.ID}
		for _, c := range row.Cmds {
			req.Commands = append(req.Commands, c.Text)
		}
		reqs = append(reqs, req)
	}
	batch, skipped, err := h.d.PolicyFix.Submit(r.Context(), actor(r), reqs)
	if errors.Is(err, ota.ErrNothingToFix) {
		h.message(w, r, http.StatusConflict, h.view(r, "Nothing to change", "policy", "Nothing was sent. "+h.refusalText(r, skipped)))
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(skipped) == 0 {
		redirect(w, r, "/transactions/"+strconv.FormatInt(int64(batch), 10))
		return
	}
	s := fixStarted{Batch: batch}
	for _, x := range skipped {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		s.Skipped = append(s.Skipped, fixSkipView{Label: d.Name, Reason: humanReason(x.Reason)})
	}
	v := h.view(r, "Make compliant", "policy", nil)
	v.Crumbs = fixCrumbs
	h.show(w, r, http.StatusOK, "policy_fix_started", v, fixStartedPage(v.Base, s))
}

// fixCrumbs is the trail of the Make compliant pages.
var fixCrumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "Make compliant"}}

// fixRender shows the plan as a page: the no-JavaScript way of "Make compliant (N)…", and the
// answer when the values changed after the dialog was drawn.
func (h *handler) fixRender(w http.ResponseWriter, r *http.Request, status int, p fixPage) {
	v := h.view(r, "Make compliant", "policy", nil)
	v.Crumbs = fixCrumbs
	h.show(w, r, status, "policy_fix", v, fixPreviewPage(v.Base, v.CSRF, p))
}

// fixDialogOf is the plan behind "Make compliant (N)…" on the policy page: every device with
// a Required deviation read live. nil when there is nothing to change.
func (h *handler) fixDialogOf(ctx context.Context, rep PolicyReport) *fixDialog {
	var ids []model.DeviceID
	seen := map[model.DeviceID]bool{}
	for _, f := range rep.Findings {
		if !f.Unknown && !advisory(f) && !seen[f.DeviceID] {
			seen[f.DeviceID] = true
			ids = append(ids, f.DeviceID)
		}
	}
	p := fixPageOf(h.fixRows(ctx, rep, ids))
	if p.Ready == 0 {
		return nil
	}
	d := &fixDialog{Ready: p.Ready, Commands: p.Commands, Fields: []ui.KVRow{{Key: "digest", Value: p.Digest}}}
	for _, id := range p.IDs {
		d.Fields = append(d.Fields, ui.KVRow{Key: "device", Value: strconv.FormatInt(int64(id), 10)})
	}
	for _, row := range p.Rows {
		if row.Blocked != "" {
			continue
		}
		var cmds []string
		for _, c := range row.Cmds {
			cmds = append(cmds, c.Text)
		}
		d.Changes = append(d.Changes, ui.KVRow{Key: row.Device.Name, Value: strings.Join(cmds, "; ")})
	}
	return d
}

func (h *handler) refusalText(r *http.Request, skipped []ota.Refusal) string {
	var b strings.Builder
	for _, x := range skipped {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		b.WriteString(d.Name + ": " + humanReason(x.Reason) + ". ")
	}
	return b.String()
}

// fixNote says what a command changes: the setting, its value now and where that came from.
func fixNote(c fixCmd) string {
	s := c.Label + ": has " + c.Have
	if c.Note != "" {
		s += ", " + c.Note
	}
	return s
}
