package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// SysConfigService is the part of sysconfig.Service the UI uses. Optional: the
// system settings page exists only when it is set.
type SysConfigService interface {
	State() []sysconfig.FieldState
	ReadOnly() []sysconfig.Row
	Skipped() map[string]string
	Apply(ctx context.Context, req sysconfig.Request) (sysconfig.Result, error)
	TestMQTT(ctx context.Context, req sysconfig.TestRequest) (string, error)
	TestSSO(ctx context.Context, req sysconfig.TestRequest) (string, error)
	Reset(ctx context.Context, req sysconfig.ResetRequest) (sysconfig.Result, error)
}

const sysPath = "/settings/system"

func (h *handler) registerSysConfig(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.SysConfig == nil {
		return
	}
	route("GET "+sysPath, adm, h.sysPage)
	route("POST "+sysPath, adm, h.sysSave)
	route("POST "+sysPath+"/stepup", adm, h.sysStepUpPost)
	route("POST "+sysPath+"/mqtt-test", adm, h.sysMQTTTest)
	route("POST "+sysPath+"/sso-test", adm, h.sysSSOTest)
	route("GET "+sysPath+"/reset", adm, h.sysResetConfirm)
	route("POST "+sysPath+"/reset", adm, h.sysReset)
}

type sysRow struct {
	sysconfig.FieldState
	Posted string // what the admin typed, shown again after an error
	Err    string
}

// ErrField is the input name of a row with an error (the field-error pattern
// compares it with the input's own name).
func (r sysRow) ErrField() string {
	if r.Err == "" {
		return ""
	}
	return r.InputName()
}

func (r sysRow) InputName() string { return "v." + r.Key }

// sysSection is a part of a tab: an optional heading and line, then its fields in the grid.
type sysSection struct {
	Heading, Intro string
	Rows           []sysRow
}

// sysTab is one tab of the page (design v2 screen "settings", sysTab). Every field has exactly
// one place (sysPlace); a tab without fields is not shown, Advanced always is (it lists the
// settings of the container).
type sysTab struct {
	Label, Slug string
	Sections    []sysSection
}

// sysTabDef is a tab of the prototype with its sections; a section is filled by sysPlace.
type sysTabDef struct {
	Slug, Label string
	Sections    []sysSection
}

// sysLayout is the prototype's tab order (SYS2) with the sections each tab draws.
var sysLayout = []sysTabDef{
	{"general", "General", []sysSection{{}}},
	{"network", "Network", []sysSection{{}, {Heading: "Health checks"}}},
	{"mqtt", "MQTT", []sysSection{{}}},
	{"discovery", "Discovery", []sysSection{{}, {Heading: "Network scan"}}},
	{"devices", "Devices", []sysSection{{Heading: "Default device credentials", Intro: "Prefilled when you add a device or accept a discovered one. You can still change them per device."}}},
	{"backups", "Backups", []sysSection{{Heading: "Schedule and retention"}}},
	{"fwsource", "Firmware source", []sysSection{{}}},
	{"metrics", "Metrics", []sysSection{{}}},
	{"signin", "Sign-in", []sysSection{{Heading: "How people sign in", Intro: "One method is active. The local admin account always works as a fallback."}, {}}},
	{"advanced", "Advanced", []sysSection{{Heading: "Updates (OTA)"}, {Heading: "Other"}}},
}

// sysKeyPlace puts single fields where the prototype has them; sysGroupPlace places the rest
// by their group. A field in neither lands in Advanced, "Other".
var sysKeyPlace = map[string][2]int{
	"DEVICE_DEFAULT_PASSWORD": {4, 0},
	"DISCOVERY_SCAN":          {3, 1},
	"DISCOVERY_SCAN_INTERVAL": {3, 1},
	"DISCOVERY_SCAN_CIDRS":    {3, 1},
	"AUTH_MODE":               {8, 0},
}

var sysGroupPlace = map[sysconfig.Group][2]int{
	sysconfig.GroupLogging:   {0, 0},
	sysconfig.GroupAudit:     {0, 0},
	sysconfig.GroupNetwork:   {1, 0},
	sysconfig.GroupHealth:    {1, 1},
	sysconfig.GroupMQTT:      {2, 0},
	sysconfig.GroupDiscovery: {3, 0},
	sysconfig.GroupBackup:    {5, 0},
	sysconfig.GroupFirmware:  {6, 0},
	sysconfig.GroupMetrics:   {7, 0},
	sysconfig.GroupSignIn:    {8, 1},
	sysconfig.GroupOTA:       {9, 0},
}

