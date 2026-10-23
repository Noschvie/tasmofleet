package health

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

func sensor(h *harness, topic, energy string) {
	h.bus.emit(mqttbus.KindSensor, topic, "SENSOR", `{"ENERGY":`+energy+`}`, false)
}

func wattsOf(s []PowerSample) []float64 {
	out := make([]float64, len(s))
	for i, p := range s {
		out[i] = p.Watts
	}
	return out
}

func equalF(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Why: the overview sparkline shows how the load moved over the last two hours.
// It is fed from the ENERGY readings MQTT already delivers, oldest first, so the
// line reads left to right; a reading that has no Power must not add a point.
func TestPowerHistoryRecordsTelemetryOldestFirst(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	sensor(h, "meter", `{"Power":10,"Total":1}`)
	h.advance(5 * time.Minute)
	sensor(h, "meter", `{"Total":1.1}`) // no Power: no point
	h.advance(5 * time.Minute)
	sensor(h, "meter", `{"Power":42.5}`)
	got := h.svc.PowerHistory(1)
	if !equalF(wattsOf(got), []float64{10, 42.5}) {
		t.Fatalf("history = %v", wattsOf(got))
	}
	if d := got[1].At.Sub(got[0].At); d != 10*time.Minute {
		t.Fatalf("samples are %v apart, want 10m (the gap must stay a gap)", d)
	}
}

// Why: a multi-channel meter (Power:[a,b]) is one device; its sparkline is the
// device's whole load, so the channels are added up.
func TestPowerHistorySumsChannels(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "dual"))
	h.listen()
	sensor(h, "dual", `{"Power":[12,40.5]}`)
	if got := wattsOf(h.svc.PowerHistory(1)); !equalF(got, []float64{52.5}) {
		t.Fatalf("history = %v", got)
	}
}

// Why: one sample per minute bounds memory and draws an even line; several
// readings in the same minute keep the newest, not the first.
func TestPowerHistoryKeepsLastReadingOfAMinute(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	h.clk.Advance(-time.Duration(h.clk.Now().Second()) * time.Second) // start of a minute
	sensor(h, "meter", `{"Power":1}`)
	h.advance(20 * time.Second)
	sensor(h, "meter", `{"Power":2}`)
	h.advance(20 * time.Second)
	sensor(h, "meter", `{"Power":3}`)
	if got := wattsOf(h.svc.PowerHistory(1)); !equalF(got, []float64{3}) {
		t.Fatalf("history = %v", got)
	}
}

// Why: the window is two hours. Older readings drop out, also when their slot
// in the ring was not overwritten because no reading came in that minute.
func TestPowerHistoryForgetsReadingsOlderThanTwoHours(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	sensor(h, "meter", `{"Power":7}`)
	h.advance(PowerHistorySlots*PowerHistoryStep - PowerHistoryStep)
	if got := wattsOf(h.svc.PowerHistory(1)); !equalF(got, []float64{7}) {
		t.Fatalf("after 119 min = %v, want the reading still inside the window", got)
	}
	h.advance(PowerHistoryStep)
	if got := h.svc.PowerHistory(1); len(got) != 0 {
		t.Fatalf("after 120 min = %v, want empty", wattsOf(got))
	}
	// A full lap later the same slot index is reused: only the new value counts.
	h.advance(PowerHistorySlots * PowerHistoryStep)
	sensor(h, "meter", `{"Power":8}`)
	if got := wattsOf(h.svc.PowerHistory(1)); !equalF(got, []float64{8}) {
		t.Fatalf("after a lap = %v", got)
	}
}

// Why: a device without an energy sensor has no sparkline (the page hides it),
// and a removed device must not keep memory.
func TestPowerHistoryEmptyWithoutMeterAndForgottenOnDelete(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "plain"), mqttDev(2, "meter"))
	h.listen()
	h.bus.emit(mqttbus.KindSensor, "plain", "SENSOR", `{"ANALOG":{"A0":3}}`, false)
	sensor(h, "meter", `{"Power":5}`)
	if got := h.svc.PowerHistory(1); got != nil {
		t.Fatalf("device without meter has history %v", got)
	}
	if got := h.svc.PowerHistory(99); got != nil {
		t.Fatalf("unknown device has history %v", got)
	}
	h.st.remove(2)
	h.advance(time.Minute)
	h.step()
	if got := h.svc.PowerHistory(2); got != nil {
		t.Fatalf("deleted device still has history %v", got)
	}
}

// Why: callers draw from the slice while telemetry keeps arriving; it must be
// a copy, not a view into the ring.
func TestPowerHistoryIsACopy(t *testing.T) {
	h := newHarness(t, true, Options{}, mqttDev(1, "meter"))
	h.listen()
	sensor(h, "meter", `{"Power":5}`)
	a := h.svc.PowerHistory(1)
	a[0].Watts = 999
	if got := h.svc.PowerHistory(1)[0].Watts; got != 5 {
		t.Fatalf("history changed through a returned slice: %v", got)
	}
}
