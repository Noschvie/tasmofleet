package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/settings"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// summaries is how many error summaries the page draws.
func summaries(body string) int { return strings.Count(body, "err-summary") }

// Why (#1098, the rule "Form errors sit at the input"): a form with one refused input shows that input's message
// once and nothing else. A summary above it, or a banner repeating the message, tells the person the same thing
// twice and makes the one place that matters (the marked input, which has the focus) compete with a second one.
// All four forms used to break it in their own way: login showed the banner and the field line, the other three the
// "1 field needs attention" banner.
func TestOneRejectedFieldShowsOnlyItsInlineError(t *testing.T) {
	pages := NewPages()
	e := setupWith(t, func(d *Deps) { d.Pages = pages })

	login := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.Login(w, r, http.StatusUnauthorized, auth.LoginData{BasePath: "/tf", Local: true, Username: "maren", Error: "Wrong username or password.", ShowAttemptsLeft: true, AttemptsLeft: 4})
	})
	addUser := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.Users(w, r, http.StatusBadRequest, auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", New: true, ErrField: "confirm", Error: "Passwords don’t match.",
			Users: []auth.UserRow{{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin"}}})
	})
	account := setupAccountBody(t, auth.SetupAccountData{BasePath: "/tf", CSRFToken: "tok", Token: "fake-token", Username: "admin", ErrField: "confirm", Error: "The two passwords differ."})
	_, edit := e.do(t, "POST", "/devices/1", "admin", url.Values{"csrf": {"tok"}, "name": {"kitchen"}, "address": {"10.0.0.1"}, "port": {"99999"}})

	for _, c := range []struct{ name, body, msg, input string }{
		{"login", login, "Wrong username or password.", "password"},
		{"add user", addUser, "Passwords don’t match.", "confirm"},
		{"setup account", account, "The two passwords differ.", "confirm"},
		{"edit device", edit, "Use a port number between 1 and 65535", "port"},
	} {
		if n := strings.Count(c.body, c.msg); n != 1 {
			t.Errorf("%s: %q shown %d times, want once", c.name, c.msg, n)
		}
		if n := summaries(c.body); n != 0 {
			t.Errorf("%s: %d error summaries for one refused input", c.name, n)
		}
		tag := inputTag(t, c.body, c.input)
		if !strings.Contains(tag, `aria-invalid="true"`) || !strings.Contains(tag, `aria-describedby="err-`+c.input+`"`) {
			t.Errorf("%s: input %s is not marked: %s", c.name, c.input, tag)
		}
	}
	// The login message carries the attempts line under the same input, in one place.
	if !strings.Contains(login, "Wrong username or password. 4 attempts left before further attempts are delayed.") {
		t.Errorf("login: the attempts line is not part of the field error: %s", login)
	}
	if strings.Contains(login, "banner tone-danger") {
		t.Error("login: a retryable wrong sign-in shows the error in a banner as well as at the password")
	}
	if strings.Contains(inputTag(t, login, "username"), "aria-invalid") {
		t.Error("login: only the password is marked (the audit's decision), not the username")
	}
}

