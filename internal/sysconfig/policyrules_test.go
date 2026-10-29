package sysconfig

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type memRules struct {
	mu sync.Mutex
	vs []store.PolicyRuleVersion
}

func (m *memRules) AddPolicyRuleVersion(_ context.Context, v store.PolicyRuleVersion) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v.ID = int64(len(m.vs) + 1)
	m.vs = append(m.vs, v)
	return v.ID, nil
}

func (m *memRules) CurrentPolicyRules(context.Context) (store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.vs) == 0 {
		return store.PolicyRuleVersion{}, store.ErrNotFound
	}
	return m.vs[len(m.vs)-1], nil
}

func (m *memRules) GetPolicyRuleVersion(_ context.Context, id int64) (store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.vs) {
		return store.PolicyRuleVersion{}, store.ErrNotFound
	}
	return m.vs[id-1], nil
}

func (m *memRules) ListPolicyRuleVersions(_ context.Context, limit int) ([]store.PolicyRuleVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.PolicyRuleVersion
	for i := len(m.vs) - 1; i >= 0; i-- {
		out = append(out, m.vs[i])
	}
	return out, nil
}

func queryable(c string) bool { return c != "MqttPassword" && c != "Restart" }

func newRules(env string, onChange func([]policy.Rule)) (*Rules, *memRules, *memAudit) {
	st, aud := &memRules{}, &memAudit{}
	o := RulesOptions{Store: st, Audit: aud, Queryable: queryable, OnChange: onChange,
		ValidateWant: func(cmd, want string) error {
			if cmd == "SetOption0" && want != "0" && want != "1" {
				return errors.New("must be 0 or 1")
			}
			return nil
		},
		Preview: func(_ context.Context, rules []policy.Rule) (PreviewResult, error) {
			return PreviewResult{Devices: 3, Rules: make([]RulePreview, len(rules))}, nil
		}}
	return NewRules(o), st, aud
}

var flash = RuleSpec{Name: "Flash wear", Command: "SaveData", Want: "0", Severity: "warn", Families: []string{"esp32*"}, Tags: []string{"kitchen"}}

