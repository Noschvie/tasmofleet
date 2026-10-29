package web

import (
	"net/url"
	"strings"
	"testing"
)

// section returns the markup of the card with the given heading id, up to its closing tag.
func section(t *testing.T, body, headingID string) string {
	t.Helper()
	i := strings.Index(body, `aria-labelledby="`+headingID+`"`)
	if i < 0 {
		t.Fatalf("no section %q in:\n%s", headingID, body)
	}
	rest := body[i:]
	if j := strings.Index(rest, "</section>"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// Why: with nothing to judge the page must not look like an alarm or offer a check that
// cannot find anything: no live-check line, no Make compliant, but a way to start (templates
// for admins who can change the rules).
func TestPolicyPageEmptyStateOffersNothingToCheck(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{}; d.PolicyFix = &fakeFix{} })
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, "No policy rules yet") {
		t.Error("no empty state")
	}
	for _, bad := range []string{"pol-line", "Run now", "Make compliant", "t-warn"} {
		if strings.Contains(body, bad) {
			t.Errorf("the empty page offers %q", bad)
		}
	}
	e, _, _ = rulesEnv(t, "")
	_, body = e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, "Use template") || !strings.Contains(body, "/policy/rules/new?template=") {
		t.Error("an admin starts from a template")
	}
	_, body = e.do(t, "GET", "/policy", "operator", nil)
	if strings.Contains(body, "Use template") {
		t.Error("a viewer cannot add a rule from a template")
	}
}

// Why: the import result belongs next to the pasted file: the preview lists what would change
// and the refusal stands under the text, once.
func TestPolicyImportResultSitsNextToTheFile(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	good := `[{"command":"SetOption0","want":"1"}]`
	_, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {good}})
	pv := section(t, body, "pio-h")
	if !strings.Contains(pv, "SetOption0") || !strings.Contains(pv, "Import 1 rule") || !strings.Contains(pv, "1 new") {
		t.Errorf("the check result is not in the preview:\n%s", body)
	}
	r, body := e.do(t, "POST", "/policy/io/import", "admin", url.Values{"csrf": {"tok"}, "json": {`[{"command":"MqttPassword","want":"x"}]`}})
	if r.StatusCode != 422 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if strings.Count(body, "Nothing was imported.") != 1 || !strings.Contains(body, `id="pio-err" role="alert">Nothing was imported.`) {
		t.Errorf("the refusal must appear once, under the pasted text:\n%s", body)
	}
	if !strings.Contains(body, "MqttPassword") {
		t.Error("the pasted text is kept")
	}
	// Viewers export; the Import tab is not theirs.
	_, body = e.do(t, "GET", "/policy/io?tab=import", "operator", nil)
	if strings.Contains(body, "/policy/io/import") || !strings.Contains(body, "Download .json") {
		t.Errorf("operator import page:\n%s", body)
	}
}

// Why: the policy pages belong to the Devices group; a trail that starts at "Policy" on one
// page and at "Devices" on the next makes people wonder where they are.
func TestPolicySubPagesShareTheDevicesBreadcrumbRoot(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	_, body := e.do(t, "GET", "/policy/io", "admin", nil)
	for _, want := range []string{`<a href="/tf/devices">Devices</a>`, `<a href="/tf/policy">Policy</a>`, `aria-current="page">Import / export</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("trail of the import / export page lacks %s", want)
		}
	}
}

// Why: the Note input has a label of its own at every width (it once hid behind a fold that
// desktop does not show), and its help says what the note is for.
func TestPolicyRuleEditorNoteHasAVisibleLabel(t *testing.T) {
	e, _, _ := rulesEnv(t, "")
	_, body := e.do(t, "GET", "/policy/rules/new?command=SaveData", "admin", nil)
	if !strings.Contains(body, `<span class="ufield-label">Note</span> <textarea name="note"`) {
		t.Errorf("no visible Note label:\n%s", body)
	}
}
