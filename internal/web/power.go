package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// DeviceCmd is the part of devicecmd.Service the UI uses (power toggle and
// console). Nil disables both features.
type DeviceCmd interface {
	Power(ctx context.Context, actor string, id model.DeviceID, relay int, action string) (string, error)
	Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error)
}

var _ DeviceCmd = devicecmd.Service(nil)

// registerDeviceCmd mounts the power toggle and the console. Both need only
// the operator role: the service decides per command what the role may send.
func (h *handler) registerDeviceCmd(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	if h.d.DeviceCmd == nil {
		return
	}
	route("POST /devices/bulk/power", op, h.devicesBulkPower)
	route("POST /devices/{id}/power/{n}", op, h.devicePower)
	route("GET /devices/{id}/console", op, h.consolePage)
	route("POST /devices/{id}/console", op, h.consoleSend)
}

type relayState struct {
	N     int
	State string
}

// powerCell is the data of the live power cell of a device row.
type powerCell struct {
	Base    string
	ID      model.DeviceID
	Enabled bool // buttons only when the service exists
	Off     bool // the device does not answer now (offline, unknown, wrong password): the switches are disabled
	Relays  []relayState
	Error   string
}

const maxRelays = 8

// powerCellOf builds the cell from the latest health. "POWER" is relay 1 of a
// single-relay device, "POWERn" relay n.
func (h *handler) powerCellOf(base string, id model.DeviceID, hl model.Health) powerCell {
	return powerCell{Base: base, ID: id, Enabled: h.d.DeviceCmd != nil, Off: hl.State != model.HealthOnline && hl.State != model.HealthStale, Relays: relayStates(hl)}
}

