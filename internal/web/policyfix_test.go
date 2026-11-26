package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/ota"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

type fakeFix struct {
	reqs    []ota.PolicyRequest
	skipped []ota.Refusal
	err     error
	actor   string
}

func (f *fakeFix) Submit(_ context.Context, actor string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error) {
	f.reqs, f.actor = reqs, actor
	return 7, f.skipped, f.err
}

// activeOn answers only for the device it names, like the real store.
type activeOn struct {
	fakeJobs
	device model.DeviceID
}

func (a *activeOn) ListJobs(_ context.Context, f store.JobFilter) ([]model.Job, error) {
	if f.DeviceID == a.device {
		return []model.Job{{ID: 5, BatchID: 3, DeviceID: a.device, State: model.JobUpgradeIssued}}, nil
	}
	return nil, nil
}

var (
	ruleOn0  = policy.Rule{ID: "r1", Command: "PowerOnState", Want: "0"}
	ruleSO0  = policy.Rule{ID: "r2", Command: "SetOption0", Want: "0"}
	ruleSave = policy.Rule{ID: "r3", Command: "SaveData", Want: "0"}
)

// fixReport: kitchen deviates live (two settings), attic deviates only in an
// old backup, cellar has an unknown value next to a live deviation, shed is fine.
func fixReport() PolicyReport {
	live := ValueSource{Kind: "live", At: time.Now().Add(-2 * time.Minute)}
	return PolicyReport{
		Rules:   []policy.Rule{ruleOn0, ruleSO0, ruleSave},
		Devices: []model.Device{{ID: 1, Name: "kitchen"}, {ID: 2, Name: "attic"}, {ID: 3, Name: "cellar"}, {ID: 4, Name: "shed"}},
		Findings: []policy.Finding{
			{DeviceID: 1, Rule: ruleOn0, Have: "3"}, {DeviceID: 1, Rule: ruleSO0, Have: "1"},
			{DeviceID: 2, Rule: ruleOn0, Have: "2"},
			{DeviceID: 3, Rule: ruleSave, Unknown: true}, {DeviceID: 3, Rule: ruleOn0, Have: "5"},
		},
		// The one evaluation the overview reads (its banner and tiles); the findings are its
		// Deviate and Unknown entries.
		Outcomes: []policy.Outcome{
			{DeviceID: 1, RuleIndex: 0, RuleID: "r1", Kind: policy.OutcomeDeviate}, {DeviceID: 1, RuleIndex: 1, RuleID: "r2", Kind: policy.OutcomeDeviate},
			{DeviceID: 2, RuleIndex: 0, RuleID: "r1", Kind: policy.OutcomeDeviate},
			{DeviceID: 3, RuleIndex: 2, RuleID: "r3", Kind: policy.OutcomeUnknown}, {DeviceID: 3, RuleIndex: 0, RuleID: "r1", Kind: policy.OutcomeDeviate},
		},
		Provenance: map[model.DeviceID]map[string]ValueSource{
			1: {"poweronstate": live, "setoption0": live},
			2: {"poweronstate": {Kind: "backup", At: time.Now().Add(-48 * time.Hour)}},
			3: {"poweronstate": live},
			4: {"poweronstate": live},
		},
	}
}

func fixEnv(t *testing.T, fix *fakeFix, jobs JobLister) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Policy = &fakePolicy{rep: fixReport()}
		d.PolicyFix = fix
		d.Jobs = jobs
	})
}

func previewFor(t *testing.T, e *env, form url.Values) string {
	t.Helper()
	form.Set("csrf", "tok")
	r, body := e.do(t, "POST", "/policy/fix", "admin", form)
	if r.StatusCode != 200 {
		t.Fatalf("preview: %d\n%s", r.StatusCode, body)
	}
	return body
}

var digestRe = regexp.MustCompile(`name="digest" value="([0-9a-f]+)"`)

