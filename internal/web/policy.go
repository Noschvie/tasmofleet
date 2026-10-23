package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// ValueSource says where a checked value came from: "live" (read from the
// device or announced over MQTT) or "backup", and when.
type ValueSource struct {
	Kind string
	At   time.Time
}

// PolicyReport is what the policy page shows. Provenance maps a device and a
// lower-case command to the source of its value; a missing entry means no
// value at all. NotLive holds the lower-case commands that cannot be read
// without side effects.
type PolicyReport struct {
	Rules      []policy.Rule
	Devices    []model.Device
	Findings   []policy.Finding
	Provenance map[model.DeviceID]map[string]ValueSource
	NotLive    map[string]bool
	// LiveThrottled is true when the live check was rate limited and
	// answered from recent reads only.
	LiveThrottled bool
}

// PolicyService builds the report from live values (read in the background,
// announced over MQTT, or read now when live is true) with backups as the
// fallback for settings that have no live value.
type PolicyService interface {
	Report(ctx context.Context, actor string, live bool) (PolicyReport, error)
}

// policyPath is the merged policy page (design v2, N3): rule cards, the editor, the live
// preview, the version history and the live-check status line. The rule changes keep their
// form routes under rulesPath and come back here.
const policyPath = "/policy"

// policyFlashes are the confirmations after a change that came back to the page (the
// prototype's toasts).
var policyFlashes = map[string]string{
	"saved":      "Rule saved",
	"deleted":    "Rule deleted",
	"duplicated": "Rule duplicated · disabled until you enable it",
	"enabled":    "Rule enabled",
	"disabled":   "Rule disabled · it no longer flags devices",
	"template":   "Rule added from template",
	"reverted":   "Reverted: the old rules are saved as a new version",
	"imported":   "Rules imported as a new version",
	"paused":     "Live check paused",
	"resumed":    "Live check resumed",
}

func (h *handler) registerPolicy(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	if h.d.Policy == nil {
		return
	}
	route("GET "+policyPath, op, func(w http.ResponseWriter, r *http.Request) {
		o, ok := editOpts(r)
		if !ok {
			h.notFound(w, r)
			return
		}
		h.policyRender(w, r, http.StatusOK, o)
	})
	route("POST "+policyPath+"/live", op, func(w http.ResponseWriter, r *http.Request) {
		h.policyRender(w, r, http.StatusOK, policyOpts{live: true})
	})
}

// policyOpts says what the page shows beyond the saved state: the rule open in the editor,
// a draft the server refused, a banner for a refused list change, a live check just run.
type policyOpts struct {
	live     bool
	edit     string // "" (the default rule), "new" or a list position
	draft    *ruleDraft
	errField string
	errMsg   string
	banner   string
	// version is the history entry picked for the diff (0: the one before the current)
	version int64
}

// editOpts reads ?edit=N, ?new=1 and ?v=ID. A position or version that is not a number is
// not a page.
func editOpts(r *http.Request) (policyOpts, bool) {
	q := r.URL.Query()
	o := policyOpts{}
	switch {
	case q.Get("new") != "":
		o.edit = "new"
	case q.Get("edit") != "":
		if _, err := strconv.Atoi(q.Get("edit")); err != nil || !idxRe.MatchString(q.Get("edit")) {
			return o, false
		}
		o.edit = q.Get("edit")
	}
	if v := q.Get("v"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return o, false
		}
		o.version = n
	}
	return o, true
}

// liveLine is the live-check status line: idle, paused or failed.
type liveLine struct {
	State    string // "idle", "paused", "failed"
	Text     string
	CanPause bool // Pause / Resume are shown (admin, and the pause service exists)
}

// ruleCard is one rule in the list, in stored order (the order is the precedence).
type ruleCard struct {
	Idx       int
	Name      string
	KV        string // "TelePeriod = 300"
	Applies   string // "All devices", "Chip family: ESP32", ...
	Required  bool
	Severity  string // Required / Advisory
	Disabled  bool
	Badge     string // "2 flagged", "1 hint", "All match", "3 unknown"; "" when not counted
	BadgeTone ui.Tone
	Flagged   int  // devices it flags now
	Counted   bool // Flagged is known (the preview ran)
	Except    []string
	Note      string
	Selected  bool
	First     bool
	Last      bool
}

