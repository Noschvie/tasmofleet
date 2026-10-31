package app

import (
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// setupDeviceCmd builds the power toggle and console service. MQTT is used
// for switching only when the bus exists; the service falls back to HTTP.
func (a *runtime) setupDeviceCmd() {
	o := devicecmd.Options{Devices: a.svc.Inventory, Client: a.svc.Client, Audit: a.svc.Audit, Health: a.tracker}
	if a.mqttCmd != nil {
		o.MQTT = a.mqttCmd
	}
	a.fwHolds = devlock.NewFirmwareHolds(time.Now, firmwareHoldTimeout)
	up := func(id model.DeviceID) int64 { return a.tracker.Get(id).UptimeSec }
	a.deviceCmd = holdingCmd{Service: devicecmd.New(o), holds: a.fwHolds, uptime: up}
	a.goBG("firmware-holds", func() error { observeFirmwareHolds(a.bgContext, a.svc.Hub, a.fwHolds); return nil })
}
