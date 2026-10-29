package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
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
	_, adm := e.do(t, "GET", "/policy", "admin", nil)
	if strings.Contains(op, "Make compliant") || strings.Contains(op, "dlg-fix") {
		t.Error("operators must not be offered the action")
	}
	// The admin's banner offers Make compliant; its dialog counts the devices that get commands
	// (kitchen and cellar: attic's value is only known from a backup), lists the commands and
	// applies exactly the reviewed plan (digest and devices).
	for _, want := range []string{"Make compliant…", "Make 2 devices compliant", `data-dialog="dlg-fix"`, `name="all" value="1"`, `name="device" value="1"`, `name="device" value="3"`,
		`name="digest"`, "PowerOnState 0; SetOption0 0", "Run 3 changes"} {
		if !strings.Contains(adm, want) {
			t.Errorf("admin page misses %q", want)
		}
	}
	if strings.Contains(adm, `name="device" value="2"`) {
		t.Error("attic deviates only in a backup: Make compliant must not include it")
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
