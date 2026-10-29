package simulator

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// energyFields are the ENERGY values of the mr-bambu-plug capture (energy-status.json), in
// Tasmota's order. kind says how a multi-channel meter sends the field (xdrv_03_energy
// EnergyFmt, SetOption129 0): per-channel fields are arrays, the counters are one scalar
// summed over the channels, Voltage is one shared scalar.
var energyFields = []struct {
	name  string
	value float64
	kind  int
}{
	{"Total", 7.228, sumScalar}, {"Yesterday", 0.001, sumScalar}, {"Today", 0.034, sumScalar},
	{"Power", 10, scaledArray}, {"ApparentPower", 23, scaledArray}, {"ReactivePower", 21, scaledArray},
	{"Factor", 0.43, plainArray}, {"Voltage", 234, plainScalar}, {"Current", 0.099, scaledArray},
}

const (
	sumScalar   = iota // scalar, single-meter value times the sum of the channel factors
	scaledArray        // array, channel i is the single-meter value times i+1
	plainArray         // array, every channel the single-meter value
	plainScalar        // scalar, the single-meter value
)

// energyBlock renders the ENERGY object for spec.Energy: plain numbers for one meter, the
// shapes of energyFields for n channels.
func energyBlock(n int) string {
	var b strings.Builder
	b.WriteString(`{"TotalStartTime":"2023-03-10T18:13:24"`)
	for _, f := range energyFields {
		b.WriteString(`,"` + f.name + `":`)
		if n < 2 {
			b.WriteString(num(f.value))
			continue
		}
		switch f.kind {
		case sumScalar:
			b.WriteString(num(f.value * float64(n*(n+1)/2)))
		case plainScalar:
			b.WriteString(num(f.value))
		default:
			b.WriteString("[")
			for i := 0; i < n; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				v := f.value
				if f.kind == scaledArray {
					v *= float64(i + 1)
				}
				b.WriteString(num(v))
			}
			b.WriteString("]")
		}
	}
	b.WriteString("}")
	return b.String()
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// sensorPayload is the tele/SENSOR payload of the device, "" when it has no meter or sensors.
func (d *simDevice) sensorPayload() string {
	if d.spec.Energy < 1 && len(d.spec.Sensors) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`{"Time":"` + time.Now().UTC().Format("2006-01-02T15:04:05") + `"`)
	if d.spec.Energy > 0 {
		b.WriteString(`,"ENERGY":` + energyBlock(d.spec.Energy))
	}
	pressure := false
	for name, vals := range d.spec.Sensors { // json.Marshal sorts the keys of each block
		k, _ := json.Marshal(name) // cannot fail for a string
		v, err := json.Marshal(vals)
		if err != nil { // only NaN or Inf, which a spec must not contain
			continue
		}
		_, p := vals["Pressure"]
		pressure = pressure || p
		b.WriteString("," + string(k) + ":" + string(v))
	}
	if len(d.spec.Sensors) > 0 {
		if pressure {
			b.WriteString(`,"PressureUnit":"hPa"`)
		}
		b.WriteString(`,"TempUnit":"C"`)
	}
	b.WriteString("}")
	return b.String()
}
