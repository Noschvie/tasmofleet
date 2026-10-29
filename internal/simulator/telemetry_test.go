package simulator

import (
	"encoding/json"
	"strings"
	"testing"
)

// sensorPayload waits for the first tele/SENSOR of a device and returns its payload.
func sensorPayload(t *testing.T, c *collector, topic string) map[string]json.RawMessage {
	t.Helper()
	prefix := "tele/" + topic + "/SENSOR="
	c.waitFor(t, prefix)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if strings.HasPrefix(m, prefix) {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(strings.TrimPrefix(m, prefix)), &doc); err != nil {
				t.Fatalf("tele/SENSOR is not JSON: %v: %s", err, m)
			}
			return doc
		}
	}
	return nil
}

// Why: tasmofleet reads energy and sensors only from tele/SENSOR; a simulator that never
// sends it leaves the Sensors box and the per-channel watts untestable end to end.
func TestSimulatorPublishesSensorTelemetry(t *testing.T) {
	_, c, f := newBrokerFleet(t, Extra{})
	add(t, f, DeviceSpec{Name: "meter", Profile: ProfileESP8266_1M, Energy: 1})
	add(t, f, DeviceSpec{Name: "dual", Profile: ProfileESP8266_1M, Energy: 2})
	add(t, f, DeviceSpec{Name: "weather", Profile: ProfileESP8266_1M, Sensors: map[string]map[string]float64{
		"BME280": {"Temperature": 21.5, "Humidity": 45}, "BH1750": {"Illuminance": 120},
	}})
	add(t, f, DeviceSpec{Name: "plain", Profile: ProfileESP8266_1M})

	single := sensorPayload(t, c, "meter")
	var e map[string]json.RawMessage
	if err := json.Unmarshal(single["ENERGY"], &e); err != nil || string(e["Power"]) == "" || e["Power"][0] == '[' {
		t.Fatalf("single meter: ENERGY.Power must be a number, got %s (%v)", single["ENERGY"], err)
	}

	dual := sensorPayload(t, c, "dual")
	if err := json.Unmarshal(dual["ENERGY"], &e); err != nil {
		t.Fatal(err)
	}
	var power []float64
	var total, volt float64
	if json.Unmarshal(e["Power"], &power) != nil || len(power) != 2 {
		t.Fatalf("two channels: Power must be an array of 2, got %s", dual["ENERGY"])
	}
	// Tasmota (EnergyFmt, SetOption129 0) sums the counters and shares one voltage.
	if json.Unmarshal(e["Total"], &total) != nil || json.Unmarshal(e["Voltage"], &volt) != nil {
		t.Fatalf("two channels: Total and Voltage must be scalars, got %s", dual["ENERGY"])
	}
	if power[0] == power[1] {
		t.Errorf("channels must differ, or a test cannot tell them apart: %v", power)
	}

	w := sensorPayload(t, c, "weather")
	if _, ok := w["ENERGY"]; ok {
		t.Error("a device with only Sensors has no ENERGY block")
	}
	var bme map[string]float64
	if json.Unmarshal(w["BME280"], &bme) != nil || bme["Temperature"] != 21.5 || bme["Humidity"] != 45 {
		t.Fatalf("BME280 = %s", w["BME280"])
	}
	if string(w["TempUnit"]) != `"C"` {
		t.Errorf("TempUnit = %s", w["TempUnit"])
	}
	if _, ok := w["PressureUnit"]; ok {
		t.Error("PressureUnit without a Pressure reading")
	}

	c.waitFor(t, "tele/plain/STATE=")
	if c.has("tele/plain/SENSOR=") {
		t.Error("a device without Energy or Sensors must not publish tele/SENSOR")
	}
}

// Why: the Sensors box exists for devices without a relay; a simulator whose every device reports
// POWER could not exercise it end to end. NoRelay removes the key from the STATE the broker sees
// and the JSON stays valid.
func TestNoRelayDeviceReportsNoPowerKey(t *testing.T) {
	_, c, f := newBrokerFleet(t, Extra{})
	add(t, f, DeviceSpec{Name: "climate", Profile: ProfileESP8266_1M, NoRelay: true, Sensors: map[string]map[string]float64{"BME280": {"Temperature": 21.5}}})
	prefix := "tele/climate/STATE="
	c.waitFor(t, prefix)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if body, ok := strings.CutPrefix(m, prefix); ok {
			if strings.Contains(body, "POWER") || !json.Valid([]byte(body)) {
				t.Fatalf("STATE of a relay-less device: %s", body)
			}
			return
		}
	}
	t.Fatal("no STATE seen")
}
