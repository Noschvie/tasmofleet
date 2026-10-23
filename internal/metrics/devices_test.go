package metrics

import (
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func f(x float64) *float64 { return &x }

func fleet() []DeviceSample {
	sig := -68
	return []DeviceSample{
		{ID: 1, Name: "kitchen-spotlight", Online: true, LastSeen: time.Unix(1792000000, 0), UptimeSec: 4210, RSSI: 64, SignalDBm: &sig,
			Relays: map[string]bool{"1": true},
			Energy: map[string]EnergySample{"": {PowerW: f(52), TotalKWh: f(133.945), TodayKWh: f(0.274), YesterdayKWh: f(0.449)}}},
		{ID: 2, Name: "dual-meter", Online: true, LastSeen: time.Unix(1792000010, 0), UptimeSec: 99,
			Relays: map[string]bool{"1": false, "2": true},
			Energy: map[string]EnergySample{"1": {PowerW: f(12)}, "2": {PowerW: f(40.5)}}},
		{ID: 3, Name: "no-sensor", Online: true, LastSeen: time.Unix(1792000020, 0), Relays: map[string]bool{"1": false}},
		{ID: 4, Name: "gone", Online: false, LastSeen: time.Unix(1791990000, 0), Relays: map[string]bool{"1": true},
			Energy: map[string]EnergySample{"": {PowerW: f(99)}}},
		{ID: 5, Name: "never-seen", Unknown: true},
	}
}

func deviceBody(t *testing.T, samples []DeviceSample) string {
	t.Helper()
	r := NewRegistry()
	s := NewStandard(r, "v1", false)
	s.SetDevices(samples)
	_, body := scrape(t, r.Handler(), "")
	var keep []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, "tasmofleet_device_") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n") + "\n"
}

// Why: dashboards and alerts depend on exact names, label sets and which series
// exist. The golden file makes any change to the exposition a visible diff.
func TestDeviceExpositionGolden(t *testing.T) {
	got := deviceBody(t, fleet())
	const golden = "testdata/devices.prom"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("exposition differs from %s (run with -update after review):\n%s", golden, got)
	}
}

// Why: unknown devices have no up series (absent, not a false 0 that would page
// someone), and an offline meter must not keep reporting its last power.
func TestDeviceSeriesRules(t *testing.T) {
	got := deviceBody(t, fleet())
	for _, bad := range []string{`device="never-seen"`, `tasmofleet_device_power_watts{channel="",device="gone"`} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected series %s", bad)
		}
	}
	if !strings.Contains(got, `tasmofleet_device_up{device="gone",id="4"} 0`) {
		t.Error("offline device must read up=0")
	}
	if strings.Contains(got, `device_power_watts{channel="",device="no-sensor"`) {
		t.Error("power series for a device without ENERGY data")
	}
}

// Why: device names are free text; an unescaped quote or newline breaks the
// whole exposition and blanks every dashboard.
func TestDeviceNameEscaping(t *testing.T) {
	got := deviceBody(t, []DeviceSample{{ID: 7, Name: "a\"b\\c\nd", Online: true}})
	if !strings.Contains(got, `tasmofleet_device_up{device="a\"b\\c\nd",id="7"} 1`) {
		t.Errorf("name not escaped:\n%s", got)
	}
	samples := 0
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "tasmofleet_device_") {
			samples++
		}
	}
	if samples != 1 {
		t.Errorf("raw newline split the sample line (%d lines start a sample):\n%q", samples, got)
	}
}

// Why: with METRICS_DEVICE_LABELS off (nil samples) a previously exported fleet
// must vanish, and so must a device deleted from the inventory.
func TestDeviceSeriesDisappear(t *testing.T) {
	r := NewRegistry()
	s := NewStandard(r, "v1", false)
	s.SetDevices(fleet())
	s.SetDevices(fleet()[:1]) // others deleted
	_, body := scrape(t, r.Handler(), "")
	if strings.Contains(body, `device="dual-meter"`) || !strings.Contains(body, `device="kitchen-spotlight"`) {
		t.Errorf("deleted device still exported:\n%s", body)
	}
	s.SetDevices(nil) // setting switched off
	_, body = scrape(t, r.Handler(), "")
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "tasmofleet_device_") {
			t.Errorf("series left after switch-off: %s", l)
		}
	}
}

// Why: payloads are device/broker controlled. Relay numbers or channel
// indexes outside 1..8 must never become series.
func TestHostileIndicesAddNoSeries(t *testing.T) {
	ch := map[string]EnergySample{"": {PowerW: f(1)}}
	rel := map[string]bool{}
	for i := 0; i < 1000; i++ {
		k := strconv.Itoa(i + 1)
		ch[k] = EnergySample{PowerW: f(1)}
		rel[k] = true
	}
	rel["evil\"x"], rel["01"] = true, true
	got := deviceBody(t, []DeviceSample{{ID: 1, Name: "d", Online: true, Energy: ch, Relays: rel}})
	n := strings.Count(got, "\ntasmofleet_device_power_watts{") + strings.Count(got, "\ntasmofleet_device_relay_on{")
	if n != 9+8 {
		t.Fatalf("series = %d, want 17:\n%s", n, got)
	}
}
