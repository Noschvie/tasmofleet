package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// formBody is the HTML of the settings form (between its opening tag and its closing tag).
func formBody(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<form id="sysform"`)
	j := strings.Index(page[max(i, 0):], "</form>")
	if i < 0 || j < 0 {
		t.Fatal("no settings form on the page")
	}
	return page[i : i+j]
}

// fieldHTML is the markup of one setting's wrapper (up to the next wrapper), so a test can ask
// what sits inside it.
func fieldHTML(t *testing.T, page, key string) string {
	t.Helper()
	marker := `data-key="` + key + `"`
	i := strings.Index(page, marker)
	if i < 0 {
		t.Fatalf("no field wrapper for %s", key)
	}
	rest := page[i+len(marker):]
	if j := strings.Index(rest, `data-key="`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// Why: the maintainer approved eight tabs (10 -> 8): Metrics joins General, Devices joins Network,
// Updates (OTA) joins Firmware. A field that no tab names must still land somewhere, never vanish.
func TestSystemSettingsHasTheEightApprovedTabs(t *testing.T) {
	want := []string{"general", "network", "mqtt", "scan", "backups", "firmware", "signin", "advanced"}
	if len(sysLayout) != len(want) {
		t.Fatalf("%d tabs, want %d", len(sysLayout), len(want))
	}
	for i, d := range sysLayout {
		if d.Slug != want[i] {
			t.Errorf("tab %d is %s, want %s", i, d.Slug, want[i])
		}
	}
	if tab, _ := sysPlace("SOMETHING_NEW"); sysLayout[tab].Slug != "advanced" {
		t.Error("a field without a place must land on Advanced, not vanish")
	}
	// The old tab names are gone and not redirected (no compatibility shims before 1.0):
	// an unknown tab shows the first one.
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "GET", "/settings/system?tab=fwsource", "admin", nil)
	if !strings.Contains(body, `<a class="utab" href="/tf/settings/system?tab=general" aria-current="page">General</a>`) {
		t.Error("an old tab name must open the first tab")
	}
}

// Why: the staggered grid came from "Reset ... on save" being its own grid cell next to the field.
// The reset control now belongs to the field that owns it, so no field can push another around.
func TestSystemSettingsResetControlIsInsideItsOwnField(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	// a stored BACKUP_KEEP (the fixture stores MQTT_URL and MQTT_PASSWORD) must offer a reset too
	e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}})
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	for _, key := range []string{"BACKUP_KEEP", "MQTT_URL", "MQTT_PASSWORD"} {
		in := fieldHTML(t, page, key)
		if !strings.Contains(in, `name="clear.`+key+`"`) {
			t.Errorf("%s is stored, so its own wrapper must hold the reset control", key)
		}
	}
	if strings.Contains(page, `class="ucheck sys-reset"`) {
		t.Error("a reset checkbox is still a grid cell of its own")
	}
	if strings.Contains(page, "to the default on save") {
		t.Error("the old two-line reset wording is still there")
	}
}

// Why: help text was most of every tab. A field's help is one short line; anything longer is
// behind the info button. "Applies after a restart." nine times is replaced by one badge.
func TestSystemSettingsHelpIsOneShortLine(t *testing.T) {
	for _, st := range sysconfig.Fields() {
		ui := sysFieldCopy(st.Key)
		if len(ui.Help) > 72 {
			t.Errorf("%s: help is %d characters (%q), at most 72; move the rest to Info", st.Key, len(ui.Help), ui.Help)
		}
		if strings.Count(ui.Help, ". ") > 2 {
			t.Errorf("%s: help has several sentences: %q", st.Key, ui.Help)
		}
	}
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	if strings.Contains(page, "Applies after a restart.") {
		t.Error("the per-field restart line is back")
	}
}

// Why: touch and keyboard users cannot hover. The longer text is a native popover opened by a
// button with a name, so it works on a phone and a screen reader finds it.
func TestSystemSettingsInfoPopoversAreAccessible(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	targets := regexp.MustCompile(`popovertarget="([^"]+)"`).FindAllStringSubmatch(page, -1)
	if len(targets) < 8 {
		t.Fatalf("only %d info buttons", len(targets))
	}
	seen := map[string]bool{}
	for _, m := range targets {
		id := m[1]
		if seen[id] {
			t.Errorf("duplicate popover target %s", id)
		}
		seen[id] = true
		if n := strings.Count(page, `id="`+id+`" popover`); n != 1 {
			t.Errorf("popover %s exists %d times", id, n)
		}
	}
	if !strings.Contains(page, `<button type="button" class="uinfo" popovertarget="info-DEVICE_NETWORKS" aria-expanded="false" aria-controls="info-DEVICE_NETWORKS" aria-label="About Device networks">i</button>`) {
		t.Error("the info button lacks its name, state or target")
	}
	if !strings.Contains(page, `role="note"`) {
		t.Error("the popover text is not a note")
	}
}

