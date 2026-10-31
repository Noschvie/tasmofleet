package stability

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

var now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func boot(ago time.Duration, count int, reason string) model.DeviceBoot {
	return model.DeviceBoot{At: now.Add(-ago), BootCount: count, Reason: reason}
}

// Why: a boot loop is the reason the page exists. Three restarts in the last hour must flag it, and
// two must not (a person restarting a device twice while testing is not a fault).
func TestBootLoopNeedsThreeRestartsInAnHour(t *testing.T) {
	two := Assess(Input{Now: now, Boots: []model.DeviceBoot{boot(10*time.Minute, 11, "Exception"), boot(20*time.Minute, 10, "Exception")}})
	if two.Loop || two.Level != Stable {
		t.Fatalf("2 restarts in 1 h: loop %v level %s, want stable", two.Loop, two.Level)
	}
	three := Assess(Input{Now: now, Boots: []model.DeviceBoot{boot(10*time.Minute, 12, "Exception"), boot(20*time.Minute, 11, "Exception"), boot(30*time.Minute, 10, "Exception")}})
	if !three.Loop || three.Level != BootLoop || three.Restarts1h != 3 {
		t.Fatalf("3 restarts in 1 h: %+v", three)
	}
}

// Why: a device that restarts every four hours never reaches 3 per hour but is just as sick; six in
// 24 h catches it. Five must not.
func TestBootLoopOverADayNeedsSixRestarts(t *testing.T) {
	var five, six []model.DeviceBoot
	for i := 1; i <= 6; i++ {
		b := boot(time.Duration(i)*3*time.Hour, i, "Software watchdog")
		if i <= 5 {
			five = append(five, b)
		}
		six = append(six, b)
	}
	if r := Assess(Input{Now: now, Boots: five}); r.Loop || r.Restarts24h != 5 {
		t.Fatalf("5 in 24 h: loop %v, restarts %d", r.Loop, r.Restarts24h)
	}
	if r := Assess(Input{Now: now, Boots: six}); !r.Loop || r.Restarts24h != 6 || r.Restarts1h != 0 {
		t.Fatalf("6 in 24 h: loop %v, 24h %d, 1h %d", r.Loop, r.Restarts24h, r.Restarts1h)
	}
}

// Why: TasmoFleet is not always listening (it restarts, MQTT drops). BootCount keeps counting on the
// device, so a jump from 10 to 14 between two rows we saw means three restarts we missed: the
// count must include them or a real loop looks like one restart.
func TestMissedRestartsAreCountedFromBootCountSteps(t *testing.T) {
	r := Assess(Input{Now: now, Boots: []model.DeviceBoot{boot(50*time.Minute, 10, "Power on"), boot(5*time.Minute, 14, "Exception")}})
	if r.Restarts1h != 5 || !r.Loop {
		t.Fatalf("restarts in 1 h = %d (loop %v), want 1 + 4 steps = 5", r.Restarts1h, r.Loop)
	}
}

// Why: after weeks offline BootCount may have moved by hundreds. Those restarts happened somewhere
// in the gap, not necessarily in the window, so a step from a row outside the window must count as
// one restart, or a healthy device would show a boot loop on its first sighting.
func TestBootCountStepFromOutsideTheWindowCountsOnce(t *testing.T) {
	r := Assess(Input{Now: now, Boots: []model.DeviceBoot{boot(30*24*time.Hour, 10, "Power on"), boot(5*time.Minute, 510, "Software/System restart")}})
	if r.Restarts1h != 1 || r.Restarts24h != 1 || r.Loop {
		t.Fatalf("1h %d 24h %d loop %v, want 1, 1, false", r.Restarts1h, r.Restarts24h, r.Loop)
	}
}

// Why: a BootCount that goes down or restarts at 1 (settings reset, a replaced board with the same
// topic) is no negative or zero step; each row still is one restart.
func TestBootCountThatDropsCountsEveryRowOnce(t *testing.T) {
	r := Assess(Input{Now: now, Boots: []model.DeviceBoot{boot(30*time.Minute, 90, ""), boot(5*time.Minute, 2, ""), boot(2*time.Minute, 0, "")}})
	if r.Restarts1h != 3 {
		t.Fatalf("restarts = %d, want 3", r.Restarts1h)
	}
}

func TestWeakWifiBySignalNeedsAnAverageBelowTheThreshold(t *testing.T) {
	samples := []model.WifiSample{{At: now.Add(-3 * time.Hour), DBm: -85}, {At: now.Add(-2 * time.Hour), DBm: -80}, {At: now.Add(-time.Hour), DBm: -90}}
	r := Assess(Input{Now: now, Samples: samples, SignalDBm: -82})
	if !r.Weak || !r.WeakBySignal || r.Level != WeakWiFi || r.AvgDBm != -84 || r.BestDBm != -80 || r.WorstDBm != -90 {
		t.Fatalf("%+v", r)
	}
	// Why: one dip to -78 in an otherwise good hour is not a weak link, only the average counts.
	ok := Assess(Input{Now: now, Samples: []model.WifiSample{{At: now.Add(-time.Hour), DBm: -60}, {At: now.Add(-2 * time.Hour), DBm: -62}}, SignalDBm: -78})
	if ok.Weak {
		t.Fatalf("average %d reported weak", ok.AvgDBm)
	}
}

