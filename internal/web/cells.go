package web

import (
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Helpers of the live cells (cells.templ).

// staleWindow is the health stale window: the setting when it is wired, else the health
// default (2 x TelePeriod + 60 s).
func (h *handler) staleWindow() time.Duration {
	if h.d.StaleAfter != nil {
		return h.d.StaleAfter()
	}
	return 11 * time.Minute
}

// channels is the MQTT and HTTP reach of a device now.
func (h *handler) channels(hl model.Health) []channelInfo {
	return channelsOf(h.d.Clock.Now(), h.staleWindow(), hl)
}

// reachableNow says whether the health tracker counts the device as answering on some transport
// (HTTP or MQTT): online, or stale (silent for a moment, still being polled). Everything that
// needs a live device asks this, so an MQTT-only device that is online stays usable. Offline,
// unknown (never answered) and a refused login are not.
func reachableNow(st model.HealthState) bool {
	return st == model.HealthOnline || st == model.HealthStale
}

// mqttDown: the device answers, but not over MQTT (the "MQTT down" badge).
func (h *handler) mqttDown(hl model.Health) bool {
	return hl.State == model.HealthOnline && h.channels(hl)[0].Tone == "bad"
}

// statusBadgeOf is the connection status of a device as ui.StatusBadge draws it: the health state is the machine word
// (data-state), its label the word and stateTone the colour. The device list, its phone cards, the device header and
// the dashboard's attention list all build it here, so the one status reads the same on every page (#1097).
func statusBadgeOf(state model.HealthState, title string) ui.StatusBadgeData {
	return ui.StatusBadgeData{State: string(state), Tone: stateTone(state), Label: label(state), Title: title}
}

// hint is the tooltip of a state or tag, why the short reason shown as text beside the state (#1073).
func (h *handler) hint(v any) string { return hintText(v, h.d.StaleAfter) }
func (h *handler) why(v any) string  { return hintWhy(v, h.d.StaleAfter) }

// ago is the compact relative time of t.
func (h *handler) ago(t time.Time) string { return ago(h.d.Clock.Now(), t) }

// stamp is a full timestamp for tooltips.
func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05 MST") }

// seenText is the Uptime column of a device that is down: "seen now", "seen 3h ago".
func seenText(ago string) string {
	if ago == "now" {
		return "seen now"
	}
	return "seen " + ago + " ago"
}

// devID is a device id in a URL or an attribute.
func devID(id model.DeviceID) string { return strconv.FormatInt(int64(id), 10) }

// hostPort is a device address with its port when one is set.
func hostPort(address string, port int) string {
	if port != 0 {
		return address + ":" + strconv.Itoa(port)
	}
	return address
}

// firmwareTitle is the tooltip of the firmware column: the version and its build variant.
func firmwareTitle(id model.Identity) string {
	s := "Tasmota " + id.Version
	if id.Variant != "" {
		s += " (" + id.Variant + ")"
	}
	return s
}

// nameID is the id of a device's name link in the list: the switches of its row are named through it.
func nameID(id model.DeviceID) string { return "dvn-" + devID(id) }

// powerLabelledBy is the aria-labelledby of a power switch: the device name, then "relay N" (rly-0, "power", when
// the relays are not known). The texts are rendered once per page (devicesTableBody), the name is the row's own.
func powerLabelledBy(id model.DeviceID, relay int) string {
	return nameID(id) + " rly-" + strconv.Itoa(relay)
}

// powerSwitchAttrs are the attributes htmx and the hover text need on a row's power switch: the toggle goes to the
// device's power route and the answer replaces the .pcell the switch sits in (the card's and the row's alike).
func powerSwitchAttrs(p powerCell, rl relayState) templ.Attributes {
	return templ.Attributes{
		"title":           relayTitle(p, rl),
		"hx-post":         p.Base + "/devices/" + strconv.FormatInt(int64(p.ID), 10) + "/power/" + strconv.Itoa(rl.N),
		"hx-vals":         `{"action":"TOGGLE"}`,
		"hx-target":       "closest .pcell",
		"hx-swap":         "innerHTML",
		"hx-disabled-elt": "this",
	}
}

// relayTitle is the tooltip of a relay switch.
func relayTitle(p powerCell, rl relayState) string {
	switch {
	case p.Off:
		return "The device does not answer now"
	case rl.State != "ON" && rl.State != "OFF":
		return "State unknown. Click to toggle relay " + strconv.Itoa(rl.N)
	}
	return "Relay " + strconv.Itoa(rl.N) + " is " + label(rl.State) + ". Click to toggle it"
}

// notice is the one-line result above a list (listNotice).
type notice struct{ Notice, Error string }
