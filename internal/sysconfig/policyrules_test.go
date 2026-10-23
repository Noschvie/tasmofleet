package sysconfig

import (
	"context"
	"errors"
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
