package web

import (
	"net/url"
	"strings"
	"testing"
)

// Why: Import / export is one of the policy views, not a fourth page with a way back. The policy tab row (the
// way between the views) must be there with the tab current, there must be no second tab strip and no "Back to
// policy" button, and the page must not repeat the Import / Export buttons the tab replaces. Every role sees the
// tab; operators only export.
func TestPolicyIOIsATabOfThePolicyShell(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	for _, role := range []string{"admin", "operator"} {
		_, body := e.do(t, "GET", "/policy/io", role, nil)
		if !strings.Contains(body, `aria-label="Policy views"`) || !strings.Contains(body, `aria-current="page">Import / export`) {
			t.Errorf("%s: the policy tab row with Import / export current is missing:\n%s", role, body)
		}
		for _, tab := range []string{">Rules", ">Deviating devices", ">History"} {
			if !strings.Contains(body, tab) {
				t.Errorf("%s: the tab row lacks %s", role, tab)
			}
		}
		for _, bad := range []string{"Back to policy", `aria-label="Import or export"`, `aria-label="Export"`, "Import…", `>Export</a>`} {
			if strings.Contains(body, bad) {
				t.Errorf("%s: the page still has %q", role, bad)
			}
		}
		if strings.Contains(body, "Run now") || strings.Contains(body, "pol-sum") {
			t.Errorf("%s: the tab needs no live check and must not run one", role)
		}
	}
	// The tab row of the other policy pages leads here.
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	if !strings.Contains(body, `href="/tf/policy/io"`) || !strings.Contains(body, "Import / export") {
		t.Errorf("the Rules tab row has no Import / export tab:\n%s", body)
	}
	// ?tab=import stays a valid bookmark; operators only export.
	_, body = e.do(t, "GET", "/policy/io?tab=import", "admin", nil)
	if !strings.Contains(body, "/policy/io/import") || !strings.Contains(body, "Download .json") {
		t.Errorf("admin ?tab=import shows both cards:\n%s", body)
	}
	_, body = e.do(t, "GET", "/policy/io", "operator", nil)
	if strings.Contains(body, "/policy/io/import") || !strings.Contains(body, "Download .json") {
		t.Errorf("operator export page:\n%s", body)
	}
}

// Why: the file is read by people, so the page shows the same JSON the download holds, in the words of the UI,
// and a legend line under the textarea says what the two keys mean, because a person pasting rules sees nothing
// else about them.
func TestPolicyIOExportAndLegendUseTheWordsOfTheUI(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, url.Values{"severity": {"info"}})
	r, body := e.do(t, "GET", "/policy/io", "admin", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "&#34;level&#34;: &#34;advisory&#34;") || !strings.Contains(body, "&#34;expected&#34;: &#34;0&#34;") {
		t.Fatalf("the page does not show the file in the words of the UI:\n%s", body)
	}
	if strings.Contains(body, "&#34;severity&#34;") || strings.Contains(body, "&#34;want&#34;") {
		t.Errorf("the page still shows the old keys:\n%s", body)
	}
	_, out := e.do(t, "GET", "/policy/io/export", "operator", nil)
	if !strings.Contains(out, `"level": "advisory"`) || strings.Contains(out, `"severity"`) {
		t.Errorf("download:\n%s", out)
	}
	if !strings.Contains(body, "pio-legend") || !strings.Contains(body, "advisory") || !strings.Contains(body, "expected") {
		t.Errorf("no legend for level and expected:\n%s", body)
	}
}

