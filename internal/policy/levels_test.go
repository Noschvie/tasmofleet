package policy

import (
	"strings"
	"testing"
)

// Why: the policy file speaks the words of the UI. A person editing the JSON by hand reads "Required" and
// "Advisory" on the page, so the file says "level": "required" / "advisory" and "expected"; the stored
// severity ("warn" is the strong one, "info" the weak one) stays internal. LevelOf is the one mapping the
// export uses, and ResolveFileKeys is its inverse for every reader of a file.
func TestLevelWordsMapToTheStoredSeverity(t *testing.T) {
	for _, c := range []struct{ severity, level string }{
		{SeverityWarn, LevelRequired}, {SeverityInfo, LevelAdvisory}, {"", LevelRequired},
	} {
		if got := LevelOf(c.severity); got != c.level {
			t.Errorf("LevelOf(%q) = %q, want %q", c.severity, got, c.level)
		}
	}
	sev, want, err := ResolveFileKeys(LevelAdvisory, "", "60", "")
	if err != nil || sev != SeverityInfo || want != "60" {
		t.Errorf("advisory/expected: %q %q %v", sev, want, err)
	}
	sev, _, err = ResolveFileKeys(LevelRequired, "", "1", "")
	if err != nil || sev != SeverityWarn {
		t.Errorf("required: %q %v", sev, err)
	}
	// An old file: no level, the stored words.
	sev, want, err = ResolveFileKeys("", SeverityInfo, "", "7")
	if err != nil || sev != SeverityInfo || want != "7" {
		t.Errorf("old keys: %q %q %v", sev, want, err)
	}
	// No severity at all stays empty: the default (Required) is applied where severities are read.
	if sev, _, err = ResolveFileKeys("", "", "1", ""); err != nil || sev != "" {
		t.Errorf("no level: %q %v", sev, err)
	}
	// Both spellings with the same meaning are fine (a file written by hand while migrating).
	if _, _, err = ResolveFileKeys(LevelRequired, SeverityWarn, "1", "1"); err != nil {
		t.Errorf("agreeing keys refused: %v", err)
	}
}

// Why: a file that says "level": "advisory" and "severity": "warn" has two answers for one question. Silently
// preferring one would make a rule stricter or weaker than its author thinks, so the file is refused and the
// message names both keys.
func TestResolveFileKeysRefusesConflicts(t *testing.T) {
	for name, c := range map[string]struct{ level, severity, expected, want, msg string }{
		"level against severity": {LevelAdvisory, SeverityWarn, "1", "", `"level" (advisory) and "severity" (warn) disagree`},
		"required against info":  {LevelRequired, SeverityInfo, "1", "", `"level" (required) and "severity" (info) disagree`},
		"expected against want":  {LevelRequired, "", "1", "2", `"expected" and "want" disagree`},
		"unknown level":          {"strict", "", "1", "", `"level" must be required or advisory`},
	} {
		_, _, err := ResolveFileKeys(c.level, c.severity, c.expected, c.want)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: err %v, want it to say %s", name, err, c.msg)
		}
	}
	// A want can be a secret (MqttPassword): the message goes to the log and must not carry either value.
	_, _, err := ResolveFileKeys("", "", "hunter2", "hunter3")
	if err == nil || strings.Contains(err.Error(), "hunter") {
		t.Errorf("the conflict message echoes a value: %v", err)
	}
}

// Why: POLICY_RULES files written today (severity, want) must keep working, and the file the export writes
// (level, expected) is a POLICY_RULES file too: ParseRules reads both spellings and refuses a file that sets
// both with different values, naming the rule.
func TestParseRulesReadsBothSpellings(t *testing.T) {
	rules, err := ParseRules([]byte(`[
		{"name":"a","command":"SaveData","expected":"1","level":"advisory"},
		{"name":"b","command":"SetOption0","want":"1","severity":"info"},
		{"name":"c","command":"SetOption1","expected":"1","level":"required"},
		{"name":"d","command":"SetOption2","want":"1"}]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 || rules[0].Severity != SeverityInfo || rules[0].Want != "1" || rules[1].Severity != SeverityInfo || rules[2].Severity != SeverityWarn || rules[3].Severity != SeverityWarn {
		t.Fatalf("rules %+v", rules)
	}
	_, err = ParseRules([]byte(`[{"command":"SaveData","want":"1"},{"command":"SetOption0","expected":"1","level":"advisory","severity":"warn"}]`), nil)
	if err == nil || !strings.Contains(err.Error(), "index 1") || !strings.Contains(err.Error(), `"level" (advisory) and "severity" (warn) disagree`) {
		t.Fatalf("conflict: %v", err)
	}
	_, err = ParseRules([]byte(`[{"command":"SaveData","expected":"1","want":"0"}]`), nil)
	if err == nil || !strings.Contains(err.Error(), "index 0") || !strings.Contains(err.Error(), `"expected" and "want" disagree`) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err = ParseRules([]byte(`[{"command":"SaveData","expected":"1","level":"strict"}]`), nil); err == nil {
		t.Fatal("an unknown level is refused, not defaulted")
	}
	// An unknown stored severity is still refused by the scope check, not turned into something else.
	if _, err = ParseRules([]byte(`[{"command":"SaveData","want":"1","severity":"loud"}]`), nil); err == nil {
		t.Fatal("an unknown severity must stay refused")
	}
}
