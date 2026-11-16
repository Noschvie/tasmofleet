package ui

import (
	"strconv"
	"time"
)

// PlanSummary is what the Dashboard and the status bar say of the update plans that wait for their start
// (#707): how many transactions, not devices ("Scheduled · N" in Transactions), and when the first starts. It
// names no device and no person. N is 0 when nothing is planned; nothing is drawn then.
type PlanSummary struct {
	N    int
	Next time.Time // the earliest start
	// In is how far Next lies ahead ("in 17 h"), or that it is due; the Dashboard shows it, the bar leaves it out.
	In string
	// Href is the Scheduled section of Transactions; empty when the person cannot open it (no link then).
	Href string
}

// countText is "2 scheduled".
func (p PlanSummary) countText() string { return strconv.Itoa(p.N) + " scheduled" }
