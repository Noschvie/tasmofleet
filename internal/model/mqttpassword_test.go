package model

import "testing"

// Why (#609): TasmoFleet learns a device's MqttPassword from its backup. The value decides the
// mode: an empty password must become the empty mode (a transfer then sends ""), not "not set",
// which blocks every transfer; a value equal to the broker password must become broker mode, so
// no copy of the broker password is stored and a later change of it follows; anything else is the
// device's own. An empty broker password must never turn an empty device password into broker mode.
func TestTheBackupsValueDecidesTheMode(t *testing.T) {
	for _, tt := range []struct {
		v, broker string
		want      MQTTPasswordMode
	}{
		{"", "brokerpw", MQTTPasswordEmpty},
		{"brokerpw", "brokerpw", MQTTPasswordBroker},
		{"devicepw", "brokerpw", MQTTPasswordOwn},
		{"", "", MQTTPasswordEmpty},
		{"devicepw", "", MQTTPasswordOwn},
	} {
		if got := MQTTPasswordModeFor(tt.v, tt.broker); got != tt.want {
			t.Errorf("MQTTPasswordModeFor(%q, %q) = %q, want %q", tt.v, tt.broker, got, tt.want)
		}
	}
}

// Why (#609): FilePassword is the one place that maps a mode to the password a transfer sends. The
// empty mode sends "" and counts as a password; not set, an unknown mode, an own mode without a
// value and broker mode without a broker password give none, so nothing is sent with a wrong one.
func TestEachModeSendsItsPassword(t *testing.T) {
	for _, tt := range []struct {
		name   string
		d      Device
		broker string
		want   string
		ok     bool
	}{
		{"empty", Device{MQTTPasswordMode: MQTTPasswordEmpty, MQTTPassword: "stale"}, "brokerpw", "", true},
		{"own", Device{MQTTPasswordMode: MQTTPasswordOwn, MQTTPassword: "devicepw"}, "brokerpw", "devicepw", true},
		{"own without a value", Device{MQTTPasswordMode: MQTTPasswordOwn}, "brokerpw", "", false},
		{"broker", Device{MQTTPasswordMode: MQTTPasswordBroker}, "brokerpw", "brokerpw", true},
		{"broker without a broker password", Device{MQTTPasswordMode: MQTTPasswordBroker}, "", "", false},
		{"not set", Device{MQTTPassword: "stale"}, "brokerpw", "", false},
		{"unknown mode", Device{MQTTPasswordMode: "bogus", MQTTPassword: "x"}, "brokerpw", "", false},
	} {
		got, ok := FilePassword(tt.d, tt.broker)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: FilePassword = %q, %v; want %q, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}
