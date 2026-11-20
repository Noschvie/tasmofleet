package web

import (
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// pwBrokerTestWarning is the sentence of the dialog and the page that give the broker password to several devices:
// the test publishes TasmoFleet's own login where the device's own account may read it.
const pwBrokerTestWarning = "The test publishes TasmoFleet's own broker password in each ticked device's command topic; every client that may read " +
	"that topic, the device's own MQTT login included, can read it."

// aclFor says the broker ACL notice (ui.BrokerACLNotice, one short warn banner, the same everywhere, #1078) stands:
// at least one of the devices' routes is MQTT and the notice applies (brokerACLOpen). The notice stands only where a
// command really goes through the broker, never on one that goes over HTTP.
func (h *handler) aclFor(routes map[model.DeviceID]transport.Path) bool {
	if !h.brokerACLOpen() {
		return false
	}
	for _, p := range routes {
		if p == transport.MQTT {
			return true
		}
	}
	return false
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

// aclWarningHidden says an admin hid the broker ACL warning (System, MQTT) and that broker is still the one: another
// host, port or user makes it void without anyone having to remove it.
func (h *handler) aclWarningHidden() bool {
	if h.d.SysConfig == nil {
		return false
	}
	c := h.mqttSettings()
	return c.MQTTACLWarningHidden != "" && c.MQTTACLWarningHidden == aclBinding(c)
}

// brokerACLOpen is the one condition of every place the broker ACL notice shows (#880, #1078): a broker is configured
// and nobody hid the warning for it. The places add their own path condition (the command or the backup goes over
// MQTT) before they ask; the dashboard's Needs attention item and System, MQTT ask it alone.
func (h *handler) brokerACLOpen() bool {
	return h.brokerConfigured() && !h.aclWarningHidden()
}

// aclHideID is the id of the option that hides the warning on System, MQTT.
const aclHideID = "mqtt-acl-hide"
