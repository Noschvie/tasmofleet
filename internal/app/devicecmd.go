package app

import (
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
)

// setupDeviceCmd builds the power toggle and console service. MQTT is used
// for switching only when the bus exists; the service falls back to HTTP.
func (a *runtime) setupDeviceCmd() {
	o := devicecmd.Options{Devices: a.svc.Inventory, Client: a.svc.Client, Audit: a.svc.Audit, Health: a.tracker}
	if a.bus != nil {
		o.Bus = a.bus
	}
	a.deviceCmd = devicecmd.New(o)
}
