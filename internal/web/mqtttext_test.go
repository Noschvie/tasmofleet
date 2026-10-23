package web

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the device page said "configured for another broker" although
// tasmofleet had no broker at all. Each situation needs its own wording, and
// the device's MQTT credentials must never appear.
func TestMQTTText(t *testing.T) {
	base := model.Device{Topic: "balcony-windowsill", MQTTHost: "mqtt.example.internal", MQTTPort: 1883, MQTT: model.MQTTOtherBroker}
	same := func(h string, p int) bool { return h == "mqtt.example.internal" && p == 1883 }
	with := func(f func(*model.Device)) model.Device { d := base; f(&d); return d }
	cases := []struct {
		name       string
		d          model.Device
		configured bool
		want       string
	}{
		{"no tasmofleet broker", base, false,
			"reports to mqtt.example.internal:1883, topic balcony-windowsill (not monitored: TasmoFleet has no MQTT broker configured)"},
		{"same broker by config", base, true, "reports to our broker (mqtt.example.internal:1883), topic balcony-windowsill"},
		{"same broker, messages seen", with(func(d *model.Device) { d.MQTT = model.MQTTOurs; d.MQTTHost = "10.0.0.5" }), true,
			"reports to our broker (10.0.0.5:1883), topic balcony-windowsill"},
		{"other broker", with(func(d *model.Device) { d.MQTTHost = "broker.example.lan"; d.MQTTPort = 8883 }), true,
			"other broker broker.example.lan:8883, topic balcony-windowsill"},
		{"disabled", with(func(d *model.Device) { d.MQTT = model.MQTTNone; d.MQTTHost = "" }), true, "MQTT disabled"},
		{"disabled without broker", with(func(d *model.Device) { d.MQTT = model.MQTTNone }), false, "MQTT disabled"},
		{"not probed", model.Device{}, true, "not probed yet"},
		{"port 0 shows the default", with(func(d *model.Device) { d.MQTTPort = 0 }), false,
			"reports to mqtt.example.internal:1883, topic balcony-windowsill (not monitored: TasmoFleet has no MQTT broker configured)"},
		{"no topic", with(func(d *model.Device) { d.Topic = "" }), true, "reports to our broker (mqtt.example.internal:1883)"},
		{"probed before host was recorded", with(func(d *model.Device) { d.MQTTHost = "" }), true,
			"MQTT configured (broker address known after the next probe), topic balcony-windowsill"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mqttText(tc.d, tc.configured, same)
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			// The row is already labelled "MQTT": the value must not repeat it.
			if strings.HasPrefix(got, "MQTT:") {
				t.Fatalf("value repeats its label: %q", got)
			}
			if strings.Contains(got, "tasmota") && strings.Contains(got, "user") {
				t.Fatal("credentials in text")
			}
		})
	}
}
