package web

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

func storedSpecs(t *testing.T, mem *rulesMem) []sysconfig.RuleSpec {
	t.Helper()
	mem.mu.Lock()
	defer mem.mu.Unlock()
	var s []sysconfig.RuleSpec
	if err := json.Unmarshal([]byte(mem.vs[len(mem.vs)-1].Rules), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ruleNames(specs []sysconfig.RuleSpec) string {
	var out []string
	for _, s := range specs {
		n := s.Name
		if s.Disabled {
			n += "(off)"
		}
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// threeRules stores A, B, C (versions 1-3) and returns the current version.
func threeRules(t *testing.T, e *env) string {
	t.Helper()
	for i, n := range []string{"A", "B", "C"} {
		saveRule(t, e, url.Values{"name": {n}, "version": {string(rune('0' + i))}})
	}
	return "3"
}

// Why: the order is the precedence (N3); drag and drop sends the full order,
// the buttons without JS move one rule by one place. Both must store exactly
// the order asked for.
func TestPolicyRulesReorder(t *testing.T) {
	e, mem, live := rulesEnv(t, "")
	v := threeRules(t, e)
	if r, body := e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {v}, "order": {"2,0,1"}}); r.StatusCode != 303 {
		t.Fatalf("order: %d\n%s", r.StatusCode, body)
	}
	if got := ruleNames(storedSpecs(t, mem)); got != "C,A,B" {
		t.Fatalf("order %s", got)
	}
	if len(*live) != 3 || (*live)[0].Name != "C" {
		t.Fatalf("the live check must use the new order: %+v", *live)
	}
	// the move names the rule by id, never by position: C,A,B -> the third rule (B) goes up
	if r, _ := e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {"4"}, "id": {ruleIDs(t, mem)[2]}, "move": {"up"}}); r.StatusCode != 303 {
		t.Fatalf("move: %d", r.StatusCode)
	}
	if got := ruleNames(storedSpecs(t, mem)); got != "C,B,A" {
		t.Fatalf("after move up %s", got)
	}
	first := ruleIDs(t, mem)[0]
	for name, f := range map[string]url.Values{
		"both":        {"version": {"5"}, "order": {"0,1,2"}, "id": {first}, "move": {"up"}},
		"neither":     {"version": {"5"}},
		"garbage":     {"version": {"5"}, "order": {"0,x,2"}},
		"bad move":    {"version": {"5"}, "id": {first}, "move": {"left"}},
		"move, no id": {"version": {"5"}, "move": {"up"}},
		"old idx":     {"version": {"5"}, "idx": {"0"}, "move": {"up"}},
	} {
		f.Set("csrf", "tok")
		if r, _ := e.do(t, "POST", "/policy/rules/order", "admin", f); r.StatusCode != 400 {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	if r, _ := e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {"5"}, "order": {"0,0,1"}}); r.StatusCode != 422 {
		t.Errorf("not a permutation: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {"5"}, "id": {"r9z9z9"}, "move": {"up"}}); r.StatusCode != 404 {
		t.Errorf("move of an unknown rule: %d", r.StatusCode)
	}
}

// Why: a list operation refers to the version the admin saw; an operation on an older
// version must change nothing and say so (409, like a stale save).
func TestPolicyRulesOpsRefuseStaleVersion(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	threeRules(t, e)
	ids := ruleIDs(t, mem)
	for _, c := range []struct {
		path string
		f    url.Values
	}{
		{"/policy/rules/order", url.Values{"order": {"2,1,0"}}},
		{"/policy/rules/order", url.Values{"id": {ids[1]}, "move": {"down"}}},
		{"/policy/rules/duplicate", url.Values{"id": {ids[0]}}},
		{"/policy/rules/disable", url.Values{"id": {ids[0]}}},
		{"/policy/rules/enable", url.Values{"id": {ids[0]}}},
	} {
		c.f.Set("csrf", "tok")
		c.f.Set("version", "2")
		if r, _ := e.do(t, "POST", c.path, "admin", c.f); r.StatusCode != 409 {
			t.Errorf("%s %v: %d", c.path, c.f, r.StatusCode)
		}
	}
	if len(mem.vs) != 3 {
		t.Fatalf("a stale operation stored a version: %d", len(mem.vs))
	}
}

// Why: Duplicate adds a disabled copy right below, Enable/Disable flips only
// that rule; and saving a disabled rule from a form without the switch must
// not quietly turn it back on.
func TestPolicyRulesDuplicateAndEnable(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	threeRules(t, e)
	post := func(path string, f url.Values, want int) {
		t.Helper()
		f.Set("csrf", "tok")
		if r, body := e.do(t, "POST", path, "admin", f); r.StatusCode != want {
			t.Fatalf("%s: %d\n%s", path, r.StatusCode, body)
		}
	}
	ids := ruleIDs(t, mem)
	// Duplicate adds the disabled copy below and opens the copy's own page.
	r, _ := e.do(t, "POST", "/policy/rules/duplicate", "admin", url.Values{"csrf": {"tok"}, "version": {"3"}, "id": {ids[0]}})
	if got := ruleNames(storedSpecs(t, mem)); got != "A,A (copy)(off),B,C" {
		t.Fatalf("duplicate %s", got)
	}
	ids = ruleIDs(t, mem)
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.HasSuffix(loc, "/policy/rules/"+ids[1]+"?ok=duplicated") || ids[1] == ids[0] {
		t.Fatalf("duplicate must land on the copy's page (a new id): %d %s", r.StatusCode, loc)
	}
	// Disable / Enable name the rule by id; back=rule returns to its page, back=list (or nothing) to the table.
	r, _ = e.do(t, "POST", "/policy/rules/disable", "admin", url.Values{"csrf": {"tok"}, "version": {"4"}, "id": {ids[2]}, "back": {"list"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.HasSuffix(loc, "/policy?ok=disabled") {
		t.Fatalf("disable, back=list: %d %s", r.StatusCode, loc)
	}
	r, _ = e.do(t, "POST", "/policy/rules/enable", "admin", url.Values{"csrf": {"tok"}, "version": {"5"}, "id": {ids[1]}, "back": {"rule"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.HasSuffix(loc, "/policy/rules/"+ids[1]+"?ok=enabled") {
		t.Fatalf("enable, back=rule: %d %s", r.StatusCode, loc)
	}
	if got := ruleNames(storedSpecs(t, mem)); got != "A,A (copy),B(off),C" {
		t.Fatalf("enable/disable %s", got)
	}
	// a form without the state switch keeps B disabled
	saveRule(t, e, url.Values{"id": {ids[2]}, "version": {"6"}, "name": {"B2"}})
	// the rule page's form sends it
	saveRule(t, e, url.Values{"id": {ids[3]}, "version": {"7"}, "name": {"C"}, "state": {"disabled"}})
	if got := ruleNames(storedSpecs(t, mem)); got != "A,A (copy),B2(off),C(off)" {
		t.Fatalf("editor state %s", got)
	}
	post("/policy/rules/duplicate", url.Values{"version": {"8"}, "id": {"r9z9z9"}}, 404)
	post("/policy/rules/enable", url.Values{"version": {"8"}, "id": {"x"}}, 400)
	post("/policy/rules/enable", url.Values{"version": {"8"}, "id": {ids[0]}, "back": {"elsewhere"}}, 400)
	post("/policy/rules/duplicate", url.Values{"version": {"8"}, "idx": {"0"}}, 400) // the position field is gone
	post("/policy/rules/save", url.Values{"version": {"8"}, "id": {"new"}, "command": {"SaveData"}, "want": {"0"}, "state": {"maybe"}}, 400)
}

// Why: templates are the empty state's way to start; a template adds an
// enabled Advisory rule (the prototype's choice), and the broker template
// expects the broker this server is configured with, never a made-up one.
func TestPolicyRulesTemplates(t *testing.T) {
	tpls := policyTemplates("")
	for _, tp := range tpls {
		if tp.Spec.Command == "MqttHost" {
			t.Fatal("without a configured broker there is no broker template")
		}
	}
	tpls = policyTemplates("mqtt.lab.home")
	if i := slices.IndexFunc(tpls, func(tp ruleTemplate) bool { return tp.ID == "broker" }); i < 0 || tpls[i].Spec.Command != "MqttHost" || tpls[i].Spec.Want != "mqtt.lab.home" {
		t.Fatalf("templates %+v", tpls)
	}
	// every template must pass the same validation as a hand-made rule
	v := sysconfig.NewRules(sysconfig.RulesOptions{Queryable: settings.Queryable, ValidateWant: func(c, w string) error {
		if f, ok := settings.Lookup(c); ok && f.Validate != nil {
			return f.Validate(w)
		}
		return nil
	}})
	for _, tp := range tpls {
		spec := tp.Spec
		if spec.Want == "" {
			spec.Want = templateWant[tp.ID] // blank: the admin types it on the draft page
		}
		if _, err := v.Validate([]sysconfig.RuleSpec{spec}); err != nil {
			t.Errorf("template %s is invalid: %v", tp.ID, err)
		}
	}
	// Templates open an unsaved draft (GET /policy/rules/new?template=ID): the old route that saved
	// one at once is gone.
	e, mem, _ := rulesEnv(t, "")
	if r, _ := e.do(t, "POST", "/policy/rules/template", "admin", url.Values{"csrf": {"tok"}, "version": {"0"}, "template": {"telemetry"}}); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Fatalf("POST /policy/rules/template: %d", r.StatusCode)
	}
	if len(mem.vs) != 0 {
		t.Fatal("nothing may be saved by a template")
	}
}

// Why: operators may look at rules but not change them; the new list
// operations are writes like save and delete.
func TestPolicyRulesOpsAreAdminOnly(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	threeRules(t, e)
	for _, p := range []string{"/policy/rules/order", "/policy/rules/duplicate", "/policy/rules/enable", "/policy/rules/disable"} {
		f := url.Values{"csrf": {"tok"}, "version": {"3"}, "id": {ruleIDs(t, mem)[0]}, "order": {"2,1,0"}}
		if r, _ := e.do(t, "POST", p, "operator", f); r.StatusCode != 403 {
			t.Errorf("POST %s as operator: %d", p, r.StatusCode)
		}
	}
	if len(mem.vs) != 3 {
		t.Fatalf("operator stored a version: %d", len(mem.vs))
	}
}

func TestMoveOrder(t *testing.T) {
	for _, c := range []struct {
		n, idx int
		up     bool
		want   string
		ok     bool
	}{
		{3, 1, true, "[1 0 2]", true}, {3, 1, false, "[0 2 1]", true},
		{3, 0, true, "[0 1 2]", true}, {3, 2, false, "[0 1 2]", true},
		{3, 3, true, "[]", false},
	} {
		got, ok := moveOrder(c.n, c.idx, c.up)
		if ok != c.ok || (ok && fmtInts(got) != c.want) {
			t.Errorf("moveOrder(%d,%d,%v) = %v %v", c.n, c.idx, c.up, got, ok)
		}
	}
}

func fmtInts(v []int) string {
	b, _ := json.Marshal(v)
	return strings.ReplaceAll(strings.ReplaceAll(string(b), ",", " "), "\"", "")
}

func cardNames(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<td class="p1" data-label="Rule"><a href="[^"]*">([^<]*)</a>`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// Why (N3): the order of the cards is the precedence, so the page shows the rules in stored
// order; a disabled rule says so (it flags nothing), and the severity uses the words people
// decide with (Required / Advisory), never the stored warn / info.
func TestPolicyPageShowsRulesInOrderWithStateAndSeverity(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	threeRules(t, e)
	e.do(t, "POST", "/policy/rules/order", "admin", url.Values{"csrf": {"tok"}, "version": {"3"}, "order": {"2,0,1"}})
	e.do(t, "POST", "/policy/rules/disable", "admin", url.Values{"csrf": {"tok"}, "version": {"4"}, "id": {ruleIDs(t, mem)[1]}})
	saveRule(t, e, url.Values{"id": {ruleIDs(t, mem)[2]}, "version": {"5"}, "name": {"B"}, "severity": {"info"}})
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	if got := strings.Join(cardNames(body), ","); got != "C,A,B" {
		t.Fatalf("cards %s, want the stored order C,A,B", got)
	}
	cards := regexp.MustCompile(`(?s)<tr class[^>]*>.*?</tr>`).FindAllString(body, -1)
	if len(cards) != 3 || !strings.Contains(cards[1], "is-dim") || !strings.Contains(cards[1], ">Disabled<") || strings.Contains(cards[0], ">Disabled<") {
		t.Fatalf("only A is disabled:\n%s", strings.Join(cards, "\n---\n"))
	}
	if !strings.Contains(cards[1], "not checked while disabled") || strings.Contains(cards[1], "health-bar") {
		t.Error("a disabled rule has no bar: it is not checked")
	}
	if !strings.Contains(cards[0], ">Required<") || !strings.Contains(cards[2], ">Advisory<") || strings.Contains(body, ">warn<") || strings.Contains(body, ">info<") {
		t.Error("severity labels must be Required / Advisory")
	}
}

// Why (N3): precedence is never silent. When two enabled rules set the same setting on the
// same devices the page says which one wins and on how many devices; two rules that expect
// the same value are called redundant instead.
func TestPolicyPageConflictBanner(t *testing.T) {
	e, _, _ := rulesEnv(t, "", func(o *sysconfig.RulesOptions) {
		o.Preview = func(_ context.Context, rules []policy.Rule) (sysconfig.PreviewResult, error) {
			res := sysconfig.PreviewResult{Devices: 5, Rules: make([]sysconfig.RulePreview, len(rules))}
			if len(rules) == 3 {
				res.Conflicts = []policy.Conflict{{Higher: 0, Lower: 2, Command: "SaveData", Devices: 3}, {Higher: 1, Lower: 2, Command: "SaveData", Devices: 1, SameWant: true}}
			}
			return res, nil
		}
	})
	threeRules(t, e)
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	for _, want := range []string{
		"“A” and “C” both set SaveData. “A” is higher in the list, so it wins for 3 devices they share. Drag to change the order.",
		"“B” and “C” both expect the same SaveData. “C” is redundant on 1 device they share.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("conflicts banner lacks %q", want)
		}
	}
}

// Why: the empty state is the way to start: an admin gets New rule and the first templates, each a
// link that opens an unsaved draft (nothing is saved by looking); an operator gets no write control.
func TestPolicyEmptyStateOffersTemplatesAsDraftLinks(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	_, b := e.do(t, "GET", "/policy", "admin", nil)
	tpls := policyTemplates("")
	if n := strings.Count(b, `href="/tf/policy/rules/new?template=`); n != 3 || !strings.Contains(b, `href="/tf/policy/rules/new?template=`+tpls[0].ID+`"`) {
		t.Errorf("want the first 3 templates as links, got %d:\n%s", n, b)
	}
	if !strings.Contains(b, `href="/tf/policy/rules/new">New rule<`) || !strings.Contains(b, "No policy rules yet") || strings.Contains(b, `class="dev-tiles"`) {
		t.Error("empty state: New rule, the heading, and no tiles without rules")
	}
	_, b = e.do(t, "GET", "/policy", "operator", nil)
	if strings.Contains(b, "template=") || strings.Contains(b, "New rule") || !strings.Contains(b, "An admin can add them.") {
		t.Errorf("an operator cannot start a rule:\n%s", b)
	}
	if len(mem.vs) != 0 {
		t.Fatal("looking at the page saved something")
	}
}

// Why (N3): the rules have no page of their own any more; the old address answers 404 (no
// redirect, CLAUDE.md: no compatibility shims), and the old html/template pages are gone, so
// nothing can render them by accident.
func TestPolicyRulesPageIsGone(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	for _, p := range []string{"/policy/rules", "/policy/rules/diff?from=1&to=1", "/policy/rules/export"} {
		if r, _ := e.do(t, "GET", p, "admin", nil); r.StatusCode != 404 && r.StatusCode != 405 {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	if _, err := fs.Stat(Assets, "templates"); err == nil {
		t.Error("html/template pages are still embedded")
	}
}

// Why: a list change is only meaningful for the version the admin saw. Every form of the page
// that changes the list carries that version, so a change made on a stale page is refused (409)
// instead of hitting another rule.
func TestPolicyPageFormsCarryTheVersion(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	threeRules(t, e)
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	forms := regexp.MustCompile(`(?s)<form method="post" action="/tf/policy/rules/(order|duplicate|enable|disable|delete|save|template)"[^>]*>.*?</form>`).FindAllStringSubmatch(body, -1)
	seen := map[string]bool{}
	for _, f := range forms {
		seen[f[1]] = true
		if !strings.Contains(f[0], `name="version" value="3"`) {
			t.Errorf("%s form without the version:\n%s", f[1], f[0])
		}
	}
	for _, op := range []string{"order", "duplicate", "disable", "delete"} {
		if !seen[op] {
			t.Errorf("no %s form on the page", op)
		}
	}
	if seen["save"] || seen["template"] || strings.Contains(body, `name="idx"`) {
		t.Error("the overview has no editor, no template form and no position field")
	}
	// the moves without JavaScript: the first rule cannot move up, the last not down
	if !strings.Contains(body, `name="move" value="up"`) || !strings.Contains(body, `name="move" value="down"`) {
		t.Error("the rule menus offer Move up / Move down")
	}
}

// Why: the preview fragment repeats what an admin typed (an error message can quote it); it
// must be escaped like the page, never markup.
func TestPolicyPreviewFragmentEscapes(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	_, body := e.do(t, "POST", "/policy/rules/preview", "admin", ruleForm(url.Values{"command": {"Hostname"}, "want": {`<img src=x onerror=alert(1)>`}}))
	if strings.Contains(body, "<img") {
		t.Fatalf("unescaped value in the fragment:\n%s", body)
	}
	_, body = e.do(t, "GET", "/policy/rules/value-control?command=Hostname&want=%3Cb%3E", "admin", nil)
	if strings.Contains(body, "<b>") {
		t.Fatalf("value control:\n%s", body)
	}
}

// Why: a ticked Enabled box sends "disabled" (the hidden default) and then "enabled"; the last
// one wins, and an unticked box sends only "disabled". Without the switch a saved rule keeps
// its state (tested in TestPolicyRulesDuplicateAndEnable).
func TestPolicyEditorEnabledSwitch(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"state": {"disabled", "enabled"}})
	if s := storedSpecs(t, mem); s[0].Disabled {
		t.Fatal("ticked: enabled")
	}
	saveRule(t, e, url.Values{"id": {ruleIDs(t, mem)[0]}, "version": {"1"}, "state": {"disabled"}, "want": {"1"}})
	if s := storedSpecs(t, mem); !s[0].Disabled {
		t.Fatal("unticked: disabled")
	}
	if r, _ := e.do(t, "POST", "/policy/rules/save", "admin", ruleForm(url.Values{"version": {"2"}, "state": {"disabled", "enabled", "enabled"}})); r.StatusCode != 400 {
		t.Errorf("three states: %d", r.StatusCode)
	}
}
