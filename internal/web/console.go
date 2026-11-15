package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

const consoleHistoryRows = 20

type historyRow struct {
	At      time.Time
	Actor   string
	Command string
	Result  string
	Refused bool
}

type consoleData struct {
	Device model.Device
	// Input pre-fills the command box. Secret commands are never pre-filled.
	Input string
	// Shown is the redacted command text for messages.
	Shown string
	// Confirm is set on the confirmation step of a destructive or lock-out
	// command: Consequence names what it does, Token stands for the command,
	// which is kept on the server (its arguments may be a secret).
	Confirm     bool
	Consequence string
	// ACLNotice shows the broker ACL notice on the confirmation: the command goes over MQTT (#840).
	ACLNotice bool
	Token     string
	Reply     string
	Error     string
	// HistoryAdd is the command the browser may keep for Arrow Up, set after the policy let it
	// through (see historyText).
	HistoryAdd string
	History    []historyRow
	Tabs       deviceTabs
}

// history reads the console history from the audit log: what was sent to the
// device and what was refused. The audit log only holds redacted text.
func (h *handler) consoleHistory(r *http.Request, id model.DeviceID) []historyRow {
	if h.d.Audit == nil {
		return nil
	}
	target := "device:" + strconv.FormatInt(int64(id), 10)
	evs, err := h.d.Audit.ListAudit(r.Context(), store.AuditFilter{Target: target, Limit: 200})
	if err != nil {
		h.log.WarnContext(r.Context(), "console history unavailable", "err", err)
		return nil
	}
	var rows []historyRow
	for _, e := range evs {
		if e.Target != target || (e.Action != "command.send" && e.Action != "command.rejected") {
			continue
		}
		var d struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(e.Detail), &d) != nil {
			continue
		}
		rows = append(rows, historyRow{At: e.At, Actor: e.Actor, Command: d.Command, Result: e.Result, Refused: e.Action == "command.rejected"})
		if len(rows) == consoleHistoryRows {
			break
		}
	}
	return rows
}

// quickCommands are the console's quick commands (prototype, D9: Status 5 instead of Status 0,
// which automated code never sends). Each is checked by the command policy like a typed one.
var quickCommands = []string{"Status 5", "Power TOGGLE", "Status 11", "TelePeriod 300", "Restart 1"}

// consoleSince reads "since" (Clear): lines older than it are not shown. Zero shows everything.
func consoleSince(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// consoleLines turns the history (newest first) into the console's lines, oldest first, and
// appends the reply or the error of the command just sent.
func consoleLines(data consoleData, since, now time.Time) []ui.ConsoleLine {
	var out []ui.ConsoleLine
	stamp := func(t time.Time) (string, string) { return t.Local().Format("15:04:05"), t.UTC().Format(time.RFC3339) }
	for i := len(data.History) - 1; i >= 0; i-- {
		row := data.History[i]
		if !since.IsZero() && row.At.Before(since) {
			continue
		}
		tm, dt := stamp(row.At)
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: "› " + row.Command, Kind: "cmd", Who: row.Actor})
		switch {
		case row.Refused:
			out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: "Refused: " + humanReason(row.Result), Kind: "err"})
		case row.Result != "" && row.Result != "ok":
			out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: humanReason(row.Result), Kind: "err"})
		}
	}
	tm, dt := stamp(now)
	switch {
	case data.Reply != "":
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: data.Reply, Kind: "reply"})
	case data.Error != "":
		out = append(out, ui.ConsoleLine{Time: tm, Datetime: dt, Text: data.Error, Kind: "alert"})
	}
	return out
}