// Why (#1098): the login error moves under the password only when the person can try again. A lockout, an expired
// one-time password or a failed single sign-on belongs to no input, so it stays a banner (and says so once); the
// focus keeps its old rule (the username when it is empty), which a wrong sign-in with an empty username needs.
func TestLoginErrorWithoutARetryStaysABanner(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	render := func(d auth.LoginData) string {
		d.BasePath, d.Local = "/tf", true
		return renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) { pages.Login(w, r, http.StatusUnauthorized, d) })
	}
	locked := render(auth.LoginData{Username: "maren", Error: "Too many attempts.", ShowAttemptsLeft: true, AttemptsLeft: 0})
	if strings.Count(locked, "Too many attempts.") != 1 || !strings.Contains(locked, `role="alert"`) || strings.Contains(locked, "ufield-err") {
		t.Errorf("lockout: want one banner and no field error: %s", locked)
	}
	if !strings.Contains(locked, "Further attempts are delayed for a while.") {
		t.Error("lockout: the banner lost the delay line")
	}
	empty := render(auth.LoginData{Error: "Wrong username or password.", ShowAttemptsLeft: true, AttemptsLeft: 4})
	if !strings.Contains(inputTag(t, empty, "username"), "autofocus") || strings.Contains(inputTag(t, empty, "password"), "autofocus") {
		t.Error("empty username: the focus stays on the username")
	}
	typed := render(auth.LoginData{Username: "maren", Error: "Wrong username or password.", ShowAttemptsLeft: true, AttemptsLeft: 4})
	if !strings.Contains(inputTag(t, typed, "password"), "autofocus") {
		t.Error("a typed username: the focus goes to the password")
	}
}

// Why (#1098): two refused settings are the case the summary exists for: the person sees at once that more than one
// thing is wrong and that nothing was saved, and each message sits at its input. The page used to say "Nothing was
// saved. Fix the marked fields." (or "Some values are invalid...") in its own words.
func TestTwoRejectedSettingsShowOneSummary(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.AUDIT_RETENTION": {"1h"}, "v.TELEPERIOD": {"0s"}})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("system settings: status %d", r.StatusCode)
	}
	wantOneSummary(t, "system settings", body, "AUDIT_RETENTION", "TELEPERIOD")

	fs := &fakeSettings{applyEr: &settings.ValidationError{Fields: map[string]string{"TelePeriod": "0 (off) or 10 to 3600 seconds", "SetOption3": "must be 0 or 1"}}}
	d := withSettings(t, fs)
	r, body = d.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "5", "SetOption3", "7"))
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("device settings: status %d", r.StatusCode)
	}
	if n := summaries(body); n != 1 || !strings.Contains(body, "Fix 2 fields.") || !strings.Contains(body, "Nothing was saved yet.") {
		t.Errorf("device settings: %d summaries, want one saying Fix 2 fields: %s", n, body)
	}
	if !strings.Contains(body, "10 to 3600") || !strings.Contains(body, "must be 0 or 1") {
		t.Error("device settings: a message is missing at its input")
	}
}

func wantOneSummary(t *testing.T, name, body string, keys ...string) {
	t.Helper()
	if n := summaries(body); n != 1 || !strings.Contains(body, "Fix 2 fields.") || !strings.Contains(body, "Nothing was saved yet.") {
		t.Errorf("%s: %d summaries, want one saying Fix 2 fields", name, n)
	}
	for _, k := range keys {
		if !strings.Contains(inputTag(t, body, "v."+k), `aria-invalid="true"`) {
			t.Errorf("%s: %s is not marked at its input", name, k)
		}
	}
	if strings.Contains(body, "marked fields") {
		t.Errorf("%s: the free-text banner is back", name)
	}
}

// Why (#1098): one refused setting shows its own message at its input and no summary or "Fix the marked fields"
// banner above it, on the setup, on System settings and on the device's Settings tab.
func TestOneRejectedSettingShowsNoSummary(t *testing.T) {
	e, _, _ := setupHandlerEnv(t)
	r, body := e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"not-a-cidr"}})
	if r.StatusCode != http.StatusUnprocessableEntity || summaries(body) != 0 || strings.Contains(body, "marked fields") {
		t.Errorf("setup: status %d, %d summaries: %s", r.StatusCode, summaries(body), body)
	}
	if !strings.Contains(inputTag(t, body, "v.DEVICE_NETWORKS"), `aria-invalid="true"`) || !strings.Contains(body, `id="err-v.DEVICE_NETWORKS"`) {
		t.Error("setup: the networks input does not carry its message")
	}

	s, _, _, _ := sysEnv(t, true)
	r, body = s.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.POLL_INTERVAL": {"not-a-duration"}})
	if r.StatusCode != http.StatusUnprocessableEntity || summaries(body) != 0 || strings.Contains(body, "marked fields") {
		t.Errorf("system settings: status %d, %d summaries", r.StatusCode, summaries(body))
	}

	fs := &fakeSettings{applyEr: &settings.ValidationError{Fields: map[string]string{"TelePeriod": "0 (off) or 10 to 3600 seconds"}}}
	d := withSettings(t, fs)
	r, body = d.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "5"))
	if r.StatusCode != http.StatusUnprocessableEntity || summaries(body) != 0 || strings.Contains(body, "Some values are invalid") || !strings.Contains(body, "10 to 3600") {
		t.Errorf("device settings: status %d, %d summaries", r.StatusCode, summaries(body))
	}
}

