package health

import (
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Power history (design v2, N8): the device overview draws a sparkline of the
// last two hours of watts. The samples come from the tele/SENSOR ENERGY blocks
// the MQTT path already receives; nothing is polled for it and nothing is
// persisted (a restart starts with an empty line).

const (
	// PowerHistorySlots is the length of the history: one slot per minute.
	PowerHistorySlots = 120
	// PowerHistoryStep is the time one slot covers.
	PowerHistoryStep = time.Minute
)

// PowerSample is the device's total power in one minute. At is the start of
// the minute.
type PowerSample struct {
	At    time.Time
	Watts float64
}

// powerRing keeps the last reading of each minute. A slot is addressed by the
// minute number modulo the ring size and remembers which minute it holds, so
// minutes without a reading (TelePeriod is usually 5 min) stay gaps instead of
// repeating an old value, and a slot from an earlier lap is never read as new.
type powerRing struct {
	minute [PowerHistorySlots]int64 // minute number + 1; 0 = never written
	watts  [PowerHistorySlots]float64
}

func minuteOf(t time.Time) int64 { return t.Unix() / int64(PowerHistoryStep/time.Second) }

func (r *powerRing) add(now time.Time, w float64) {
	m := minuteOf(now)
	i := m % PowerHistorySlots
	r.minute[i], r.watts[i] = m+1, w
}

// samples returns the readings of the last PowerHistorySlots minutes up to
// and including now's minute, oldest first.
func (r *powerRing) samples(now time.Time) []PowerSample {
	cur := minuteOf(now)
	var out []PowerSample
	for m := cur - PowerHistorySlots + 1; m <= cur; m++ {
		if m < 0 {
			continue
		}
		i := m % PowerHistorySlots
		if r.minute[i] == m+1 {
			out = append(out, PowerSample{At: time.Unix(m*int64(PowerHistoryStep/time.Second), 0).UTC(), Watts: r.watts[i]})
		}
	}
	return out
}

// totalWatts adds the Power of every channel of one ENERGY reading. ok is
// false when the reading carried no Power at all (a Today/Total-only block).
func totalWatts(ch map[string]EnergyChannel) (w float64, ok bool) {
	for _, c := range ch {
		if c.Power != nil {
			w += *c.Power
			ok = true
		}
	}
	return w, ok
}

// PowerHistory returns the device's power readings of the last two hours,
// oldest first, at most one per minute. It is empty for a device without an
// energy sensor, an unknown device and after a restart. The slice is a copy.
func (s *Service) PowerHistory(id model.DeviceID) []PowerSample {
	now := s.d.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.devs[id]
	if !ok || ds.power == nil {
		return nil
	}
	return ds.power.samples(now)
}
