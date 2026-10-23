package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
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

// Why: the live preview is where a person sees which devices a rule flags and what they have
// instead; a value that could not be read says so instead of looking like a deviation.
func TestPolicyPreviewListsTheDevicesTheOpenRuleFlags(t *testing.T) {
	rule := policy.Rule{Command: "SaveData", Want: "0"}
	fp := &fakePolicy{rep: PolicyReport{
		Rules: []policy.Rule{rule}, Devices: []model.Device{{ID: 1, Name: "kitchen"}, {ID: 2, Name: "attic"}, {ID: 3, Name: "shed"}},
		Findings: []policy.Finding{{DeviceID: 2, Rule: rule, Unknown: true}, {DeviceID: 1, Rule: rule, Have: "1"}},
		Provenance: map[model.DeviceID]map[string]ValueSource{
			1: {"savedata": {Kind: "live", At: time.Now().Add(-5 * time.Minute)}},
		},
	}}
	e := setupWith(t, func(d *Deps) { d.Policy = fp })
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	pv := section(t, body, "pv-h")
	k, a := strings.Index(pv, "kitchen"), strings.Index(pv, "attic")
	if k < 0 || a < 0 || k > a {
		t.Fatalf("the deviation comes first, then the unknown value:\n%s", pv)
	}
	if !strings.Contains(pv, "1 → <b>0</b>") || !strings.Contains(pv, "unknown → <b>0</b>") {
		t.Errorf("the preview must show what the device has and what the rule expects:\n%s", pv)
	}
	if strings.Contains(pv, "shed") {
		t.Error("a compliant device is not flagged")
	}
	if !strings.Contains(body, "1 rule · 1 device deviates") {
		t.Error("the subtitle counts deviating devices only (an unknown value is not a deviation)")
	}
}

// Why: with nothing to judge the page must not look like an alarm or offer a check that
// cannot find anything: no live-check line, no Make compliant, but a way to start (templates
// for admins who can change the rules).
func TestPolicyPageEmptyStateOffersNothingToCheck(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{}; d.PolicyFix = &fakeFix{} })
	_, body := e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, "No rules yet") {
		t.Error("no empty state")
	}
	for _, bad := range []string{"pol-line", "Run now", "Make compliant", "t-warn"} {
		if strings.Contains(body, bad) {
			t.Errorf("the empty page offers %q", bad)
		}
	}
	e, _, _ = rulesEnv(t, "")
	_, body = e.do(t, "GET", "/policy", "admin", nil)
	if !strings.Contains(body, "Use template") || !strings.Contains(body, "/policy/rules/template") {
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
	_, body := e.do(t, "GET", "/policy?new=1", "admin", nil)
	if !strings.Contains(body, `<span class="ufield-label">Note</span> <textarea name="note"`) {
		t.Errorf("no visible Note label:\n%s", body)
	}
}