// Why: the preview is the admin's only chance to see what will be written to
// the devices. It must show the exact command for a deviation read live, and
// must list but not act on a value that is unknown or only known from an old
// backup. It sends nothing, and there is no confirmation tick: preview plus
// Apply is the whole protocol.
func TestPolicyFixPreviewShowsExactCommandsOnlyForKnownLiveDeviations(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, nil)
	body := previewFor(t, e, url.Values{"all": {"1"}})
	for _, want := range []string{cmdHTML("PowerOnState 0"), cmdHTML("SetOption0 0"), "kitchen", "cellar", "saved to the device once", "Run 3 changes"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Count(body, cmdHTML("PowerOnState 0")) != 2 {
		t.Errorf("PowerOnState 0 only for kitchen and cellar (live), not for attic (backup only)")
	}
	if strings.Contains(body, cmdHTML("SaveData 0")) {
		t.Error("cellar's SaveData value is unknown: no command")
	}
	for _, want := range []string{"the current value is unknown", "only known from a backup, not read live"} {
		if !strings.Contains(body, want) {
			t.Errorf("skipped reason %q not listed", want)
		}
	}
	if strings.Contains(body[strings.Index(body, `class="pol-page"`):], `type="checkbox"`) {
		t.Error("no confirmation checkbox: preview plus Apply is enough")
	}
	if len(fix.reqs) != 0 {
		t.Fatal("the preview must not start anything")
	}
}

// Why: Apply recomputes the plan and compares it with what was previewed, so a
// value that changed in between (or a tampered form) can never apply commands
// nobody looked at; the exact reviewed commands are what is submitted.
func TestPolicyFixApplySubmitsTheReviewedCommandsAndRedirects(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, nil)
	body := previewFor(t, e, url.Values{"only": {"1"}})
	m := digestRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no digest in preview: %s", body)
	}
	r, out := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"1"}})
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/transactions/7") {
		t.Fatalf("apply: %d %s\n%s", r.StatusCode, r.Header.Get("Location"), out)
	}
	if len(fix.reqs) != 1 || fix.reqs[0].DeviceID != 1 || strings.Join(fix.reqs[0].Commands, "|") != "PowerOnState 0|SetOption0 0" || fix.actor != "test:admin" {
		t.Fatalf("submitted %+v by %q", fix.reqs, fix.actor)
	}
	// a stale or forged digest applies nothing
	fix.reqs = nil
	r, out = e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {"deadbeef"}, "device": {"1"}})
	if r.StatusCode != 409 || len(fix.reqs) != 0 || !strings.Contains(out, "changed since you looked") {
		t.Fatalf("stale digest: %d, submitted %+v\n%s", r.StatusCode, fix.reqs, out)
	}
}

// Why: a device that another transaction is working on must not get a second
// writer; the preview says so and the apply leaves it out.
func TestPolicyFixLeavesDevicesWithAnActiveJobOut(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, &activeOn{device: 1})
	body := previewFor(t, e, url.Values{"all": {"1"}})
	if !strings.Contains(body, "Another transaction is running on this device (#3)") || !strings.Contains(body, "Run 1 change<") {
		t.Fatalf("preview %s", body)
	}
	m := digestRe.FindStringSubmatch(body)
	e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"1", "3"}})
	if len(fix.reqs) != 1 || fix.reqs[0].DeviceID != 3 {
		t.Fatalf("submitted %+v: kitchen has a running job", fix.reqs)
	}
}

// Why: devices the locks refused at submit time are skipped with a reason;
// the admin must see which ones, because the transaction page does not list them.
func TestPolicyFixShowsDevicesTheServerSkipped(t *testing.T) {
	fix := &fakeFix{skipped: []ota.Refusal{{DeviceID: 1, Reason: "another operation on this device is running (console)"}}}
	e := fixEnv(t, fix, nil)
	m := digestRe.FindStringSubmatch(previewFor(t, e, url.Values{"only": {"3"}}))
	r, out := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"3"}})
	if r.StatusCode != 200 || !strings.Contains(out, "/transactions/7") || !strings.Contains(out, "kitchen") || !strings.Contains(out, "console") {
		t.Fatalf("%d\n%s", r.StatusCode, out)
	}
}

