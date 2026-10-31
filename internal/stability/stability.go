// Package stability judges how stable a device is from the restarts and Wi-Fi samples TasmoFleet
// saw (#325): a boot loop, a weak Wi-Fi link or neither. It is pure: data in, decision out. The
// thresholds are fixed on purpose (maintainer's decision), so every page gives the same answer.
package stability

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Thresholds (approved design of #325).
const (
	BootLoopPerHour = 3   // restarts in 1 h that make a boot loop
	BootLoopPerDay  = 6   // restarts in 24 h that make a boot loop
	WeakDBm         = -75 // an average signal below this is weak
	WeakReconnects  = 10  // Wi-Fi reconnects since boot that make the link weak
)

// Level is the verdict shown as a badge.
type Level string

// Levels, worst first when both apply.
const (
	Stable   Level = "stable"
	BootLoop Level = "bootloop"
	WeakWiFi Level = "weakwifi"
)

// Label is the badge text.
func (l Level) Label() string {
	switch l {
	case BootLoop:
		return "Boot loop"
	case WeakWiFi:
		return "Weak Wi-Fi"
	case Stable:
	}
	return "Stable"
}

// Input is everything the verdict is made from.
type Input struct {
	Now     time.Time
	Boots   []model.DeviceBoot // any order
	Samples []model.WifiSample // any order
	// SignalDBm is the current Wifi.Signal (0 = not reported), LinkCount the device's Wifi.LinkCount
	// (0 = not reported; one connect is the normal start), UptimeSec its current uptime.
	SignalDBm int
	LinkCount int
	UptimeSec int64
}

// Result is the verdict with the numbers behind it.
type Result struct {
	Level Level
	// Loop and Weak say which problems apply; Level shows the worse one (a boot loop).
	Loop, Weak bool
	// Restarts counted from the BootCount deltas, so restarts TasmoFleet did not see are included.
	Restarts1h, Restarts24h, Restarts7d int
	LongestUptime                       time.Duration // the longest run in the last 24 h, the current one included
	Last                                *model.DeviceBoot
	LastReason                          Reason
	DominantReason                      Reason // the most frequent reason of the last 24 h (the newest on a tie)
	DominantCount, WindowBoots          int    // how many of the last 24 h's stored restarts have it, and how many there are

	Reconnects int // since boot: LinkCount - 1
	// The signal over the last 24 h: the hourly samples and, when reported, the current reading.
	HasAvg                    bool
	AvgDBm, BestDBm, WorstDBm int
	WeakBySignal              bool
	WeakByReconnects          bool
	Spark                     []int // dBm per sample of the last 24 h, oldest first
}

// Assess judges one device.
func Assess(in Input) Result {
	boots := append([]model.DeviceBoot(nil), in.Boots...)
	sort.SliceStable(boots, func(i, j int) bool { return boots[i].At.Before(boots[j].At) })
	var r Result
	r.Restarts1h = restartsSince(boots, in.Now.Add(-time.Hour))
	r.Restarts24h = restartsSince(boots, in.Now.Add(-24*time.Hour))
	r.Restarts7d = restartsSince(boots, in.Now.Add(-7*24*time.Hour))
	r.Loop = r.Restarts1h >= BootLoopPerHour || r.Restarts24h >= BootLoopPerDay

	r.LongestUptime = time.Duration(min(in.UptimeSec, 24*3600)) * time.Second
	counts, lastSeen := map[string]int{}, map[string]int{}
	byLabel := map[string]Reason{}
	for i, b := range boots {
		if b.At.Before(in.Now.Add(-24 * time.Hour)) {
			continue
		}
		r.LongestUptime = max(r.LongestUptime, b.UptimeBefore)
		rs := Classify(b.Reason)
		counts[rs.Label]++
		lastSeen[rs.Label], byLabel[rs.Label] = i, rs
	}
	bestLabel := ""
	for l, n := range counts { // most frequent; a tie goes to the one seen last
		if bestLabel == "" || n > counts[bestLabel] || n == counts[bestLabel] && lastSeen[l] > lastSeen[bestLabel] {
			bestLabel = l
		}
	}
	r.DominantReason = byLabel[bestLabel]
	r.DominantCount = counts[bestLabel]
	for _, n := range counts {
		r.WindowBoots += n
	}
	if n := len(boots); n > 0 {
		last := boots[n-1]
		r.Last, r.LastReason = &last, Classify(last.Reason)
	}

	r.Reconnects = max(0, in.LinkCount-1)
	r.WeakByReconnects = r.Reconnects >= WeakReconnects
	var vals []int
	samples := append([]model.WifiSample(nil), in.Samples...)
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	for _, s := range samples {
		if !s.At.Before(in.Now.Add(-24 * time.Hour)) {
			vals = append(vals, s.DBm)
		}
	}
	r.Spark = append([]int(nil), vals...)
	if in.SignalDBm != 0 {
		vals = append(vals, in.SignalDBm)
	}
	if len(vals) > 0 {
		sum := 0
		r.BestDBm, r.WorstDBm = vals[0], vals[0]
		for _, v := range vals {
			sum += v
			r.BestDBm, r.WorstDBm = max(r.BestDBm, v), min(r.WorstDBm, v)
		}
		r.HasAvg, r.AvgDBm = true, int(math.Round(float64(sum)/float64(len(vals))))
		r.WeakBySignal = float64(sum)/float64(len(vals)) < WeakDBm
	}
	r.Weak = r.WeakBySignal || r.WeakByReconnects
	switch {
	case r.Loop:
		r.Level = BootLoop
	case r.Weak:
		r.Level = WeakWiFi
	default:
		r.Level = Stable
	}
	return r
}

// restartsSince counts the restarts at or after from. A row counts as one restart, or as the
// BootCount step to the row before it when that row is inside the window too: the steps in between
// were restarts TasmoFleet did not see, and both ends being inside the window proves they happened
// inside it. Boots must be sorted oldest first.
func restartsSince(boots []model.DeviceBoot, from time.Time) int {
	n := 0
	for i, b := range boots {
		if b.At.Before(from) {
			continue
		}
		w := 1
		if i > 0 && !boots[i-1].At.Before(from) && b.BootCount > 0 && boots[i-1].BootCount > 0 && b.BootCount > boots[i-1].BootCount {
			w = b.BootCount - boots[i-1].BootCount
		}
		n += w
	}
	return n
}

// Reason is Tasmota's RestartReason in plain words.
type Reason struct {
	Label string // "Crash (exception)"
	Short string // lower case, for a sentence: "crash"
	Raw   string // as the device sent it
	// Attention: a reason that points at a fault (crash, watchdog, brown-out). Known is false for a
	// text the mapping does not know; Label is then the raw text.
	Attention bool
	Known     bool
}

// Classify maps a RestartReason text by keyword. ESP8266 and ESP32 word the same events
// differently ("Software/System restart" and "Software reset CPU"), and the texts change between
// Tasmota versions, so matching is by keyword and an unknown text is shown as it came.
func Classify(raw string) Reason {
	s := strings.ToLower(strings.TrimSpace(raw))
	mk := func(label, short string, attention bool) Reason {
		return Reason{Label: label, Short: short, Raw: raw, Attention: attention, Known: true}
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case s == "":
		return Reason{Label: "Unknown", Short: "unknown reason", Raw: raw}
	case has("watchdog", "wdt"):
		return mk("Device hung, watchdog reset", "watchdog reset", true)
	case has("exception", "panic"):
		return mk("Crash (exception)", "crash", true)
	case has("brownout", "brown-out", "brown out"):
		return mk("Supply voltage dropped (brown-out)", "brown-out", true)
	case has("power on", "power-on", "poweron", "vbat"):
		return mk("Power was switched on", "power-on", false)
	case has("deep") && has("sleep"):
		return mk("Woke from deep sleep", "deep sleep wake-up", false)
	case has("external"):
		return mk("Reset button or pin", "reset button", false)
	case has("software", "system restart"):
		return mk("Restarted by command or update", "restart by command", false)
	}
	return Reason{Label: strings.TrimSpace(raw), Short: strings.TrimSpace(raw), Raw: raw}
}
