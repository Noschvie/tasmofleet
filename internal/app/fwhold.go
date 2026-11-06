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

// firmwareHoldTimeout is how long a typed firmware command (or an update job that found its device flashing
// from another URL, #504) holds the device for update starts at most. The
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
	// asks is told of every command that restarts a device (cmdpolicy.RestartsDevice), so that the restart
	// that follows is stored as expected and does not count towards a boot loop (#325).
	asks *restartAsks
	// onBroker is told of a device that was sent MqttHost or MqttPort and answered (#598); nil: nobody.
	onBroker func(model.DeviceID)
	// onWrite is told of every command that may have changed a setting of a device (#662); nil: nobody.
	onWrite func(model.DeviceID)
}

func (c holdingCmd) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	raw, err := c.Service.Run(ctx, p, id, cmd)
	// An unknown outcome (a timeout over HTTP, a lost reply over MQTT, an unconfirmed publish) may have
	// reached the device, so it holds too: holding too long costs ten minutes, too short risks a brick.
	// Only what certainly never left TasmoFleet does not.
	if c.asks != nil && !neverSent(err) && cmdpolicy.RestartsDevice(cmd) {
		c.asks.Note(id)
	}
	if c.onWrite != nil && !neverSent(err) && writesSettings(cmd) {
		c.onWrite(id)
	}
	if c.onBroker != nil && err == nil && setsBrokerTarget(cmd) {
		c.onBroker(id)
	}
	if !neverSent(err) && cmdpolicy.StartsFirmwareUpgrade(cmd) {
		startHold(c.holds, c.uptime, id)
	}
	return raw, err
}

// writesSettings reports whether cmd may change a setting of the device: the console's admin class (queries
// and the harmless actions Power and Restart change none). A line the policy refuses was never sent.
func writesSettings(cmd string) bool {
	v, err := cmdpolicy.Classify(cmd)
	return err == nil && v.Class == cmdpolicy.ClassAdmin
}

// settingsWritten tells the Settings service that a writer other than its own Apply sent to the device (the
// console, Make compliant): a read that started before the write is not remembered as current (#662).
func (a *runtime) settingsWritten(id model.DeviceID) {
	if a.settings != nil {
		a.settings.Forget(id)
	}
}

// startHold holds id for update starts from now, with the uptime the health tracker has cached for it as the
// baseline (uptime may be nil: no baseline).
func startHold(holds *devlock.FirmwareHolds, uptime func(model.DeviceID) int64, id model.DeviceID) {
	var up int64
	if uptime != nil {
		up = uptime(id)
	}
	holds.Start(id, up)
}

// cachedUptime is the uptime the health tracker has cached for a device, the baseline of its hold.
func (a *runtime) cachedUptime(id model.DeviceID) int64 { return a.tracker.Get(id).UptimeSec }

// holdFlashing is ota.Options.HoldFlashing: an update job found that its device flashes from another URL
// and is about to fail, which releases the device. The hold keeps the next update away from it until it has
// restarted, as it does after a typed firmware command (#504).
func (a *runtime) holdFlashing(id model.DeviceID) { startHold(a.fwHolds, a.cachedUptime, id) }

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
	return fmt.Sprintf("A firmware update typed in the console, sent as a bulk command or started by another tool %s is probably still running on this device; wait until it has restarted (TasmoFleet stops waiting after %d min). Two updates at once can leave the device unusable", ago, int(timeout/time.Minute))
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
