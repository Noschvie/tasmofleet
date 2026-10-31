package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// firmwareHoldTimeout is how long a typed firmware command holds the device for update starts at most. The
// device downloads and flashes for about a minute; ten minutes cover a slow link and a slow reboot, and a
// missed restart (the report was lost, the device has no MQTT) does not hold it for ever.
const firmwareHoldTimeout = 10 * time.Minute

// holdingCmd records every typed firmware command (cmdpolicy.StartsFirmware) that reached a device, from
// the console and from bulk command jobs alike: both send through this service. Why: the command's job
// ends as soon as the device answers, but the device keeps flashing; the hold keeps a TasmoFleet update
// from starting a second update on it (#347).
type holdingCmd struct {
	devicecmd.Service
	holds *devlock.FirmwareHolds
	// uptime is the uptime the health tracker has cached for a device, the baseline of the hold.
	uptime func(model.DeviceID) int64
}

func (c holdingCmd) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	raw, err := c.Service.Run(ctx, p, id, cmd)
	// An unknown outcome (a timeout over HTTP, a lost reply over MQTT, an unconfirmed publish) may have
	// reached the device, so it holds too: holding too long costs ten minutes, too short risks a brick.
	// Only what certainly never left TasmoFleet does not.
	if !neverSent(err) && cmdpolicy.StartsFirmwareUpgrade(cmd) {
		var up int64
		if c.uptime != nil {
			up = c.uptime(id)
		}
		c.holds.Start(id, up)
	}
	return raw, err
}

// neverSent reports whether err says the command certainly did not leave TasmoFleet: a policy refusal, a
// busy device, a vanished inventory entry, a refused cut-off.
func neverSent(err error) bool {
	return err != nil && (errors.Is(err, cmdpolicy.ErrInvalid) || errors.Is(err, cmdpolicy.ErrForbidden) || errors.Is(err, cmdpolicy.ErrRole) ||
		errors.Is(err, cmdpolicy.ErrUpdateJob) || errors.Is(err, devlock.ErrBusy) || errors.Is(err, store.ErrNotFound) || errors.Is(err, transport.ErrCutOff))
}

// observeFirmwareHolds feeds the health tracker's reports to holds until ctx is done: a restarted device
// ends its hold, unless it runs minimal firmware (a two-stage update: the first restart only brings up the
// minimal image, which then flashes the real one). A lost report (a resync marker) only delays that until the timeout.
func observeFirmwareHolds(ctx context.Context, hub *events.Hub, holds *devlock.FirmwareHolds, minimal func(model.DeviceID) bool) {
	ch, cancel := hub.Subscribe(events.TopicHealth)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if h, isHealth := ev.Payload.(model.Health); isHealth {
				holds.Observe(h.DeviceID, h.LWT, h.UptimeSec, minimal != nil && minimal(h.DeviceID))
			}
		}
	}
}

// fwHoldReason is the refusal of an update start on a held device.
func fwHoldReason(since, now time.Time, timeout time.Duration) string {
	ago := "less than a minute ago"
	if m := int(now.Sub(since) / time.Minute); m >= 1 {
		ago = fmt.Sprintf("%d min ago", m)
	}
	return fmt.Sprintf("A firmware update typed in the console or sent as a bulk command %s is probably still running on this device; wait until it has restarted (TasmoFleet stops waiting after %d min). Two updates at once can leave the device unusable", ago, int(timeout/time.Minute))
}

// heldReasons returns the refusal text of every device of ids that a typed firmware command holds.
func (g deviceGuard) heldReasons(ids []model.DeviceID) map[model.DeviceID]string {
	if g.holds == nil {
		return nil
	}
	var out map[model.DeviceID]string
	for _, id := range ids {
		if since, ok := g.holds.Holding(id); ok {
			if out == nil {
				out = map[model.DeviceID]string{}
			}
			out[id] = fwHoldReason(since, g.holds.Now(), g.holds.Timeout())
		}
	}
	return out
}

// refuseHeld returns the RefusedError for the held devices among ids, or nil.
func (g deviceGuard) refuseHeld(ids []model.DeviceID) error {
	reasons := g.heldReasons(ids)
	if len(reasons) == 0 {
		return nil
	}
	var refused []ota.Refusal
	for _, id := range ids {
		if r, ok := reasons[id]; ok {
			refused = append(refused, ota.Refusal{DeviceID: id, Reason: r})
			delete(reasons, id) // once per device
		}
	}
	return &ota.RefusedError{Refused: refused}
}