// Why: changing devices is admin work. Operators neither see the buttons nor
// can they post to the routes; anonymous users get 401 (CSRF is checked too).
func TestPolicyFixIsAdminOnly(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, nil)
	for _, path := range []string{"/policy/fix", "/policy/fix/apply"} {
		if r, _ := e.do(t, "POST", path, "operator", url.Values{"csrf": {"tok"}, "all": {"1"}, "digest": {"x"}}); r.StatusCode != 403 {
			t.Errorf("operator %s: %d", path, r.StatusCode)
		}
		if r, _ := e.do(t, "POST", path, "", url.Values{"csrf": {"tok"}}); r.StatusCode != 401 {
			t.Errorf("anonymous %s: %d", path, r.StatusCode)
		}
	}
	_, op := e.do(t, "GET", "/policy", "operator", nil)
	if strings.Contains(op, "Make compliant") {
		t.Error("operators must not be offered the action")
	}
	// The overview no longer opens Make compliant itself (#1091): its banner has the one action Show, and Make compliant
	// is on the Deviating devices tab, for admins only.
	_, adm := e.do(t, "GET", "/policy", "admin", nil)
	if strings.Contains(adm, "Make compliant") || strings.Contains(adm, "dlg-fix") {
		t.Error("the overview banner has one action; Make compliant lives on the Deviating devices tab")
	}
	_, opDev := e.do(t, "GET", "/policy/deviations", "operator", nil)
	_, admDev := e.do(t, "GET", "/policy/deviations", "admin", nil)
	if strings.Contains(opDev, "Make compliant") || !strings.Contains(admDev, "Make compliant…") {
		t.Errorf("Make compliant on the Deviating devices tab: admin yes (%v), operator no (%v)", strings.Contains(admDev, "Make compliant…"), strings.Contains(opDev, "Make compliant"))
	}
	if len(fix.reqs) != 0 {
		t.Fatal("nothing may be submitted by refused requests")
	}
}

// Why: without the feature wired (no job engine) the routes must not exist
// and the page must not promise them.
func TestPolicyFixHiddenWithoutTheService(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: fixReport()} })
	if r, _ := e.do(t, "POST", "/policy/fix", "admin", url.Values{"csrf": {"tok"}, "all": {"1"}}); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Errorf("status %d", r.StatusCode)
	}
	if _, body := e.do(t, "GET", "/policy", "admin", nil); strings.Contains(body, "Make compliant") {
		t.Error("button without a service")
	}
}

// Why: a selection with nothing to change must not offer an Apply button that
// would start an empty transaction.
func TestPolicyFixNothingToDo(t *testing.T) {
	e := fixEnv(t, &fakeFix{}, nil)
	body := previewFor(t, e, url.Values{"only": {"4"}})
	if strings.Contains(body, ">Run ") || !strings.Contains(body, "Already compliant") {
		t.Fatalf("%s", body)
	}
}

// cmdHTML is a command as the plan shows it.
func cmdHTML(cmd string) string { return `<span class="mono">` + cmd + `</span>` }

// Why (design v2, N3): an Advisory rule is a hint only. Make compliant includes Required
// rules only, so an Advisory deviation read live is never written to a device by it.
func TestPolicyFixLeavesAdvisoryRulesOut(t *testing.T) {
	rep := fixReport()
	adv := policy.Rule{Command: "SetOption0", Want: "0", Severity: policy.SeverityInfo}
	for i, f := range rep.Findings {
		if f.Rule.Command == "SetOption0" {
			rep.Findings[i].Rule, rep.Findings[i].Severity = adv, policy.SeverityInfo
		}
	}
	fix := &fakeFix{}
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: rep}; d.PolicyFix = fix })
	body := previewFor(t, e, url.Values{"all": {"1"}})
	if strings.Contains(body, cmdHTML("SetOption0 0")) || !strings.Contains(body, cmdHTML("PowerOnState 0")) {
		t.Fatalf("the plan must hold the Required command only:\n%s", body)
	}
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	if strings.Contains(page, "SetOption0 0") {
		t.Error("the Make compliant dialog lists an Advisory command")
	}
}

// advisoryFixReport is fixReport with kitchen's SetOption0 rule (r2) turned Advisory: kitchen
// deviates from one Required rule (r1) and one hint (r2), both read live.
func advisoryFixReport() PolicyReport {
	rep := fixReport()
	adv := ruleSO0
	adv.Severity = policy.SeverityInfo
	for i, f := range rep.Findings {
		if f.Rule.ID == "r2" {
			rep.Findings[i].Rule, rep.Findings[i].Severity = adv, policy.SeverityInfo
		}
	}
	return rep
}

func advisoryEnv(t *testing.T, fix *fakeFix) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: advisoryFixReport()}; d.PolicyFix = fix })
}

var kitchenAdvisoryBox = regexp.MustCompile(`<input type="checkbox"[^>]*name="pick" value="1:r2"[^>]*>`)

