package metrics

import (
	"flag"
	"fmt"
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

// unixText is how the exposition writes a Unix time.
func unixText(t time.Time) string { return strconv.FormatFloat(float64(t.Unix()), 'g', -1, 64) }

func backupBody(t *testing.T, s *Standard, r *Registry) string {
	t.Helper()
	_, body := scrape(t, r.Handler(), "")
	var keep []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "tasmofleet_backup_last_success_timestamp_seconds{") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// Why: the metric exists to alert on devices without a recent backup. Device
// names are not unique (Tasmota's default is "Tasmota"), so keyed by name two
// devices shared one series and the last one written won: the alert watched
// whichever device came last and the other one's stale backup was invisible.
// The inventory id keeps the series apart, as for the tasmofleet_device_*
// gauges.
func TestBackupAgeOfDevicesWithTheSameNameStaysApart(t *testing.T) {
	r := NewRegistry()
	s := NewStandard(r, "v1", false)
	today := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	s.SetBackupAges([]BackupSample{
		{ID: 1, Name: "Tasmota", Taken: today},
		{ID: 2, Name: "Tasmota", Taken: today.AddDate(0, 0, -40)},
	}, true)
	got := backupBody(t, s, r)
	want := fmt.Sprintf("tasmofleet_backup_last_success_timestamp_seconds{device=\"Tasmota\",id=\"1\"} %s\n"+
		"tasmofleet_backup_last_success_timestamp_seconds{device=\"Tasmota\",id=\"2\"} %s", unixText(today), unixText(today.AddDate(0, 0, -40)))
	if got != want {
		t.Errorf("series:\n%s\nwant:\n%s", got, want)
	}
}

// Why: without device labels no name or id may leave the host, so the alert
// gets one series: the oldest "latest backup" of the fleet (the device that
// has gone longest without one). A device that left the inventory, or a
// switched-off setting, must not leave its series behind.
func TestBackupAgeWithoutDeviceLabelsIsTheFleetsOldest(t *testing.T) {
	r := NewRegistry()
	s := NewStandard(r, "v1", false)
	now := time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)
	fleetOf := []BackupSample{
		{ID: 1, Name: "kitchen", Taken: now},
		{ID: 2, Name: "garage", Taken: now.AddDate(0, 0, -40)},
		{ID: 3, Name: "shed", Taken: now.AddDate(0, 0, -3)},
	}
	s.SetBackupAges(fleetOf, true)
	s.SetBackupAges(fleetOf, false)
	got := backupBody(t, s, r)
	want := fmt.Sprintf("tasmofleet_backup_last_success_timestamp_seconds{device=\"fleet\",id=\"\"} %s", unixText(now.AddDate(0, 0, -40)))
	if got != want {
		t.Errorf("series after switching the labels off:\n%s\nwant:\n%s", got, want)
	}
	s.SetBackupAges(fleetOf[:1], true) // devices 2 and 3 deleted, labels on again
	if got := backupBody(t, s, r); strings.Contains(got, "garage") || strings.Contains(got, `device="fleet"`) || !strings.Contains(got, `device="kitchen"`) {
		t.Errorf("stale series after the fleet changed:\n%s", got)
	}
	s.SetBackupAges(nil, false) // no backup anywhere: no series, not a false "never"
	if got := backupBody(t, s, r); got != "" {
		t.Errorf("series without any backup:\n%s", got)
	}
}
