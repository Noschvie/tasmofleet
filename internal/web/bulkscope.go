package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The bulk action pages of design v2 (WP P7, prototype screen "bulk"): one scope page for every
// action of the bulk bar (the selected devices with who is skipped, the options of the action in
// an aside, the run button) and one result page (Succeeded / Failed / Skipped tiles, a row per
// device). The selection is the scope: there is no "all devices" choice, and nothing runs without
// devices posted from the list (the bar's "Select all N" selects the whole fleet).

type bulkKind string

const (
	bulkCmd     bulkKind = "cmd"
	bulkBackup  bulkKind = "backup"
	bulkRestart bulkKind = "restart"
	bulkOn      bulkKind = "on"
	bulkOff     bulkKind = "off"
)

// bulkWords are the prototype's words per action (KM in design/v2/template.html): page title,
// subtitle, the run button's verb, the title of the finished result.
var bulkWords = map[bulkKind][4]string{
	bulkCmd:     {"Send a console command", "Runs the command on each device and collects every response.", "Send to", "Command finished"},
	bulkBackup:  {"Back up devices", "Downloads each device’s configuration and stores it with a label.", "Back up", "Backups finished"},
	bulkRestart: {"Restart devices", "Each device reboots. Relays switch off for a few seconds.", "Restart", "Restart finished"},
	bulkOn:      {"Power on", "Switches the relays of each device on.", "Switch on", "Power on finished"},
	bulkOff:     {"Power off", "Switches the relays of each device off.", "Switch off", "Power off finished"},
}

// bulkRunning is the result's title while the transaction still runs.
var bulkRunning = map[bulkKind]string{bulkCmd: "Command running", bulkBackup: "Backups running", bulkRestart: "Restart running"}

// bulkRow is one selected device on the scope page.
type bulkRow struct {
	ID                 model.DeviceID
	Name, Addr, Status string
	Tone               ui.Tone
	Skip               bool // not online: nothing is sent to it
}

// bulkScopeView is the scope page of one bulk action.
type bulkScopeView struct {
	Kind        bulkKind
	Base, CSRF  string
	Self        string // the scope route: "Remove" posts the form here without the device
	Run         string // the form's action
	Rows        []bulkRow
	RunN, SkipN int
	Token       string // restart: the server-held command and devices the run takes
	Error       string // the action cannot run as posted (banner)
	// Send command
	Command, CommandError string
	Wait, Serial          bool
	// Back up
	Label string
	Keep  bool
	// Restart
	Parallel int
	// Power on / off
	Action  string // ON or OFF
	Relay   string // "all" or "1"
	Stagger bool
}

func (v bulkScopeView) Title() string { return bulkWords[v.Kind][0] }
func (v bulkScopeView) Sub() string   { return bulkWords[v.Kind][1] }

// Verb is the run button: "Send to 6 devices".
func (v bulkScopeView) Verb() string { return bulkWords[v.Kind][2] + " " + plural(v.RunN, "device") }

// Summary is the line above the run button: "6 devices run · 2 skipped".
func (v bulkScopeView) Summary() string {
	return fmt.Sprintf("%s run · %d skipped", plural(v.RunN, "device"), v.SkipN)
}

// SkipNote closes the device list when some devices are not online.
func (v bulkScopeView) SkipNote() string {
	if v.SkipN == 1 {
		return "1 device is offline or unknown and will be skipped."
	}
	return fmt.Sprintf("%d devices are offline or unknown and will be skipped.", v.SkipN)
}

