package health

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// Why: the per-device metrics must come from what the MQTT path already cached.
// A scrape that polled devices would hit ESP8266 heaps once per scrape interval.
func TestTelemetryComesFromCacheWithoutDeviceIO(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	h.bus.emit(mqttbus.KindState, "meter", "STATE", `{"UptimeSec":10,"Wifi":{"RSSI":64,"Signal":-68}}`, false)
	h.bus.emit(mqttbus.KindSensor, "meter", "SENSOR",
		`{"ENERGY":{"Total":133.945,"Yesterday":0.449,"Today":0.274,"Power":52,"Voltage":233}}`, false)
	before := len(h.cl.commands())

	for i := 0; i < 3; i++ { // three "scrapes"
		_ = h.svc.Telemetry()
		_ = h.svc.All()
	}
	if got := len(h.cl.commands()); got != before {
		t.Fatalf("reading telemetry sent %d device commands", got-before)
	}
	tel := h.svc.Telemetry()[1]
	if !tel.HasSignal || tel.SignalDBm != -68 {
		t.Fatalf("signal = %+v", tel)
	}
	e, ok := tel.Energy[""]
	if !ok || len(tel.Energy) != 1 {
		t.Fatalf("energy channels = %+v", tel.Energy)
	}
	if *e.Power != 52 || *e.Total != 133.945 || *e.Today != 0.274 || *e.Yesterday != 0.449 {
		t.Fatalf("energy = %+v", e)
	}
}

// Why: two-channel meters send arrays; one series per channel, never a merged sum.
func TestEnergyArraysBecomeChannels(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "dual"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "dual", "SENSOR", `{"ENERGY":{"Power":[12,40.5],"Total":[1.5,2.5]}}`, false)
	e := h.svc.Telemetry()[1].Energy
	if len(e) != 2 || *e["1"].Power != 12 || *e["2"].Power != 40.5 || *e["2"].Total != 2.5 {
		t.Fatalf("energy = %+v", e)
	}
	if e["1"].Today != nil {
		t.Fatal("a field that was not sent must stay unset")
	}
}

// Why: a device without an energy sensor must not get power series.
func TestNoEnergyBlockNoEnergy(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "plain"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "plain", "SENSOR", `{"Time":"x","ANALOG":{"A0":3}}`, false)
	h.bus.emit(mqttbus.KindSensor, "plain", "SENSOR", `not json`, false)
	if len(h.svc.Telemetry()[1].Energy) != 0 {
		t.Fatal("energy without ENERGY block")
	}
}

func TestTelemetryOfVanishedDeviceIsForgotten(t *testing.T) {
	h := newHarness(t, false, Options{}, httpDev(1), httpDev(2))
	h.step()
	h.st.remove(2)
	h.advance(time.Minute)
	h.step()
	if _, ok := h.svc.Telemetry()[2]; ok {
		t.Fatal("deleted device still has telemetry")
	}
}

// Why: a publisher can send any number of channels; only MaxChannels are kept.
func TestEnergyChannelsAreCapped(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	arr := "0" + strings.Repeat(",1", 999)
	h.bus.emit(mqttbus.KindSensor, "meter", "SENSOR", `{"ENERGY":{"Power":[`+arr+`],"Bogus":[1,2]}}`, false)
	if n := len(h.svc.Telemetry()[1].Energy); n != MaxChannels {
		t.Fatalf("channels = %d, want %d", n, MaxChannels)
	}
}

func sensorsOf(t *testing.T, h *harness, id model.DeviceID) map[string]SensorReading {
	t.Helper()
	out := map[string]SensorReading{}
	for _, r := range h.svc.Telemetry()[id].Sensors {
		out[r.Sensor+"."+r.Name] = r
	}
	return out
}