// pvRow is a device the selected rule flags.
type pvRow struct {
	ID       model.DeviceID
	Name     string
	Have     string
	Want     string
	Unknown  bool
	Advisory bool
	Note     string // where the value came from: "checked 5m ago (live)", "from backup of …", "not readable live"
}

// versionRow is one entry of the version history.
type versionRow struct {
	ID       int64
	What     string // the change note, long identifiers cut to 8 characters (they cannot wrap)
	Full     string // the whole note when What is shortened (a tooltip)
	By       string
	At       time.Time
	Current  bool
	Selected bool
}

// fixDialog is "Make compliant (N)…": the reviewed plan the dialog applies.
type fixDialog struct {
	Ready    int
	Commands int
	Changes  []ui.KVRow
	Fields   []ui.KVRow
}

type policyView struct {
	Base, CSRF string
	Admin      bool
	Sub        string
	Dev        int // devices that deviate (the subtitle and the live-check toast)
	Version    int64
	HasRules   bool
	Line       *liveLine
	Conflicts  []string
	Cards      []ruleCard
	Banner     string
	Editor     *ruleEditor
	Selected   int // the card the preview belongs to; -1: none
	Preview    rulePreview
	PvRows     []pvRow
	PvDisabled bool
	History    []versionRow
	Diff       []sysconfig.RuleChange
	RevertDiff []sysconfig.RuleChange // what a revert to DiffFrom changes: current -> DiffFrom
	DiffFrom   int64
	SelAt      time.Time
	Templates  []ruleTemplate
	Fix        *fixDialog
	CanRules   bool // the rules can be changed here (the rules service exists)
}

// policyRender writes the merged page.
func (h *handler) policyRender(w http.ResponseWriter, r *http.Request, status int, o policyOpts) {
	v := h.view(r, "Policy", "policy", nil)
	p, err := h.policyData(r, v, o)
	if errors.Is(err, sysconfig.ErrNoSuchRule) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if f := policyFlashes[r.URL.Query().Get("ok")]; f != "" {
		v.Flash = f
	}
	if o.live && p.Line != nil && p.Line.State != "failed" {
		v.Flash = "Live check finished · " + deviateText(p.Dev)
	}
	h.show(w, r, status, "policy", v, policyPage(p))
}

