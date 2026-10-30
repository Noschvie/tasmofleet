package restore

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// mqttLockout says why a restore over MQTT could cut TasmoFleet off from the device: the backup
// changes how the device reaches the broker or under which topic it is addressed, and TasmoFleet
// reaches this device only over MQTT. "" when nothing in the backup differs. Names only, never
// values (the password is one of the fields).
func (m *Manager) mqttLockout(ctx context.Context, dev model.Device, b model.Backup) string {
	const tail = "; TasmoFleet reaches the device only over MQTT and may lose it"
	dec, err := m.decodedOf(ctx, b.ID)
	if err != nil || dec == nil {
		return "the backup cannot be read, so its MQTT settings cannot be compared with the device's" + tail
	}
	var d struct {
		Topic     *string `json:"mqtt_topic"`
		FullTopic *string `json:"mqtt_fulltopic"`
		GrpTopic  *string `json:"mqtt_grptopic"`
		Host      *string `json:"mqtt_host"`
		Port      *int    `json:"mqtt_port"`
		User      *string `json:"mqtt_user"`
		Pwd       *string `json:"mqtt_pwd"`
	}
	if json.Unmarshal(dec, &d) != nil {
		return "the backup cannot be read, so its MQTT settings cannot be compared with the device's" + tail
	}
	var diff []string
	add := func(name string, differs bool) {
		if differs {
			diff = append(diff, name)
		}
	}
	// A field the backup does not hold is not compared; one the device side does not know is not either,
	// except the topic and the user, which TasmoFleet can always read.
	add("Topic", d.Topic != nil && *d.Topic != dev.Topic)
	add("FullTopic", d.FullTopic != nil && fullTopic(*d.FullTopic) != fullTopic(dev.FullTopic))
	add("GroupTopic", d.GrpTopic != nil && dev.GroupTopic != "" && *d.GrpTopic != dev.GroupTopic)
	add("MqttHost", d.Host != nil && dev.MQTTHost != "" && !strings.EqualFold(*d.Host, dev.MQTTHost))
	add("MqttPort", d.Port != nil && dev.MQTTPort != 0 && *d.Port != dev.MQTTPort)
	if d.User != nil {
		add("MqttUser", m.liveMqttUser(ctx, dev) != *d.User)
	}
	if d.Pwd != nil {
		add("MqttPassword", differsPassword(*d.Pwd, dev, m.o.BrokerPassword))
	}
	if len(diff) == 0 {
		return ""
	}
	return "this backup changes " + strings.Join(diff, ", ") + tail
}

// fullTopic normalises a FullTopic: empty is Tasmota's default, a trailing '/' is not significant.
func fullTopic(s string) string {
	if s == "" {
		s = "%prefix%/%topic%/"
	}
	return strings.TrimRight(s, "/")
}

// differsPassword compares the backup's MqttPassword with the one TasmoFleet knows for the device:
// its own stored one, or the broker password in broker mode. Without either there is nothing to
// compare against, and the transport refuses such a device anyway.
func differsPassword(backup string, dev model.Device, broker func() string) bool {
	switch dev.MQTTPasswordMode {
	case model.MQTTPasswordOwn:
		return backup != dev.MQTTPassword
	case model.MQTTPasswordBroker:
		if broker != nil && broker() != "" {
			return backup != broker()
		}
	case model.MQTTPasswordNone:
		// nothing to compare against
	}
	return false
}

// liveMqttUser reads MqttUser from the device; a device that does not answer gives "\x00" so that
// the comparison flags it (the user could not be checked).
func (m *Manager) liveMqttUser(ctx context.Context, dev model.Device) string {
	raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(dev), "MqttUser")
	if err != nil {
		return "\x00"
	}
	var r struct{ MqttUser *string }
	if json.Unmarshal(raw, &r) != nil || r.MqttUser == nil {
		return "\x00"
	}
	return *r.MqttUser
}
