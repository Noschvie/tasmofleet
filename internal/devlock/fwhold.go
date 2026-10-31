package devlock

import (
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// FirmwareHolds remembers the devices on which a person has just started a firmware update with a typed
// command (console or bulk "Upgrade 1" / "OtaUrl"). That command's job ends as soon as the device
// answers, but the device keeps downloading and flashing for about a minute. A TasmoFleet update started
// in that window would be a second update on the same device, which can brick it. So the device is held
// for update starts until it has been seen to restart, or until the timeout has passed.
//
// The state is in memory on purpose: a TasmoFleet restart forgets the holds. That is proportionate for a
// one-minute window; the typed command stays the person's responsibility (docs: cmdpolicy.whyFirmware).
type FirmwareHolds struct {
	mu      sync.Mutex
	now     func() time.Time
	timeout time.Duration
	held    map[model.DeviceID]*fwHold
}

type fwHold struct {
	at         time.Time
	uptime     int64 // the uptime the tracker had cached when the command was sent (0: none)
	sawOffline bool  // an LWT "Offline" was seen since
}

// uptimeSlack absorbs the one-second resolution of Tasmota's uptime and the delay of a report.
const uptimeSlack = 2 * time.Second

// NewFirmwareHolds returns an empty set. now is the clock; timeout is how long a hold lasts at most.
func NewFirmwareHolds(now func() time.Time, timeout time.Duration) *FirmwareHolds {
	return &FirmwareHolds{now: now, timeout: timeout, held: map[model.DeviceID]*fwHold{}}
}

// Now is the clock of the holds.
func (h *FirmwareHolds) Now() time.Time { return h.now() }

// Timeout is how long a hold lasts at most.
func (h *FirmwareHolds) Timeout() time.Duration { return h.timeout }

// Start holds id from now. uptimeSec is the uptime the health tracker has cached for the device (0 when
// none): a later report of the same value is the old one repeated, not a new reading.
func (h *FirmwareHolds) Start(id model.DeviceID, uptimeSec int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[id] = &fwHold{at: h.now(), uptime: uptimeSec}
}

// Holding reports whether id is held, and since when. A hold that has outlasted the timeout is gone.
func (h *FirmwareHolds) Holding(id model.DeviceID) (since time.Time, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[id]
	if hd == nil {
		return time.Time{}, false
	}
	if h.now().Sub(hd.at) >= h.timeout {
		delete(h.held, id)
		return time.Time{}, false
	}
	return hd.at, true
}

// Observe feeds what the health tracker reports for a device: its LWT ("Online", "Offline" or "") and its
// uptime, and whether it runs minimal firmware. The hold ends when the device has restarted: an LWT Offline
// followed by Online, or an uptime reading that says the device booted after the command. An unchanged
// uptime is the tracker's cached value, not a reading, and says nothing. While the device runs minimal
// firmware nothing ends the hold: a two-stage update restarts into the minimal image first, which then
// flashes the real one and restarts again.
func (h *FirmwareHolds) Observe(id model.DeviceID, lwt string, uptimeSec int64, minimal bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hd := h.held[id]
	if hd == nil {
		return
	}
	switch {
	case strings.EqualFold(lwt, "offline"):
		hd.sawOffline = true
	case strings.EqualFold(lwt, "online") && hd.sawOffline && !minimal:
		delete(h.held, id)
		return
	}
	if minimal {
		return
	}
	if uptimeSec <= 0 || uptimeSec == hd.uptime {
		return
	}
	elapsed := h.now().Sub(hd.at)
	// A reading shorter than the time since the command says the device booted after it. A reading merely
	// lower than the cached one proves nothing: a poll that began earlier can arrive late.
	if time.Duration(uptimeSec)*time.Second+uptimeSlack < elapsed {
		delete(h.held, id)
		return
	}
	hd.uptime = uptimeSec
}