// Why (#145): the device page shows a Sensors box from what the device already sends over
// MQTT. Units come from the payload's TempUnit/PressureUnit, so a Fahrenheit device is not
// shown as Celsius; ENERGY is the meter, not a sensor, and stays out of the list.
func TestSensorBlocksBecomeReadingsWithUnits(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "weather"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "weather", "SENSOR",
		`{"Time":"2026-10-29T10:00:00","BME280":{"Temperature":21.5,"Humidity":45.2,"DewPoint":9.4,"Pressure":1013.2},`+
			`"BH1750":{"Illuminance":123},"ENERGY":{"Power":5},"PressureUnit":"hPa","TempUnit":"F"}`, false)
	got := sensorsOf(t, h, 1)
	want := map[string]SensorReading{
		"BME280.Temperature": {Sensor: "BME280", Name: "Temperature", Value: 21.5, Unit: "°F"},
		"BME280.Humidity":    {Sensor: "BME280", Name: "Humidity", Value: 45.2, Unit: "%"},
		"BME280.DewPoint":    {Sensor: "BME280", Name: "DewPoint", Value: 9.4, Unit: "°F"},
		"BME280.Pressure":    {Sensor: "BME280", Name: "Pressure", Value: 1013.2, Unit: "hPa"},
		"BH1750.Illuminance": {Sensor: "BH1750", Name: "Illuminance", Value: 123, Unit: "lx"},
	}
	if len(got) != len(want) {
		t.Fatalf("readings = %+v, want %d (ENERGY, Time and the unit keys are not sensors)", got, len(want))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if e := h.svc.Telemetry()[1].Energy; len(e) != 1 || *e[""].Power != 5 {
		t.Fatalf("the ENERGY block must still feed the metrics: %+v", e)
	}
}

// Why: a payload with several blocks is shown in a stable order, not in Go's random map order.
func TestSensorReadingsAreSorted(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "weather"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "weather", "SENSOR", `{"SHT3X":{"Temperature":1,"Humidity":2},"BH1750":{"Illuminance":3}}`, false)
	var names []string
	for _, r := range h.svc.Telemetry()[1].Sensors {
		names = append(names, r.Sensor+"."+r.Name)
	}
	if strings.Join(names, ",") != "BH1750.Illuminance,SHT3X.Humidity,SHT3X.Temperature" {
		t.Fatalf("order = %v", names)
	}
}

// Why: a publisher controls the payload. Non-numeric values (a DS18B20 Id, nested objects,
// strings) are no readings; a reading without a sensor block (ANALOG-style or ENERGY only)
// must not wipe the previous sensors, and an ENERGY-only payload is not a sensor reading.
func TestSensorsDropNonNumericAndKeepLastReading(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "probe"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "probe", "SENSOR", `{"DS18B20":{"Id":"0300A2791234","Temperature":19.5,"Note":null,"Deep":{"x":1},"S":"7"},"Flag":true,"Str":"x","Arr":[1]}`, false)
	got := sensorsOf(t, h, 1)
	if len(got) != 1 || got["DS18B20.Temperature"].Value != 19.5 {
		t.Fatalf("readings = %+v", got)
	}
	h.bus.emit(mqttbus.KindSensor, "probe", "SENSOR", `{"ENERGY":{"Power":3}}`, false)
	h.bus.emit(mqttbus.KindSensor, "probe", "SENSOR", `not json`, false)
	if len(sensorsOf(t, h, 1)) != 1 {
		t.Fatal("a payload without sensor blocks must keep the last readings")
	}
}

// Why: every reading is held in memory per device and a hostile or broken publisher can send
// any amount; count and name length are capped.
func TestSensorReadingsAreBounded(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "noisy"))
	h.listen()
	var b strings.Builder
	b.WriteString(`{"Big":{`)
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"V%03d":%d`, i, i)
	}
	b.WriteString(`},"` + strings.Repeat("N", 5000) + `":{"Temperature":1}}`)
	h.bus.emit(mqttbus.KindSensor, "noisy", "SENSOR", b.String(), false)
	rs := h.svc.Telemetry()[1].Sensors
	if len(rs) != MaxSensorReadings {
		t.Fatalf("readings = %d, want the cap %d", len(rs), MaxSensorReadings)
	}
	for _, r := range rs {
		if len(r.Sensor) > MaxSensorNameLen || len(r.Name) > MaxSensorNameLen {
			t.Fatalf("over-long name kept: %q", r.Sensor)
		}
	}
}

// Why: Telemetry() hands out snapshots; a caller must not be able to change the tracker's state.
func TestSensorSnapshotIsACopy(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "w"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "w", "SENSOR", `{"BH1750":{"Illuminance":3}}`, false)
	h.svc.Telemetry()[1].Sensors[0].Value = 99
	if h.svc.Telemetry()[1].Sensors[0].Value != 3 {
		t.Fatal("Telemetry shares its slice with the tracker")
	}
}
