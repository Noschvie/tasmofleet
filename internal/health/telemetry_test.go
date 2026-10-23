package health

import (
	"strings"
	"testing"
	"time"

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