// Why: a setting fixed by the container configuration used to look like an editable input.
// It now shows the Environment badge and the key, and has no input at all.
func TestSystemSettingsLockedFieldIsReadOnlyWithABadge(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, nil, "TASMOFLEET_METRICS_TOKEN=fake-env-token-6")
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	in := fieldHTML(t, page, "METRICS_TOKEN")
	if strings.Contains(in, "<input") || strings.Contains(in, "<textarea") {
		t.Errorf("a locked field has a control: %.300s", in)
	}
	for _, want := range []string{"Environment", "METRICS_TOKEN", "container configuration"} {
		if !strings.Contains(in, want) {
			t.Errorf("locked field lacks %q: %.400s", want, in)
		}
	}
	if strings.Contains(page, "fake-env-token-6") {
		t.Error("the environment token leaked into the page")
	}
}

// Why: boxes next to switches were inconsistent. Every on/off setting is a switch, and each still
// carries the b.KEY marker so that "off" is saved.
func TestSystemSettingsOnOffSettingsAreSwitches(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	n := 0
	for _, f := range sysconfig.Fields() {
		if !f.Bool {
			continue
		}
		n++
		in := fieldHTML(t, page, f.Key)
		if !strings.Contains(in, `role="switch"`) || !strings.Contains(in, `name="v.`+f.Key+`"`) || !strings.Contains(in, `name="b.`+f.Key+`"`) {
			t.Errorf("%s is not a switch with its marker: %.300s", f.Key, in)
		}
		if strings.Contains(in, `class="ucheck`) {
			t.Errorf("%s is still a checkbox", f.Key)
		}
	}
	if n == 0 {
		t.Fatal("no on/off settings found")
	}
}

// Why: scan interval and networks do nothing while the scan is off. They stay in the form (and are
// posted, so nothing is lost) but are marked as depending on the switch, which the page dims.
func TestSystemSettingsDependentFieldsFollowTheirSwitch(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	dep := regexp.MustCompile(`data-depends="v\.DISCOVERY_SCAN"`).FindStringIndex(page)
	if dep == nil {
		t.Fatal("no dependent group for the scheduled scan")
	}
	rest := page[dep[1]:]
	for _, name := range []string{`name="v.DISCOVERY_SCAN_INTERVAL"`, `name="v.DISCOVERY_SCAN_CIDRS"`} {
		if !strings.Contains(rest, name) {
			t.Errorf("%s is not inside the dependent group", name)
		}
	}
	if !strings.Contains(page, `data-depends="v.AUTH_MODE=oidc"`) {
		t.Error("the identity provider cards must depend on the sign-in mode")
	}
	if !strings.Contains(page, `data-depends="v.METRICS_ENABLED"`) {
		t.Error("the scrape token must depend on the metrics switch")
	}
}

// Why: the danger zone sat inside the form directly above Save, and the container list is
// read-only data. Both belong outside the form, on the Advanced tab.
func TestSystemSettingsDangerZoneAndContainerListAreOutsideTheForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
	form := formBody(t, page)
	for _, s := range []string{"Reset all settings", "Reset settings…", "Set by the container"} {
		if strings.Contains(form, s) {
			t.Errorf("%q is inside the settings form", s)
		}
		if !strings.Contains(page, s) {
			t.Errorf("%q is missing from the Advanced tab", s)
		}
	}
	// and only on Advanced: another tab keeps it hidden
	_, page = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	i := strings.Index(page, `id="after-advanced"`)
	if i < 0 || !strings.Contains(page[i-80:i+80], "hidden") {
		t.Error("the Advanced extras must be hidden while another tab is shown")
	}
}

// Why: one page-wide Save, in a bar that lists the tabs with unsaved changes (script) and works
// without script: Discard and Save sit in the form, once.
func TestSystemSettingsHasOneSaveBarInsideTheForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system", "admin", nil)
	form := formBody(t, page)
	if strings.Count(form, `data-sys-bar`) != 1 || strings.Count(form, ">Save settings</button>") != 1 {
		t.Errorf("want one save bar with one Save button in the form")
	}
	if !strings.Contains(form, `>Discard</a>`) || !strings.Contains(form, "No unsaved changes") {
		t.Error("the bar lacks Discard or its idle text")
	}
}

// Why: on a phone eight tabs scroll sideways. A section picker (a select) replaces the strip, and
// lists the same eight sections.
func TestSystemSettingsOffersASectionPickerForPhones(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	i := strings.Index(page, `data-sys-picker`)
	if i < 0 {
		t.Fatal("no section picker")
	}
	sel := page[i:]
	sel = sel[:strings.Index(sel, "</select>")]
	if strings.Count(sel, "<option") != 8 || !strings.Contains(sel, `value="mqtt" selected`) {
		t.Errorf("the picker must list all eight sections with the current one selected: %s", sel)
	}
}

