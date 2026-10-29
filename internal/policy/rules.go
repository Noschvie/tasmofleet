package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
)

// MaxRules bounds the policy file.
const MaxRules = 100

var (
	commandRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,31}$`)
	wantRe    = regexp.MustCompile(`^[A-Za-z0-9.,:+_/ -]{1,64}$`)
	// RuleIDRe is the format of a rule id: "r" plus 1 to 15 lowercase letters or digits.
	RuleIDRe = regexp.MustCompile(`^r[a-z0-9]{1,15}$`)
)

// LegacyID is the id given to the rule at position i (0-based) of a list that
// has no ids: rules stored before 0.19 and POLICY_RULES files without ids. It
// is derived from the position, so the same list always gets the same ids.
func LegacyID(i int) string { return "r" + strconv.Itoa(i+1) }

var knownFamilies = []model.ChipFamily{
	model.ChipESP8266, model.ChipESP32, model.ChipESP32S2, model.ChipESP32S3, model.ChipESP32C2,
	model.ChipESP32C3, model.ChipESP32C5, model.ChipESP32C6, model.ChipESP32C61, model.ChipESP32P4,
}

// AllESP32 in a rule's "families" stands for every ESP32 chip family, so a
// new variant cannot silently fall outside an "ESP32 exception".
const AllESP32 = "esp32*"

// FamilyNames lists the values a rule's "families" accepts: the wildcard
// first, then every known chip family. The rule editor offers exactly these,
// so it cannot drift from what ParseRules accepts.
func FamilyNames() []string {
	out := []string{AllESP32}
	for _, f := range knownFamilies {
		out = append(out, string(f))
	}
	return out
}

func esp32Families() []model.ChipFamily {
	var out []model.ChipFamily
	for _, f := range knownFamilies {
		if f.IsESP32() {
			out = append(out, f)
		}
	}
	return out
}

type ruleJSON struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Severity string   `json:"severity"`
	Tags     []string `json:"tags"`
	Devices  []string `json:"devices"`
	Command  string   `json:"command"`
	Want     string   `json:"want"`
	Note     string   `json:"note"`
	Families []string `json:"families"`
	Except   []string `json:"except"`
	Disabled bool     `json:"disabled"`
}

// ParseRules reads the POLICY_RULES JSON array. The array order is the
// precedence of the rules (see Check). A disabled rule is validated like any
// other, so enabling it later cannot fail. Unknown JSON keys, bad
// command names, bad families and an empty "want" are errors: a policy that is
// silently half-understood would report compliance that does not exist.
// allowed (may be nil) is asked whether a command may be queried from a
// device; it keeps rules from naming commands that act when sent bare.
func ParseRules(data []byte, allowed func(command string) bool) ([]Rule, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var in []ruleJSON
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("policy rules: %w", err)
	}
	if dec.More() {
		return nil, errors.New("policy rules: trailing data after the array")
	}
	if len(in) > MaxRules {
		return nil, fmt.Errorf("policy rules: at most %d rules", MaxRules)
	}
	out := make([]Rule, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, j := range in {
		if j.ID != "" && !RuleIDRe.MatchString(j.ID) {
			return nil, fmt.Errorf("policy rules: rule at index %d: invalid id %q", i, j.ID)
		}
		r := Rule{ID: j.ID, Name: j.Name, Command: j.Command, Want: j.Want, Note: j.Note, Except: j.Except, Tags: j.Tags, Devices: j.Devices, Severity: j.Severity, Disabled: j.Disabled}
		if r.Severity == "" {
			r.Severity = SeverityWarn
		}
		if r.ID == "" {
			r.ID = LegacyID(i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("policy rules: rule at index %d: duplicate id %q", i, r.ID)
		}
		seen[r.ID] = true
		if err := checkScopeFields(i, j); err != nil {
			return nil, err
		}
		switch {
		case !commandRe.MatchString(j.Command):
			return nil, fmt.Errorf("policy rules: rule at index %d: invalid command %q", i, j.Command)
		case commandProblem(j.Command, j.Want) != nil:
			return nil, fmt.Errorf("policy rules: rule at index %d: command %q cannot be checked: %w", i, j.Command, commandProblem(j.Command, j.Want))
		case allowed != nil && !allowed(j.Command):
			return nil, fmt.Errorf("policy rules: rule at index %d: command %q cannot be checked (secret, unknown or side-effecting commands are refused)", i, j.Command)
		case !wantRe.MatchString(j.Want):
			return nil, fmt.Errorf("policy rules: rule at index %d: invalid or empty want", i)
		case len(j.Note) > 300:
			return nil, fmt.Errorf("policy rules: rule at index %d: note too long", i)
		}
		for _, e := range j.Except {
			if e == "" {
				return nil, fmt.Errorf("policy rules: rule at index %d: empty exception", i)
			}
		}
		for _, f := range j.Families {
			if f == AllESP32 {
				r.Families = append(r.Families, esp32Families()...)
				continue
			}
			cf := model.ChipFamily(f)
			if !hasFamily(knownFamilies, cf) {
				return nil, fmt.Errorf("policy rules: rule at index %d: unknown family %q", i, f)
			}
			r.Families = append(r.Families, cf)
		}
		out = append(out, r)
	}
	return out, nil
}

// commandProblem says why a rule may not name command, or nil. A curated
// setting keeps exactly its former behaviour (its own validation, secrets
// refused). Any other command is a generic rule: it must be readable by its
// bare name, and its set form must be sendable by an admin without a
// confirmation (cmdpolicy), because Make compliant sends it unasked.
func commandProblem(command, want string) error {
	if canon, curated := settings.Curated(command); curated {
		f, isField := settings.Lookup(canon)
		switch {
		case canon != command:
			return fmt.Errorf("write the setting as %s", canon)
		case isField && f.Secret:
			return errors.New("secret settings cannot be checked")
		}
		return nil
	}
	if ok, why := cmdpolicy.PolicyReadable(command); !ok {
		return errors.New(why)
	}
	if !wantRe.MatchString(want) {
		return nil // reported as the want's own problem
	}
	_, err := cmdpolicy.PolicySetText(command, want)
	return err
}

const (
	maxNameLen   = 80
	maxScopeList = 32
	maxScopeLen  = 64
)

// checkScopeFields validates the fields that only describe or scope a rule.
func checkScopeFields(i int, j ruleJSON) error {
	bad := func(msg string) error { return fmt.Errorf("policy rules: rule at index %d: %s", i, msg) }
	switch j.Severity {
	case "", SeverityWarn, SeverityInfo:
	default:
		return bad("severity must be info or warn")
	}
	if len(j.Name) > maxNameLen || hasControl(j.Name) {
		return bad("invalid name (at most 80 characters, no control characters)")
	}
	for label, list := range map[string][]string{"tags": j.Tags, "devices": j.Devices} {
		if len(list) > maxScopeList {
			return bad("too many " + label)
		}
		for _, e := range list {
			if e == "" || len(e) > maxScopeLen || hasControl(e) {
				return bad("invalid entry in " + label)
			}
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Chip labels shown for a scope that covers a whole family.
const (
	LabelAllESP32 = "ESP32 (all variants)"
	LabelESP8266  = "ESP8266"
)

// ScopeLabels turns a rule's chip families into what people read: when the
// list covers every ESP32 variant (the esp32* wildcard, or all of them spelled
// out) it is one "ESP32 (all variants)" entry, and esp8266 reads "ESP8266".
// Anything narrower keeps its exact names, so a partial scope is never
// mistaken for the whole family. Order of first appearance is kept.
func ScopeLabels(families []string) []string {
	have := map[string]bool{}
	for _, f := range families {
		have[f] = true
	}
	all := have[AllESP32]
	if !all {
		all = true
		for _, f := range esp32Families() {
			all = all && have[string(f)]
		}
	}
	var out []string
	done := false
	for _, f := range families {
		switch {
		case f == string(model.ChipESP8266):
			out = append(out, LabelESP8266)
		case f == AllESP32 || (all && model.ChipFamily(f).IsESP32()):
			if !done {
				out = append(out, LabelAllESP32)
				done = true
			}
		default:
			out = append(out, f)
		}
	}
	return out
}

// FamilyLabel is the editor chip text for one accepted "families" value.
func FamilyLabel(value string) string {
	switch value {
	case AllESP32:
		return LabelAllESP32
	case string(model.ChipESP8266):
		return LabelESP8266
	}
	return value
}