// policyData builds the page. A failed report (the live check could not run) still shows the
// rules: the status line says what failed and offers "Retry now".
func (h *handler) policyData(r *http.Request, v view, o policyOpts) (policyView, error) {
	ctx := r.Context()
	p := policyView{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin, Selected: -1, Banner: o.banner, CanRules: h.d.PolicyRules != nil}
	rep, repErr := h.d.Policy.Report(ctx, actor(r), o.live)
	var st sysconfig.RulesState
	if h.d.PolicyRules != nil {
		var err error
		if st, err = h.d.PolicyRules.State(ctx); err != nil {
			return p, err
		}
	} else {
		st.Specs = specsOf(rep.Rules)
	}
	p.Version = st.VersionID
	p.HasRules = len(st.Specs) > 0
	editable := p.Admin && p.CanRules

	// Counts per rule with precedence, and the conflicts, from the preview (backups and recent
	// reads; it reads no device). A failed preview leaves the counts out, never the list.
	var res sysconfig.PreviewResult
	counted := false
	if p.CanRules && p.HasRules {
		if pr, err := h.d.PolicyRules.Preview(ctx, st.Specs); err == nil && len(pr.Rules) == len(st.Specs) {
			res, counted = pr, true
		}
	}
	names := make([]string, len(st.Specs))
	for i, s := range st.Specs {
		names[i] = ruleName(s)
	}
	for _, c := range res.Conflicts {
		if c.Higher < len(names) && c.Lower < len(names) {
			p.Conflicts = append(p.Conflicts, conflictText(names[c.Higher], names[c.Lower], c))
		}
	}

	// Which rule is open: the one asked for, else the first that flags devices, else the first.
	sel := -1
	switch {
	case o.edit == "new":
	case o.edit != "":
		n, _ := strconv.Atoi(o.edit) // editOpts and the forms checked it is a number
		if n >= len(st.Specs) {
			return p, sysconfig.ErrNoSuchRule
		}
		sel = n
	case p.HasRules:
		sel = 0
		for i := range st.Specs {
			if counted && res.Rules[i].Flagged > 0 {
				sel = i
				break
			}
		}
	}
	p.Selected = sel

	for i, s := range st.Specs {
		c := ruleCard{
			Idx: i, Name: names[i], KV: s.Command + " = " + s.Want, Applies: appliesText(s),
			Required: s.Severity != policy.SeverityInfo, Severity: policy.SeverityLabel(s.Severity),
			Disabled: s.Disabled, Except: s.Except, Note: s.Note, Selected: i == sel,
			First: i == 0, Last: i == len(st.Specs)-1,
		}
		if counted && !s.Disabled {
			c.Badge, c.BadgeTone = ruleBadge(res.Rules[i], c.Required)
			c.Flagged, c.Counted = res.Rules[i].Flagged, true
		}
		p.Cards = append(p.Cards, c)
	}

	// The live-check line, the subtitle and Make compliant come from the report.
	dev := 0
	if repErr == nil {
		dev = len(deviating(rep))
	}
	p.Dev = dev
	p.Sub = plural(len(st.Specs), "rule") + " · " + deviateText(dev) + " · when rules overlap, the higher one wins"
	if p.HasRules {
		p.Line = h.liveLine(rep, repErr, dev, p.Admin)
	}
	if repErr == nil && p.Admin && h.d.PolicyFix != nil {
		p.Fix = h.fixDialogOf(ctx, rep)
	}

	// The selected rule's preview: the counts with precedence and the devices it flags now.
	if sel >= 0 {
		s := st.Specs[sel]
		p.PvDisabled = s.Disabled
		if counted {
			p.Preview = rulePreview{OK: true, Flagged: res.Rules[sel].Flagged, Unknown: res.Rules[sel].Unknown, Devices: res.Devices}
		}
		if repErr == nil {
			p.PvRows = pvRows(rep, sel, s, h.d.Clock.Now())
		}
	}
	if editable {
		if err := h.policyEditor(r, &p, st, o, sel); err != nil {
			return p, err
		}
	}
	if !p.HasRules && editable {
		p.Templates = policyTemplates(h.brokerHost())
	}
	if p.CanRules {
		p.History, p.Diff, p.DiffFrom, p.SelAt = h.historyOf(ctx, st, o.version)
		if editable && p.DiffFrom != 0 && p.DiffFrom != st.VersionID {
			// The revert dialog lists what the revert does, the other way round from the history's
			// "vN → current" diff.
			d, err := h.d.PolicyRules.VersionDiff(ctx, st.VersionID, p.DiffFrom)
			if err != nil {
				h.log.WarnContext(ctx, "policy revert diff", "err", err)
			}
			p.RevertDiff = d
		}
	}
	return p, nil
}

// policyEditor opens the editor: a new rule, the selected rule, or the draft the server
// refused (shown again with the problem at its field).
func (h *handler) policyEditor(r *http.Request, p *policyView, st sysconfig.RulesState, o policyOpts, sel int) error {
	var rp rulesPage
	rp.Current = st.VersionID
	switch {
	case o.draft != nil:
		if err := h.buildEditor(r, &rp, o.draft.Idx, o.draft.Spec, st.Specs); err != nil {
			return err
		}
		rp.Editor.Err, rp.Editor.ErrField = o.errMsg, o.errField
		rp.Editor.Control.Invalid = o.errField == "want"
		rp.Editor.Enabled = !o.draft.Spec.Disabled
	case o.edit == "new":
		if err := h.buildEditor(r, &rp, "new", sysconfig.RuleSpec{Severity: policy.SeverityInfo}, st.Specs); err != nil {
			return err
		}
		rp.Editor.Enabled = true
		p.Preview = rp.Editor.Preview
	case sel >= 0:
		if err := h.buildEditor(r, &rp, strconv.Itoa(sel), st.Specs[sel], st.Specs); err != nil {
			return err
		}
		rp.Editor.Enabled = !st.Specs[sel].Disabled
	default:
		return nil
	}
	p.Editor = rp.Editor
	if o.draft != nil || o.edit == "new" {
		// A draft or a new rule is not a saved card: its numbers come from the editor's preview.
		p.Preview = rp.Editor.Preview
		if o.draft != nil {
			p.PvRows = nil
		}
	}
	return nil
}

