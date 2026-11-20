package app

import (
	"time"

	"github.com/hellwand/tasmofleet/internal/devicecmd"
	"github.com/hellwand/tasmofleet/internal/devlock"
)

// setupDeviceCmd builds the power toggle and console service. MQTT is used
// for switching only when the bus exists and the router's write decision
// allows it (transport.Router.WritesOverMQTT, #1053); the service falls back
// to HTTP.
func (a *runtime) setupDeviceCmd() {
	o := devicecmd.Options{Devices: a.svc.Inventory, Client: a.svc.Client, Audit: a.svc.Audit, Health: a.tracker}
	if a.mqttCmd != nil {
		o.MQTT = a.mqttCmd
		router := a.svc.Router
		o.WritesOverMQTT = router.WritesOverMQTT
		o.NoteUnanswered = router.NoteUnansweredWrite // the mark WritesOverMQTT reads through the facts (#842)
	}
	a.fwHolds = devlock.NewFirmwareHolds(time.Now, firmwareHoldTimeout)
	a.deviceCmd = holdingCmd{Service: devicecmd.New(o), holds: a.fwHolds, uptime: a.cachedUptime, asks: a.restartAsks, onBroker: a.brokerHost.later, onWrite: a.settingsWritten,
		onGroupTopic: a.forgetGroupTopics, onIdentity: a.brokerHost.identityWritten}
	a.goBG("firmware-holds", func() error { observeFirmwareHolds(a.bgContext, a.svc.Hub, a.fwHolds, a.tracker.Minimal); return nil })
}
