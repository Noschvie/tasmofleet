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
// shown raw rather than guessed. The texts are the ones in Tasmota's support.ino and support_esp32.ino.
func TestClassifyMapsTheRealEsp8266AndEsp32Strings(t *testing.T) {
	const (
		power   = "Power was switched on"
		soft    = "Restarted by command or update"
		sleep   = "Woke from deep sleep"
		button  = "Reset button or pin"
		hung    = "Device hung, watchdog reset"
		crash   = "Crash (exception)"
		brown   = "Supply voltage dropped (brown-out)"
		glitch  = "Supply glitch"
		unknown = ""
	)
	for _, c := range []struct {
		raw, label string
		attention  bool
	}{
		// ESP8266 (ESP.getResetReason) and Tasmota's own
		{"Power on", power, false},
		{"Hardware Watchdog", hung, true},
		{"Exception", crash, true},
		{"Software Watchdog", hung, true}, // not "software restart": the watchdog wins
		{"Software/System restart", soft, false},
		{"Deep-Sleep Wake", sleep, false},
		{"External System", button, false},
		{"Blocked Loop", hung, true},
		// ESP32 (ESP32GetResetReason)
		{"Vbat power on reset", power, false},
		{"Software reset digital core", soft, false},
		{"Legacy watch dog reset digital core", hung, true},
		{"Deep Sleep reset digital core", sleep, false},
		{"Timer Group0 Watch dog reset digital core", hung, true},
		{"Timer Group1 Watch dog reset digital core", hung, true},
		{"RTC Watch dog Reset digital core", hung, true},
		{"Time Group0 reset CPU", hung, true},
		{"Software reset CPU", soft, false},
		{"RTC Watch dog Reset CPU", hung, true},
		{"Reset when the vdd voltage is not stable", brown, true},
		{"RTC Watch dog reset digital core and rtc module", hung, true},
		{"Time Group1 reset CPU", hung, true},
		{"Super watchdog reset digital core and rtc module", hung, true},
		{"Glitch reset digital core and rtc module", glitch, true},
		{"Power glitch reset digital core and rtc module", glitch, true},
		// no useful words: shown as they came
		{"or APP CPU, reseted by PRO CPU", unknown, false},
		{"Efuse reset digital core", unknown, false},
		{"Flux capacitor", unknown, false},
	} {
		r := Classify(c.raw)
		want := c.label
		if want == unknown {
			want = c.raw
		}
		if r.Label != want || r.Attention != c.attention || r.Known != (c.label != unknown) {
			t.Errorf("%q -> %+v, want %q attention %v", c.raw, r, want, c.attention)
		}
	}
	if r := Classify(""); r.Known || r.Label != "Unknown" {
		t.Errorf("empty: %+v", r)
	}
}

// Why: Tasmota raises BootCount only after 10 s of uptime, so a loop of faster boots repeats the same
// BootCount. Every stored row is a restart whatever its BootCount says, or the worst loops are invisible.
func TestFastBootLoopWithAFrozenBootCountIsStillALoop(t *testing.T) {
	var b []model.DeviceBoot
	for i := 1; i <= 4; i++ {
		b = append(b, boot(time.Duration(i)*5*time.Minute, 77, "Exception"))
	}
	if r := Assess(Input{Now: now, Boots: b}); r.Restarts1h != 4 || !r.Loop {
		t.Fatalf("restarts %d loop %v, want 4 and a loop", r.Restarts1h, r.Loop)
	}
}

// Why: an update restarts the device twice (through the minimal image) and a retry again; three
// restarts in an hour that TasmoFleet caused itself are no boot loop. The thresholds stay as they are:
// the same three restarts without that cause are one.
func TestRestartsTasmoFleetCausedDoNotCountTowardsABootLoop(t *testing.T) {
	var caused, own []model.DeviceBoot
	for i := 1; i <= 3; i++ {
		b := boot(time.Duration(i)*8*time.Minute, 40-i, "Software/System restart")
		own = append(own, b)
		b.Expected = true
		caused = append(caused, b)
	}
	r := Assess(Input{Now: now, Boots: caused})
	if r.Loop || r.Restarts1h != 0 || r.Last == nil {
		t.Fatalf("update restarts: loop %v, 1h %d (last restart must still show)", r.Loop, r.Restarts1h)
	}
	if !Assess(Input{Now: now, Boots: own}).Loop {
		t.Fatal("the same restarts with no known cause are a loop")
	}
	// Restarts missed before an expected one still count: BootCount went 10 -> 14 across it.
	m := []model.DeviceBoot{boot(50*time.Minute, 10, "Exception"), boot(5*time.Minute, 14, "Software/System restart")}
	m[1].Expected = true
	if r := Assess(Input{Now: now, Boots: m}); r.Restarts1h != 1+3 {
		t.Fatalf("restarts %d, want the first row plus 3 missed ones", r.Restarts1h)
	}
}