// Why: an import REPLACES all rules. The preview must name every rule with its fate (New, Changed, Unchanged and
// Removed, the removed ones by name, because a rule that the file leaves out is deleted and that is what the
// person must see before confirming), the replace note is a warning above the button and not small print beside
// it, and the button says what it does.
func TestPolicyIOPreviewListsEveryFateAndWarnsAboveTheButton(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	seed := `[{"name":"Keep","command":"SaveData","expected":"0","level":"required","families":["esp32*"],"tags":["kitchen","attic","garage"],"except":["lamp"],"note":"keep flash alive"},
	{"name":"Change","command":"SetOption0","expected":"1"},{"name":"Gone","command":"SetOption1","expected":"1"}]`
	if r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {seed}, "confirm": {"1"}}); r.StatusCode != 303 {
		t.Fatalf("seed: %d\n%s", r.StatusCode, body)
	}
	n := len(mem.vs)
	file := `[{"name":"Keep","command":"SaveData","expected":"0","level":"required","families":["esp32*"],"tags":["kitchen","attic","garage"],"except":["lamp"],"note":"keep flash alive"},
	{"name":"Change","command":"SetOption0","expected":"0","level":"advisory"},
	{"name":"Fresh","command":"SetOption2","expected":"1"}]`
	r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {file}})
	if r.StatusCode != 200 || len(mem.vs) != n {
		t.Fatalf("check: %d, versions %d -> %d", r.StatusCode, n, len(mem.vs))
	}
	pv := section(t, body, "pio-h")
	for _, want := range []string{">New<", ">Changed<", ">Unchanged<", ">Removed<", "Fresh", "Change", "Keep", "Gone"} {
		if !strings.Contains(pv, want) {
			t.Errorf("the preview lacks %s:\n%s", want, pv)
		}
	}
	if strings.Contains(pv, ">Update<") || strings.Contains(pv, ">Remove<") {
		t.Errorf("old fate words: %s", pv)
	}
	gone := strings.Index(pv, "Gone")
	if gone < 0 || !strings.Contains(pv[strings.LastIndex(pv[:gone], `class="pio-row"`):gone], ">Removed<") {
		t.Errorf("the removed rule is not listed by name under Removed:\n%s", pv)
	}
	banner, button := strings.Index(pv, `class="banner t-warn"`), strings.Index(pv, "Replace rules with these 3")
	if banner < 0 || button < 0 || banner > button {
		t.Fatalf("the warn banner (at %d) must come before the button (at %d):\n%s", banner, button, pv)
	}
	if !strings.Contains(pv[banner:button], "replaces all current rules") {
		t.Errorf("the banner does not say the file replaces the rules:\n%s", pv[banner:button])
	}
	if strings.Contains(pv, "Import 3 rules") || strings.Contains(pv, "btn btn-primary") && strings.Contains(pv[:banner], "btn btn-primary") {
		t.Errorf("old button or a primary action before the warning:\n%s", pv)
	}
	// The legend sits under the textarea, once.
	if strings.Count(body, `class="pio-legend"`) != 1 {
		t.Errorf("legend count %d", strings.Count(body, `class="pio-legend"`))
	}
}

// Why: when the file holds exactly the saved rules there is nothing to confirm; a warning that the file "replaces
// all rules" next to a button that then answers "nothing changed" would be noise, so the preview says so instead.
func TestPolicyIOPreviewOfAnUnchangedFileOffersNoReplace(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	saveRule(t, e, nil)
	_, out := e.do(t, "GET", "/policy/io/export", "admin", nil)
	_, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {out}})
	pv := section(t, body, "pio-h")
	if !strings.Contains(pv, ">Unchanged<") || !strings.Contains(pv, "No difference") || strings.Contains(pv, `name="confirm"`) || strings.Contains(pv, "t-warn") {
		t.Errorf("an unchanged file:\n%s", pv)
	}
}

// Why: the order of the rules is their precedence, and Save stores a reordered file as a new version, but every
// row of the preview reads Unchanged. The page must offer the replace anyway (banner and button) and must not say
// "No difference": it decides by the test Save uses, so a reorder is never blocked by a false statement.
func TestPolicyIOReorderedFileCanBeReplaced(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	ab := `[{"name":"A","command":"SaveData","expected":"0"},{"name":"B","command":"SetOption0","expected":"1"}]`
	ba := `[{"name":"B","command":"SetOption0","expected":"1"},{"name":"A","command":"SaveData","expected":"0"}]`
	if r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {ab}, "confirm": {"1"}}); r.StatusCode != 303 {
		t.Fatalf("seed: %d\n%s", r.StatusCode, body)
	}
	n := len(mem.vs)
	_, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {ba}})
	pv := section(t, body, "pio-h")
	if strings.Contains(pv, "No difference") || !strings.Contains(pv, `class="banner t-warn"`) || !strings.Contains(pv, "Replace rules with these 2") || !strings.Contains(pv, "their order") {
		t.Fatalf("a reordered file must offer the replace and say why:\n%s", pv)
	}
	if r, _ := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {ba}, "confirm": {"1"}}); r.StatusCode != 303 || len(mem.vs) != n+1 {
		t.Errorf("the reorder was not stored: %d, versions %d -> %d", r.StatusCode, n, len(mem.vs))
	}
}

// Why: files written before 0.46.1 (severity, want) keep working through the page, and a file that gives both
// spellings with different values is refused under the textarea, naming the rule, without storing anything.
func TestPolicyIOImportReadsOldFilesAndRefusesConflicts(t *testing.T) {
	e, mem, _ := rulesEnv(t, "")
	old := `[{"command":"SetOption0","want":"1","severity":"info"}]`
	if r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {old}, "confirm": {"1"}}); r.StatusCode != 303 || len(mem.vs) != 1 || !strings.Contains(mem.vs[0].Rules, `"severity":"info"`) {
		t.Fatalf("old file: %d\n%s", r.StatusCode, body)
	}
	r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {`[{"command":"SetOption0","expected":"1","level":"advisory","severity":"warn"}]`}})
	if r.StatusCode != 422 || len(mem.vs) != 1 || !strings.Contains(body, `disagree`) || !strings.Contains(body, "rule 1") {
		t.Errorf("conflict: %d\n%s", r.StatusCode, body)
	}
}