// renderConsole shows the console tab: the device header, a pending confirmation, the console.
func (h *handler) renderConsole(w http.ResponseWriter, r *http.Request, status int, d model.Device, data consoleData) {
	data.Device = d
	data.History = h.consoleHistory(r, d.ID)
	v := h.view(r, d.Name+" console", "devices", nil)
	data.Tabs = h.tabsOf(r, v, d, "console")
	since := r.FormValue("since")
	now := h.d.Clock.Now()
	f, paths, pathsOK := h.transportOf(r.Context(), d.ID)
	c := ui.ConsoleData{
		Title:  "Console · " + consoleVia(f, paths, pathsOK) + " · " + data.Tabs.Address,
		Action: v.Base + devPath(d.ID, "/console"), CSRF: v.CSRF, Input: data.Input,
		ClearHref:  v.Base + devPath(d.ID, "/console?since="+strconv.FormatInt(now.Unix(), 10)),
		HistoryAdd: data.HistoryAdd,
		Lines:      consoleLines(data, consoleSince(since), now),
		Quick:      quickCommands,
	}
	if !consoleSince(since).IsZero() {
		c.Hidden = []ui.KVRow{{Key: "since", Value: since}}
	}
	h.headPage(w, r, status, "console", v, d, consolePage(data, c, v.CSRF))
}

func (h *handler) consolePage(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	if h.d.ProbeHTTP != nil {
		// A change of the MQTT connection asks first unless HTTP answered lately (app.reachedOnlyOverMQTT), and a
		// device fine on MQTT is not polled over HTTP: opening the console is a moment to check, as on the device page.
		h.d.ProbeHTTP(d.ID)
	}
	h.renderConsole(w, r, http.StatusOK, d, consoleData{})
}

// consoleVia names the transports the console's commands take, for its title: MQTT when reads or changes go over
// MQTT and HTTP is failing, "MQTT + HTTP" when they go MQTT first while HTTP works (HTTP carries what MQTT cannot),
// else HTTP (#605).
func consoleVia(f transport.Facts, p transport.Paths, ok bool) string {
	switch {
	case !ok || (p.Commands != transport.MQTT && p.Writes != transport.MQTT):
		return "HTTP"
	case f.HTTP.State == transport.HTTPFailing:
		return "MQTT"
	}
	return "MQTT + HTTP"
}

// consoleError maps service errors to a status and a sentence. Device output
// and credentials never reach the sentence.
func consoleError(err error) (int, string) {
	switch {
	case errors.Is(err, cmdpolicy.ErrInvalid):
		return http.StatusUnprocessableEntity, "Malformed command. Send exactly one command: no semicolons, control characters or invisible characters."
	case errors.Is(err, cmdpolicy.ErrForbidden):
		return http.StatusForbidden, "This command is not sent from TasmoFleet."
	case errors.Is(err, cmdpolicy.ErrRole):
		return http.StatusForbidden, "This command needs the admin role."
	case errors.Is(err, cmdpolicy.ErrUpdateJob):
		return http.StatusConflict, updateJobHoldsText
	case errors.Is(err, transport.ErrCutOff), errors.Is(err, transport.ErrRouteChanged):
		return http.StatusConflict, err.Error() // names the command or the route, never its arguments
	}
	status, msg := deviceErrorText(err)
	if msg == "" {
		return http.StatusInternalServerError, "The command failed."
	}
	return status, msg
}

// updateJobHoldsText is the refusal of Upgrade/OtaUrl while a TasmoFleet update job holds the device.
const updateJobHoldsText = "A TasmoFleet update job (queued or running) holds this device. Upgrade and OtaUrl are refused until it has finished, because two updates at once can leave the device unusable."

// updateHeld returns the devices among ids that a TasmoFleet update job (queued or running) holds. A
// job of another kind (command, backup, policy) is not an update: the executing services refuse those
// with the generic busy text.
func (h *handler) updateHeld(ctx context.Context, ids ...model.DeviceID) (map[model.DeviceID]bool, error) {
	held := map[model.DeviceID]bool{}
	if h.d.Jobs == nil {
		return held, nil
	}
	for _, id := range ids {
		jobs, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{DeviceID: id, States: activeJobStates})
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.IsUpdate() {
				held[id] = true
			}
		}
	}
	return held, nil
}

