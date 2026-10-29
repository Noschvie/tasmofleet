package simulator

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// energyFields are the ENERGY values of the mr-bambu-plug capture (energy-status.json), in
// Tasmota's order. Power, ApparentPower and ReactivePower differ per channel and Voltage is
// shared, so tests can tell the channels apart.
var energyFields = []struct {
	name    string
	value   float64
	perChan bool // scaled by the channel number
}{
	{"Total", 7.228, true}, {"Yesterday", 0.001, true}, {"Today", 0.034, true},
	{"Power", 10, true}, {"ApparentPower", 23, true}, {"ReactivePower", 21, true},
	{"Factor", 0.43, false}, {"Voltage", 234, false}, {"Current", 0.099, true},
}

// energyBlock renders the ENERGY object for spec.Energy: numbers for one meter, arrays of n
// for n channels (channel i is the single-meter value times i+1).
func energyBlock(n int) string {
	var b strings.Builder
	b.WriteString(`{"TotalStartTime":"2023-03-10T18:13:24"`)
	for _, f := range energyFields {
		b.WriteString(`,"` + f.name + `":`)
		if n < 2 {
			b.WriteString(num(f.value))
			continue
		}
		b.WriteString("[")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			v := f.value
			if f.perChan {
				v *= float64(i + 1)
			}
			b.WriteString(num(v))
		}
		b.WriteString("]")
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
	if len(d.spec.Sensors) > 0 {
		for name, vals := range d.spec.Sensors { // json.Marshal sorts the keys of each block
			k, _ := json.Marshal(name) // cannot fail for a string
			v, err := json.Marshal(vals)
			if err != nil { // only NaN or Inf, which a spec must not contain
				continue
			}
			b.WriteString("," + string(k) + ":" + string(v))
		}
	}
	if d.spec.Energy > 0 {
		b.WriteString(`,"ENERGY":` + energyBlock(d.spec.Energy))
	}
	if len(d.spec.Sensors) > 0 {
		b.WriteString(`,"PressureUnit":"hPa","TempUnit":"C"`)
	}
	b.WriteString("}")
	return b.String()
}
