package web

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/cmdpolicy"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The read-back of a write (#947), as the Settings tab's result and the Make compliant card show it.

// readBackView is one value's read-back: a badge, the device's own value when it differs (Have) and a short note.
type readBackView struct {
	Label string
	Tone  ui.Tone
	Have  string // the device's value; empty when it is not shown
	Note  string
}

// readBackOf words a check. A check without a state (the write failed or was not sent) shows nothing.
func readBackOf(c settings.Check) (readBackView, bool) {
	switch c.State {
	case settings.CheckVerified:
		return readBackView{Label: "Verified", Tone: ui.ToneOK}, true
	case settings.CheckDiffers:
		v := readBackView{Label: "Differs", Tone: ui.ToneWarn, Have: c.Have}
		if c.Hidden {
			v.Note = "the device has another value (hidden, like the rule's)"
		}
		return v, true
	case settings.CheckNotReported:
		return readBackView{Label: "Not reported", Tone: ui.ToneNeutral, Note: "the device answered without this value"}, true
	case settings.CheckNotReadBack:
		if c.Why == settings.WhySecret {
			return readBackView{Label: "Set", Tone: ui.ToneNeutral, Note: "secret, not read back"}, true
		}
		return readBackView{Label: "Not read back", Tone: ui.ToneWarn, Note: c.Why}, true
	}
	return readBackView{}, false
}

// policyDone is a policy job's done event as the pages read it (ota.PolicyDoneDetail): its note and, when the
// read-back found drift, the checks.
type policyDone struct {
	Note     string
	ReadBack []settings.Check
}

func policyDoneOf(detail string) policyDone {
	note, raw := ota.PolicyDoneDetail(detail)
	d := policyDone{Note: note}
	if raw != nil && json.Unmarshal(raw, &d.ReadBack) != nil {
		d.ReadBack = nil // unreadable checks: the card shows the job's state, never a guess
	}
	return d
}

// policyDoneText is the log line of a policy job's done event: its note and how many values differ, never the JSON.
func policyDoneText(detail string) string {
	d := policyDoneOf(detail)
	n := differing(d.ReadBack)
	if n == 0 {
		return strings.TrimSpace("Done " + d.Note)
	}
	text := "Done: " + plural(n, "value") + " " + map[bool]string{true: "differs", false: "differ"}[n == 1] + " from the plan (policy drift)"
	if d.Note != "" {
		text += ". " + d.Note
	}
	return text
}

func differing(checks []settings.Check) int {
	n := 0
	for _, c := range checks {
		if c.State == settings.CheckDiffers {
			n++
		}
	}
	return n
}

// policySkipped are the lines a policy job's Apply left out on purpose, from its latest verify event; nil when it
// left out none.
func policySkipped(evs []model.JobEvent) []string {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].To == model.JobVerify && evs[i].Detail != "" {
			return ota.PolicySkipped(evs[i].Detail)
		}
	}
	return nil
}

// policyDoneDetail is the detail of the job's done event, or "".
func policyDoneDetail(evs []model.JobEvent) string {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Event == "done" && evs[i].To == model.JobSucceeded {
			return evs[i].Detail
		}
	}
	return ""
}

// succeededChange is one line of a succeeded "Make compliant" job. Verify only succeeds when every line it read
// back reports its value, so a line without a check of its own is verified, unless Apply left it out or it is a
// secret, which is never read back (#809): that line was set.
func succeededChange(line string, checks []settings.Check, skipped []string) fixChange {
	ch := fixChange{Text: line}
	for _, s := range skipped {
		if s == line {
			ch.Label, ch.Tone = "Not sent", ui.ToneNeutral
			return ch
		}
	}
	key, _, _ := strings.Cut(line, " ")
	for _, c := range checks {
		if c.Key == key {
			if v, ok := readBackOf(c); ok {
				ch.Label, ch.Tone, ch.Have, ch.Note = v.Label, v.Tone, v.Have, v.Note
				return ch
			}
		}
	}
	if cmdpolicy.SecretName(key) {
		v, _ := readBackOf(settings.Check{Key: key, State: settings.CheckNotReadBack, Why: settings.WhySecret})
		ch.Label, ch.Tone, ch.Note = v.Label, v.Tone, v.Note
		return ch
	}
	ch.Label, ch.Tone = "Verified", ui.ToneOK
	return ch
}

// driftNote is the card's note for a device whose read-back found drift.
func driftNote(n int) string {
	if n == 1 {
		return "1 value differs from the plan: the device kept its own. It counts as policy drift."
	}
	return strconv.Itoa(n) + " values differ from the plan: the device kept its own. They count as policy drift."
}
