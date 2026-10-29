package web

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// devReport: kitchen breaks a Required and an Advisory rule (both read live), attic breaks a
// Required rule only according to an old backup, pantry cannot be judged for OtaUrl (a backup
// decodes it) and cellar for LedState (only a live read tells), shed is fine.
func devReport() PolicyReport {
	rules := []policy.Rule{
		{ID: "r1", Name: "Power after outage", Command: "PowerOnState", Want: "0"},
		{ID: "r2", Name: "Local timezone", Command: "Timezone", Want: "99", Severity: policy.SeverityInfo},
		{ID: "r3", Name: "Firmware server", Command: "OtaUrl", Want: "http://fw/x.bin"},
		{ID: "r4", Name: "Status LED", Command: "LedState", Want: "0"},
	}
	live := ValueSource{Kind: "live", At: time.Now().Add(-6 * time.Minute)}
	o := func(dev, rule int, k policy.OutcomeKind, have string) policy.Outcome {
		return policy.Outcome{DeviceID: model.DeviceID(dev), RuleIndex: rule, RuleID: rules[rule].ID, Kind: k, Have: have}
	}
	dev := func(id int, name string) model.Device {
		return model.Device{ID: model.DeviceID(id), Name: name, Address: "10.0.0." + string(rune('0'+id)), Identity: model.Identity{Family: model.ChipESP8266}}
	}
	rep := PolicyReport{
		Rules:   rules,
		Devices: []model.Device{dev(1, "kitchen"), dev(2, "attic"), dev(3, "pantry"), dev(4, "cellar"), dev(5, "shed")},
		Outcomes: []policy.Outcome{
			o(1, 0, policy.OutcomeDeviate, "3"), o(1, 1, policy.OutcomeDeviate, "1"),
			o(2, 0, policy.OutcomeDeviate, "2"),
			o(3, 2, policy.OutcomeUnknown, ""), o(4, 3, policy.OutcomeUnknown, ""),
			o(5, 0, policy.OutcomeMatch, "0"),
		},
		Provenance: map[model.DeviceID]map[string]ValueSource{
			1: {"poweronstate": live, "timezone": live},
			2: {"poweronstate": {Kind: "backup", At: time.Now().Add(-48 * time.Hour)}},
		},
	}
	for _, out := range rep.Outcomes {
		if out.Kind == policy.OutcomeDeviate || out.Kind == policy.OutcomeUnknown {
			rep.Findings = append(rep.Findings, policy.Finding{DeviceID: out.DeviceID, Rule: rules[out.RuleIndex], RuleIndex: out.RuleIndex,
				Have: out.Have, Severity: rules[out.RuleIndex].Severity, Unknown: out.Kind == policy.OutcomeUnknown})
		}
	}
	return rep
}

func devPage(t *testing.T, who, query string, rep PolicyReport) string {
	t.Helper()
	e := setupWith(t, func(d *Deps) {
		d.Policy = &fakePolicy{rep: rep}
		d.PolicyFix = &fakeFix{}
		d.BackupBatch = &fakeBackupBatch{}
	})
	r, body := e.do(t, "GET", "/policy/deviations"+query, who, nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d\n%s", r.StatusCode, body)
	}
	return body
}

// boxOf is the checkbox of a (device, rule) pair, or "" when the row has none.
func boxOf(body, pair string) string {
	return regexp.MustCompile(`<input type="checkbox" name="pick" value="` + pair + `"[^>]*>`).FindString(body)
}