// Why: the boundary is "below -75": exactly -75 dBm is still acceptable (some fixtures sit there).
func TestMinusSeventyFiveIsNotWeak(t *testing.T) {
	if Assess(Input{Now: now, SignalDBm: -75}).Weak {
		t.Fatal("-75 dBm is weak, want not weak")
	}
	if !Assess(Input{Now: now, SignalDBm: -76}).Weak {
		t.Fatal("-76 dBm is not weak, want weak")
	}
}

// Why: samples older than a day describe a link that may have been fixed since.
func TestOldSamplesDoNotCount(t *testing.T) {
	r := Assess(Input{Now: now, SignalDBm: -60, Samples: []model.WifiSample{{At: now.Add(-30 * time.Hour), DBm: -95}}})
	if r.Weak || len(r.Spark) != 0 {
		t.Fatalf("old sample counted: %+v", r)
	}
}

// Why: Tasmota's LinkCount starts at 1 (the first connect after boot is normal), so 10 reconnects
// means LinkCount 11, and a device that reports no LinkCount must never be called weak.
func TestWeakByReconnectsCountsFromLinkCountMinusOne(t *testing.T) {
	if r := Assess(Input{Now: now, LinkCount: 10}); r.Weak || r.Reconnects != 9 {
		t.Fatalf("LinkCount 10: weak %v reconnects %d", r.Weak, r.Reconnects)
	}
	if r := Assess(Input{Now: now, LinkCount: 11}); !r.Weak || !r.WeakByReconnects || r.Reconnects != 10 {
		t.Fatalf("LinkCount 11: %+v", r)
	}
	if r := Assess(Input{Now: now}); r.Weak || r.Reconnects != 0 {
		t.Fatalf("no data: %+v", r)
	}
}

// Why: when both apply the badge must show the worse one, a boot loop, while Weak stays visible
// for the notice.
func TestBootLoopOutranksWeakWifi(t *testing.T) {
	r := Assess(Input{Now: now, SignalDBm: -90, Boots: []model.DeviceBoot{boot(time.Minute, 3, ""), boot(2*time.Minute, 2, ""), boot(3*time.Minute, 1, "")}})
	if r.Level != BootLoop || !r.Weak {
		t.Fatalf("level %s weak %v", r.Level, r.Weak)
	}
}

func TestLongestUptimeAndDominantReason(t *testing.T) {
	b := []model.DeviceBoot{boot(3*time.Hour, 1, "Power on"), boot(2*time.Hour, 2, "Exception"), boot(time.Hour, 3, "Exception")}
	b[1].UptimeBefore, b[2].UptimeBefore = 12*time.Minute, 9*time.Minute
	r := Assess(Input{Now: now, Boots: b, UptimeSec: 300})
	if r.LongestUptime != 12*time.Minute {
		t.Fatalf("longest %v", r.LongestUptime)
	}
	if r.DominantReason.Label != "Crash (exception)" || r.LastReason.Label != "Crash (exception)" {
		t.Fatalf("dominant %q last %q", r.DominantReason.Label, r.LastReason.Label)
	}
}

// Why: both chip families word the same events differently, and the maintainer wants unknown texts
// shown raw rather than guessed. Texts are the ones the fixtures and Tasmota's source use.
func TestClassifyMapsEsp8266AndEsp32Strings(t *testing.T) {
	for _, c := range []struct {
		raw, label string
		attention  bool
	}{
		{"Power on", "Power was switched on", false},
		{"Power on reset", "Power was switched on", false},
		{"Vbat power on reset", "Power was switched on", false},
		{"Software/System restart", "Restarted by command or update", false},
		{"Software reset CPU", "Restarted by command or update", false},
		{"Software reset digital core", "Restarted by command or update", false},
		{"Deep-Sleep Wake", "Woke from deep sleep", false},
		{"Deep Sleep Wake", "Woke from deep sleep", false},
		{"External System", "Reset button or pin", false},
		{"Hardware Watchdog", "Device hung, watchdog reset", true},
		{"Software Watchdog", "Device hung, watchdog reset", true}, // not "software restart": watchdog wins
		{"Task watchdog reset CPU", "Device hung, watchdog reset", true},
		{"Interrupt wdt reset", "Device hung, watchdog reset", true},
		{"Exception", "Crash (exception)", true},
		{"Panic", "Crash (exception)", true},
		{"Brownout reset", "Supply voltage dropped (brown-out)", true},
	} {
		r := Classify(c.raw)
		if r.Label != c.label || r.Attention != c.attention || !r.Known {
			t.Errorf("%q -> %+v, want %q attention %v", c.raw, r, c.label, c.attention)
		}
	}
	if r := Classify("Flux capacitor"); r.Known || r.Label != "Flux capacitor" || r.Attention {
		t.Errorf("unknown text: %+v", r)
	}
	if r := Classify(""); r.Known || r.Label != "Unknown" {
		t.Errorf("empty: %+v", r)
	}
}
