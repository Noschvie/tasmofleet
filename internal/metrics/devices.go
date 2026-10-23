package metrics

import (
	"sort"
	"strconv"
	"time"
)

// DeviceSample is the cached state of one inventory device, as the exporter
// needs it. The caller fills it from the health cache; nothing here talks to
// a device.
type DeviceSample struct {
	ID   int64
	Name string

	Online  bool // false: stale, offline or auth_failed
	Unknown bool // nothing observed since start: no up series, no values

	LastSeen  time.Time // zero if never
	UptimeSec int64     // 0 if unknown
	RSSI      int       // percent, 0 if unknown
	SignalDBm *int

	// Relays maps the relay number ("1", "2", ...) to its state.
	Relays map[string]bool
	// Energy by channel ("" = single meter); only set when the device
	// reported an ENERGY block.
	Energy map[string]EnergySample
}

// EnergySample is one meter channel. A nil field is not exported.
type EnergySample struct {
	PowerW, TodayKWh, YesterdayKWh, TotalKWh *float64
}

// MaxIndex bounds relay numbers and energy channels per device: labels are
// derived only from the validated numbers "1".."8" (energy: also "" for a
// single meter), never from payload text, so a hostile publisher cannot create
// series. At most 8 relays + 4*9 energy series + 6 plain series per device.
const MaxIndex = 8

func validIndex(s string, allowEmpty bool) bool {
	if s == "" {
		return allowEmpty
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= MaxIndex && strconv.Itoa(n) == s
}

// SetDevices replaces all per-device series. Every call starts from empty, so
// a device that left the inventory (or a switched-off setting, nil samples)
// leaves no stale series behind. It makes at most one series per device and
// channel or relay. Power and energy are exported only for online devices: the
// last reading of a silent meter would be a lie.
func (s *Standard) SetDevices(samples []DeviceSample) {
	for _, v := range []*Vec{s.DeviceUp, s.DeviceUptime, s.DeviceRSSI, s.DeviceSignal, s.DevicePower,
		s.DeviceEnergyTotal, s.DeviceEnergyToday, s.DeviceEnergyYesterday, s.DeviceRelayOn, s.DeviceLastSeen} {
		v.Reset()
	}
	for _, d := range samples {
		id := strconv.FormatInt(d.ID, 10)
		if d.Unknown {
			continue
		}
		up := 0.0
		if d.Online {
			up = 1
		}
		s.DeviceUp.With(d.Name, id).Set(up)
		if !d.LastSeen.IsZero() {
			s.DeviceLastSeen.With(d.Name, id).Set(float64(d.LastSeen.Unix()))
		}
		if d.UptimeSec > 0 {
			s.DeviceUptime.With(d.Name, id).Set(float64(d.UptimeSec))
		}
		if d.RSSI > 0 {
			s.DeviceRSSI.With(d.Name, id).Set(float64(d.RSSI))
		}
		if d.SignalDBm != nil {
			s.DeviceSignal.With(d.Name, id).Set(float64(*d.SignalDBm))
		}
		relays := make([]string, 0, len(d.Relays))
		for r := range d.Relays {
			relays = append(relays, r)
		}
		sort.Strings(relays)
		for _, r := range relays {
			if !validIndex(r, false) {
				continue
			}
			on := 0.0
			if d.Relays[r] {
				on = 1
			}
			s.DeviceRelayOn.With(d.Name, id, r).Set(on)
		}
		if !d.Online {
			continue
		}
		for ch, e := range d.Energy {
			if !validIndex(ch, true) {
				continue
			}
			set := func(v *Vec, x *float64) {
				if x != nil {
					v.With(d.Name, id, ch).Set(*x)
				}
			}
			set(s.DevicePower, e.PowerW)
			set(s.DeviceEnergyTotal, e.TotalKWh)
			set(s.DeviceEnergyToday, e.TodayKWh)
			set(s.DeviceEnergyYesterday, e.YesterdayKWh)
		}
	}
}