// Why (#1098): the setup form has an input for every setting it may change, so a refusal of any of them has a place
// to sit. If one lost its message element, the refusal would count towards the summary and show nowhere.
func TestEverySetupSettingShowsItsRefusalAtTheInput(t *testing.T) {
	errs := map[string]string{}
	for k := range setupKeys {
		errs[k] = "refused " + k
	}
	var sb strings.Builder
	if err := setupView(setupPage{Errors: errs}).Render(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	body := sb.String()
	for k := range setupKeys {
		if !strings.Contains(body, `id="err-v.`+k+`"`) {
			t.Errorf("%s: its refusal has no place on the setup page", k)
		}
	}
	if n := summaries(body); n != 1 || !strings.Contains(body, "Fix 4 fields.") {
		t.Errorf("%d summaries for four refused settings, want one saying Fix 4 fields", n)
	}
}

// Why (#1098): dropping the free-text summary must not drop a message. A refusal that has no input on the page (the
// role on the add-user page has a control but no place for one; BACKUP_KEEP is a setting of the Backups page, not of
// Settings) used to be lost behind "Fix the marked fields" with nothing marked. It stays a banner, once.
func TestARefusalWithoutAnInputIsStillSaid(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.Users(w, r, http.StatusBadRequest, auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", New: true, ErrField: "role", Error: "Choose operator or admin.",
			Users: []auth.UserRow{{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin"}}})
	})
	if n := strings.Count(body, "Choose operator or admin."); n != 1 || summaries(body) != 0 {
		t.Errorf("add user, role: message shown %d times, %d summaries", n, summaries(body))
	}

	e, _, _, _ := sysEnv(t, true)
	r, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"0"}})
	if r.StatusCode != http.StatusUnprocessableEntity || !regexp.MustCompile(`Nothing was saved\. Backups kept per device: `).MatchString(body) || summaries(body) != 0 {
		t.Errorf("a setting of another page: status %d, want its message in one banner: %s", r.StatusCode, body)
	}
}

// Why (#1098, DS-027): the summary was worded three ways because every page could write its own. The component
// takes the count only, so these phrases may live nowhere else; the markup test (ui_markup_test.go) already keeps
// the banner markup out of the pages, this keeps the words and the class out.
func TestNoPageWordsAFormSummaryItself(t *testing.T) {
	banned := []string{"field needs attention", "fields need attention", "err-summary", "marked fields", "Some values are invalid", "wasn’t added yet", "wasn’t created yet"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	tpl, _ := filepath.Glob("*.templ")
	for _, f := range append(files, tpl...) {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "_templ.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range banned {
			if strings.Contains(string(b), w) {
				t.Errorf("%s words a form summary itself (%q): use ui.ErrorSummary(n)", f, w)
			}
		}
	}
}

