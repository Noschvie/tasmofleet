package policy

import "fmt"

// The words of a rule's level in a policy file, as the UI says them (design system, Wording): a Required rule is
// stored as severity "warn", an Advisory rule as severity "info". The stored words stay as they are (stored
// versions and POLICY_RULES files written before 0.46.1 use them); only files say "level" and "expected".
const (
	LevelRequired = "required"
	LevelAdvisory = "advisory"
)

// LevelOf names a stored severity in the words of a policy file. Empty and unknown values read as required,
// as SeverityLabel does.
func LevelOf(severity string) string {
	if severity == SeverityInfo {
		return LevelAdvisory
	}
	return LevelRequired
}

// ResolveFileKeys turns the keys of one rule in a policy file into the stored severity and want. A file says
// "level" ("required" or "advisory") and "expected"; files written before 0.46.1 say "severity" ("warn" or
// "info") and "want", and both spellings are read. A file that gives both spellings of one thing with different
// values is refused (silently preferring one would make a rule stricter or weaker than its author thinks), and so
// is a level that is neither word. With neither level nor severity the severity stays empty: the default is
// applied where severities are read. An unknown severity is passed on; the rule's own checks refuse it.
// This is the one mapping: the export, ParseImport, ImportLegacy and ParseRules all go through it and LevelOf.
func ResolveFileKeys(level, severity, expected, want string) (sev, wantOut string, err error) {
	wantOut = want
	if expected != "" {
		if want != "" && want != expected {
			return "", "", fmt.Errorf("%q and %q disagree: use one of them", "expected", "want") // no values: a want can be a secret
		}
		wantOut = expected
	}
	sev = severity
	if level == "" {
		return sev, wantOut, nil
	}
	var fromLevel string
	switch level {
	case LevelRequired:
		fromLevel = SeverityWarn
	case LevelAdvisory:
		fromLevel = SeverityInfo
	default:
		return "", "", fmt.Errorf(`"level" must be %s or %s`, LevelRequired, LevelAdvisory)
	}
	if severity == "" || severity == SeverityWarn || severity == SeverityInfo {
		if severity != "" && severity != fromLevel {
			return "", "", fmt.Errorf("%q (%s) and %q (%s) disagree: use one of them", "level", level, "severity", severity)
		}
		sev = fromLevel
	}
	return sev, wantOut, nil
}
