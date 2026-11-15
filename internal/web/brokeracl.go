package web

import (
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/config"
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

// aclTextFor is text when at least one of the devices' routes is MQTT and the notice applies (brokerACLOpen), and empty
// otherwise: the notice stands only where a command really goes through the broker.
func (h *handler) aclTextFor(routes map[model.DeviceID]transport.Path, text string) string {
	if !h.brokerACLOpen() {
		return ""
	}
	for _, p := range routes {
		if p == transport.MQTT {
			return text
		}
	}
	return ""
}

// brokerConfigured says a broker is configured, so whatever TasmoFleet sends through MQTT can reach every client of it.
func (h *handler) brokerConfigured() bool {
	return h.d.SysConfig != nil && h.storedMQTTURL() != ""
}

// aclBinding names the broker an admin's confirmation is given for (#880): host, port and user. The scheme is left out
// (TLS to the broker does not change who may read the topics); a port that is left empty is the scheme's default.
func aclBinding(c config.Config) string {
	b, err := config.SplitMQTTURL(c.MQTTURL)
	if err != nil || b.Host == "" {
		return ""
	}
	return strings.ToLower(b.Host) + ":" + strconv.Itoa(b.Port) + "|" + c.MQTTUser
}

// mqttSettings is the broker, its user and the ACL confirmation as the settings service holds them now.
func (h *handler) mqttSettings() config.Config {
	var c config.Config
	for _, f := range h.d.SysConfig.State() {
		switch f.Key {
		case "MQTT_URL":
			c.MQTTURL = f.Value
		case "MQTT_USER":
			c.MQTTUser = f.Value
		case "MQTT_ACL_CONFIRMED":
			c.MQTTACLConfirmed = f.Value
		}
	}
	return c
}

// aclConfirmed says an admin confirmed that the broker in use has an ACL (System, MQTT) and that broker is still the
// one: another host, port or user makes the confirmation void without anyone having to remove it.
func (h *handler) aclConfirmed() bool {
	if h.d.SysConfig == nil {
		return false
	}
	c := h.mqttSettings()
	return c.MQTTACLConfirmed != "" && c.MQTTACLConfirmed == aclBinding(c)
}

// brokerACLOpen is the one condition of every place the broker ACL notice shows (#880): a broker is configured and
// nobody confirmed that it has an ACL. The places add their own path condition (the command, the backup or the restore
// goes over MQTT) before they ask.
func (h *handler) brokerACLOpen() bool {
	return h.brokerConfigured() && !h.aclConfirmed()
}

// backupACLText is the sentence of the notice by what starts a backup (#853), empty without a broker or after the
// confirmation: the backup buttons of the fleet and the bulk backup page share it.
func (h *handler) backupACLText() string {
	if h.brokerACLOpen() {
		return aclTextBackup
	}
	return ""
}