// historyOf lists the versions, newest first, and the diff of the picked one against the
// current rules (default: the version before the current one).
func (h *handler) historyOf(ctx context.Context, st sysconfig.RulesState, pick int64) ([]versionRow, []sysconfig.RuleChange, int64, time.Time) {
	if len(st.History) == 0 {
		return nil, nil, 0, time.Time{}
	}
	if pick == 0 {
		pick = st.VersionID
		for _, hv := range st.History {
			if hv.ID != st.VersionID {
				pick = hv.ID
				break
			}
		}
	}
	var rows []versionRow
	var at time.Time
	for _, hv := range st.History {
		what := hv.Note
		switch {
		case what == "" && hv.RevertOf > 0:
			what = "Reverted to v" + strconv.FormatInt(hv.RevertOf, 10)
		case what == "":
			what = "Rules saved"
		}
		short := longToken.ReplaceAllStringFunc(what, func(t string) string { return t[:8] })
		full := ""
		if short != what {
			full = what
		}
		rows = append(rows, versionRow{ID: hv.ID, What: short, Full: full, By: h.pr.Text(hv.Actor), At: hv.At, Current: hv.ID == st.VersionID, Selected: hv.ID == pick})
		if hv.ID == pick {
			at = hv.At
		}
	}
	if pick == st.VersionID {
		return rows, nil, pick, at
	}
	diff, err := h.d.PolicyRules.VersionDiff(ctx, pick, st.VersionID)
	if err != nil {
		h.log.WarnContext(ctx, "policy version diff", "err", err)
	}
	return rows, diff, pick, at
}

// specsOf turns report rules into specs, for a page without the rules service.
func specsOf(rules []policy.Rule) []sysconfig.RuleSpec {
	out := make([]sysconfig.RuleSpec, len(rules))
	for i, r := range rules {
		fams := make([]string, len(r.Families))
		for j, f := range r.Families {
			fams[j] = string(f)
		}
		out[i] = sysconfig.RuleSpec{Name: r.Name, Command: r.Command, Want: r.Want, Note: r.Note, Severity: r.Severity,
			Families: fams, Tags: r.Tags, Devices: r.Devices, Except: r.Except, Disabled: r.Disabled}
	}
	return out
}

// ruleName is the title of a rule: its name, else the setting's label.
func ruleName(s sysconfig.RuleSpec) string {
	if s.Name != "" {
		return s.Name
	}
	l, _ := settingLabel(s.Command)
	return l
}

// appliesText says which devices a rule covers, in the prototype's words.
func appliesText(s sysconfig.RuleSpec) string {
	var parts []string
	if len(s.Families) > 0 {
		parts = append(parts, "Chip family: "+strings.Join(policy.ScopeLabels(s.Families), ", "))
	}
	if len(s.Tags) > 0 {
		parts = append(parts, "Tag: "+strings.Join(s.Tags, ", "))
	}
	switch len(s.Devices) {
	case 0:
	case 1:
		parts = append(parts, "Device: "+s.Devices[0])
	default:
		parts = append(parts, strconv.Itoa(len(s.Devices))+" named devices")
	}
	if len(parts) == 0 {
		return "All devices"
	}
	return strings.Join(parts, " · ")
}