// sysPlace is the tab and section of a field.
func sysPlace(st sysconfig.FieldState) (tab, section int) {
	if p, ok := sysKeyPlace[st.Key]; ok {
		return p[0], p[1]
	}
	if p, ok := sysGroupPlace[st.Group]; ok {
		return p[0], p[1]
	}
	return 9, 1
}

// sysMQTT is the broker of the MQTT tab as host, port and TLS (N18). Legacy is set when the
// stored URL cannot be written as these parts; the page then keeps the URL field.
type sysMQTT struct {
	Host, Port string
	TLS        bool
	Legacy     string // why the parts cannot hold the stored URL; empty when they can
}

// sysDiscovery is the status card of the Discovery tab: what MQTT discovery does right now.
type sysDiscovery struct {
	Tone        string // ok, warn, danger, neutral
	Title, Text string
	Waiting     int  // new devices waiting to be added
	Link        bool // the Discovered page exists
}

type sysPage struct {
	Base, CSRF   string
	Tabs         []sysTab
	ReadOnly     []sysconfig.Row
	Skipped      map[string]string
	Pending      []string // keys that wait for a restart
	Error        string
	NeedStepUp   bool
	StepUp       ui.StepUpData
	CannotStepUp bool
	StepUpKeys   []string
	MQTTResult   string
	SSOResult    string
	Active       string // slug of the tab shown
	StepUpErr    string // why the last confirmation failed (wrong password, too many attempts)
	MQTT         sysMQTT
	Discovery    sysDiscovery
	Storage      *sysStorage // Backups tab: storage summary and orphans (N9); nil without them
	Resetting    bool        // the step-up dialog is for "Reset settings…"
}

// sysStepUp is what the principal can do about a confirmation. Header
// principals can never confirm: the proxy is their only authority and
// tasmofleet has nothing to re-check (docs/security/web-import.md).
func (h *handler) sysStepUp(r *http.Request) sysconfig.StepUp {
	if p, _ := auth.FromContext(r.Context()); p.Method == "header" {
		return sysconfig.StepUp{}
	}
	return sysconfig.StepUp{Possible: true, Done: !h.needsStepUp(r)}
}

func (h *handler) sysData(r *http.Request, posted map[string]string, errs map[string]string) sysPage {
	svc := h.d.SysConfig
	p := sysPage{Base: httpBase(r), CSRF: h.d.Guard.CSRFToken(r), ReadOnly: svc.ReadOnly(), Skipped: svc.Skipped()}
	tabs := make([]sysTab, len(sysLayout))
	for i, def := range sysLayout {
		tabs[i] = sysTab{Label: def.Label, Slug: def.Slug, Sections: slices.Clone(def.Sections)}
	}
	discoveryOn := false
	for _, st := range svc.State() {
		row := sysRow{FieldState: st, Posted: st.Value, Err: errs[st.Key]}
		if v, ok := posted[st.Key]; ok && !st.Secret {
			row.Posted = v
		}
		t, s := sysPlace(st)
		tabs[t].Sections[s].Rows = append(tabs[t].Sections[s].Rows, row)
		if st.PendingRestart {
			p.Pending = append(p.Pending, st.Key)
		}
		switch st.Key {
		case "MQTT_URL":
			p.MQTT = sysMQTTOf(row.Posted)
		case "DISCOVERY_MQTT":
			discoveryOn = row.Posted == "true"
		}
	}
	// The prototype's General tab starts with the log level; the audit retention follows.
	slices.SortStableFunc(tabs[0].Sections[0].Rows, func(a, b sysRow) int {
		return sysFirst(a) - sysFirst(b)
	})
	p.Discovery = h.sysDiscoveryOf(r, discoveryOn)
	for _, tab := range tabs {
		n := 0
		for _, s := range tab.Sections {
			n += len(s.Rows)
		}
		if n > 0 || tab.Slug == "advanced" {
			p.Tabs = append(p.Tabs, tab)
		}
	}
	p.Active = r.URL.Query().Get("tab")
	for _, tab := range p.Tabs {
		for _, s := range tab.Sections {
			for _, row := range s.Rows {
				if row.Err != "" || row.NeedsReentry {
					p.Active = tab.Slug // an error is shown where it happened
				}
			}
		}
	}
	if !slices.ContainsFunc(p.Tabs, func(t sysTab) bool { return t.Slug == p.Active }) && len(p.Tabs) > 0 {
		p.Active = p.Tabs[0].Slug
	}
	return p
}

