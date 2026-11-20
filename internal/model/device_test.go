package model

import "testing"

// Why (#289): the CR warning compares used against available characters of the settings text
// area; a CR that does not parse must give no number rather than a wrong percentage.
func TestFWRFactsCRUsage(t *testing.T) {
	for _, tt := range []struct {
		cr          string
		used, total int
		ok          bool
	}{
		{"650/699", 650, 699, true},
		{"0/699", 0, 699, true},
		{"", 0, 0, false},
		{"650", 0, 0, false},
		{"a/b", 0, 0, false},
		{"5/0", 5, 0, false},
	} {
		used, total, ok := FWRFacts{CR: tt.cr}.CRUsage()
		if used != tt.used || total != tt.total || ok != tt.ok {
			t.Errorf("CR %q: got %d/%d ok=%v, want %d/%d ok=%v", tt.cr, used, total, ok, tt.used, tt.total, tt.ok)
		}
	}
}

// Why (#1053): "on our broker" is the first question of every MQTT read and write and of the power toggle. A device
// is reached there only when it reports to TasmoFleet's broker and has a topic to send to; a device on another
// broker, one whose broker is not known yet, or one without a topic is reached over HTTP and needs no MQTT facts.
func TestOnOurBroker(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Device
		want bool
	}{
		{"our broker, topic set", Device{MQTT: MQTTOurs, Topic: "plug"}, true},
		{"our broker, no topic", Device{MQTT: MQTTOurs}, false},
		{"another broker, topic set", Device{MQTT: MQTTOtherBroker, Topic: "plug"}, false},
		{"broker not known yet, topic set", Device{MQTT: MQTTUnknown, Topic: "plug"}, false},
		{"no MQTT, topic set", Device{MQTT: MQTTNone, Topic: "plug"}, false},
	} {
		if got := tc.d.OnOurBroker(); got != tc.want {
			t.Errorf("%s: OnOurBroker = %v, want %v", tc.name, got, tc.want)
		}
	}
}