// Why (PO decision: the difference between Required and Advisory may blur): an Advisory deviation
// can be written, but only because the admin ticked it. The device tile and the banner keep
// their Required-only selection and list the hint unticked; ticking it re-draws the plan with the
// command; the apply form then carries the pick and the digest binds it, so the same reviewed
// plan without the pick is refused as stale.
func TestPolicyFixAdvisoryIsWrittenOnlyWhenPicked(t *testing.T) {
	fix := &fakeFix{}
	e := advisoryEnv(t, fix)
	for _, form := range []url.Values{{"only": {"1"}}, {"all": {"1"}}, {"device": {"1"}}} {
		body := previewFor(t, e, form)
		if strings.Contains(body, cmdHTML("SetOption0 0")) || !strings.Contains(body, cmdHTML("PowerOnState 0")) {
			t.Fatalf("%v: the preselected plan is Required only:\n%s", form, body)
		}
		if box := kitchenAdvisoryBox.FindString(body); box == "" || strings.Contains(box, "checked") {
			t.Fatalf("%v: the Advisory deviation is offered unticked, got %q", form, box)
		}
	}

	picked := previewFor(t, e, url.Values{"only": {"1"}, "pick": {"1:r2"}})
	if !strings.Contains(picked, cmdHTML("SetOption0 0")) || !strings.Contains(picked, cmdHTML("PowerOnState 0")) {
		t.Fatalf("the picked hint joins the plan:\n%s", picked)
	}
	if box := kitchenAdvisoryBox.FindString(picked); !strings.Contains(box, "checked") {
		t.Errorf("a picked hint stays ticked, got %q", box)
	}
	if !strings.Contains(picked, `name="pick" value="1:r2"`) {
		t.Error("the apply form carries the pick")
	}
	m := digestRe.FindStringSubmatch(picked)
	r, out := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"1"}, "pick": {"1:r2"}})
	if r.StatusCode != 303 || len(fix.reqs) != 1 || strings.Join(fix.reqs[0].Commands, "|") != "PowerOnState 0|SetOption0 0" {
		t.Fatalf("apply: %d, submitted %+v\n%s", r.StatusCode, fix.reqs, out)
	}
	// the digest of the reviewed plan does not authorise a plan without (or with another) pick
	fix.reqs = nil
	if r, _ = e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"1"}}); r.StatusCode != 409 || len(fix.reqs) != 0 {
		t.Errorf("without the pick the reviewed digest is stale: %d, %+v", r.StatusCode, fix.reqs)
	}
}

// Why (#1091, DS-007): a banner has at most one action (design-system.md, Inline banners). The overview's deviation
// banner offered Show and Make compliant…, two buttons in one banner; it keeps Show, which opens the Deviating devices
// tab, where the picked rows and Make compliant are. The dialog that only the second button opened is gone with it:
// it would be a plan computed on every load for a button nobody can press.
func TestPolicyOverviewDeviationBannerHasOneAction(t *testing.T) {
	e := fixEnv(t, &fakeFix{}, nil)
	_, page := e.do(t, "GET", "/policy", "admin", nil)
	i := strings.Index(page, "deviate from a required rule")
	if i < 0 {
		t.Fatalf("no deviation banner on the overview:\n%s", page)
	}
	start := strings.LastIndex(page[:i], `<div class="banner `)
	end := start + strings.Index(page[start:], `</a></div>`) + len(`</a></div>`)
	banner := page[start:end]
	if n := strings.Count(banner, "banner-btn"); n != 1 || !strings.Contains(banner, `href="/tf/policy/deviations">Show</a>`) {
		t.Errorf("the banner has %d actions, want exactly Show to the Deviating devices tab:\n%s", n, banner)
	}
	if strings.Contains(page, "dlg-fix") || strings.Contains(page, "<button type=\"submit\" class=\"banner-btn\"") {
		t.Error("the overview carries no Make compliant dialog or button")
	}
}

// Why: the Deviating devices tab posts "pick" only: the plan is exactly the ticked rows, nothing
// else of the same device (kitchen's other Required deviation stays out), and the apply form carries
// the picks so the reviewed commands are what runs.
func TestPolicyFixByPickWritesExactlyTheTickedRows(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, nil)
	body := previewFor(t, e, url.Values{"pick": {"1:r1"}})
	if !strings.Contains(body, cmdHTML("PowerOnState 0")) || strings.Contains(body, cmdHTML("SetOption0 0")) {
		t.Fatalf("only the ticked rule is in the plan:\n%s", body)
	}
	if strings.Contains(body, `name="device"`) || !strings.Contains(body, `name="pick" value="1:r1"`) {
		t.Error("a by-pick plan carries picks, not devices")
	}
	m := digestRe.FindStringSubmatch(body)
	e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "pick": {"1:r1"}})
	if len(fix.reqs) != 1 || strings.Join(fix.reqs[0].Commands, "|") != "PowerOnState 0" {
		t.Fatalf("submitted %+v", fix.reqs)
	}
	// an Advisory row ticked by hand on the tab is written
	adv := advisoryEnv(t, fix)
	fix.reqs = nil
	body = previewFor(t, adv, url.Values{"pick": {"1:r2"}})
	if !strings.Contains(body, cmdHTML("SetOption0 0")) || strings.Contains(body, cmdHTML("PowerOnState 0")) {
		t.Fatalf("the ticked hint alone:\n%s", body)
	}
	if !strings.Contains(body, `<span class="bdg t-accent">Advisory</span>`) {
		t.Error("the preview marks the command that comes from an Advisory rule")
	}
	if !strings.Contains(body, `href="/tf/policy/deviations">Cancel`) {
		t.Error("Cancel on a preview opened from the tab goes back to the tab")
	}
}

