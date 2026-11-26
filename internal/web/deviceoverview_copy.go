package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/stability"
)

// The words of the device overview (#1112, A2-042), in one place so they can be reviewed as a set. Per page, not a
// catalogue for the app: the Stability card, the Wi-Fi card, the Connection card, the relay box and the Identity card
// say their sentences here and nowhere else on the overview. Tasmota's own names (BootCount, LinkCount, StatusFWR)
// never stand in a label; where a person might search for one, it is secondary text beside the plain label.

// An unread fact is a dash in a row (orDash, the same one the user pages use): "Unknown" is a status word (a device that
// never answered) and reads like a fault as a value (A2-018).

// The leads of the two notes that say why a control cannot act (A2-042: the words move here, they do not change). A
// shared shape with the device header's grouped reasons is a separate issue that covers the header too.
const (
	relaysWhyLead   = "Switching is disabled: "
	connTestWhyLead = "Test not possible: "
)

// The Connection card (the paths) and its two folds. The summaries name what is inside.
const (
	connectionTitle = "Connection"
	actionsAllOK    = "Actions: all available"
)

// actionsSummaryWords is the closed Actions fold's line: how many actions have no way now.
func actionsSummaryWords(notAvailable int) string {
	if notAvailable == 0 {
		return actionsAllOK
	}
	return "Actions: " + strconv.Itoa(notAvailable) + " not available"
}

// Firmware details: the fold of the Identity card that holds what the device reports about its firmware.
const (
	firmwareDetailsTitle = "Firmware details"
	firmwareNotRead      = "Most firmware details are not read yet. Refresh identity reads them."
)

// fwrLabels are the plain labels of the StatusFWR fields; the key is Tasmota's name, shown beside the label as
// secondary text and kept as the id of the explanation.
var fwrLabels = map[string]string{
	"Version":       "Version",
	"BuildDateTime": "Build date",
	"Core":          "Arduino core",
	"SDK":           "Espressif SDK",
	"CpuFrequency":  "CPU speed",
	"Hardware":      "Chip",
	"CR":            "Settings memory used",
}

// cpuSpeed is the clock as a person reads it.
func cpuSpeed(mhz int) string { return strconv.Itoa(mhz) + " MHz" }

// settingsMemory is CR ("445/699": characters used and available in the settings text area) in words.
func settingsMemory(f model.FWRFacts) string {
	if used, total, ok := f.CRUsage(); ok {
		return strconv.Itoa(used) + " of " + strconv.Itoa(total)
	}
	return f.CR
}

// countWord is a count in a sentence: "none", "once", "3 times".
func countWord(n int) string {
	switch n {
	case 0:
		return "none"
	case 1:
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

// restartCountWords is the Restarts row: the counts of the rolling windows, each with its window in words ("1 in 24 h ·
// 1 in 7 days" read like a ratio). The windows are rolling, not "today" and "this week". A device that did not restart
// in a week says that once.
func restartCountWords(r stability.Result) string {
	var parts []string
	if r.Restarts1h > 0 {
		parts = append(parts, countWord(r.Restarts1h)+" in the last hour")
	}
	if r.Restarts7d == 0 {
		return capital("none in the last 7 days")
	}
	parts = append(parts, countWord(r.Restarts24h)+" in the last 24 hours", countWord(r.Restarts7d)+" in the last 7 days")
	return capital(strings.Join(parts, ", "))
}

// longestUptimeWords is the Longest uptime row of a boot loop.
func longestUptimeWords(d time.Duration) string { return minutesText(d) + " in the last 24 hours" }

// Wi-Fi card rows that moved out of Stability.
const (
	signalHistoryKey = "Signal, last 24 hours"
	wifiDropsKey     = "Wi-Fi drops"
)

// signalHistoryWords is the 24 hour signal: the range of a healthy link, the average of a weak one.
func signalHistoryWords(r stability.Result) string {
	if r.Level == stability.WeakWiFi {
		return fmt.Sprintf("average %s, best %d, worst %d", dbmText(r.AvgDBm), r.BestDBm, r.WorstDBm)
	}
	return fmt.Sprintf("%d to %s", r.WorstDBm, dbmText(r.BestDBm))
}

// wifiDropsWords is how often the Wi-Fi link dropped since the device last started, and for how long in total when
// that is worth a sentence (a few seconds is the connect after boot).
func wifiDropsWords(reconnects int, down time.Duration, level stability.Level) string {
	s := countWord(reconnects) + " since the last restart"
	if reconnects == 0 {
		return capital(s)
	}
	if down >= time.Minute || down > 0 && level == stability.WeakWiFi {
		s += ", offline for " + exactDuration(down) + " in total"
	}
	return capital(s)
}
