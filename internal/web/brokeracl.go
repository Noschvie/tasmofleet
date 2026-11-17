package web

import (
	"context"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/httpx"
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
	aclTextDevicePassword = "A backup or restore over MQTT sends this password in the start command of the dump and of every file, then the " +
		"device's whole configuration, every password in it included, and on a firmware that sends files over MQTT its files, " +
		"through the broker. Without an ACL every client of the broker can read all of it."
	// aclTextConsole stands on the confirmation of a console or bulk command that goes over MQTT.
	aclTextConsole = "This command, with any password in it, goes through the broker. Unless the broker has an ACL, every client of the broker can read it."
	// aclTextRestore stands on the confirmation of a restore over MQTT.
	aclTextRestore = "This restore sends the device's whole configuration, every password in it, through the broker. " +
		"Unless the broker has an ACL, every client of the broker can read it."
	// aclTextPasswordTest stands on the page that gives the broker password to several devices and tests each one.
	aclTextPasswordTest = "Each password test sends the device's start command with its password and, when the device accepts it, the device's " +
		"settings (every password in them) in one chunk through the broker. Unless the broker has an ACL, every client of the broker can read them."
	// aclTextBackup stands on the page that starts backups.
	aclTextBackup = "A backup of a device that is online on the broker travels over MQTT: the device's whole configuration, every password in it, " +
		"and, on a firmware that sends files over MQTT, its files (Berry scripts with their tokens, the TLS private key) pass through " +
		"the broker. Unless the broker has an ACL, every client of the broker can read them."
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

// aclBinding names the broker the hidden warning is given for (#880, #962): host, port and user. The scheme is left out
// (TLS to the broker does not change who may read the topics); a port that is left empty is the scheme's default.
func aclBinding(c config.Config) string {
	b, err := config.SplitMQTTURL(c.MQTTURL)
	if err != nil || b.Host == "" {
		return ""
	}
	return strings.ToLower(b.Host) + ":" + strconv.Itoa(b.Port) + "|" + c.MQTTUser
}

// mqttSettings is the broker, its user and the hidden ACL warning as the settings service holds them now.
func (h *handler) mqttSettings() config.Config {
	var c config.Config
	for _, f := range h.d.SysConfig.State() {
		switch f.Key {
		case "MQTT_URL":
			c.MQTTURL = f.Value
		case "MQTT_USER":
			c.MQTTUser = f.Value
		case "MQTT_ACL_WARNING_HIDDEN":
			c.MQTTACLWarningHidden = f.Value
		}
	}
	return c
}

// aclWarningHidden says an admin hid the red ACL warning (System, MQTT) and that broker is still the one: another
// host, port or user makes it void without anyone having to remove it.
func (h *handler) aclWarningHidden() bool {
	if h.d.SysConfig == nil {
		return false
	}
	c := h.mqttSettings()
	return c.MQTTACLWarningHidden != "" && c.MQTTACLWarningHidden == aclBinding(c)
}

// brokerACLOpen is the one condition of every place the broker ACL notice shows (#880): a broker is configured and
// nobody hid the warning for it. The places add their own path condition (the command, the backup or the restore
// goes over MQTT) before they ask.
func (h *handler) brokerACLOpen() bool {
	return h.brokerConfigured() && !h.aclWarningHidden()
}

// aclHideID is the id of the option that hides the warning on System, MQTT: the target of the notice's link.
const aclHideID = "mqtt-acl-hide"

// aclHideHref is where the notice's "Hide it" leads (#962): straight to the option on System, MQTT, which the CSS
// scrolls to and highlights (:target). Only an admin gets it, as only an admin may save the setting; everyone else,
// and a request without a principal, gets "" and so plain text, never a link into a page they cannot use. It reads the
// principal and the base path from the request context, the same two sources h.view uses, so the pages that show the
// notice need no extra fields and the role decision is made in one place.
func aclHideHref(ctx context.Context) string {
	if p, ok := auth.FromContext(ctx); !ok || p.Role < model.RoleAdmin {
		return ""
	}
	return httpx.BasePath(ctx) + "/settings/system?tab=mqtt#" + aclHideID
}

// backupACLText is the sentence of the notice by what starts a backup (#853), empty without a broker or after the
// warning was hidden: the backup buttons of the fleet and the bulk backup page share it.
func (h *handler) backupACLText() string {
	if h.brokerACLOpen() {
		return aclTextBackup
	}
	return ""
}