// sysMQTTOf splits the broker URL into the parts the MQTT tab edits.
func sysMQTTOf(raw string) sysMQTT {
	b, err := config.SplitMQTTURL(raw)
	if err != nil {
		return sysMQTT{Legacy: err.Error()}
	}
	m := sysMQTT{Host: b.Host, TLS: b.TLS}
	if b.Port != 0 {
		m.Port = strconv.Itoa(b.Port)
	}
	return m
}

// sysDiscoveryOf words the state of MQTT discovery for the card on top of the Discovery tab:
// switched off, waiting for a broker, or listening (with the new devices that wait).
func (h *handler) sysDiscoveryOf(r *http.Request, on bool) sysDiscovery {
	d := sysDiscovery{Link: h.d.Discovery != nil}
	state := ""
	if h.d.MQTT != nil {
		state = h.d.MQTT()
	}
	switch {
	case !on:
		d.Tone, d.Title, d.Text = "neutral", "MQTT discovery is off", "Announcements on tasmota/discovery/+/config are ignored. Switch them on below."
	case state == "connected":
		d.Tone, d.Title, d.Text = "ok", "MQTT discovery is listening", "Topic tasmota/discovery/+/config"
		if d.Link {
			d.Waiting = h.newDeviceCount(r)
		}
	case state == "disconnected":
		d.Tone, d.Title, d.Text = "danger", "MQTT discovery is waiting for the broker", "The broker is not connected; announcements arrive once it is."
	default:
		d.Tone, d.Title, d.Text = "warn", "MQTT discovery needs a broker", "Set the broker on the MQTT tab; until then devices are only found by a network scan."
	}
	return d
}

// sysStepUpData is the "Confirm it's you" dialog for a refused save: a local account types its
// password, a single sign-on account signs in at the identity provider again. Both come back to
// the tab with ?restore=1, where form.js puts the typed values back.
func (h *handler) sysStepUpData(r *http.Request, p sysPage) ui.StepUpData {
	pr, _ := auth.FromContext(r.Context())
	name := pr.Name
	if name == "" {
		name = h.pr.Text(pr.Subject)
	}
	sub := "Signed in with a local account"
	if pr.Method == "oidc" {
		sub = "Signed in with " + h.idpName()
	}
	var labels []string
	for _, k := range p.StepUpKeys {
		if f, ok := sysconfig.Lookup(k); ok {
			labels = append(labels, f.Label)
		}
	}
	what := "Changing the sign-in settings" // a failed password confirmation no longer knows the keys
	switch {
	case p.Resetting:
		what = "Resetting all settings"
	case len(labels) > 0:
		what = "Changing " + strings.Join(labels, ", ")
	}
	back := sysPath + "?tab=" + p.Active + "&restore=1"
	d := ui.StepUpData{ID: "stepup", Name: name, Sub: sub, What: what,
		CSRF: p.CSRF, Next: back, Open: true, Error: p.StepUpErr}
	if h.stepUpKind(r) == stepUpOIDC {
		d.IdP, d.IdPHref, d.Action = h.idpName(), p.Base+"/oidc/stepup", p.Base+"/oidc/stepup"
	} else {
		d.Local, d.Action = true, p.Base+sysPath+"/stepup?tab="+p.Active
	}
	return d
}

// idpName is the identity provider as the sign-in button names it (OIDC_DISPLAY_NAME).
func (h *handler) idpName() string {
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "OIDC_DISPLAY_NAME" && st.Value != "" {
			return st.Value
		}
	}
	return "single sign-on"
}