// Why: the tab is where an admin decides what Make compliant writes. A Required row starts ticked,
// an Advisory row (a hint) never does, and a value known only from an old backup has no box at
// all: it can never be fixed, so offering a box would promise something the plan refuses.
func TestPolicyDeviationsBoxesFollowSeverityAndSource(t *testing.T) {
	body := devPage(t, "admin", "", devReport())
	if b := boxOf(body, "1:r1"); b == "" || !strings.Contains(b, " checked") {
		t.Errorf("Required, live: ticked box expected, got %q", b)
	}
	if b := boxOf(body, "1:r2"); b == "" || strings.Contains(b, " checked") {
		t.Errorf("Advisory, live: unticked box expected, got %q", b)
	}
	if b := boxOf(body, "2:r1"); b != "" {
		t.Errorf("a backup-only value gets no box, got %q", b)
	}
	for _, want := range []string{"only known from a backup, not read live", "Make compliant… (1 selected)", "kitchen", "1 required · 1 advisory"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Why: operators may look, not change devices: no boxes, no Make compliant, no Back up.
func TestPolicyDeviationsOperatorSeesNoActions(t *testing.T) {
	body := devPage(t, "operator", "", devReport())
	for _, bad := range []string{`name="pick"`, "Make compliant", "Back up these", `name="id"`} {
		if strings.Contains(body, bad) {
			t.Errorf("operator page contains %q", bad)
		}
	}
	if !strings.Contains(body, "Run now") || !strings.Contains(body, "Power after outage") {
		t.Error("operators still read the deviations and may run the live check")
	}
}

// Why: a backup can only tell what it decodes. "Back up these N" must hold exactly the devices
// whose unknown setting a backup decodes (OtaUrl), never one that only a live read can answer
// (LedState): a backup would run for nothing and the device would stay unjudged.
func TestPolicyDeviationsBackUpListsOnlyBackupReadableUnknowns(t *testing.T) {
	body := devPage(t, "admin", "", devReport())
	i := strings.Index(body, `id="unjudged"`)
	if i < 0 {
		t.Fatal("no fold of unjudged devices")
	}
	fold := body[i:]
	form := regexp.MustCompile(`(?s)<form[^>]*/devices/bulk/backup".*?</form>`).FindString(fold)
	if form == "" || !strings.Contains(form, `name="id" value="3"`) || strings.Contains(form, `name="id" value="4"`) || !strings.Contains(form, "Back up these 1") {
		t.Errorf("backup form must hold pantry (3) only:\n%s", form)
	}
	if !strings.Contains(fold, "Only a live read tells these") || !strings.Contains(fold, `name="back" value="deviations"`) {
		t.Error("cellar's hint and Run now are missing")
	}
	if strings.Count(fold, "cellar") != 1 || strings.Count(fold, "pantry") != 1 {
		t.Error("each unjudged device is listed once")
	}
}

// Why: the empty state must say so, and the fold of unjudged devices stays even then.
func TestPolicyDeviationsEmptyStateKeepsTheFold(t *testing.T) {
	rep := devReport()
	rep.Outcomes = rep.Outcomes[3:]
	body := devPage(t, "admin", "", rep)
	if !strings.Contains(body, "No device deviates from a rule.") || !strings.Contains(body, "cannot be judged") {
		t.Errorf("empty state:\n%s", body)
	}
	if strings.Contains(body, `id="dev-pick"`) || strings.Contains(body, "Make compliant") {
		t.Error("nothing to pick, no Make compliant")
	}
}

// Why: the filters narrow the table only; the numbers on the chips stay the whole report's.
func TestPolicyDeviationsFilters(t *testing.T) {
	adv := devPage(t, "admin", "?show=advisory", devReport())
	if strings.Contains(adv, "Power after outage") || !strings.Contains(adv, "Local timezone") {
		t.Error("show=advisory lists Advisory rows only")
	}
	if !strings.Contains(adv, `class="dv-n">2</span>`) {
		t.Error("the All chip keeps counting every row")
	}
	q := devPage(t, "admin", "?q=attic", devReport())
	if strings.Contains(q, ">kitchen<") || !strings.Contains(q, ">attic<") {
		t.Error("q narrows by device name")
	}
	unk := devPage(t, "admin", "?show=unknown", devReport())
	if strings.Contains(unk, `id="deviations"`) || !regexp.MustCompile(`<details class="fold" id="unjudged" open`).MatchString(unk) {
		t.Error("show=unknown opens the fold and hides the table")
	}
	if strings.Contains(devPage(t, "admin", "?show=<script>", devReport()), "<script>") {
		t.Error("an unknown show value is ignored")
	}
}

type devFailingPolicy struct{ fakePolicy }

func (devFailingPolicy) Report(context.Context, string, bool) (PolicyReport, error) {
	return PolicyReport{}, errors.New("probe failed")
}

// Why: a failed live check must not look like "no deviations".
func TestPolicyDeviationsFailedReportSaysSo(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &devFailingPolicy{} })
	_, body := e.do(t, "GET", "/policy/deviations", "admin", nil)
	if !strings.Contains(body, "No deviations can be shown") || strings.Contains(body, "No device deviates") {
		t.Errorf("%s", body)
	}
}

// Why: the tab is linked from the shared header on the overview, with the number of deviating devices.
func TestPolicyTabsLinkTheDeviationsTab(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: devReport()} })
	_, body := e.do(t, "GET", "/policy", "operator", nil)
	if !regexp.MustCompile(`href="/tf/policy/deviations">Deviating devices <span class="bdg t-accent">2</span>`).MatchString(body) {
		t.Error("the shared header links the Deviating devices tab with its badge")
	}
}
