package app

import (
	"context"
	"time"

	"github.com/hellwand/tasmofleet/internal/devicecmd"
	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// setupDeviceCmd builds the power toggle and console service. MQTT is used
// for switching only when the bus exists and the write rule allows it
// (writesOverMQTT); the service falls back to HTTP.
func (a *runtime) setupDeviceCmd() {
	o := devicecmd.Options{Devices: a.svc.Inventory, Client: a.svc.Client, Audit: a.svc.Audit, Health: a.tracker}
	if a.mqttCmd != nil {
		o.MQTT = a.mqttCmd
		o.WritesOverMQTT = a.writesOverMQTT
	}
	a.fwHolds = devlock.NewFirmwareHolds(time.Now, firmwareHoldTimeout)
	a.deviceCmd = holdingCmd{Service: devicecmd.New(o), holds: a.fwHolds, uptime: a.cachedUptime, asks: a.restartAsks, onBroker: a.brokerHost.later, onWrite: a.settingsWritten}
	a.goBG("firmware-holds", func() error { observeFirmwareHolds(a.bgContext, a.svc.Hub, a.fwHolds, a.tracker.Minimal); return nil })
}

// writesOverMQTT reports whether a change of d goes over MQTT under the router's write rule (#605): the writes
// path Choose gives for the device's facts (usable on our broker under the strict rule: its own GroupTopic read
// and no other device's unread; not while its MQTT reads go unanswered and HTTP works). The power toggle asks
// it, so a switch is published only where any other change would be. Without facts: no.
func (a *runtime) writesOverMQTT(ctx context.Context, d model.Device) bool {
	if a.facts == nil {
		return false
	}
	f, err := a.facts.FactsFor(ctx, d)
	return err == nil && transport.Choose(f).Writes == transport.MQTT
}
