package health

import (
	"encoding/json"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// MaxChannels bounds the ENERGY channels kept per device. Payloads are
// controlled by whoever can publish on the broker, and every channel becomes
// metric series, so anything beyond it is dropped.
const MaxChannels = 8

// EnergyChannel is the last ENERGY reading of one meter channel. A nil field
// was not part of the reading.
type EnergyChannel struct {
	Power     *float64 // W
	Today     *float64 // kWh
	Yesterday *float64 // kWh
	Total     *float64 // kWh
}

// Telemetry is what the metrics exporter needs besides model.Health. It is
// read from the cache the MQTT and poll paths already fill: reading it never
// talks to a device.
type Telemetry struct {
	SignalDBm int  // Wifi.Signal from STATE
	HasSignal bool //
	// Energy holds the last tele/SENSOR ENERGY block by channel. The key is
	// "" for a single meter and "1", "2", ... when Tasmota sends arrays.
	// Empty for devices without an energy sensor.
	Energy map[string]EnergyChannel
}

// Telemetry returns a snapshot for every tracked device.
func (s *Service) Telemetry() map[model.DeviceID]Telemetry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[model.DeviceID]Telemetry, len(s.devs))
	for id, ds := range s.devs {
		t := Telemetry{SignalDBm: ds.sigDBm, HasSignal: ds.hasSig}
		if len(ds.energy) > 0 {
			t.Energy = make(map[string]EnergyChannel, len(ds.energy))
			for k, v := range ds.energy {
				t.Energy[k] = v
			}
		}
		out[id] = t
	}
	return out
}

// applyEnergy merges the ENERGY block of a tele/SENSOR payload. Tasmota sends
// a number per field for one meter and an array per field for several
// (Power:[12,40]); the array index becomes the channel "1", "2", ...
// It returns the channels of this reading (nil when it had no ENERGY block).
func applyEnergy(ds *devState, payload []byte) map[string]EnergyChannel {
	var doc struct {
		Energy map[string]json.RawMessage `json:"ENERGY"`
	}
	if json.Unmarshal(payload, &doc) != nil || doc.Energy == nil {
		return nil
	}
	ch := map[string]EnergyChannel{}
	set := func(field string, f func(*EnergyChannel, *float64)) {
		raw, ok := doc.Energy[field]
		if !ok {
			return
		}
		var one float64
		if json.Unmarshal(raw, &one) == nil {
			c := ch[""]
			f(&c, &one)
			ch[""] = c
			return
		}
		var many []float64
		if json.Unmarshal(raw, &many) != nil {
			return
		}
		for i := range many {
			if i >= MaxChannels {
				break
			}
			v := many[i]
			k := strconv.Itoa(i + 1)
			c := ch[k]
			f(&c, &v)
			ch[k] = c
		}
	}
	set("Power", func(c *EnergyChannel, v *float64) { c.Power = v })
	set("Today", func(c *EnergyChannel, v *float64) { c.Today = v })
	set("Yesterday", func(c *EnergyChannel, v *float64) { c.Yesterday = v })
	set("Total", func(c *EnergyChannel, v *float64) { c.Total = v })
	if len(ch) > 0 {
		ds.energy = ch
	}
	return ch
}