func (h *handler) sysRender(w http.ResponseWriter, r *http.Request, status int, p sysPage) {
	if p.NeedStepUp {
		p.StepUp = h.sysStepUpData(r, p)
	}
	v := h.view(r, "Settings", "system", nil)
	switch q := r.URL.Query(); {
	case q.Get("ok") == "reset":
		v.Flash = "All settings are back at their defaults."
	case p.Active == "backups" && backupsNotice(r) != "":
		v.Flash = backupsNotice(r) // a backup of a deleted device was kept, released or deleted
	}
	if p.Active == "backups" && p.Storage == nil {
		st, err := h.sysStorageOf(r, p.Base)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.Storage = st
	}
	h.show(w, r, status, "system_settings", v, sysSettingsPage(p))
}

func (h *handler) sysPage(w http.ResponseWriter, r *http.Request) {
	h.sysRender(w, r, http.StatusOK, h.sysData(r, nil, nil))
}

// sysResetText is what "Reset settings…" does, in the dialog and on its page.
const sysResetText = "Every setting on this page goes back to its default: the device networks, the MQTT broker, " +
	"the sign-in method and everything else that was saved here. Settings set by the container configuration stay. " +
	"Devices, backups and users are kept."

// sysResetConfirm is the confirmation of "Reset settings…" as a page (no JavaScript).
func (h *handler) sysResetConfirm(w http.ResponseWriter, r *http.Request) {
	h.renderActionConfirm(w, r, "system", actionConfirm{
		Title: "Reset all settings?", Heading: "Reset all settings", Text: sysResetText,
		Crumbs: []confirmLink{{"/settings/system", "Settings"}, {sysPath + "?tab=advanced", "Advanced"}}, Last: "Reset",
		Action: sysPath + "/reset", Verb: "Reset settings", CancelHref: sysPath + "?tab=advanced", CancelLabel: "Cancel",
	})
}