// Why: "0s" for "manual only" was cryptic. The interval is behind a switch: off stores 0 (manual
// only), on needs a real interval. The setting itself is unchanged (BACKUP_INTERVAL).
func TestSystemSettingsBackupScheduleSwitchIsAViewOfTheInterval(t *testing.T) {
	post := func(t *testing.T, form url.Values) (*env, *sysconfig.Service, int, string) {
		t.Helper()
		e, svc, _, _ := sysEnv(t, true)
		form.Set("csrf", "tok")
		form.Set("swm.BACKUP_INTERVAL", "1")
		r, body := e.do(t, "POST", "/settings/system", "admin", form)
		return e, svc, r.StatusCode, body
	}
	_, svc, code, _ := post(t, url.Values{"v.BACKUP_INTERVAL": {"6h"}, "sw.BACKUP_INTERVAL": {"1"}})
	if code != 303 || svc.Snapshot().BackupSchedule.Hours() != 6 {
		t.Fatalf("on with 6h: %d, %v", code, svc.Snapshot().BackupSchedule)
	}
	_, svc, code, _ = post(t, url.Values{"v.BACKUP_INTERVAL": {"6h"}})
	if code != 303 || svc.Snapshot().BackupSchedule != 0 {
		t.Fatalf("switch off must store 0 (manual only): %d, %v", code, svc.Snapshot().BackupSchedule)
	}
	_, _, code, body := post(t, url.Values{"v.BACKUP_INTERVAL": {"0"}, "sw.BACKUP_INTERVAL": {"1"}})
	if code != 422 || !strings.Contains(body, "how often") {
		t.Fatalf("on with no interval is a field error: %d", code)
	}
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=backups", "admin", nil)
	if !strings.Contains(page, `name="sw.BACKUP_INTERVAL"`) || !strings.Contains(page, `data-depends="sw.BACKUP_INTERVAL"`) {
		t.Error("the schedule switch and its dependent interval are missing")
	}
}

// Why: "Upstream sources" was free text although the set is fixed. One checkbox per known source;
// a save with none ticked stores none instead of silently keeping the old value.
func TestSystemSettingsFirmwareSourcesAreCheckboxes(t *testing.T) {
	e, svc, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=firmware", "admin", nil)
	for _, src := range config.KnownFirmwareSources() {
		if !strings.Contains(page, `name="v.FIRMWARE_SOURCES" value="`+src+`"`) {
			t.Errorf("no checkbox for source %s", src)
		}
	}
	r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.FIRMWARE_SOURCES": {"1"}, "v.FIRMWARE_SOURCES": {config.KnownFirmwareSources()[0]}})
	if r.StatusCode != 303 {
		t.Fatalf("one source: %d", r.StatusCode)
	}
	if got := svc.Snapshot().FWSources; len(got) != 1 {
		t.Errorf("sources %v", got)
	}
	// Unticking every box is a real choice (no source) and must be saved, not ignored.
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.FIRMWARE_SOURCES": {"1"}}); r.StatusCode != 303 || len(svc.Snapshot().FWSources) != 0 {
		t.Errorf("no source ticked: %d, sources %v", r.StatusCode, svc.Snapshot().FWSources)
	}
	if r, _ := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "m.BACKUP_KEEP": {"1"}}); r.StatusCode != 400 {
		t.Errorf("a marker for a setting without choices: %d, want 400", r.StatusCode)
	}
}

// Why: the SSO test button posts the whole form, which now carries the broker parts and the
// schedule switch as well. Before, the form's own extra fields made the SSO test answer 400.
func TestSystemSettingsSSOTestAcceptsTheWholeForm(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system/sso-test", "admin", url.Values{"csrf": {"tok"}, "v.OIDC_ISSUER": {"https://idp.example/"},
		"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "swm.BACKUP_INTERVAL": {"1"}, "sw.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {"24h"}})
	if r.StatusCode != 200 || !strings.Contains(body, "discovery OK") {
		t.Fatalf("status %d", r.StatusCode)
	}
}

// Why: eight sign-in settings ask the admin to confirm, and nothing said so until Save. Cards
// whose settings need it carry the badge; the discovery test sits next to the issuer it tests.
func TestSystemSettingsSignInCardsSayWhatTheyAsk(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=signin", "admin", nil)
	if n := strings.Count(page, "Asks you to confirm"); n != 3 {
		t.Errorf("%d cards ask to confirm, want 3 (mode, identity provider, roles)", n)
	}
	issuer := strings.Index(page, `data-key="OIDC_ISSUER"`)
	test := strings.Index(page, "Test discovery")
	client := strings.Index(page, `data-key="OIDC_CLIENT_ID"`)
	if issuer >= test || test >= client {
		t.Errorf("Test discovery must sit right after the issuer (issuer %d, test %d, client %d)", issuer, test, client)
	}
}