// confirmation adds to v the consequence of cutting off a device that is reached over MQTT only, and
// says whether it did: that is what a confirmation may consent to (transport.WithCutOffConsent).
func (h *handler) confirmation(ctx context.Context, cmd string, v cmdpolicy.Verdict, ids ...model.DeviceID) (cmdpolicy.Verdict, bool) {
	if h.d.MQTTOnly == nil || !cmdpolicy.CutsMQTTOff(cmd) || !h.d.MQTTOnly(ctx, ids...) {
		return v, false
	}
	return cmdpolicy.WithMQTTOnly(cmd, v), true
}

// askConsole shows the console's confirmation of cmd when it needs one, with status, and reports whether it did: the
// consequence of a destructive or lock-out command (with the cut-off of a device reached over MQTT only; cutOff: the
// router just found HTTP silent, so that consequence is named whatever is on record), and the transport a command
// whose arguments the audit hides takes (#826). The token keeps the cut-off consent and the route for the send. When
// it does not ask, routes is the route a send without a confirmation must carry (one that goes nowhere), or nil.
func (h *handler) askConsole(w http.ResponseWriter, r *http.Request, d model.Device, data consoleData, cmd string, status int, cutOff bool) (bool, map[model.DeviceID]transport.Path) {
	ctx := r.Context()
	v0, err := cmdpolicy.Classify(cmd)
	if err != nil {
		return false, nil
	}
	canon, err := cmdpolicy.Canonical(cmd)
	if err != nil {
		return false, nil
	}
	v, cut := h.confirmation(ctx, cmd, v0, d.ID)
	if cutOff {
		v, cut = cmdpolicy.WithMQTTOnly(cmd, v0), true
	}
	routes, notice := h.consoleRoute(ctx, canon, d.ID)
	if v.Confirm == "" && notice == "" {
		return false, routes
	}
	data.Confirm, data.Consequence = true, strings.TrimSpace(v.Confirm+" "+notice)
	data.ACLNotice = routes[d.ID] == transport.MQTT && h.brokerACLOpen()
	data.Token = h.cmds.add(h.d.Clock.Now(), pendingCmd{binding: h.bindingOf(r), cmd: canon, ids: []model.DeviceID{d.ID}, cutOff: cut,
		routes: routes, notice: notice})
	h.renderConsole(w, r, status, d, data)
	return true, nil
}

