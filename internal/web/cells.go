package web

import (
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
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

// mqttDown: the device answers, but not over MQTT (the "MQTT down" badge).
func (h *handler) mqttDown(hl model.Health) bool {
	return hl.State == model.HealthOnline && h.channels(hl)[0].Tone == "bad"
}

// hint is the tooltip of a state or tag.
func (h *handler) hint(v any) string { return hintText(v, h.d.StaleAfter) }

// ago is the compact relative time of t.
func (h *handler) ago(t time.Time) string { return ago(h.d.Clock.Now(), t) }

// stamp is a full timestamp for tooltips.
func stamp(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") }

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