func postForm(v url.Values) *http.Request {
	r := httptest.NewRequest("POST", "/policy/fix", nil)
	r.PostForm = v
	return r
}

// Why: a pick must be a real finding of the report, or a forged one could name devices the admin never
// saw (known filter); and in the device modes a pick may only add to a device that is selected
// (sel.has filter), never pull another device into the plan.
func TestPolicyFixSelectionDropsPicksOutsideTheReportAndTheSelection(t *testing.T) {
	rep := fixReport()
	if sel := fixSelection(postForm(url.Values{"pick": {"1:r1", "1:rzzz", "99:r1"}}), rep); !sel.ByPick || len(sel.IDs) != 1 || sel.IDs[0] != 1 || len(sel.Picks) != 1 {
		t.Errorf("only the real finding survives: %+v", sel)
	}
	if sel := fixSelection(postForm(url.Values{"only": {"4"}, "pick": {"1:r1"}}), rep); len(sel.Picks) != 0 || sel.has(1) {
		t.Errorf("a pick never adds a device to a device selection: %+v", sel)
	}
}

// Why: the digest is what binds the reviewed plan; two plans with the same commands but different
// picks must not share one, or a form could swap the picks under a reviewed digest.
func TestPolicyFixDigestCoversThePicks(t *testing.T) {
	row := func(pairs ...fixPair) fixRow {
		return fixRow{Device: model.Device{ID: 1}, Cmds: []fixCmd{{Text: "SetOption0 0"}}, Pairs: pairs}
	}
	a := fixPageOf([]fixRow{row(fixPair{1, "r1"})}, true).Digest
	b := fixPageOf([]fixRow{row(fixPair{1, "r2"})}, true).Digest
	if a == b {
		t.Error("the digest ignores which rule was picked")
	}
}

// Why: a forged or stale form must never widen what is written. A pick for a rule the device does
// not deviate from, for a device that is not in the report, or for another selected device
// than the one picked, adds nothing; and a deviation known only from an old backup is never
// fixed, even when picked.
func TestPolicyFixPickForAPairNotInTheReportIsIgnored(t *testing.T) {
	fix := &fakeFix{}
	e := fixEnv(t, fix, nil)
	for name, form := range map[string]url.Values{
		"unknown rule id":   {"pick": {"1:rzzz"}},
		"unknown device":    {"pick": {"99:r1"}},
		"shed is fine":      {"pick": {"4:r1"}},
		"malformed":         {"pick": {"1", ":r1", "x:r1", "1:"}},
		"other device only": {"only": {"4"}, "pick": {"1:r1", "1:r2"}},
	} {
		body := previewFor(t, e, form)
		if strings.Contains(body, cmdHTML("PowerOnState 0")) || strings.Contains(body, cmdHTML("SetOption0 0")) {
			t.Errorf("%s widened the plan:\n%s", name, body)
		}
	}
	body := previewFor(t, e, url.Values{"pick": {"2:r1"}}) // attic: value only in an old backup
	if strings.Contains(body, cmdHTML("PowerOnState 0")) || !strings.Contains(body, "only known from a backup, not read live") {
		t.Errorf("a backup-only value is never fixed:\n%s", body)
	}
	m := digestRe.FindStringSubmatch(body)
	if m != nil {
		e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "pick": {"2:r1"}})
	}
	if len(fix.reqs) != 0 {
		t.Fatalf("nothing may be submitted: %+v", fix.reqs)
	}
}