// Why (#1098, "Form errors sit at the input"): with the summary gone for one refused setting, the refusal is a full
// page render that opens at the top, and a card below the fold on a phone leaves nothing on screen that says the save
// failed (System settings even reads "No unsaved changes"). The browser scrolls the first refused input into view when
// it carries autofocus, so exactly that one does: not several (the last would win), not none.
func TestOneRefusedSettingTakesTheFocusOnEveryMultiFieldForm(t *testing.T) {
	e, _, _ := setupHandlerEnv(t)
	_, body := e.do(t, "POST", "/setup/finish", "admin", url.Values{"csrf": {"tok"}, "v.DEVICE_NETWORKS": {"not-a-cidr"}})
	if n := strings.Count(body, "autofocus"); n != 1 || !strings.Contains(inputTag(t, body, "v.DEVICE_NETWORKS"), "autofocus") {
		t.Errorf("setup: %d autofocus, want one on the networks input", n)
	}

	s, _, _, _ := sysEnv(t, true)
	_, body = s.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.POLL_INTERVAL": {"not-a-duration"}})
	if n := strings.Count(body, "autofocus"); n != 1 || !strings.Contains(inputTag(t, body, "v.POLL_INTERVAL"), "autofocus") {
		t.Errorf("system settings: %d autofocus, want one on the poll interval", n)
	}
	// Two refusals on the shown tab: the first one in page order only (TELEPERIOD follows POLL_INTERVAL in its card).
	_, body = s.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.POLL_INTERVAL": {"not-a-duration"}, "v.TELEPERIOD": {"0s"}})
	if n := strings.Count(body, "autofocus"); n != 1 || !strings.Contains(inputTag(t, body, "v.POLL_INTERVAL"), "autofocus") {
		t.Errorf("system settings, two refusals: %d autofocus, want one on the first refused input", n)
	}

	fs := &fakeSettings{applyEr: &settings.ValidationError{Fields: map[string]string{"TelePeriod": "0 (off) or 10 to 3600 seconds", "SetOption3": "must be 0 or 1"}}}
	d := withSettings(t, fs)
	_, body = d.do(t, "POST", "/devices/1/settings", "admin", baseForm("TelePeriod", "5", "SetOption3", "7"))
	if !strings.Contains(inputTag(t, body, "SetOption3"), "autofocus") || strings.Contains(inputTag(t, body, "TelePeriod"), "autofocus") {
		t.Errorf("device settings: the focus belongs on the first refused input in page order (SetOption3, the first card), not the second")
	}
}

// Why (#1098): System settings shows one tab at a time, and a refusal on another tab is counted by the summary. Without
// a mark on the tab bar, "Fix 2 fields." sends the person looking for a second field that is not on the screen.
func TestTabsMarkTheRefusedSettingsTheyHold(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, body := e.do(t, "POST", "/settings/system", "admin", url.Values{"csrf": {"tok"}, "v.AUDIT_RETENTION": {"1h"}, "v.TELEPERIOD": {"0s"}})
	for _, tab := range []string{"General", "Network &amp; devices"} {
		if !regexp.MustCompile(`class="utab"[^>]*>` + tab + ` <span class="bdg t-accent">1</span>`).MatchString(body) {
			t.Errorf("the %s tab does not say it holds a refused setting", tab)
		}
	}
	if regexp.MustCompile(`class="utab"[^>]*>MQTT <span class="bdg`).MatchString(body) {
		t.Error("the MQTT tab holds no refusal and carries a mark")
	}
}

// Why (#1098): a locked (environment-fixed) setting draws no input and no message, so a refusal that names one cannot
// be counted as "shown at an input"; it would feed the summary and appear nowhere.
func TestARefusalOfALockedSettingIsNotCountedAsShown(t *testing.T) {
	p := sysPage{Tabs: []sysTab{{Slug: "general", Cards: []sysCard{{Rows: []sysRow{
		{FieldState: sysconfig.FieldState{Key: "LOG_LEVEL", Locked: true}, Err: "refused"},
		{FieldState: sysconfig.FieldState{Key: "AUDIT_RETENTION"}, Err: "refused too"},
	}}}}}}
	if got := p.refusedCount(); got != 1 {
		t.Errorf("refusedCount = %d, want 1 (the locked row has no input)", got)
	}
}
