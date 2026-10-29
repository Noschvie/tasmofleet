// Package policy compares devices against a declared fleet policy (M2) and
// reports drift, e.g. the flash-wear policy PowerOnState 0,
// SetOption0 0 (save_state), SaveData 0, Timezone 99, with per-device or
// per-family exceptions (ESP32: SetOption0 1). Values come from decoded
// backups when available, else from live status. Fixing drift is an explicit
// bulk action, never automatic.
package policy

import (
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Rule is one expected setting.
type Rule struct {
	// ID names the rule across versions and reorders (see RuleIDRe, LegacyID).
	ID      string
	Name    string // short title shown in the editor and the report, optional
	Command string // e.g. "PowerOnState"
	Want    string // e.g. "0"
	Note    string // human description, SetOption numbers always with their name
	// Families limits the rule to these chip families (exact match). Empty
	// means every family. A device of unknown family cannot be judged by a
	// family-limited rule and is reported as unknown.
	Families []model.ChipFamily
	// Except lists device names or tags that are exempt from the rule.
	Except []string
	// Tags and Devices limit the rule to devices carrying one of the tags or
	// named in Devices (the union). Both empty means the whole fleet. Families
	// narrow the scope further.
	Tags    []string
	Devices []string
	// Severity is SeverityWarn (default) or SeverityInfo.
	Severity string
	// Disabled rules are kept (and versioned) but never evaluated: they
	// produce no finding and take no part in precedence.
	Disabled bool
}

// Rule severities.
const (
	SeverityWarn = "warn"
	SeverityInfo = "info"
)

// Finding is one deviation or one value that could not be checked.
type Finding struct {
	DeviceID model.DeviceID
	Rule     Rule
	// RuleIndex is the position of Rule in the list given to Check, so a
	// caller can attribute findings to one rule card.
	RuleIndex int
	Have      string // the value found; empty when Unknown
	Severity  string // the rule's severity, never empty
	// Source says where Have came from: "backup" or "live". Check leaves it
	// empty, the caller knows where each device's values were read.
	Source string
	// Unknown is true when no value (or no family) was available. It is
	// never "ok": a missing value must not look like compliance.
	Unknown bool
}

// Check returns the findings for every device and rule, in device then rule
// order. Compliant device/rule pairs produce no finding. values maps a device
// to its settings by command name (case-insensitive); a missing device or
// command means "unknown". Pure: no I/O.
//
// The order of rules is their precedence: when two enabled rules apply to the
// same command on the same device, only the first one is evaluated for that
// device (the later one is shadowed there, see Conflicts). Disabled rules are
// skipped entirely.
func Check(rules []Rule, devices []model.Device, values map[model.DeviceID]map[string]string) []Finding {
	var out []Finding
	for _, d := range devices {
		have := lowerKeys(values[d.ID])
		claimed := map[string]bool{}
		for i, r := range rules {
			key := strings.ToLower(r.Command)
			if r.Disabled || claimed[key] || !applies(r, d) {
				continue
			}
			claimed[key] = true
			f := Finding{DeviceID: d.ID, Rule: r, RuleIndex: i, Severity: severityOf(r)}
			v, ok := have[key]
			switch {
			case len(r.Families) > 0 && d.Identity.Family == model.ChipUnknown, !ok:
				f.Unknown = true
			case equal(v, r.Want):
				continue
			default:
				f.Have = v
			}
			out = append(out, f)
		}
	}
	return out
}

// applies reports whether r is evaluated for d: in scope, not excepted, and of
// a matching chip family. A device of unknown family is claimed by a
// family-limited rule (it is reported as unknown there), so a lower rule never
// judges a device the higher one could not rule out.
func applies(r Rule, d model.Device) bool {
	if excepted(r, d) || !inScope(r, d) {
		return false
	}
	return len(r.Families) == 0 || d.Identity.Family == model.ChipUnknown || hasFamily(r.Families, d.Identity.Family)
}

// Conflict is an overlap of two enabled rules on the same command: on Devices
// devices both apply, and only Higher (the earlier rule) is evaluated there.
type Conflict struct {
	Higher, Lower int // positions in the rule list, Higher < Lower
	Command       string
	Devices       int  // devices both rules apply to
	SameWant      bool // both expect the same value: redundant rather than contradictory
}

// Conflicts lists every pair of enabled rules that apply to the same command
// on at least one common device that both could judge (a device of unknown
// family is shared only when one of the rules is not family-limited), ordered by Higher then Lower. Pure: the page
// shows them so precedence is never silent.
func Conflicts(rules []Rule, devices []model.Device) []Conflict {
	var out []Conflict
	for i, a := range rules {
		if a.Disabled {
			continue
		}
		for j := i + 1; j < len(rules); j++ {
			b := rules[j]
			if b.Disabled || !strings.EqualFold(a.Command, b.Command) {
				continue
			}
			n := 0
			for _, d := range devices {
				if !applies(a, d) || !applies(b, d) {
					continue
				}
				// Two family-limited rules both report a device of unknown family as unknown:
				// neither can judge it, so the higher one "winning" there changes nothing.
				if d.Identity.Family == model.ChipUnknown && len(a.Families) > 0 && len(b.Families) > 0 {
					continue
				}
				n++
			}
			if n > 0 {
				out = append(out, Conflict{Higher: i, Lower: j, Command: a.Command, Devices: n, SameWant: equal(a.Want, b.Want)})
			}
		}
	}
	return out
}

// Severity labels as people read them (design v2, wording table): a Required
// deviation is flagged, counted and preselected for Make compliant; an
// Advisory one is a hint. Nothing is ever written to a device automatically.
const (
	LabelRequired = "Required"
	LabelAdvisory = "Advisory"
)

// SeverityLabel names a rule severity; empty and unknown values read as
// Required, the stricter default.
func SeverityLabel(severity string) string {
	if severity == SeverityInfo {
		return LabelAdvisory
	}
	return LabelRequired
}

func lowerKeys(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func severityOf(r Rule) string {
	if r.Severity == "" {
		return SeverityWarn
	}
	return r.Severity
}

// inScope reports whether the rule's tag/device scope includes d.
func inScope(r Rule, d model.Device) bool {
	if len(r.Tags) == 0 && len(r.Devices) == 0 {
		return true
	}
	for _, n := range r.Devices {
		if n == d.Name {
			return true
		}
	}
	for _, t := range r.Tags {
		for _, dt := range d.Tags {
			if t == dt {
				return true
			}
		}
	}
	return false
}

func excepted(r Rule, d model.Device) bool {
	for _, e := range r.Except {
		if e == d.Name {
			return true
		}
		for _, t := range d.Tags {
			if e == t {
				return true
			}
		}
	}
	return false
}

func hasFamily(list []model.ChipFamily, f model.ChipFamily) bool {
	for _, x := range list {
		if x == f {
			return true
		}
	}
	return false
}

// equal compares values the way Tasmota reports them: ON/OFF are 1/0 and
// whitespace around a value does not matter.
func equal(have, want string) bool { return canon(have) == canon(want) }

func canon(v string) string {
	v = strings.TrimSpace(v)
	switch strings.ToUpper(v) {
	case "ON":
		return "1"
	case "OFF":
		return "0"
	}
	return v
}