// sysReset puts every stored setting back to its default (N9). It always needs the step-up:
// without it the Advanced tab opens the "Confirm it's you" dialog and nothing changes.
func (h *handler) sysReset(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err := h.d.SysConfig.Reset(r.Context(), sysconfig.ResetRequest{Actor: actor(r), StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	var need *sysconfig.StepUpRequiredError
	switch {
	case err == nil:
		redirect(w, r, sysPath+"?tab=advanced&ok=reset")
	case errors.As(err, &need):
		p := h.sysData(r, nil, nil)
		p.Active = "advanced"
		if need.Possible {
			p.NeedStepUp, p.Resetting = true, true
		} else {
			p.Error = "Nothing was reset. Resetting all settings needs a confirmation that this account cannot give."
		}
		h.sysRender(w, r, http.StatusForbidden, p)
	default:
		h.sysOutcome(w, r, nil, err, nil)
	}
}

// sysForm reads the posted form. Only keys the page offers are accepted: v.KEY
// for an editable field, clear.KEY to go back to the default, b.KEY for an on/off
// field drawn as a checkbox, and csrf. Any other key fails the whole request (no
// mass assignment). A checkbox sends nothing when it is off, so b.KEY says the
// field was on the page: without v.KEY next to it the field is "false".
func (h *handler) sysForm(w http.ResponseWriter, r *http.Request, allowClear bool, extras ...string) (values map[string]string, clear []string, extra map[string]string, ok bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, nil, false
	}
	values, extra = map[string]string{}, map[string]string{}
	var boxes []string
	for k, vs := range r.PostForm {
		switch {
		case k == "csrf":
		case strings.HasPrefix(k, "b."):
			f, known := sysconfig.Lookup(strings.TrimPrefix(k, "b."))
			if !known || !f.Bool || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			boxes = append(boxes, f.Key)
		case slices.Contains(extras, k) && len(vs) == 1:
			extra[k] = vs[0]
		case strings.HasPrefix(k, "v."):
			f, known := sysconfig.Lookup(strings.TrimPrefix(k, "v."))
			if !known || len(vs) != 1 {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			v := strings.TrimSpace(vs[0])
			if f.Multiline {
				v = strings.Join(strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' || c == ',' || c == ' ' }), ",")
			}
			if f.Secret && v == "" {
				continue // write-only: empty means unchanged
			}
			values[f.Key] = v
		case allowClear && strings.HasPrefix(k, "reuse."):
			if _, known := sysconfig.Lookup(strings.TrimPrefix(k, "reuse.")); !known || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			extra[k] = "1"
		case allowClear && strings.HasPrefix(k, "clear."):
			if _, known := sysconfig.Lookup(strings.TrimPrefix(k, "clear.")); !known || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			clear = append(clear, strings.TrimPrefix(k, "clear."))
		default:
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return nil, nil, nil, false
		}
	}
	for _, k := range boxes {
		if _, on := values[k]; !on && !slices.Contains(clear, k) {
			values[k] = "false"
		}
	}
	for _, k := range clear {
		if _, both := values[k]; both {
			http.Error(w, "a setting cannot be set and cleared at once", http.StatusBadRequest)
			return nil, nil, nil, false
		}
	}
	return values, clear, extra, true
}

func (h *handler) sysSave(w http.ResponseWriter, r *http.Request) {
	values, clear, extra, ok := h.sysForm(w, r, true, mqttPartKeys...)
	if !ok {
		return
	}
	var reuse []string
	for k := range extra {
		if strings.HasPrefix(k, "reuse.") {
			reuse = append(reuse, strings.TrimPrefix(k, "reuse."))
		}
	}
	if err := composeMQTTURL(values, extra, h.storedMQTTURL()); err != nil {
		h.sysOutcome(w, r, values, err, nil)
		return
	}
	_, err := h.d.SysConfig.Apply(r.Context(), sysconfig.Request{
		Actor: actor(r), Values: values, Clear: clear, Reuse: reuse, StepUp: h.sysStepUp(r), Audit: h.d.Recorder,
	})
	// Back to the tab that was saved, so the values just written are what the person sees.
	back := sysPath + "?ok=saved"
	if tab := sysTabSlug(r.URL.Query().Get("tab")); tab != "" {
		back = sysPath + "?tab=" + tab + "&ok=saved"
	}
	h.sysOutcome(w, r, values, err, func() { redirect(w, r, back) })
}

// sysOutcome renders the answer to Apply or TestMQTT errors, or calls done.
func (h *handler) sysOutcome(w http.ResponseWriter, r *http.Request, posted map[string]string, err error, done func()) {
	var ve *sysconfig.ValidationError
	var need *sysconfig.StepUpRequiredError
	switch {
	case err == nil:
		done()
	case errors.As(err, &ve):
		p := h.sysData(r, posted, ve.Fields)
		p.Error = "Nothing was saved. Fix the marked fields."
		// A message about a setting this page does not edit (for instance PUBLIC_URL
		// for single sign-on) has no field to sit at: say it in the summary.
		for _, k := range sortedKeys(ve.Fields) {
			if _, editable := sysconfig.Lookup(k); !editable {
				p.Error += " " + k + ": " + ve.Fields[k] + " (set it in the container configuration)."
			}
		}
		h.sysRender(w, r, http.StatusUnprocessableEntity, p)
	case errors.Is(err, sysconfig.ErrLocked):
		p := h.sysData(r, nil, nil)
		p.Error = "Nothing was saved. A setting in the request is set by the environment and cannot be changed here."
		h.sysRender(w, r, http.StatusForbidden, p)
	case errors.As(err, &need):
		p := h.sysData(r, posted, nil)
		p.StepUpKeys = need.Keys
		if need.Possible {
			p.NeedStepUp = true // the dialog says what happens; nothing was saved
		} else {
			p.CannotStepUp = true
			p.Error = "Nothing was saved. These settings need a confirmation that this account cannot give; change them in the container configuration instead."
		}
		h.sysRender(w, r, http.StatusForbidden, p)
	case errors.Is(err, sysconfig.ErrUnknownKey):
		http.Error(w, "unknown setting", http.StatusBadRequest)
	case errors.Is(err, sysconfig.ErrTooSoon):
		p := h.sysData(r, posted, nil)
		p.Error = "Wait a few seconds between connection tests."
		h.sysRender(w, r, http.StatusTooManyRequests, p)
	default:
		h.fail(w, r, err)
	}
}

// sysMQTTTest checks the broker. The "Test MQTT" button sits in the MQTT tab of the settings
// form, so the whole form arrives (every key is still validated by sysForm); only the broker
// fields count, and nothing is saved. A password typed into the form is the one tested,
// otherwise the setup page's test_password, otherwise the stored one (bound to its broker).
func (h *handler) sysMQTTTest(w http.ResponseWriter, r *http.Request) {
	all, _, extra, ok := h.sysForm(w, r, true, append([]string{"test_password"}, mqttPartKeys...)...)
	if !ok {
		return
	}
	if err := composeMQTTURL(all, extra, h.storedMQTTURL()); err != nil {
		h.sysOutcome(w, r, all, err, nil)
		return
	}
	values := map[string]string{}
	for _, k := range []string{"MQTT_URL", "MQTT_USER", "MQTT_FULLTOPIC"} {
		if v, ok := all[k]; ok {
			values[k] = v
		}
	}
	password := extra["test_password"]
	if password == "" {
		password = all["MQTT_PASSWORD"]
	}
	msg, err := h.d.SysConfig.TestMQTT(r.Context(), sysconfig.TestRequest{Actor: actor(r), Values: values, Password: password, StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	if errors.Is(err, sysconfig.ErrNoTester) {
		http.NotFound(w, r)
		return
	}
	h.sysOutcome(w, r, values, err, func() {
		p := h.sysData(r, values, nil)
		p.MQTTResult = msg
		h.sysRender(w, r, http.StatusOK, p)
	})
}

// sysStepUpPost takes the password of a local admin (OIDC admins use
// POST /oidc/stepup, header principals cannot confirm).
func (h *handler) sysStepUpPost(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	page := func(status int, msg string) {
		p := h.sysData(r, nil, nil)
		p.NeedStepUp, p.StepUpErr = true, msg
		h.sysRender(w, r, status, p)
	}
	p, _ := auth.FromContext(r.Context())
	su, ok := h.d.Guard.(stepUpper)
	if p.Method != "local" || !ok {
		h.auditImport(r, "sysconfig.stepup", "denied: password step-up needs a local account ("+p.Method+")")
		page(http.StatusForbidden, "A password confirmation is only available for local accounts. Accounts from the identity provider confirm at the identity provider.")
		return
	}
	okPw, limited := su.StepUp(r, r.PostFormValue("current"))
	switch {
	case limited:
		page(http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
	case !okPw:
		page(http.StatusForbidden, "The password is wrong.")
	default:
		back := sysPath + "?restore=1" // form.js puts the typed values back
		if tab := sysTabSlug(r.URL.Query().Get("tab")); tab != "" {
			back = sysPath + "?tab=" + tab + "&restore=1"
		}
		redirect(w, r, back)
	}
}

// sysTabSlug accepts a tab name from a URL only when it is one of the page's tabs.
func sysTabSlug(s string) string {
	for _, t := range sysLayout {
		if t.Slug == s {
			return s
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sysSSOTest checks that the issuer answers discovery. Like the MQTT test it is a button of
// the settings form: the whole form arrives, only the issuer counts, nothing is saved.
func (h *handler) sysSSOTest(w http.ResponseWriter, r *http.Request) {
	all, _, _, ok := h.sysForm(w, r, true)
	if !ok {
		return
	}
	values := map[string]string{}
	if v, ok := all["OIDC_ISSUER"]; ok {
		values["OIDC_ISSUER"] = v
	}
	msg, err := h.d.SysConfig.TestSSO(r.Context(), sysconfig.TestRequest{Actor: actor(r), Values: values, Audit: h.d.Recorder})
	if errors.Is(err, sysconfig.ErrNoTester) {
		http.NotFound(w, r)
		return
	}
	h.sysOutcome(w, r, values, err, func() {
		p := h.sysData(r, values, nil)
		p.SSOResult = msg
		h.sysRender(w, r, http.StatusOK, p)
	})
}

// mqttPartKeys are the broker fields of the MQTT tab (N18): host, port and a
// TLS checkbox instead of one URL field. They are not settings themselves:
// composeMQTTURL turns them into MQTT_URL, the stored value.
var mqttPartKeys = []string{"mqtt.host", "mqtt.port", "mqtt.tls"}

// composeMQTTURL replaces the posted broker parts with the MQTT_URL they
// compose (config.JoinMQTTURL). A form without mqtt.host is left alone (the
// plain v.MQTT_URL field still works); a form with both is refused. An empty
// host turns MQTT off. Errors are field errors of MQTT_URL. When the parts
// name the broker stored already (current), current is kept as it is: a
// stored mqtt:// or tls:// URL, or one without a port, would otherwise be
// rewritten on every save, which reconnects MQTT and, because the scheme is
// part of the MQTT password's target, asks for that password again.
func composeMQTTURL(values, extra map[string]string, current string) error {
	host, ok := extra["mqtt.host"]
	if !ok {
		if _, port := extra["mqtt.port"]; port || extra["mqtt.tls"] != "" {
			return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "enter the broker host"}}
		}
		return nil
	}
	if _, both := values["MQTT_URL"]; both {
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "send the broker either as a URL or as host, port and TLS"}}
	}
	b := config.Broker{Host: host}
	switch extra["mqtt.tls"] {
	case "":
	case "1", "on", "true":
		b.TLS = true
	default:
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "TLS must be on or off"}}
	}
	if p := strings.TrimSpace(extra["mqtt.port"]); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "port must be 1-65535"}}
		}
		b.Port = n
	}
	u, err := config.JoinMQTTURL(b)
	if err != nil {
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": err.Error()}}
	}
	if cur, err := config.SplitMQTTURL(current); err == nil && current != "" {
		if now, _ := config.SplitMQTTURL(u); now == cur {
			u = current
		}
	}
	values["MQTT_URL"] = u
	for _, k := range mqttPartKeys {
		delete(extra, k)
	}
	return nil
}

