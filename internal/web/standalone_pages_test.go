package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/auth"
)

// Why: the sign-in, forced-change and denial pages are rendered outside the app
// shell. Without data-theme-auto on <html> they ignore the system dark mode and
// flash light in a dark browser (QA v0.9.0).
func TestStandalonePagesFollowTheSystemTheme(t *testing.T) {
	p := NewPages()
	render := map[string]func(w *httptest.ResponseRecorder){
		"login": func(w *httptest.ResponseRecorder) {
			p.Login(w, httptest.NewRequest("GET", "/login", nil), 200, auth.LoginData{Local: true})
		},
		"forced password": func(w *httptest.ResponseRecorder) {
			p.ChangePassword(w, httptest.NewRequest("GET", "/account/password", nil), 200, auth.PasswordPageData{Forced: true})
		},
		"message": func(w *httptest.ResponseRecorder) {
			p.Message(w, httptest.NewRequest("GET", "/", nil), 403, auth.MessageData{Title: "Access denied", Text: "x"})
		},
		"setup step 1": func(w *httptest.ResponseRecorder) {
			p.SetupAccount(w, httptest.NewRequest("GET", "/setup/account", nil), 200, auth.SetupAccountData{NeedToken: true})
		},
	}
	for name, f := range render {
		w := httptest.NewRecorder()
		f(w)
		if !strings.Contains(w.Body.String(), `<html lang="en" data-theme-auto>`) {
			t.Errorf("%s: <html> lacks data-theme-auto", name)
		}
	}
}

// Why: the forced password change has no app shell, so it is the only place that
// can say which account is about to change its password; the sign-in page says
// why it appeared (login notice).
func TestForcedChangeNamesTheAccountAndLoginShowsItsNotice(t *testing.T) {
	p := NewPages()
	w := httptest.NewRecorder()
	p.ChangePassword(w, httptest.NewRequest("GET", "/account/password", nil), 200, auth.PasswordPageData{Forced: true, Self: "adam"})
	if !strings.Contains(w.Body.String(), "Signed in as <b>adam</b>") {
		t.Errorf("forced change does not name the account: %s", w.Body)
	}
	w = httptest.NewRecorder()
	p.Login(w, httptest.NewRequest("GET", "/login", nil), 200, auth.LoginData{Local: true, Notice: "You have been signed out."})
	if !strings.Contains(w.Body.String(), `role="status"`) || !strings.Contains(w.Body.String(), "You have been signed out.") {
		t.Errorf("login drops its notice: %s", w.Body)
	}
}

// Why: a 404 for a missing device or transaction must lead somewhere (its list) and
// name what is missing; the switched-off page must make the way out ("Open the
// system settings") the primary action and show the setting as code.
func TestNotFoundAndSwitchedOffPagesLeadOn(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/999", "admin", nil)
	if !strings.Contains(body, "Device not found") || !strings.Contains(body, `href="/tf/devices"`) {
		t.Errorf("device 404 is a dead end: %s", body)
	}
	// the missing page belongs to Devices: the navigation says so, not Dashboard
	if !strings.Contains(body, `aria-current="true">Devices<`) || strings.Contains(body, `aria-current="true">Dashboard<`) {
		t.Errorf("device 404 is filed under the wrong navigation group: %s", body)
	}
	e, _, _, _ = setupOTA(t)
	_, body = e.do(t, "GET", "/transactions/999", "admin", nil)
	if !strings.Contains(body, "Transaction not found") || !strings.Contains(body, `href="/tf/transactions"`) {
		t.Errorf("transaction 404 is a dead end: %s", body)
	}
}

// Why: the switched-off page names the setting in its code box, and for an admin the primary
// button is the way out (system settings), not a loop back to the dashboard.
func TestSwitchedOffPageNamesTheSetting(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	if !strings.Contains(body, `<div class="errpage-path">FIRMWARE_BASE_URL</div>`) {
		t.Errorf("setting not shown as code: %s", body)
	}
	e, _, _, _ = sysEnv(t, true)
	_, body = e.do(t, "GET", "/firmware", "admin", nil)
	settings := strings.Index(body, `<a class="btn btn-primary" href="/tf/settings/system">Open the system settings</a><a class="btn" href="/tf/">Go to the dashboard</a>`)
	if settings < 0 {
		t.Errorf("admin: system settings must be the primary button, ahead of the dashboard link: %s", body)
	}
}

// Why: the object that is missing decides the words, not the route prefix. A device
// that exists but whose backup does not must not be reported as a missing device.
func TestNotFoundNamesWhatIsMissing(t *testing.T) {
	e := setup(t)
	resp, body := e.do(t, "GET", "/devices/1/backups/diff?a=98&b=99", "operator", nil)
	if resp.StatusCode != 404 || strings.Contains(body, "Device not found") || strings.Contains(body, "This device doesn") {
		t.Errorf("existing device reported missing (%d): %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "POST", "/devices/1/power/99", "operator", url.Values{})
	if resp.StatusCode == 404 && strings.Contains(body, "This device doesn") {
		t.Errorf("bad relay number reported as missing device: %s", body)
	}
}

// Why: a signed-in user without a mapped group sees a standalone page without the app
// shell; without a Sign out form there it is a dead end (no way to switch accounts).
func TestAccessDeniedPageOffersSignOut(t *testing.T) {
	w := httptest.NewRecorder()
	NewPages().Message(w, httptest.NewRequest("GET", "/", nil), 403, auth.MessageData{BasePath: "/tf", Title: "Access denied", Text: "x", SignOut: true, CSRFToken: "tok"})
	body := w.Body.String()
	if !strings.Contains(body, `action="/tf/logout"`) || !strings.Contains(body, `name="csrf" value="tok"`) {
		t.Errorf("no CSRF-backed Sign out form: %s", body)
	}
}

// Why: next is the way back after single sign-on. It is a query value inside the SSO link, so a
// page with its own query ("/devices?q=a&tag=b") must survive whole; unescaped, "&tag=b" became
// a second parameter of /login/sso and the user came back to a different list.
func TestSSOLinkKeepsTheWholeWayBack(t *testing.T) {
	next := "/devices?q=a&tag=b"
	w := httptest.NewRecorder()
	NewPages().Login(w, httptest.NewRequest("GET", "/login", nil), 200, auth.LoginData{Local: true, SSO: "Authentik", BasePath: "/tf", Next: next})
	want := `href="/tf/login/sso?next=` + url.QueryEscape(next) + `"`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("SSO link does not carry next escaped (%s): %s", want, w.Body)
	}
}