// bulkScope builds the scope page from the posted devices, in the order they were posted (the
// order of the list they were selected in). A device that no longer exists is left out; a device
// that is not online is listed and skipped. "drop" removes one device ("×" on a row).
func (h *handler) bulkScope(r *http.Request, kind bulkKind, self, run string) (bulkScopeView, []model.DeviceID, error) {
	v := h.view(r, "", "devices", nil)
	sv := bulkScopeView{Kind: kind, Base: v.Base, CSRF: v.CSRF, Self: v.Base + self, Run: v.Base + run}
	ids := h.formIDs(r)
	if drop, err := strconv.ParseInt(r.PostFormValue("drop"), 10, 64); err == nil {
		kept := ids[:0]
		for _, id := range ids {
			if id != model.DeviceID(drop) {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		return sv, nil, err
	}
	byID := make(map[model.DeviceID]model.Device, len(devs))
	for _, d := range devs {
		byID[d.ID] = d
	}
	var ready []model.DeviceID
	for _, id := range ids {
		d, ok := byID[id]
		if !ok {
			continue
		}
		st := h.d.Health.Get(id).State
		row := bulkRow{ID: id, Name: d.Name, Addr: d.Address, Status: label(st), Tone: healthTone(st)}
		if d.Port != 0 {
			row.Addr += ":" + strconv.Itoa(d.Port)
		}
		if reachableNow(st) {
			ready = append(ready, id)
		} else {
			row.Skip = true
			sv.SkipN++
		}
		sv.Rows = append(sv.Rows, row)
	}
	sv.RunN = len(ready)
	if len(sv.Rows) == 0 {
		sv.Error = "Select the devices in the device list first."
	} else if sv.RunN == 0 {
		sv.Error = "None of the devices is online."
	}
	return sv, ready, nil
}

// firstVisit reports whether the scope page is opened from the bar: its form always posts
// opts=1, so an option left unchecked there stays unchecked on a redraw.
func firstVisit(r *http.Request) bool { return r.PostFormValue("opts") == "" }

// dropping reports whether the post is a "×" on a row, which only redraws the scope page.
func dropping(r *http.Request) bool { return r.PostFormValue("drop") != "" }

// showBulkScope renders the scope page; a page that cannot run answers 422.
func (h *handler) showBulkScope(w http.ResponseWriter, r *http.Request, sv bulkScopeView) {
	status := http.StatusOK
	if sv.Error != "" || sv.CommandError != "" {
		status = http.StatusUnprocessableEntity
	}
	v := h.view(r, bulkTitle, "devices", nil)
	v.Crumbs = bulkCrumbs
	h.show(w, r, status, "bulk_scope", v, bulkScopePage(sv))
}

// bulkTitle names every bulk page in the window title and the phone header (the prototype's TITLE).
const bulkTitle = "Bulk action"

var bulkCrumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Bulk action"}}

// healthTone is the dot of a device's health: online green, offline red, the rest grey.
func healthTone(st model.HealthState) ui.Tone {
	switch st {
	case model.HealthOnline:
		return ui.ToneOK
	case model.HealthOffline, model.HealthAuthFailed:
		return ui.ToneDanger
	case model.HealthStale:
		return ui.ToneWarn
	case model.HealthUnknown:
	}
	return ui.ToneNeutral
}

// skippedReason is the result line of a device that got nothing because it was not online.
func skippedReason(st model.HealthState) string {
	switch st {
	case model.HealthOffline:
		return "Offline. Not attempted."
	case model.HealthUnknown:
		return "Has never answered. Not attempted."
	case model.HealthOnline, model.HealthStale, model.HealthAuthFailed:
	}
	return label(st) + ". Not attempted."
}

// bulkResultRow is one device of the result page.
type bulkResultRow struct {
	Name, Addr, Label, Detail string
	Tone                      ui.Tone
}

// bulkResultView is the result page of a bulk action.
type bulkResultView struct {
	Kind                bulkKind
	Base                string
	Self                string // the page's own URL while it refreshes; "" when finished
	Running             bool
	Command             string // Send command: the redacted command line
	HistoryAdd          string // Send command: the command the browser may keep for Arrow Up ("" keeps nothing)
	When                time.Time
	Took, By            string
	Batch               model.BatchID // 0: no transaction (power is switched in the request)
	OK, Failed, Skipped int
	Rows                []bulkResultRow
	CanRetry            bool
	Toast               string // power: the toast of the finished run
}

func (v bulkResultView) Title() string {
	if v.Running {
		return bulkRunning[v.Kind]
	}
	return bulkWords[v.Kind][3]
}

// State is the badge beside the title.
func (v bulkResultView) State() (ui.Tone, string) {
	switch {
	case v.Running:
		return ui.ToneAccent, "Running"
	case v.Failed > 0:
		return ui.ToneDanger, fmt.Sprintf("Finished · %d failed", v.Failed)
	}
	return ui.ToneOK, "Completed"
}

// ToastTone is the tone of the finished run's toast.
func (v bulkResultView) ToastTone() string {
	if v.Failed > 0 {
		return "danger"
	}
	return "ok"
}

// resultURL is the result page of a bulk transaction. It carries the posted devices in their
// order: the page lists them so, and the ones without a job in the transaction (not online, so
// never started) as skipped.
func resultURL(batch model.BatchID, posted []model.DeviceID) string {
	q := url.Values{}
	for _, id := range posted {
		q.Add("id", strconv.FormatInt(int64(id), 10))
	}
	u := "/devices/bulk/result/" + strconv.FormatInt(int64(batch), 10)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// bulkResult shows a bulk transaction (Send command, Back up, Restart) the way the prototype
// shows a finished bulk action, refreshing itself while it runs. The transaction page keeps the
// full per-device log.
func (h *handler) bulkResult(w http.ResponseWriter, r *http.Request) {
	p, ok := h.batchData(w, r)
	if !ok {
		return
	}
	first := p.Jobs[0].Job
	kind := bulkCmd
	switch {
	case first.IsBackup():
		kind = bulkBackup
	case first.IsCommand() && first.Commands == restartCommand:
		kind = bulkRestart
	case !first.IsCommand():
		h.notFoundBatch(w, r) // an update or Make compliant: the transaction page shows it
		return
	}
	started, end := first.CreatedAt, first.UpdatedAt
	for _, j := range p.Jobs {
		if j.Job.CreatedAt.Before(started) {
			started = j.Job.CreatedAt
		}
		if j.Job.UpdatedAt.After(end) {
			end = j.Job.UpdatedAt
		}
	}
	if p.Active {
		end = h.d.Clock.Now()
	}
	v := h.view(r, "", "devices", nil)
	rv := bulkResultView{
		Kind: kind, Base: v.Base, Running: p.Active, Batch: p.Batch, When: started,
		Took: humanDuration(end.Sub(started).Round(time.Second)), By: h.pr.Text(p.By),
		CanRetry: !p.Active && p.Failed+p.Canceled > 0 && v.IsAdmin,
	}
	if kind == bulkCmd {
		rv.Command = strings.TrimPrefix(txTarget(first), "Send ") // the redacted command
		rv.HistoryAdd = historyText(rv.Command)
	}
	if rv.Running {
		rv.Self = v.Base + r.URL.RequestURI() // handlers see the path below the base
	}
	byDev := map[model.DeviceID]bulkResultRow{}
	var order []model.DeviceID
	for _, j := range p.Jobs {
		byDev[j.Job.DeviceID] = h.jobResultRow(r, j, &rv)
		order = append(order, j.Job.DeviceID)
	}
	slices.Sort(order) // without the posted order: the order the jobs were created in
	if posted := h.formIDs(r); len(posted) > 0 {
		for _, id := range order {
			if !slices.Contains(posted, id) {
				posted = append(posted, id)
			}
		}
		order = posted
	}
	for _, id := range order {
		if row, ok := byDev[id]; ok {
			rv.Rows = append(rv.Rows, row)
			continue
		}
		d, err := h.d.Inventory.Get(r.Context(), id)
		if err != nil {
			continue
		}
		rv.Rows = append(rv.Rows, bulkResultRow{Name: d.Name, Addr: d.Address, Label: "Skipped", Tone: ui.ToneNeutral, Detail: skippedReason(h.d.Health.Get(id).State)})
		rv.Skipped++
	}
	v.Title, v.Crumbs, v.Live = bulkTitle, bulkCrumbs, true
	h.show(w, r, http.StatusOK, "bulk_result", v, bulkResultPage(rv))
}

// jobResultRow is the result row of one device's job; it counts the job in the tiles.
func (h *handler) jobResultRow(r *http.Request, j jobView, rv *bulkResultView) bulkResultRow {
	row := bulkResultRow{Name: j.Device}
	if evs, err := h.d.Jobs.JobEvents(r.Context(), j.Job.ID); err == nil && len(evs) > 0 {
		last := evs[len(evs)-1]
		row.Detail = eventText(last, j.Job, h.pr, eventStageMinimal(evs, len(evs)-1, j.Job))
		if last.Detail != "" && last.Event != "cancel" { // a cancel's detail is a subject id; eventText names the person
			row.Detail = last.Detail // the device's reply or the reason, without the step's name
		}
	}
	if d, err := h.d.Inventory.Get(r.Context(), j.Job.DeviceID); err == nil {
		row.Addr = d.Address
	}
	switch j.Job.State { //nolint:exhaustive // every other state is a running step
	case model.JobSucceeded:
		row.Label, row.Tone = "Succeeded", ui.ToneOK
		rv.OK++
	case model.JobFailed:
		row.Label, row.Tone = "Failed", ui.ToneDanger
		rv.Failed++
		if row.Detail == "" {
			row.Detail = humanReason(j.Job.LastError)
		}
	case model.JobCancelled:
		row.Label, row.Tone = "Cancelled", ui.ToneNeutral
		rv.Failed++
	case model.JobQueued:
		row.Label, row.Tone = "Queued", ui.ToneNeutral
	default:
		// The device's reply of an earlier step is not shown while the job still runs: the
		// row would say "Running" next to a finished answer. The step in progress is.
		row.Label, row.Tone = "Running", ui.ToneAccent
		if j.StepLabel != "" {
			row.Detail = j.StepLabel
		}
	}
	return row
}