// Why (D13): Make compliant never confirms a change that can cut off a device known to be reached over MQTT only
// (HTTP failing), so the preview does not promise it: the command leaves the plan and shows as skipped with the
// reason. A device whose HTTP is merely not proven usable (MQTTOnly, the console's hint) keeps it, or a broker move
// (#598) would skip every device live on MQTT; the router decides per device at the send (#605 review, round 3).
func TestPolicyFixPreviewSkipsCutOffLinesOfMQTTOnlyDevices(t *testing.T) {
	topic := policy.Rule{ID: "r2", Command: "Topic", Want: "other"}
	rep := fixReport()
	rep.Rules[1] = topic
	rep.Findings[1] = policy.Finding{DeviceID: 1, Rule: topic, Have: "kitchen"}
	rep.Provenance[1]["topic"] = rep.Provenance[1]["setoption0"]
	only := true
	e := setupWith(t, func(d *Deps) {
		d.Policy, d.PolicyFix = &fakePolicy{rep: rep}, &fakeFix{}
		d.MQTTOnly = func(context.Context, ...model.DeviceID) bool { return true }
		d.KnownMQTTOnly = func(context.Context, ...model.DeviceID) bool { return only }
	})
	body := previewFor(t, e, url.Values{"device": {"1"}})
	if strings.Contains(body, cmdHTML("Topic other")) || !strings.Contains(body, "reached over MQTT only (use the console)") || !strings.Contains(body, cmdHTML("PowerOnState 0")) {
		t.Fatalf("the plan:\n%s", body)
	}
	only = false
	if body = previewFor(t, e, url.Values{"device": {"1"}}); !strings.Contains(body, cmdHTML("Topic other")) {
		t.Fatalf("HTTP not known to fail: the command stays:\n%s", body)
	}
}

// advisoryOnlyReport is advisoryFixReport without kitchen's Required deviation (r1): kitchen is
// left with one Advisory deviation read live and nothing else, the case of #1002.
func advisoryOnlyReport() PolicyReport {
	rep := advisoryFixReport()
	var keep []policy.Finding
	for _, f := range rep.Findings {
		if f.DeviceID != 1 || f.Rule.ID != "r1" {
			keep = append(keep, f)
		}
	}
	rep.Findings = keep
	return rep
}

func advisoryOnlyEnv(t *testing.T, fix *fakeFix, jobs JobLister) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Policy, d.PolicyFix, d.Jobs = &fakePolicy{rep: advisoryOnlyReport()}, fix, jobs
	})
}

var (
	h1Re     = regexp.MustCompile(`<h1[^>]*>([^<]*)</h1>`)
	cancelRe = regexp.MustCompile(`<a class="btn" href="([^"]*)">Cancel</a>`)
)

// Why (#1002): the device page offers Make compliant for a device whose only deviation is an
// Advisory one. The page used to count it as not ready ("Make 0 devices compliant") and described
// commands that do not exist. Now the title names the one device and the subtitle says there is
// nothing to run yet and what to do, so the admin is not told about a plan that is empty.
func TestPolicyFixNamesTheOneDeviceAndSaysNothingToRunYet(t *testing.T) {
	e := advisoryOnlyEnv(t, &fakeFix{}, nil)
	body := previewFor(t, e, url.Values{"only": {"1"}})
	if m := h1Re.FindStringSubmatch(body); m == nil || m[1] != "Make kitchen compliant" {
		t.Errorf("the title names the one selected device, got %v", m)
	}
	if !strings.Contains(body, "Nothing to run yet: tick an Advisory deviation and choose Update plan") {
		t.Errorf("the subtitle says there is nothing to run yet:\n%s", body)
	}
	if strings.Contains(body, "These are the exact commands") {
		t.Error("the subtitle must not describe commands that are not there")
	}
	if strings.Contains(body, "data-plan-run") || strings.Contains(body, "Run 0 change") {
		t.Error("there is nothing to run: no Run button")
	}
}

// Why (#1002): the title counts the devices the admin selected, not only those that get commands;
// a device that cannot be changed (attic: its value is only known from a backup) is still on the
// page, so "Make 1 device compliant" over two cards was wrong.
func TestPolicyFixTitleCountsTheSelectedDevices(t *testing.T) {
	e := fixEnv(t, &fakeFix{}, nil)
	body := previewFor(t, e, url.Values{"device": {"1", "2"}})
	if m := h1Re.FindStringSubmatch(body); m == nil || m[1] != "Make 2 devices compliant" {
		t.Errorf("two selected devices, got %v", m)
	}
	if !strings.Contains(body, "Run 2 changes") {
		t.Errorf("kitchen's two commands still run:\n%s", body)
	}
}