// storedMQTTURL is the current MQTT_URL from the settings service.
func (h *handler) storedMQTTURL() string {
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "MQTT_URL" {
			return f.Value
		}
	}
	return ""
}

// sysTabLinks are the tabs of the page as links (?tab=), the shown one current.
func sysTabLinks(p sysPage) []ui.Tab {
	out := make([]ui.Tab, 0, len(p.Tabs))
	for _, t := range p.Tabs {
		out = append(out, ui.Tab{Label: t.Label, Href: p.Base + sysPath + "?tab=" + t.Slug, Current: t.Slug == p.Active})
	}
	return out
}

// sysLevels are the log levels in the prototype's order and wording (Error, Warn, Info, Debug).
var sysLevels = []string{"error", "warn", "info", "debug"}

func sysLevelOptions(cur string) []ui.Option {
	out := make([]ui.Option, 0, len(sysLevels))
	for _, l := range sysLevels {
		out = append(out, ui.Option{Value: l, Label: strings.ToUpper(l[:1]) + l[1:], Checked: l == cur})
	}
	return out
}

// sysAuthOptions are the sign-in methods as the prototype's radio cards.
func sysAuthOptions(cur string) []ui.Option {
	return []ui.Option{
		{Value: "local", Label: "Local accounts", Text: "Usernames and passwords stored in TasmoFleet.", Checked: cur == "local"},
		{Value: "oidc", Label: "Single sign-on (OpenID Connect)", Text: "Authentik, Keycloak, Authelia and similar. Accounts are created on first sign-in.", Checked: cur == "oidc"},
	}
}

// sysInvalid marks the input of a field the server rejected and points to its message.
func sysInvalid(row sysRow) templ.Attributes {
	if row.Err == "" {
		return templ.Attributes{}
	}
	return templ.Attributes{"aria-invalid": "true", "aria-describedby": "err-" + row.InputName()}
}

// sysSecretHint is the placeholder of a write-only field: whether a value is stored.
func sysSecretHint(row sysRow) string {
	if row.HasValue {
		return "unchanged (a value is stored)"
	}
	return "not set"
}

func sysPlaceholder(row sysRow) string {
	if row.Key == "DEVICE_NETWORKS" {
		return "172.31.13.0/24"
	}
	return ""
}

// sysSetWord says whether a secret from the environment has a value, never the value.
func sysSetWord(set bool) string {
	if set {
		return "a value is set"
	}
	return "not set"
}

// sysFirst orders the General tab: the log level before the audit retention.
func sysFirst(r sysRow) int {
	if r.Key == "LOG_LEVEL" {
		return 0
	}
	return 1
}
