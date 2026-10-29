package health

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// MaxChannels bounds the ENERGY channels kept per device. Payloads are
// controlled by whoever can publish on the broker, and every channel becomes
// metric series, so anything beyond it is dropped.
const MaxChannels = 8

const (
	// MaxSensorReadings bounds the sensor readings kept per device; the payload is as
	// untrusted as the ENERGY one.
	MaxSensorReadings = 32
	// MaxSensorNameLen bounds the length of a sensor or reading name.
	MaxSensorNameLen = 32
)

// SensorReading is one numeric value of a tele/SENSOR block other than ENERGY, for example
// BME280 Temperature. Unit is "" when Tasmota names none for it. Readings are shown on the
// device page only; they are not exported as metrics.
type SensorReading struct {
	Sensor string
	Name   string
	Value  float64
	Unit   string
}

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
	// Sensors holds the readings of the last tele/SENSOR payload that had any, sorted by
	// sensor and name. At most MaxSensorReadings.
	Sensors []SensorReading
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
		t.Sensors = append([]SensorReading(nil), ds.sensors...)
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

// sensorUnits are the units Tasmota implies by reading name. Temperature and pressure units
// come from the payload's TempUnit and PressureUnit keys instead.
var sensorUnits = map[string]string{
	"Humidity":      "%",
	"Illuminance":   "lx",
	"Distance":      "cm",
	"CarbonDioxide": "ppm",
	"eCO2":          "ppm",
	"TVOC":          "ppb",
}

// applySensors collects the numeric values of every top-level object of a tele/SENSOR
// payload except ENERGY (the meter) and records them when there is at least one.
// Non-numeric values and nested objects are dropped; names and count are bounded. A payload
// without any reading leaves the last ones in place.
func applySensors(ds *devState, payload []byte) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(payload, &doc) != nil {
		return
	}
	unit := func(key, def string) string {
		var u string
		if json.Unmarshal(doc[key], &u) != nil || u == "" || len(u) > 8 {
			return def
		}
		return u
	}
	tempUnit, pressUnit := "°"+unit("TempUnit", "C"), unit("PressureUnit", "hPa")
	sensors := make([]string, 0, len(doc))
	for k := range doc {
		if k != "ENERGY" && len(k) <= MaxSensorNameLen {
			sensors = append(sensors, k)
		}
	}
	sort.Strings(sensors)
	var out []SensorReading
	for _, sn := range sensors {
		var block map[string]json.RawMessage
		if json.Unmarshal(doc[sn], &block) != nil { // Time, TempUnit, ...: not an object
			continue
		}
		names := make([]string, 0, len(block))
		for k := range block {
			if len(k) <= MaxSensorNameLen {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			var v float64
			// json.Unmarshal accepts null for a float64; a null is no reading.
			if c := block[n]; len(c) == 0 || c[0] == 'n' || json.Unmarshal(c, &v) != nil {
				continue
			}
			u := sensorUnits[n]
			switch n {
			case "Temperature", "DewPoint", "ObjectTemperature":
				u = tempUnit
			case "Pressure", "SeaPressure":
				u = pressUnit
			}
			if len(out) == MaxSensorReadings {
				break
			}
			out = append(out, SensorReading{Sensor: sn, Name: n, Value: v, Unit: u})
		}
		if len(out) == MaxSensorReadings {
			break
		}
	}
	if len(out) > 0 {
		ds.sensors = out
	}
}
