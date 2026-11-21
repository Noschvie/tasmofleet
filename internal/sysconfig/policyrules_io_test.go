package sysconfig

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/policy"
)

var (
	advisoryRule = RuleSpec{Name: "Quiet", Command: "SetOption0", Want: "1", Severity: policy.SeverityInfo}
	requiredRule = RuleSpec{Name: "Flash wear", Command: "SaveData", Want: "0", Severity: policy.SeverityWarn}
)

// Why: the file is read by people. The page says Required and Advisory and "expected value", so the file says
// "level": "required" | "advisory" and "expected"; the stored words (severity warn / info, want) are internal.
// The round trip must keep each rule's level: the export of an Advisory rule that came back as Required would
// make a hint into a deviation on every device.
func TestExportUsesTheWordsOfTheUIAndRoundTrips(t *testing.T) {
	r, _, _ := newRules("", nil)
	ctx := context.Background()
	if _, err := r.Save(ctx, "a", "", AnyVersion, []RuleSpec{advisoryRule, requiredRule}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := r.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := string(data)
	for _, want := range []string{`"level": "advisory"`, `"level": "required"`, `"expected": "1"`, `"expected": "0"`} {
		if !strings.Contains(f, want) {
			t.Errorf("the file lacks %s:\n%s", want, f)
		}
	}
	for _, old := range []string{`"severity"`, `"want"`} {
		if strings.Contains(f, old) {
			t.Errorf("the file still says %s:\n%s", old, f)
		}
	}
	specs, err := r.ParseImport(data)
	if err != nil || len(specs) != 2 {
		t.Fatalf("specs %+v err %v", specs, err)
	}
	if specs[0].Severity != policy.SeverityInfo || specs[1].Severity != policy.SeverityWarn || specs[0].Want != "1" || specs[1].Want != "0" {
		t.Fatalf("the round trip changed a level or a value: %+v", specs)
	}
	// What was just exported is no change when it is imported again, so the confirm step says so.
	if _, err := r.Save(ctx, "a", "import", AnyVersion, specs, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("re-importing the export: %v", err)
	}
}

// Why: a rule stored without a severity (every version before 0.19 may have none) is Required. Its export says
// "required"; importing that must not look like an edit, or an unedited export would be offered as a change.
func TestExportOfAStoredRuleWithoutSeverityImportsAsNoChange(t *testing.T) {
	r, st, _ := newRules("", nil)
	ctx := context.Background()
	legacyVersion(st, legacyTwo)
	data, err := r.Export(ctx)
	if err != nil || !strings.Contains(string(data), `"level": "required"`) {
		t.Fatalf("%v\n%s", err, data)
	}
	specs, err := r.ParseImport(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Save(ctx, "a", "import", AnyVersion, specs, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("an unedited export must be ErrNoChange: %v", err)
	}
}

// Why: files written before 0.46.1 (and every POLICY_RULES variable in use) say severity and want. They keep
// working: "warn" is Required, "info" is Advisory. This guards the old spelling; the new one is guarded above.
func TestImportReadsTheOldKeys(t *testing.T) {
	r, _, _ := newRules("", nil)
	specs, err := r.ParseImport([]byte(`[{"command":"SaveData","want":"0","severity":"warn"},{"command":"SetOption0","want":"1","severity":"info"}]`))
	if err != nil || len(specs) != 2 || specs[0].Severity != policy.SeverityWarn || specs[1].Severity != policy.SeverityInfo || specs[0].Want != "0" {
		t.Fatalf("specs %+v err %v", specs, err)
	}
}

// Why: a file that sets both spellings with different values has two answers for one question; taking either
// silently would make a rule stricter or weaker than its author thinks. The import is refused and says which rule
// and which keys. (Today the unknown key "level" is refused for another reason, so this checks the message.)
func TestImportRefusesConflictingKeys(t *testing.T) {
	r, _, _ := newRules("", nil)
	for name, c := range map[string]struct{ file, msg string }{
		"level against severity": {`[{"command":"SaveData","want":"0"},{"command":"SetOption0","expected":"1","level":"advisory","severity":"warn"}]`, `"level" (advisory) and "severity" (warn) disagree`},
		"expected against want":  {`[{"command":"SaveData","expected":"0","want":"1"}]`, `"expected" and "want" disagree`},
		"unknown level":          {`[{"command":"SaveData","expected":"0","level":"strict"}]`, `"level" must be required or advisory`},
	} {
		_, err := r.ParseImport([]byte(c.file))
		var re *RuleError
		if !errors.As(err, &re) || !strings.Contains(re.Msg, c.msg) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		wantIdx := 0
		if name == "level against severity" {
			wantIdx = 1
		}
		if re.Index != wantIdx {
			t.Errorf("%s: the error names rule index %d, want %d", name, re.Index, wantIdx)
		}
	}
	// Why (safety rule 4): the conflict is found before the secret check, and its message ends up in the log and
	// on the settings import: a want for a secret setting must not be echoed there.
	secret := `[{"command":"MqttPassword","expected":"hunter2","want":"hunter3"}]`
	_, err := r.ParseImport([]byte(secret))
	if err == nil || strings.Contains(err.Error(), "hunter") {
		t.Errorf("ParseImport echoes a value: %v", err)
	}
	_, err = r.ImportLegacy(context.Background(), "env", []byte(secret), nil)
	if err == nil || strings.Contains(err.Error(), "hunter") {
		t.Errorf("ImportLegacy echoes a value: %v", err)
	}
	// The same file is refused by the POLICY_RULES reader, and nothing is stored.
	_, err = r.ImportLegacy(context.Background(), "env", []byte(`[{"command":"SaveData","expected":"0","want":"1"}]`), nil)
	var re *RuleError
	if !errors.As(err, &re) {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if has, _ := r.HasVersions(context.Background()); has {
		t.Fatal("a refused file stored a version")
	}
}

// Why: the POLICY_RULES variable is read by ImportLegacy, not by ParseImport, and the file a person exports is
// a valid POLICY_RULES value. Both spellings must land as version 1 with the position ids a file without ids
// always got (a link to a rule must survive the import), and with the stored words, so nothing is stored in a
// new format.
func TestImportLegacyReadsNewKeysAndKeepsPositionIDs(t *testing.T) {
	r, st, _ := newRules("", nil)
	ctx := context.Background()
	id, err := r.ImportLegacy(ctx, "env", []byte(`[{"name":"A","command":"SaveData","expected":"0","level":"advisory"},{"name":"B","command":"Timezone","want":"99","severity":"warn"}]`), nil)
	if err != nil || id != 1 {
		t.Fatalf("id %d err %v", id, err)
	}
	cur, _ := st.CurrentPolicyRules(ctx)
	for _, want := range []string{`"id":"r1"`, `"id":"r2"`, `"want":"0"`, `"severity":"info"`, `"severity":"warn"`} {
		if !strings.Contains(cur.Rules, want) {
			t.Errorf("stored version lacks %s: %s", want, cur.Rules)
		}
	}
	if strings.Contains(cur.Rules, `"level"`) || strings.Contains(cur.Rules, `"expected"`) {
		t.Errorf("the file's keys leaked into the stored format: %s", cur.Rules)
	}
}

// Why: the confirm step of an import replaces ALL rules, so the preview must show every rule with its fate: the
// ones the file changes, adds, keeps, and the ones it removes by leaving them out. A removal that is not listed
// by name is the data loss the person cannot see coming. DiffSpecs (the audit detail of a saved version) stays
// as it was: it never lists unchanged rules.
func TestImportPreviewListsEveryRuleWithItsFate(t *testing.T) {
	cur := []RuleSpec{
		{ID: "rkeep00001", Name: "Keep", Command: "SaveData", Want: "0"},
		{ID: "rchange0001", Name: "Change", Command: "SetOption0", Want: "1"},
		{ID: "rgone000001", Name: "Gone", Command: "Timezone", Want: "99"},
	}
	next := []RuleSpec{
		{ID: "rnew0000001", Name: "Fresh", Command: "SetOption1", Want: "1"},
		{ID: "rchange0001", Name: "Change", Command: "SetOption0", Want: "0"},
		{ID: "rkeep00001", Name: "Keep", Command: "SaveData", Want: "0"},
	}
	got := ImportPreview(cur, next)
	var kinds []string
	for _, c := range got {
		kinds = append(kinds, c.Kind+":"+c.Name)
	}
	if want := "added:Fresh changed:Change unchanged:Keep removed:Gone"; strings.Join(kinds, " ") != want {
		t.Fatalf("preview %v, want %s", kinds, want)
	}
	if got[1].Detail == "" || got[3].RuleID != "rgone000001" {
		t.Fatalf("details: %+v", got)
	}
	// The audit diff of the same pair: removed and changed in the old rules' order, then added; no unchanged.
	var d []string
	for _, c := range DiffSpecs(cur, next) {
		d = append(d, c.Kind+":"+c.Name)
	}
	if want := "changed:Change removed:Gone added:Fresh"; strings.Join(d, " ") != want {
		t.Fatalf("DiffSpecs %v, want %s", d, want)
	}
	if c := DiffSpecs(cur, next)[0]; !reflect.DeepEqual(c, RuleChange{Kind: "changed", Rule: "Change (SetOption0)", Name: "Change", RuleID: "rchange0001", Detail: `expected "1" -> "0"`, Fields: []string{"expected value"}}) {
		t.Fatalf("the audit detail changed: %+v", c)
	}
	// An identical file has only unchanged rows.
	for _, c := range ImportPreview(cur, cur) {
		if c.Kind != "unchanged" {
			t.Errorf("identical file: %+v", c)
		}
	}
	// An empty file removes everything, by name.
	if got := ImportPreview(cur, nil); len(got) != 3 || got[0].Kind != "removed" || got[0].Name != "Keep" {
		t.Fatalf("empty file: %+v", got)
	}
}