// ruleBadge is the count next to a rule's name: what it flags now (Required: "flagged",
// Advisory: "hint"), what it cannot judge, or "All match" when every device was judged and fine.
func ruleBadge(c sysconfig.RulePreview, required bool) (string, ui.Tone) {
	switch {
	case c.Flagged > 0 && required:
		return strconv.Itoa(c.Flagged) + " flagged", ui.ToneWarn
	case c.Flagged > 0:
		return plural(c.Flagged, "hint"), ui.ToneNeutral
	case c.Unknown > 0:
		return strconv.Itoa(c.Unknown) + " unknown", ui.ToneNeutral
	}
	return "All match", ui.ToneOK
}

// conflictText is one line of the conflicts banner.
func conflictText(higher, lower string, c policy.Conflict) string {
	if c.SameWant {
		return "“" + higher + "” and “" + lower + "” both expect the same " + c.Command + ". “" + lower +
			"” is redundant on " + plural(c.Devices, "device") + " they share."
	}
	return "“" + higher + "” and “" + lower + "” both set " + c.Command + ". “" + higher +
		"” is higher in the list, so it wins for " + plural(c.Devices, "device") + " they share. Drag to change the order."
}

// deviating lists the devices with at least one value that differs from a rule (an unknown
// value is not a deviation), as the dashboard counts them.
func deviating(rep PolicyReport) map[model.DeviceID]bool {
	out := map[model.DeviceID]bool{}
	for _, f := range rep.Findings {
		if !f.Unknown {
			out[f.DeviceID] = true
		}
	}
	return out
}

// deviateText is "1 device deviates", "2 devices deviate".
func deviateText(n int) string {
	if n == 1 {
		return "1 device deviates"
	}
	return plural(n, "device") + " deviate"
}

// advisory says whether a finding comes from an Advisory rule (a hint: never in Make compliant).
func advisory(f policy.Finding) bool {
	return f.Severity == policy.SeverityInfo || f.Rule.Severity == policy.SeverityInfo
}

// pvRows are the devices the rule at idx flags now, from the report (precedence applied).
// The age and source of each value are part of the verdict: a stale or backup value says so.
func pvRows(rep PolicyReport, idx int, s sysconfig.RuleSpec, now time.Time) []pvRow {
	names := map[model.DeviceID]string{}
	for _, d := range rep.Devices {
		names[d.ID] = d.Name
	}
	var rows []pvRow
	for _, f := range rep.Findings {
		if f.RuleIndex != idx || !strings.EqualFold(f.Rule.Command, s.Command) || f.Rule.Want != s.Want {
			continue
		}
		row := pvRow{ID: f.DeviceID, Name: names[f.DeviceID], Have: f.Have, Want: f.Rule.Want, Unknown: f.Unknown, Advisory: advisory(f)}
		key := strings.ToLower(f.Rule.Command)
		if src, ok := rep.Provenance[f.DeviceID][key]; ok {
			row.Note = sourceNote(src, now)
		} else if f.Unknown && rep.NotLive[key] {
			row.Note = "not readable live"
		}
		rows = append(rows, row)
	}
	// Deviations first, then the values that could not be read.
	var out []pvRow
	for _, unknown := range []bool{false, true} {
		for _, r := range rows {
			if r.Unknown == unknown {
				out = append(out, r)
			}
		}
	}
	return out
}

// liveLine says what the live check does: paused (by whom, since when), failed (why), or
// running with its interval and the time of the newest live value.
func (h *handler) liveLine(rep PolicyReport, repErr error, dev int, admin bool) *liveLine {
	l := &liveLine{State: "idle", CanPause: admin && h.d.PolicyLivePause != nil}
	if repErr != nil {
		l.State = "failed"
		l.Text = "Live check failed: " + sentence(repErr.Error()) + " No deviations can be shown until it runs again."
		return l
	}
	last := lastLiveRead(rep)
	now := h.d.Clock.Now()
	if h.d.PolicyLivePause != nil {
		if s := h.d.PolicyLivePause.State(); s.Paused {
			l.State = "paused"
			l.Text = "Live check paused by " + h.pr.Text(s.By) + " at " + s.At.Local().Format("15:04")
			if !last.IsZero() {
				l.Text += " · deviations shown are from " + last.Local().Format("15:04")
			}
			return l
		}
	}
	parts := []string{}
	switch iv := h.liveInterval(); iv {
	case "":
	case "0", "0s":
		parts = append(parts, "Automatic live check is off")
	default:
		parts = append(parts, "Live check runs every "+iv)
	}
	if last.IsZero() {
		parts = append(parts, "no device read yet")
	} else {
		parts = append(parts, "last run "+agoWords(now, last))
	}
	parts = append(parts, deviateText(dev))
	if rep.LiveThrottled {
		parts = append(parts, "the check ran recently, these are its results")
	}
	l.Text = strings.Join(parts, " · ")
	return l
}

