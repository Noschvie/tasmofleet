package web

import (
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// The sentences of the broker ACL notice (#840): the lead and the link are the same everywhere (ui.BrokerACLLead,
// ui.BrokerACLDocURL); each place adds the one sentence that says what it puts on the broker.
const (
	// aclTextBroker stands on the page where the broker is configured.
	aclTextBroker = "TasmoFleet sends commands, changed settings, device passwords and whole configuration backups through this broker, " +
		"and the broker delivers them to every client that may subscribe to a device's topics: Home Assistant, Node-RED, " +
		"MQTT Explorer, other devices. TLS to the broker does not change that, because the device's own leg is plain. " +
		"Only an ACL that keeps the other clients away from the devices' commands and backups stops that."
	// aclTextDevicePassword stands by the device's MQTT password.
	aclTextDevicePassword = "A backup or restore over MQTT sends this password in its start command and then the device's whole configuration, " +
		"every password in it included, through the broker. Without an ACL every client of the broker can read both."
	// aclTextConsole stands on the confirmation of a console or bulk command that goes over MQTT.
	aclTextConsole = "This command, with any password in it, goes through the broker. Unless the broker has an ACL, every client of the broker can read it."
	// aclTextRestore stands on the confirmation of a restore over MQTT.
	aclTextRestore = "This restore sends the device's whole configuration, every password in it, through the broker. " +
		"Unless the broker has an ACL, every client of the broker can read it."
	// aclTextBackup stands on the page that starts backups.
	aclTextBackup = "A backup of a device that is online on the broker travels over MQTT: the device's whole configuration, every password in it, " +
		"passes through the broker. Unless the broker has an ACL, every client of the broker can read it."
)

// aclTextFor is text when at least one of the devices' routes is MQTT, and empty otherwise: the notice stands only
// where a command really goes through the broker.
func aclTextFor(routes map[model.DeviceID]transport.Path, text string) string {
	for _, p := range routes {
		if p == transport.MQTT {
			return text
		}
	}
	return ""
}
