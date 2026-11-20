package web

import (
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// pwKind is which of the six words the device's MQTT password shows (#609).
type pwKind int

const (
	pwNotNeeded pwKind = iota // the device does not use TasmoFleet's broker
	pwUnusable                // a mode is set but no value can be sent
	pwRefused                 // the device answered Error 1
	pwWorks                   // the device accepted it in a test or a transfer, or a backup holds it
	pwNotTested               // typed or bulk-set, never accepted
	pwNotKnown                // not set, and TasmoFleet takes it from backups
	pwNotSet                  // not set, and taking it from backups is off
)

// pwInput is what the state of a device's password is worked out from. It is built from the device and its
// transport facts and cannot carry the password: the value of the secret never gets into the web layer's rules.
type pwInput struct {
	Ours     bool // the device reports to TasmoFleet's broker (the transport facts' own word)
	Mode     model.MQTTPasswordMode
	Source   model.MQTTPasswordSource
	SetAt    time.Time
	OKAt     time.Time
	BadAt    time.Time
	Unusable string
}

func pwInputOf(d model.Device, f transport.Facts) pwInput {
	return pwInput{Ours: f.MQTT.Ours, Mode: f.Password.Mode, Source: d.MQTTPasswordSource, SetAt: d.MQTTPasswordSetAt,
		OKAt: d.MQTTPasswordOKAt, BadAt: f.Password.BadAt, Unusable: f.Password.Unusable}
}

// pwState is one state of the password: the word, its tone, and the sentence around an optional time.
type pwState struct {
	Kind    pwKind
	Word    string
	Tone    ui.Tone
	Line    string    // the sentence; with At it ends before the lead
	Lead    string    // text between Line and the time ("", or ", taken from the backup of")
	At      time.Time // the date the sentence is about (zero: none)
	Tail    string    // text after the time
	Enables string    // what the password is for
}

const (
	enablesLine = "Lets TasmoFleet back up, restore, read files and upload firmware over MQTT."
	neededLine  = "Needed to back up, restore, read files and upload firmware over MQTT; commands never use it."
)

// mqttPasswordState is the one rule of the state word and the line under it, for the device page's row and for the
// card on System, MQTT (which counts devices by Kind). learnOn is MQTT_PASSWORD_FROM_BACKUPS. The order is the order
// of precedence: another broker needs nothing; an unusable value, a refusal and a test beat what is stored; a
// device with no mode is "Not known" while backups can still teach it and "Not set" when that is off.
func mqttPasswordState(in pwInput, learnOn bool) pwState {
	switch {
	case !in.Ours:
		return pwState{Kind: pwNotNeeded, Word: "Not needed", Tone: ui.ToneNeutral,
			Line: "The device does not report to TasmoFleet's broker, so TasmoFleet does not transfer files to it over MQTT."}
	case in.Unusable != "":
		return pwState{Kind: pwUnusable, Word: "Unusable", Tone: ui.ToneWarn, Line: sentence(in.Unusable), Enables: neededLine}
	case in.Mode == model.MQTTPasswordNone:
		if learnOn {
			return pwState{Kind: pwNotKnown, Word: "Not known", Tone: ui.ToneWarn,
				Line: "TasmoFleet takes the password from a backup and has none to take yet.", Enables: neededLine}
		}
		return pwState{Kind: pwNotSet, Word: "Not set", Tone: ui.ToneNeutral,
			Line: "TasmoFleet does not take passwords from backups (turned off), and none was entered.", Enables: neededLine}
	}
	src := pwSource(in.Mode)
	switch {
	case !in.BadAt.IsZero():
		// "Choose No password" is advice only where No password is not the mode already set.
		advice := "Enter the device's own password, or choose No password if its MqttPassword is empty."
		if in.Mode == model.MQTTPasswordEmpty {
			advice = "Enter the device's own password."
		}
		return pwState{Kind: pwRefused, Word: "Refused", Tone: ui.ToneDanger, Line: src + ". Refused", At: in.BadAt,
			Tail:    ": the device refused it, so backups and restores over MQTT are off. " + advice,
			Enables: neededLine}
	case !in.OKAt.IsZero():
		return pwState{Kind: pwWorks, Word: "Works", Tone: ui.ToneOK, Line: src + ". Tested", At: in.OKAt, Tail: ": the device accepted it.", Enables: enablesLine}
	case in.Source == model.MQTTPasswordFromBackup && !in.SetAt.IsZero():
		return pwState{Kind: pwWorks, Word: "Works", Tone: ui.ToneOK, Line: src, Lead: ", taken from the backup of", At: in.SetAt, Tail: ".", Enables: enablesLine}
	}
	s := pwState{Kind: pwNotTested, Word: "Not tested", Tone: ui.ToneNeutral, Line: src, Enables: enablesLine}
	if !in.SetAt.IsZero() {
		s.Line, s.Lead, s.At = src+". Set", "", in.SetAt
		s.Tail = ": the device has not accepted it yet. The next backup over MQTT tests it, or press Test."
	} else {
		s.Line += ". The device has not accepted it yet. The next backup over MQTT tests it, or press Test."
	}
	return s
}

// pwSource says what the stored password is, without a value.
func pwSource(mode model.MQTTPasswordMode) string {
	switch mode {
	case model.MQTTPasswordBroker:
		return "Same as TasmoFleet's broker password"
	case model.MQTTPasswordEmpty:
		return "No password (TasmoFleet sends an empty one)"
	case model.MQTTPasswordOwn:
		return "The device's own password, stored encrypted"
	case model.MQTTPasswordNone:
	}
	return "Not recognised"
}

// pwActs are the buttons under the row.
type pwActs struct {
	Test        bool
	TestLabel   string
	TestWhy     string // why Test is disabled, "" when it can be pressed
	ACL         bool   // the broker ACL notice by Test (false where the header's notice covers it or none applies)
	Change      bool
	ChangeLabel string
	UseBroker   bool // a link to the confirmation of "Use the broker password" for this device
}

// pwView is what the device page adds to the row: where the switch stands, why a device has no password from a
// backup, which buttons exist for the viewer. The zero value is the switch on and no buttons.
type pwView struct {
	LearnOff  bool
	Gap       PasswordGap
	CanTest   bool // a tester is wired and the viewer is an operator
	CanChange bool // the viewer is an admin
	CanBroker bool // an admin, with a fleet service and a broker password to use
	// TestACL is the broker ACL notice for the Test button (T18): set where a test would put the password and a
	// settings chunk on the broker (a broker is configured, the warning is not hidden) and the page's header does
	// not carry the Back up notice already (the backup does not go over MQTT).
	TestACL bool
}

// passwordRow is the row "Device MQTT password" of the card: one word, one line, the buttons.
func passwordRow(d model.Device, f transport.Facts, pw pwView) pathRow {
	st := mqttPasswordState(pwInputOf(d, f), !pw.LearnOff)
	r := pathRow{Key: "Device MQTT password", Value: st.Word, Tone: st.Tone, Sub: st.Line, StampLead: st.Lead, StampAt: st.At, StampTail: st.Tail, Enables: st.Enables}
	if !st.At.IsZero() {
		r.StampText = ui.StampDayText(f.Now, st.At)
	}
	if st.Kind == pwNotKnown && pw.Gap != GapNone {
		r.Sub = gapText(pw.Gap, f) // the reason says what the generic line would, and why
	}
	var a pwActs
	if pw.CanTest && st.Kind != pwNotNeeded {
		a.Test, a.TestLabel = true, "Test"
		if st.Kind == pwRefused {
			a.TestLabel = "Test again"
		}
		a.TestWhy = transport.PasswordTestBlocker(f)
		if a.TestWhy == "" {
			a.ACL = pw.TestACL
		}
	}
	if pw.CanChange {
		a.Change, a.ChangeLabel = true, "Change…"
		if st.Kind == pwNotKnown || st.Kind == pwNotSet {
			a.ChangeLabel = "Enter…"
			a.UseBroker = pw.CanBroker
		}
	}
	if a.Test || a.Change {
		r.PWActs = &a
	}
	return r
}

// pwTestPath is the operator route of the device page's Test button, below the device.
const pwTestPath = "/mqtt-password/test"

// learnPasswordsOn is the switch MQTT_PASSWORD_FROM_BACKUPS as saved. Without a settings service the learner has
// no switch to read either; it runs, so the answer is on.
func (h *handler) learnPasswordsOn() bool {
	if h.d.SysConfig == nil {
		return true
	}
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "MQTT_PASSWORD_FROM_BACKUPS" {
			return st.Value != "false"
		}
	}
	return true
}

// brokerHasPassword says TasmoFleet's broker password is set: without it "use the broker password" would send
// nothing.
func (h *handler) brokerHasPassword() bool {
	if h.d.SysConfig == nil {
		return false
	}
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "MQTT_PASSWORD" {
			return st.HasValue
		}
	}
	return false
}