func (h *handler) consoleSend(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	cmd := r.PostFormValue("command")
	p, _ := auth.FromContext(r.Context())
	ctx := r.Context()
	data := consoleData{Shown: cmdpolicy.Redacted(cmd)}
	// Text goes back into the page (input value) only when the policy would
	// also log it: arguments of most commands may be secrets.
	if canon, err := cmdpolicy.Canonical(cmd); err == nil && canon == data.Shown {
		if _, err := cmdpolicy.Classify(cmd); err == nil {
			data.Input = cmd
		}
	}
	// A firmware command is refused before it asks for a confirmation while an update job holds the device.
	if cmdpolicy.StartsFirmware(cmd) && p.Role >= model.RoleAdmin {
		held, err := h.updateHeld(ctx, d.ID)
		if err != nil {
			h.log.ErrorContext(ctx, "console: active jobs", "device", d.ID, "err", err)
			data.Error = "The command failed."
			h.renderConsole(w, r, http.StatusInternalServerError, d, data)
			return
		}
		if held[d.ID] {
			data.Error = updateJobHoldsText
			h.renderConsole(w, r, http.StatusConflict, d, data)
			return
		}
	}
	// A destructive or lock-out command asks once, naming the consequence.
	// Routine commands go straight through. The flow only decides whether the
	// extra screen is shown: the service still checks the role for every send.
	if token := r.PostFormValue("token"); token != "" {
		pc, ok := h.cmds.take(h.d.Clock.Now(), token, h.bindingOf(r))
		if !ok || len(pc.ids) != 1 || pc.ids[0] != d.ID {
			data.Error = "The confirmation expired. Send the command again."
			h.renderConsole(w, r, http.StatusConflict, d, data)
			return
		}
		cmd, data.Shown, data.Input = pc.cmd, cmdpolicy.Redacted(pc.cmd), ""
		if why := h.apiOffBlockCommand(ctx, d.ID, cmd); why != "" {
			data.Error = why
			h.renderConsole(w, r, http.StatusConflict, d, data)
			return
		}
		if pc.cutOff {
			ctx = transport.WithCutOffConsent(ctx) // the person was told, and confirmed
		}
		if pc.routes != nil {
			// The sentence the person confirmed must still be the one that holds (the route, the broker URL);
			// otherwise nothing is sent and the console asks again (#826). The router keeps to the route after this.
			if _, notice := h.consoleRoute(ctx, cmd, d.ID); notice != pc.notice {
				data.Error = "Not sent: the way to the device changed since the confirmation. Confirm again."
				if asked, _ := h.askConsole(w, r, d, data, cmd, http.StatusConflict, false); !asked {
					h.renderConsole(w, r, http.StatusConflict, d, data) // it goes nowhere now: say so, send nothing
				}
				return
			}
			ctx = transport.WithRoute(ctx, pc.routes[d.ID])
		}
	} else if why := h.apiOffBlockCommand(ctx, d.ID, cmd); why != "" {
		// Refused before the confirmation: a device whose HTTP API is off and which nothing else reaches would only fail.
		data.Error = why
		h.renderConsole(w, r, http.StatusConflict, d, data)
		return
	} else if p.Role >= model.RoleAdmin {
		asked, routes := h.askConsole(w, r, d, data, cmd, http.StatusOK, false)
		if asked {
			return
		}
		if routes != nil {
			ctx = transport.WithRoute(ctx, routes[d.ID]) // it goes nowhere (#826): nothing may go anywhere unannounced
		}
	}
	raw, err := h.d.DeviceCmd.Run(ctx, p, d.ID, cmd)
	switch {
	case errors.Is(err, transport.ErrCutOff) && p.Role >= model.RoleAdmin:
		// The question above follows HTTP's answer on record; the router asked HTTP right now and got none (HTTP
		// stopped since). A silence is not remembered, so a resend would skip the question again: ask here.
		if asked, _ := h.askConsole(w, r, d, data, cmd, http.StatusOK, true); asked {
			return
		}
	case errors.Is(err, transport.ErrRouteChanged) && p.Role >= model.RoleAdmin:
		// The router would take another transport than the confirmation named (#826): nothing was sent; ask again
		// with the route that holds now.
		data.Error = err.Error()
		if asked, _ := h.askConsole(w, r, d, data, cmd, http.StatusConflict, false); asked {
			return
		}
	}
	if !errors.Is(err, cmdpolicy.ErrInvalid) && !errors.Is(err, cmdpolicy.ErrForbidden) && !errors.Is(err, cmdpolicy.ErrRole) && !errors.Is(err, transport.ErrCutOff) &&
		!errors.Is(err, transport.ErrRouteChanged) {
		data.HistoryAdd = historyText(cmd) // a device that failed to answer is worth a retry; a refusal is not
	}
	status := http.StatusOK
	if err != nil {
		status, data.Error = consoleError(err)
		if status == http.StatusInternalServerError {
			h.log.ErrorContext(r.Context(), "console command failed", "device", d.ID, "err", err)
		}
	} else {
		// Sent: the box is empty for the next command (it keeps the text only for a correction).
		data.Reply, data.Input = compactJSON(raw), ""
	}
	h.renderConsole(w, r, status, d, data)
}

// historyText is the form of cmd the browser may keep in its command history, or "" for none. It
// is the same rule the audit log uses (cmdpolicy.ShownInFull): a command whose logged form hides
// or masks anything holds, or may hold, a secret and is not kept. A line that already shows the
// mask is a redacted one (the bulk result page only has those).
func historyText(cmd string) string {
	canon, err := cmdpolicy.Canonical(cmd)
	if err != nil || canon == "" || !cmdpolicy.ShownInFull(cmd) {
		return ""
	}
	return canon
}

// compactJSON is a device reply on one console line; anything unparsable is shown as is. The
// page escapes it either way.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// prettyJSON indents a device reply; anything unparsable is shown as is.
// The template escapes it either way.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