// Why: the editor must refuse what POLICY_RULES refuses: secret and
// side-effecting queries, bad values, injection. The error names the rule.
func TestRulesValidationMatchesTheFileFormat(t *testing.T) {
	r, _, _ := newRules("", nil)
	for name, spec := range map[string]RuleSpec{
		"secret query":  {Command: "MqttPassword", Want: "x"},
		"not queryable": {Command: "Restart", Want: "1"},
		"injection":     {Command: "SaveData", Want: "0;Restart 1"},
		"bad want":      {Command: "SetOption0", Want: "7"},
		"bad severity":  {Command: "SaveData", Want: "0", Severity: "fatal"},
		"empty want":    {Command: "SaveData"},
	} {
		_, err := r.Validate([]RuleSpec{flash, spec})
		var re *RuleError
		if !errors.As(err, &re) || re.Index != 1 {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := r.Validate([]RuleSpec{flash}); err != nil {
		t.Fatal(err)
	}
}

// Why: every save is a version that can be diffed and restored, and the audit
// log records what changed.
func TestRulesAreVersionedWithDiffAndRevert(t *testing.T) {
	var live []policy.Rule
	r, st, aud := newRules("", func(rs []policy.Rule) { live = rs })
	ctx := context.Background()
	v1, err := r.Save(ctx, "alice", "first", []RuleSpec{flash}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Name != "Flash wear" {
		t.Fatalf("live push: %+v", live)
	}
	edited := flash
	edited.Want = "1"
	v2, err := r.Save(ctx, "bob", "", []RuleSpec{edited, {Command: "Timezone", Want: "99"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Save(ctx, "bob", "", []RuleSpec{edited, {Command: "Timezone", Want: "99"}}, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("an identical save must not create a version: %v", err)
	}
	d, err := r.VersionDiff(ctx, v1, v2)
	if err != nil || len(d) != 2 || d[0].Kind != "changed" || !strings.Contains(d[0].Detail, `expected "0" -> "1"`) || d[1].Kind != "added" {
		t.Fatalf("diff %+v err %v", d, err)
	}
	e := aud.last()
	if e.Action != "policy.rules.save" || e.Actor != "bob" || !strings.Contains(e.Detail, `"kind":"added"`) {
		t.Fatalf("audit %+v", e)
	}
	v3, err := r.Revert(ctx, "carol", v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := st.CurrentPolicyRules(ctx)
	if cur.ID != v3 || cur.RevertOf != v1 || len(live) != 1 || live[0].Want != "0" {
		t.Fatalf("revert: %+v live %+v", cur, live)
	}
	if aud.last().Action != "policy.rules.revert" {
		t.Fatalf("audit %+v", aud.last())
	}
}

func TestRulesExportImportRoundTrip(t *testing.T) {
	r, _, _ := newRules("", nil)
	ctx := context.Background()
	if _, err := r.Save(ctx, "a", "", []RuleSpec{flash, {Command: "SetOption0", Want: "1", Except: []string{"lamp"}}}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := r.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The export is the POLICY_RULES format: policy.ParseRules reads it.
	if _, err := policy.ParseRules(data, nil); err != nil {
		t.Fatalf("export does not parse as POLICY_RULES: %v\n%s", err, data)
	}
	specs, err := r.ParseImport(data)
	if err != nil || len(specs) != 2 || specs[1].Except[0] != "lamp" {
		t.Fatalf("specs %+v err %v", specs, err)
	}
	for name, bad := range map[string]string{
		"unknown key": `[{"command":"SaveData","want":"0","evil":1}]`, "not json": `x`, "secret": `[{"command":"MqttPassword","want":"x"}]`,
		"huge": strings.Repeat(" ", 300<<10),
	} {
		if _, err := r.ParseImport([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRulesPreviewValidatesFirst(t *testing.T) {
	r, _, _ := newRules("", nil)
	res, err := r.Preview(context.Background(), []RuleSpec{flash})
	if err != nil || res.Devices != 3 || len(res.Rules) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.Preview(context.Background(), []RuleSpec{{Command: "MqttPassword", Want: "x"}}); err == nil {
		t.Fatal("a preview of an invalid rule must fail")
	}
}

// Why: upgrading must not lose a rule. A POLICY_RULES file was only ever checked by
// ParseRules; the stricter value rules of the editor must not drop its rules.
func TestImportLegacyKeepsRulesThatOnlyParseRulesChecked(t *testing.T) {
	r, st, _ := newRules("", nil)
	ctx := context.Background()
	legacy := []byte(`[{"command":"SetOption0","want":"7"},{"command":"SaveData","want":"0"}]`) // 7 fails the editor's value rule
	if _, err := r.Validate([]RuleSpec{{Command: "SetOption0", Want: "7"}}); err == nil {
		t.Fatal("the editor refuses want 7 for SetOption0")
	}
	id, err := r.ImportLegacy(ctx, "env-import", legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := st.CurrentPolicyRules(ctx)
	if cur.ID != id || !strings.Contains(cur.Rules, `"want":"7"`) || cur.Note != "imported from POLICY_RULES" {
		t.Fatalf("%+v", cur)
	}
	if _, err := r.ImportLegacy(ctx, "env-import", legacy, nil); err == nil {
		t.Fatal("a second import must not overwrite managed rules")
	}
	if _, err := r.ImportLegacy(ctx, "x", []byte(`[{"command":"MqttPassword","want":"x"}]`), nil); err == nil {
		t.Fatal("secret queries are refused as always")
	}
}

// legacyVersion seeds a version as 0.18 stored it: no ids.
func legacyVersion(st *memRules, rules string) {
	st.vs = append(st.vs, store.PolicyRuleVersion{ID: int64(len(st.vs) + 1), Rules: rules, Actor: "old"})
}

const legacyTwo = `[{"name":"A","command":"SaveData","want":"0"},{"name":"B","command":"Timezone","want":"99"}]`

// Why: deep links to a rule must survive the upgrade. A version stored before
// 0.19 decodes with r1..rN, and the first save persists exactly those ids.
func TestLegacyVersionGetsPositionIDsAndASaveKeepsThem(t *testing.T) {
	r, st, _ := newRules("", nil)
	ctx := context.Background()
	legacyVersion(st, legacyTwo)
	state, err := r.State(ctx)
	if err != nil || state.Specs[0].ID != "r1" || state.Specs[1].ID != "r2" {
		t.Fatalf("state %+v err %v", state.Specs, err)
	}
	edited := slices.Clone(state.Specs)
	edited[1].Want = "98"
	if _, err := r.Save(ctx, "alice", "", edited, nil); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.CurrentPolicyRules(ctx)
	if !strings.Contains(cur.Rules, `"id":"r1"`) || !strings.Contains(cur.Rules, `"id":"r2"`) {
		t.Fatalf("saved version lost the ids: %s", cur.Rules)
	}
	if old, _ := st.GetPolicyRuleVersion(ctx, 1); strings.Contains(old.Rules, `"id"`) {
		t.Fatal("old version rows must never be rewritten")
	}
}

// Why: merely opening a legacy version and pressing save must not create a
// version; synthesised ids are not an edit (also when the form sends no ids).
func TestSavingALegacyVersionUneditedIsNoChange(t *testing.T) {
	r, st, _ := newRules("", nil)
	ctx := context.Background()
	legacyVersion(st, legacyTwo)
	state, _ := r.State(ctx)
	if _, err := r.Save(ctx, "a", "", state.Specs, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("with ids: %v", err)
	}
	bare := slices.Clone(state.Specs)
	for i := range bare {
		bare[i].ID = ""
	}
	if _, err := r.Save(ctx, "a", "", bare, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("without ids: %v", err)
	}
}

// Why: a link follows the rule, not the slot.
func TestReorderKeepsIDsAndDuplicateGetsAFreshOne(t *testing.T) {
	r, _, _ := newRules("", nil)
	ctx := context.Background()
	v, err := r.Save(ctx, "a", "", []RuleSpec{flash, {Command: "Timezone", Want: "99"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st0, _ := r.State(ctx)
	idA, idB := st0.Specs[0].ID, st0.Specs[1].ID
	if !policy.RuleIDRe.MatchString(idA) || idA == idB || isLegacyID(idA) {
		t.Fatalf("new rules need distinct random ids, got %q %q", idA, idB)
	}
	v, err = r.Reorder(ctx, "a", v, []int{1, 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st1, _ := r.State(ctx)
	if st1.Specs[0].ID != idB || st1.Specs[1].ID != idA {
		t.Fatalf("reorder moved the ids away from their rules: %+v", st1.Specs)
	}
	if _, _, err := r.Duplicate(ctx, "a", v, 0, nil); err != nil {
		t.Fatal(err)
	}
	st2, _ := r.State(ctx)
	if st2.Specs[1].ID == st2.Specs[0].ID || st2.Specs[1].ID == "" || r.IndexOf(st2.Specs, st2.Specs[1].ID) != 1 {
		t.Fatalf("a copy needs its own id: %+v", st2.Specs)
	}
	if r.IndexOf(st2.Specs, "rnone") != -1 || r.IndexOf(st2.Specs, "") != -1 {
		t.Fatal("IndexOf must say -1 for an unknown id")
	}
}

// Why: a half-understood policy is worse than none, so a repeated id or a
// malformed one is refused with the rule's position.
func TestImportRefusesDuplicateAndMalformedIDs(t *testing.T) {
	r, _, _ := newRules("", nil)
	for name, in := range map[string]string{
		"duplicate": `[{"id":"rabc","command":"SaveData","want":"0"},{"id":"rabc","command":"Timezone","want":"99"}]`,
		"no r":      `[{"id":"abc","command":"SaveData","want":"0"}]`,
		"upper":     `[{"id":"rAbc","command":"SaveData","want":"0"}]`,
		"too long":  `[{"id":"rabcdefghijklmnop","command":"SaveData","want":"0"}]`,
		"bare r":    `[{"id":"r","command":"SaveData","want":"0"}]`,
	} {
		_, err := r.ParseImport([]byte(in))
		var re *RuleError
		if !errors.As(err, &re) || re.Index < 0 {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	specs, err := r.ParseImport([]byte(`[{"id":"rkeep1","command":"SaveData","want":"0"},{"command":"Timezone","want":"99"}]`))
	if err != nil || specs[0].ID != "rkeep1" || specs[1].ID != "" {
		t.Fatalf("valid ids are kept, missing ones stay open: %+v %v", specs, err)
	}
}

// Why: an export then import must give the same links back.
func TestExportIncludesIDsAndRoundTripKeepsThem(t *testing.T) {
	r, _, _ := newRules("", nil)
	ctx := context.Background()
	if _, err := r.Save(ctx, "a", "", []RuleSpec{flash, {Command: "Timezone", Want: "99"}}, nil); err != nil {
		t.Fatal(err)
	}
	before, _ := r.State(ctx)
	data, _ := r.Export(ctx)
	if !strings.Contains(string(data), `"id": "`+before.Specs[0].ID+`"`) {
		t.Fatalf("export lacks ids:\n%s", data)
	}
	specs, err := r.ParseImport(data)
	if err != nil || specs[0].ID != before.Specs[0].ID || specs[1].ID != before.Specs[1].ID {
		t.Fatalf("round trip %+v %v", specs, err)
	}
}

// Why: a renamed rule is one change, not a removal plus an addition, when both
// versions know the rule by a real id; position ids name slots, so those fall
// back to matching by name.
func TestDiffMatchesByRealIDBeforeName(t *testing.T) {
	a := []RuleSpec{{ID: "rreal01", Name: "Old", Command: "SaveData", Want: "0"}}
	b := []RuleSpec{{ID: "rreal01", Name: "New", Command: "SaveData", Want: "0"}}
	d := DiffSpecs(a, b)
	if len(d) != 1 || d[0].Kind != "changed" || !strings.Contains(d[0].Detail, `name "Old" -> "New"`) {
		t.Fatalf("real ids: %+v", d)
	}
	a[0].ID, b[0].ID = "r1", "r1"
	d = DiffSpecs(a, b)
	if len(d) != 2 {
		t.Fatalf("position ids must not match a renamed rule: %+v", d)
	}
}

// Why: POLICY_RULES files get the same ids as the stored legacy versions, so a
// link means the same rule in env mode and after the import.
func TestImportLegacyPersistsPositionIDs(t *testing.T) {
	r, st, _ := newRules("", nil)
	if _, err := r.ImportLegacy(context.Background(), "env", []byte(legacyTwo), nil); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.CurrentPolicyRules(context.Background())
	if !strings.Contains(cur.Rules, `"id":"r1"`) || !strings.Contains(cur.Rules, `"id":"r2"`) {
		t.Fatalf("%s", cur.Rules)
	}
}

// Why: a new rule (no id) next to an explicit "r1" must not trip the duplicate
// check with the position id ParseRules would give it; validate fills ids first.
func TestSaveNewRuleBesideExplicitR1Succeeds(t *testing.T) {
	r, _, _ := newRules("", nil)
	specs := []RuleSpec{{Command: "SaveData", Want: "0"}, {ID: "r1", Command: "Timezone", Want: "99"}}
	if _, err := r.Save(context.Background(), "a", "", specs, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := r.State(context.Background())
	if st.Specs[1].ID != "r1" || st.Specs[0].ID == "" || st.Specs[0].ID == "r1" {
		t.Fatalf("%+v", st.Specs)
	}
}

// Why: a POLICY_RULES file mixing an id-less rule with an explicit "r1" must
// import; the id-less rule at position 0 may not take the id r1 (decodeSpecs
// falls back to a random id when the position id is taken).
func TestImportLegacyWithIDClashingPositionID(t *testing.T) {
	r, st, _ := newRules("", nil)
	raw := []byte(`[{"command":"SaveData","want":"0"},{"id":"r1","command":"Timezone","want":"99"}]`)
	if _, err := r.ImportLegacy(context.Background(), "env", raw, nil); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.CurrentPolicyRules(context.Background())
	if strings.Count(cur.Rules, `"id":"r1"`) != 1 {
		t.Fatalf("exactly one rule may hold r1: %s", cur.Rules)
	}
}
