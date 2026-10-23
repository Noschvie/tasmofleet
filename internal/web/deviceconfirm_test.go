package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Why: a restart interrupts a device, so only admins may send it, it goes
// through the console service (policy, device lock, audit) as exactly
// "Restart 1", and nothing is sent by merely opening the confirmation page.
func TestRestartIsAdminOnlyAndSendsRestart1(t *testing.T) {
	f := &fakeCmd{reply: `{"Restart":"Restarting"}`}
	e := cmdEnv(t, f)
	if resp, _ := e.do(t, "POST", "/devices/1/restart", "operator", url.Values{}); resp.StatusCode != 403 {
		t.Fatalf("operator: status %d", resp.StatusCode)
	}
	if resp, body := e.do(t, "GET", "/devices/1/restart", "admin", nil); resp.StatusCode != 200 || !strings.Contains(body, "Restart 1") || !strings.Contains(body, "offline for a few seconds") {
		t.Fatalf("confirmation page must name the consequence: %d %.300s", resp.StatusCode, body)
	}
	if len(f.ran()) != 0 {
		t.Fatalf("the confirmation page sent %v", f.ran())
	}
	resp, _ := e.do(t, "POST", "/devices/1/restart", "admin", url.Values{"from": {"list"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/tf/devices?ok=restarted" {
		t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := f.ran(); len(got) != 1 || got[0] != "test:admin|1|Restart 1" {
		t.Fatalf("service saw %v", got)
	}
}

// Why: when the device is busy or down the admin must read why, not see a blank page.
func TestRestartFailureIsExplained(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{runErr: errUnreachable()})
	resp, body := e.do(t, "POST", "/devices/1/restart", "admin", url.Values{})
	if resp.StatusCode != 502 || !strings.Contains(body, "unreachable") {
		t.Fatalf("status %d: %.200s", resp.StatusCode, body)
	}
}

// Why (N2): deleting a device decides what happens to its backups, so the delete page shows
// them (what Keep leaves as orphans and Delete removes), offers both choices with Keep, the
// choice that loses nothing, preselected (the reference's default), and asks for the one
// acknowledgement. It is a plain form posting to the delete route with the CSRF token in the
// body, so it works without JavaScript; only admins may open it.
func TestDeletePageListsBackupsAndOffersBothChoices(t *testing.T) {
	e := setup(t)
	t1 := time.Date(2026, 10, 22, 9, 41, 0, 0, time.UTC)
	t0 := time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC)
	e.bk.list = []model.Backup{ // newest first, two runs of dump + snapshot
		{ID: 4, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t1, Version: "14.2.0", Size: 4198, Trigger: model.TriggerManual},
		{ID: 3, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t1, Size: 1000},
		{ID: 2, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: t0, Version: "14.2.0", Size: 4096, Kept: true, Trigger: model.TriggerScheduled},
		{ID: 1, DeviceID: 1, Kind: model.BackupSnapshot, TakenAt: t0, Size: 1000, Kept: true},
	}
	if resp, _ := e.do(t, "GET", "/devices/1/delete", "operator", nil); resp.StatusCode != 403 {
		t.Fatalf("operator: status %d", resp.StatusCode)
	}
	resp, body := e.do(t, "GET", "/devices/1/delete", "admin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{
		"<h1>Delete kitchen?</h1>", " · ESP8266 · Tasmota 16.0.0",
		"This device still has 2 backups", "1 kept, 10.1 KiB, newest 22 Oct 2026, 09:41", // every file counts: Delete removes them all
		"22 Oct 2026, 09:41", "Manual · 14.2.0 · 4.1 KiB", "Kept · Scheduled · 14.2.0 · 4.0 KiB",
		`method="post" action="/tf/devices/1/delete"`, `name="csrf"`,
		`<input type="radio" name="backups" value="keep" checked>`, `<input type="radio" name="backups" value="delete">`,
		"All 2 backups are removed.", `<input type="checkbox" name="ack" value="1">`, ">Delete device</button>",
		`href="/tf/devices/1">Cancel</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("delete page lacks %q", want)
		}
	}
	if strings.Contains(body, "style=") {
		t.Error("the page carries an inline style (CSP, D6)")
	}
	if resp, _ := e.do(t, "GET", "/devices/99/delete", "admin", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown device: status %d", resp.StatusCode)
	}
	// A device without backups has nothing to choose: the form still sends a valid choice.
	e.bk.list = nil
	_, body = e.do(t, "GET", "/devices/1/delete", "admin", nil)
	if !strings.Contains(body, "This device has no backups.") || !strings.Contains(body, `<input type="hidden" name="backups" value="delete">`) || strings.Contains(body, `type="radio"`) {
		t.Errorf("no backups:\n%s", body)
	}
	// The v0.10 dialogs posted no fields; that still deletes (the backups go, as they did).
	if r, _ := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{}); r.StatusCode != 303 {
		t.Fatalf("delete: status %d", r.StatusCode)
	}
}

// Why (N2): a refused delete must not lose the admin's place or choice: the page comes back
// with the field error (not a bare message page), the posted choice still selected, and
// nothing is deleted.
func TestRefusedDeleteShowsThePageWithTheFieldError(t *testing.T) {
	e := setup(t)
	e.bk.list = []model.Backup{{ID: 1, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: time.Now(), Size: 4}}
	resp, body := e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"delete"}})
	if resp.StatusCode != 422 {
		t.Fatalf("without ack: status %d", resp.StatusCode)
	}
	for _, want := range []string{"<h1>Delete kitchen?</h1>", `name="ack" value="1" aria-invalid="true"`, needConfirm, `value="delete" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("refused page lacks %q", want)
		}
	}
	if strings.Contains(body, `value="keep" checked`) {
		t.Error("the refused page forgot the posted choice")
	}
	resp, body = e.do(t, "POST", "/devices/1/delete", "admin", url.Values{"backups": {"maybe"}, "ack": {"1"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "Choose whether to keep the backups.") || strings.Contains(body, `aria-invalid="true"`) {
		t.Errorf("bad choice: %d\n%s", resp.StatusCode, body)
	}
	if _, err := e.inv.Get(t.Context(), 1); err != nil {
		t.Fatal("a refused delete removed the device")
	}
	if resp, _ := e.do(t, "POST", "/devices/99/delete", "admin", url.Values{"backups": {"keep"}}); resp.StatusCode != 404 {
		t.Errorf("unknown device: status %d", resp.StatusCode)
	}
}

var getWithCSRF = regexp.MustCompile(`(?i)(href|action|formaction)="[^"]*[?&;]csrf=`)

// Why: a GET submit button inside a POST form would put the CSRF token into
// the URL (history, logs, Referer). No page may render one or link with a
// token (PORT.md "CSRF (must)").
func TestNoGetFallbackCarriesTheCSRFToken(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	for _, p := range []string{"/", "/devices", "/devices/1", "/styleguide", "/devices/1/delete", "/devices/1/restart"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if strings.Contains(body, `formmethod="get"`) {
			t.Errorf("%s: a button submits a form by GET", p)
		}
		if getWithCSRF.MatchString(body) {
			t.Errorf("%s: a URL carries csrf=", p)
		}
	}
}

// Why: "Auto" must follow the system theme without JavaScript, which needs the
// data-theme-auto marker on <html>; Sign out lives in the account menu and in the drawer.
func TestShellMarksAutoThemeAndHasAccountMenu(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{`<html lang="en" data-theme-auto>`, `<nav class="tb-groups" aria-label="Main">`, `<dialog id="nav-drawer" class="drawer"`, `<p class="dr-label">Devices</p>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !regexp.MustCompile(`(?s)<div class="um-menu">.*Sign out.*</div></details>`).MatchString(body) {
		t.Error("Sign out is not inside the account menu")
	}
}

// Why (D17): the style guide shows sample data only, so it discloses nothing and is for every
// signed-in user, not only admins: everybody who uses the UI benefits from seeing its states.
func TestStyleguideIsOpenToEverySignedInUser(t *testing.T) {
	e := setup(t)
	for _, who := range []string{"operator", "admin"} {
		resp, body := e.do(t, "GET", "/styleguide", who, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", who, resp.StatusCode)
		}
		for _, want := range []string{"<h1>Styleguide</h1>", `id="sg-banners"`, `id="sg-buttons"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", who, want)
			}
		}
	}
}

func errUnreachable() error { return tasmota.ErrUnreachable }