// liveInterval is the configured interval of the automatic live check ("1h", "10m"), "" when
// the system settings are not available.
func (h *handler) liveInterval() string {
	if h.d.SysConfig == nil {
		return ""
	}
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "POLICY_LIVE_INTERVAL" {
			d, err := time.ParseDuration(f.Value)
			if err != nil {
				return f.Value
			}
			return shortDuration(d)
		}
	}
	return ""
}

// shortDuration writes an interval the way the status line reads it: "10 min", "1 h", "1 h 30 min".
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d.Hours())) + " h"
	}
	return strconv.Itoa(int(d.Hours())) + " h " + strconv.Itoa(int(d.Minutes())%60) + " min"
}

// lastLiveRead is the time of the newest value read live.
func lastLiveRead(rep PolicyReport) time.Time {
	var last time.Time
	for _, m := range rep.Provenance {
		for _, s := range m {
			if s.Kind == "live" && s.At.After(last) {
				last = s.At
			}
		}
	}
	return last
}

// sourceNote is the per-value label: "checked 5m ago (live)" or
// "from backup of 2026-10-01".
func sourceNote(s ValueSource, now time.Time) string {
	if s.Kind == "backup" {
		return "from backup of " + s.At.Format("2006-01-02")
	}
	return "checked " + agoWords(now, s.At) + " (live)"
}

// deleteText is the body of a rule's delete dialog.
func deleteText(c ruleCard) string {
	if !c.Counted {
		return "The rule is removed from the policy. The change is saved as a new version you can revert to."
	}
	cmd, _, _ := strings.Cut(c.KV, " = ")
	return "It currently flags " + plural(c.Flagged, "device") + ". They stop showing deviations for " + cmd +
		". The change is saved as a new version you can revert to."
}

// scopeChipText is a scope choice as people read it.
func scopeChipText(c chip) string {
	if c.Label != "" {
		return c.Label
	}
	return c.Value
}

// editParam keeps the open rule when a history entry is picked.
func editParam(p policyView) string {
	if p.Editor != nil && p.Editor.Idx != "new" {
		return "&edit=" + p.Editor.Idx
	}
	return ""
}

// changeText is one difference between two versions: what happened to the rule, and how.
func changeText(c sysconfig.RuleChange) string {
	kind := map[string]string{"added": "Added", "removed": "Removed", "changed": "Changed"}[c.Kind]
	if kind == "" {
		kind = c.Kind
	}
	if c.Detail != "" {
		return kind + ": " + c.Detail
	}
	return kind
}

// revertRows are the differences as the revert dialog lists them.
func revertRows(d []sysconfig.RuleChange) []ui.KVRow {
	out := make([]ui.KVRow, 0, len(d))
	for _, c := range d {
		out = append(out, ui.KVRow{Key: c.Rule, Value: changeText(c)})
	}
	return out
}

// countTone is the colour of an import preview's header count.
func countTone(kind string) ui.Tone {
	switch kind {
	case "New":
		return ui.ToneOK
	case "Remove":
		return ui.ToneDanger
	}
	return ui.ToneAccent
}

// unknownRows counts the preview's devices whose value could not be read (folded below the
// deviations: they are counted, never shown as compliant).
func unknownRows(rows []pvRow) int {
	n := 0
	for _, r := range rows {
		if r.Unknown {
			n++
		}
	}
	return n
}