// Why (#1002): "No Required rule deviates" is a hint about how to proceed, not a reason the device
// cannot be changed. Blocked is for the latter, and it decides whether a row runs and what the
// apply loop sends; a hint in Blocked hid the real reason of a device that is also busy. The
// last checks guard the rule that a row without commands never reaches the engine (they pass
// without the fix too, because Blocked rows were skipped; they fail if a hint row were sent).
func TestPolicyFixAdvisoryHintIsNotABlock(t *testing.T) {
	hint := regexp.MustCompile(`<p class="pol-fix-hint">No Required rule deviates\. Tick an Advisory deviation below to include it\.</p>`)
	blocked := regexp.MustCompile(`<p class="pol-fix-skip">([^<]*)</p>`)
	body := previewFor(t, advisoryOnlyEnv(t, &fakeFix{}, nil), url.Values{"only": {"1"}})
	if !hint.MatchString(body) || blocked.MatchString(body) {
		t.Errorf("the hint is shown as a hint, not as a reason the device cannot be changed:\n%s", body)
	}
	if !strings.Contains(body, `name="pick" value="1:r2"`) {
		t.Error("the Advisory deviation is offered to tick")
	}

	// the device is busy: that is the reason shown, not the hint
	body = previewFor(t, advisoryOnlyEnv(t, &fakeFix{}, &activeOn{device: 1}), url.Values{"only": {"1"}})
	if m := blocked.FindStringSubmatch(body); m == nil || !strings.Contains(m[1], "Another transaction is running on this device (#3)") || hint.MatchString(body) {
		t.Errorf("a running job is the reason, got %v:\n%s", m, body)
	}

	if strings.Contains(body, "tick an Advisory deviation") || strings.Contains(body, "These are the exact commands") {
		t.Errorf("a busy device gets no plan from a tick and no commands to describe:\n%s", body)
	}

	// a row without commands is never ready, with or without a block: nothing is submitted for it
	p := fixPageOf([]fixRow{{Device: model.Device{ID: 1}, Hint: "hint"}}, false)
	if p.Ready != 0 || len(p.IDs) != 0 || len(p.Fields) != 0 {
		t.Errorf("a hint row is not ready: %+v", p)
	}
	fix := &fakeFix{}
	e := advisoryOnlyEnv(t, fix, nil)
	m := digestRe.FindStringSubmatch(previewFor(t, e, url.Values{"only": {"1"}}))
	if m == nil {
		t.Fatal("no digest")
	}
	r, _ := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {m[1]}, "device": {"1"}})
	if len(fix.reqs) != 0 {
		t.Fatalf("a row without commands is never submitted: %d %+v", r.StatusCode, fix.reqs)
	}
}

// Why (#1002): the title of the page now carries the device name, which can be one long word
// (underscores) wider than a phone. The page header must break it, as the device page does, or the
// whole page scrolls sideways at 375 px.
func TestPageHeaderTitleBreaksLongNames(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil || !regexp.MustCompile(`(?s)\.pghead h1,\s*\.pghead \.h1-sample \{[^}]*overflow-wrap: anywhere;`).Match(css) {
		t.Error(".pghead h1 no longer breaks a long device name")
	}
}

// Why (#1002): the subtitle must not promise anything the page cannot deliver. "Tick an Advisory
// deviation" is only true where a card carries that hint; a device that cannot be changed (nothing
// read live, a running job) gets no plan from a tick, and no Run button exists, so describing
// "the exact commands" would be wrong too. Those pages say neutrally that nothing can run.
func TestPolicyFixSubtitleOnlyPromisesWhatTheCardsCanDo(t *testing.T) {
	const neutral = "Nothing to run: no device on this page can be changed now; each card says why."
	for name, body := range map[string]string{
		"nothing read live":   previewFor(t, fixEnv(t, &fakeFix{}, nil), url.Values{"only": {"2"}}),
		"busy, Required rule": previewFor(t, fixEnv(t, &fakeFix{}, &activeOn{device: 1}), url.Values{"only": {"1"}}),
	} {
		if !strings.Contains(body, neutral) {
			t.Errorf("%s: the neutral subtitle is missing:\n%s", name, body)
		}
		if strings.Contains(body, "These are the exact commands") || strings.Contains(body, "tick an Advisory deviation and choose") {
			t.Errorf("%s: the subtitle describes commands or a tick that cannot help", name)
		}
	}
}

