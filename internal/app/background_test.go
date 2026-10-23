package app

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: Tasmota names the only relay of a single-relay device "POWER" and the
// relays of a multi-relay device "POWER1".."POWERn"; the metric must use one
// relay number for both, and never two series for the same relay.
func TestDeviceSamplesRelayNumbersAndUnknown(t *testing.T) {
	devs := []model.Device{{ID: 1, Name: "single"}, {ID: 2, Name: "multi"}, {ID: 3, Name: "silent"}, {ID: 4, Name: "absent"}}
	all := map[model.DeviceID]model.Health{
		1: {State: model.HealthOnline, Power: map[string]string{"POWER": "ON"}},
		2: {State: model.HealthOnline, Power: map[string]string{"POWER": "OFF", "POWER1": "ON", "POWER2": "OFF"}},
		3: {State: model.HealthUnknown},
	}
	tel := map[model.DeviceID]health.Telemetry{1: {SignalDBm: -61, HasSignal: true}}
	got := deviceSamples(devs, all, tel)
	if len(got) != 4 {
		t.Fatalf("samples = %d", len(got))
	}
	if r := got[0].Relays; len(r) != 1 || !r["1"] {
		t.Errorf("single relay = %v", r)
	}
	if r := got[1].Relays; len(r) != 2 || !r["1"] || r["2"] {
		t.Errorf("multi relay = %v (bare POWER must not override POWER1)", r)
	}
	if got[0].SignalDBm == nil || *got[0].SignalDBm != -61 || got[1].SignalDBm != nil {
		t.Errorf("signal mapping wrong: %v %v", got[0].SignalDBm, got[1].SignalDBm)
	}
	if !got[2].Unknown || !got[3].Unknown {
		t.Error("devices without observation must be marked unknown")
	}
}

// Why: POWER999 from a hostile publisher must not become a relay series.
func TestDeviceSamplesDropOutOfRangeRelays(t *testing.T) {
	devs := []model.Device{{ID: 1, Name: "d"}}
	all := map[model.DeviceID]model.Health{1: {State: model.HealthOnline,
		Power: map[string]string{"POWER2": "ON", "POWER999": "ON", "POWER0": "ON"}}}
	got := deviceSamples(devs, all, nil)
	if r := got[0].Relays; len(r) != 1 || !r["2"] {
		t.Fatalf("relays = %v", r)
	}
}