// relayStates reads the relays from the latest health, in relay order.
func relayStates(hl model.Health) []relayState {
	var out []relayState
	byN := map[int]string{}
	for k, v := range hl.Power {
		rest, ok := strings.CutPrefix(k, "POWER")
		if !ok {
			continue
		}
		n := 1
		if rest != "" {
			var err error
			if n, err = strconv.Atoi(rest); err != nil {
				continue
			}
		}
		if n >= 1 && n <= maxRelays {
			byN[n] = v
		}
	}
	for n, v := range byN {
		out = append(out, relayState{N: n, State: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out
}

// powerErrorText maps service errors to short sentences without device
// output. The returned status is the HTTP status to answer with.
func powerErrorText(err error) (int, string) {
	switch {
	case errors.Is(err, devicecmd.ErrBadAction):
		return http.StatusBadRequest, "Power action must be ON, OFF or TOGGLE."
	case errors.Is(err, devicecmd.ErrNoRelay):
		return http.StatusUnprocessableEntity, "The device has no such relay."
	case errors.Is(err, devicecmd.ErrUnconfirmed):
		return http.StatusGatewayTimeout, "The device did not confirm the command in time. Check its state before trying again."
	}
	return deviceErrorText(err)
}

// deviceErrorText maps device client errors to a status and a sentence.
func deviceErrorText(err error) (int, string) {
	switch {
	case errors.Is(err, devlock.ErrBusy):
		return http.StatusConflict, "Another operation on this device is running (" + busyHolder(err) + "). Try again when it has finished."
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "The device does not exist."
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return http.StatusBadGateway, "The device likely refuses HTTP API commands from this address. Set SetOption128 1 on it."
	case errors.Is(err, tasmota.ErrUnreachable):
		return http.StatusBadGateway, "The device is unreachable."
	case errors.Is(err, tasmota.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "The device did not answer in time."
	case errors.Is(err, tasmota.ErrAuth):
		return http.StatusBadGateway, "The device rejected the stored credentials."
	case errors.Is(err, tasmota.ErrUnknownCommand):
		return http.StatusUnprocessableEntity, "The device does not know this command."
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return http.StatusBadGateway, "The device address is outside the configured device networks."
	case errors.Is(err, tasmota.ErrBadResponse):
		return http.StatusBadGateway, "The device answered with something unreadable."
	}
	return http.StatusInternalServerError, ""
}

func (h *handler) devicePower(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	limitBody(w, r)
	action := r.PostFormValue("action")
	if action == "" {
		action = "TOGGLE"
	}
	state, perr := h.d.DeviceCmd.Power(r.Context(), actor(r), d.ID, n, action)
	hl := h.d.Health.Get(d.ID)
	cell := h.powerCellOf(httpBase(r), d.ID, hl)
	status := http.StatusOK
	switch {
	case perr != nil:
		var msg string
		if status, msg = powerErrorText(perr); msg == "" {
			h.fail(w, r, perr)
			return
		}
		cell.Error = msg
	case state != "":
		cell.setState(n, state)
	}
	if r.Header.Get("HX-Request") == "" {
		if perr != nil {
			v := h.view(r, d.Name, "devices", cell.Error)
			h.message(w, r, status, v)
			return
		}
		if r.PostFormValue("back") == "device" { // the switches of the device page are plain forms
			redirect(w, r, devPath(d.ID, ""))
			return
		}
		redirect(w, r, "/devices")
		return
	}
	h.component(w, r, status, powerCellView(cell))
}

// setState shows the state the device just reported, ahead of the health
// update that follows over SSE.
func (c *powerCell) setState(n int, state string) {
	for i := range c.Relays {
		if c.Relays[i].N == n {
			c.Relays[i].State = state
			return
		}
	}
	c.Relays = append(c.Relays, relayState{N: n, State: state})
	sort.Slice(c.Relays, func(i, j int) bool { return c.Relays[i].N < c.Relays[j].N })
}

// busyHolder names the operation that holds the device lock.
func busyHolder(err error) string {
	var b devlock.BusyError
	if errors.As(err, &b) {
		return b.Holder
	}
	return "unknown"
}

// bulkLimit bounds how many devices one bulk switch may touch.
const bulkLimit = 50

// bulkStagger is the pause between two devices switched on with "Stagger by 1 s".
const bulkStagger = time.Second

// devicesBulkPower is Power on / Power off of the bulk bar. The first post shows the scope page
// (the selected devices, the relay choice, for Power on the stagger); its confirm=1 switches.
// Each device goes through the same service as the single switch (policy, device lock, audit);
// devices that are not online are skipped and say why, relays already in the wanted state are
// left alone. Devices are handled one after the other: a bulk action must not become a burst on
// the network, and switching on can wait a second between devices so the inrush currents do
// not add up. The result is the page itself (nothing runs in the background).
func (h *handler) devicesBulkPower(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	action := strings.ToUpper(r.PostFormValue("action"))
	if action != "ON" && action != "OFF" {
		http.Error(w, "action must be ON or OFF", http.StatusBadRequest)
		return
	}
	if n := len(h.formIDs(r)); n == 0 || n > bulkLimit {
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Nothing switched", "devices",
			fmt.Sprintf("Select between 1 and %d devices.", bulkLimit)))
		return
	}
	kind := bulkOn
	if action == "OFF" {
		kind = bulkOff
	}
	sv, _, err := h.bulkScope(r, kind, "/devices/bulk/power", "/devices/bulk/power")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sv.Action, sv.Relay = action, "all"
	if r.PostFormValue("relay") == "1" {
		sv.Relay = "1"
	}
	sv.Stagger = action == "ON" && (firstVisit(r) || r.PostFormValue("stagger") == "1")
	if r.PostFormValue("confirm") != "1" || dropping(r) || sv.Error != "" {
		h.showBulkScope(w, r, sv)
		return
	}
	stagger := action == "ON" && r.PostFormValue("stagger") == "1"
	v := h.view(r, "", "devices", nil)
	rv := bulkResultView{Kind: kind, Base: v.Base, When: h.d.Clock.Now(), By: h.pr.Text(actor(r))}
	switched := 0
	for _, row := range sv.Rows {
		res := bulkResultRow{Name: row.Name, Addr: row.Addr}
		if row.Skip {
			res.Label, res.Tone, res.Detail = "Skipped", ui.ToneNeutral, skippedReason(h.d.Health.Get(row.ID).State)
			rv.Skipped++
			rv.Rows = append(rv.Rows, res)
			continue
		}
		var todo, already []relayState
		for _, rl := range h.powerCellOf(v.Base, row.ID, h.d.Health.Get(row.ID)).Relays {
			switch {
			case sv.Relay == "1" && rl.N != 1:
			case strings.EqualFold(rl.State, action):
				already = append(already, rl)
			default:
				todo = append(todo, rl)
			}
		}
		if len(todo)+len(already) == 0 {
			res.Label, res.Tone, res.Detail = "Skipped", ui.ToneNeutral, "No relay state known yet. Not attempted."
			rv.Skipped++
			rv.Rows = append(rv.Rows, res)
			continue
		}
		if len(todo) > 0 && stagger && switched > 0 {
			select {
			case <-r.Context().Done():
				return // the browser went away: switch nothing more
			case <-h.d.Clock.After(bulkStagger):
			}
		}
		if len(todo) > 0 {
			switched++
		}
		res.Label, res.Tone = "Succeeded", ui.ToneOK
		var done []string
		for _, rl := range todo {
			state, perr := h.d.DeviceCmd.Power(r.Context(), actor(r), row.ID, rl.N, action)
			if perr != nil {
				_, msg := powerErrorText(perr)
				if msg == "" {
					msg = "The command failed."
				}
				res.Label, res.Tone, done = "Failed", ui.ToneDanger, []string{fmt.Sprintf("Relay %d: %s", rl.N, msg)}
				break
			}
			done = append(done, fmt.Sprintf("POWER%d = %s", rl.N, state))
		}
		if res.Label == "Succeeded" && len(already) > 0 {
			done = append(done, "was already "+strings.ToLower(label(action)))
		}
		res.Detail = strings.Join(done, " · ")
		if res.Label == "Failed" {
			rv.Failed++
		} else {
			rv.OK++
		}
		rv.Rows = append(rv.Rows, res)
	}
	rv.Toast = fmt.Sprintf("%s · %d succeeded", bulkWords[kind][3], rv.OK)
	if rv.Failed > 0 {
		rv.Toast += fmt.Sprintf(", %d failed", rv.Failed)
	}
	v.Title, v.Crumbs = bulkTitle, bulkCrumbs
	h.show(w, r, http.StatusOK, "bulk_power", v, bulkResultPage(rv))
}