// Why (#1002): Cancel returns to where the page was opened. The device page marks its form with
// from=device; that marker is the only thing the server reads, never a URL, so nothing the
// client posts can become a redirect target. The marker has to survive Update plan and the stale
// page (both forms re-emit it), or Cancel would fall back to the policy page after one tick.
func TestPolicyFixCancelReturnsToTheOpeningDevice(t *testing.T) {
	e := advisoryEnv(t, &fakeFix{})
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no marker":                  {url.Values{"only": {"1"}}, "/tf/policy"},
		"from the device page":       {url.Values{"only": {"1"}, "from": {"device"}}, "/tf/devices/1"},
		"after Update plan":          {url.Values{"device": {"1"}, "from": {"device"}, "pick": {"1:r2"}}, "/tf/devices/1"},
		"an absolute URL":            {url.Values{"only": {"1"}, "from": {"https://evil.example/"}}, "/tf/policy"},
		"a path":                     {url.Values{"only": {"1"}, "from": {"/devices/1"}}, "/tf/policy"},
		"another case":               {url.Values{"only": {"1"}, "from": {"Device"}}, "/tf/policy"},
		"an empty marker":            {url.Values{"only": {"1"}, "from": {""}}, "/tf/policy"},
		"two devices":                {url.Values{"device": {"1", "3"}, "from": {"device"}}, "/tf/policy"},
		"the deviations tab picking": {url.Values{"pick": {"1:r1"}}, "/tf/policy/deviations"},
		"a marker with picks only":   {url.Values{"pick": {"1:r1"}, "from": {"device"}}, "/tf/policy/deviations"},
	} {
		body := previewFor(t, e, tc.form)
		m := cancelRe.FindStringSubmatch(body)
		if m == nil || m[1] != tc.want {
			t.Errorf("%s: Cancel goes to %v, want %s", name, m, tc.want)
		}
	}

	// the marker is carried by both forms of the page, as the constant and never as the posted text
	body := previewFor(t, e, url.Values{"only": {"1"}, "from": {"device"}})
	if n := strings.Count(body, `name="from" value="device"`); n != 2 {
		t.Errorf("the Update plan form and the apply form both carry the marker, found %d", n)
	}
	if body = previewFor(t, e, url.Values{"only": {"1"}, "from": {"https://evil.example/"}}); strings.Contains(body, "evil.example") || strings.Contains(body, `name="from"`) {
		t.Error("a posted marker that is not the constant is dropped, never echoed")
	}
	// the stale page keeps the marker too
	r, out := e.do(t, "POST", "/policy/fix/apply", "admin", url.Values{"csrf": {"tok"}, "digest": {"deadbeef"}, "device": {"1"}, "from": {"device"}})
	if m := cancelRe.FindStringSubmatch(out); r.StatusCode != 409 || m == nil || m[1] != "/tf/devices/1" {
		t.Errorf("the stale page keeps the way back: %d %v", r.StatusCode, m)
	}
}

// Why (#1104): the note under a command reads "Power on state: has 3, checked 2 min ago (live)". The sentence is one
// piece of text; written as separate template pieces, the template put a space before the comma ("has 3 , checked"),
// a visible wording change the date work must not bring. A backup value has its day as a <time> element in the same
// sentence, still with no space before the comma.
func TestPolicyFixNoteReadsAsOneSentence(t *testing.T) {
	e := fixEnv(t, &fakeFix{}, nil)
	body := previewFor(t, e, url.Values{"all": {"1"}})
	notes := regexp.MustCompile(`<span class="pol-fix-note">(.*?)</span>`).FindAllStringSubmatch(body, -1)
	if len(notes) == 0 {
		t.Fatal("the preview shows no note under a command")
	}
	for _, n := range notes {
		if !strings.Contains(n[1], ": has ") || !strings.Contains(n[1], ", checked ") || strings.Contains(n[1], " ,") {
			t.Errorf("the note is one sentence with the comma against the value: %q", n[1])
		}
	}
	var b strings.Builder
	c := fixCmd{Label: "Power on state", Have: "2", Note: ui.Stamped{Lead: "from backup of ", At: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), Stamp: ui.StampDate}}
	if err := ui.StampedText(fixNote(c)).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.HasPrefix(got, "Power on state: has 2, from backup of <time ") {
		t.Errorf("a backup note keeps the day as a time element in the same sentence: %s", got)
	}
	if got := fixNote(fixCmd{Label: "Power on state", Have: "2"}); got.Lead != "Power on state: has 2" || !got.IsZero() {
		t.Errorf("a command with no source note says only what it has: %+v", got)
	}
}
