package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/httpx"
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
	if !strings.Contains(body, "Job not found") || !strings.Contains(body, `href="/tf/transactions"`) {
		t.Errorf("transaction 404 is a dead end: %s", body)
	}
}

// Why: the switched-off page shows an admin the setting in its code box, and the admin's primary button is the way
// out (system settings), not a loop back to the dashboard. An operator cannot change the setting: the box shows the
// address of the page instead (#1085; TestSwitchedOffPagesSpeakToTheRole checks the words).
func TestSwitchedOffPageNamesTheSetting(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/firmware", "operator", nil)
	if !strings.Contains(body, `<div class="errpage-path">/tf/firmware</div>`) || strings.Contains(body, "FIRMWARE_BASE_URL") {
		t.Errorf("operator: the box must show the page's address and no setting: %s", body)
	}
	e, _, _, _ = sysEnv(t, true)
	_, body = e.do(t, "GET", "/firmware", "admin", nil)
	if !strings.Contains(body, `<div class="errpage-path">FIRMWARE_BASE_URL</div>`) {
		t.Errorf("admin: setting not shown as code: %s", body)
	}
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

// Why (#1084): a form left open past a sign-in change ends on this page, before any principal is
// known. It is the standalone card (no navigation that would need a role), it follows the system
// theme like every card, it says what happened in the person's words, and it leads on: Reload goes
// back to the page the form was on (the caller has made that a local path), Dashboard to the start
// under the base path. The answer is a refusal, so it keeps the 403 and is never cached.
func TestStaleFormPageLeadsOn(t *testing.T) {
	w := httptest.NewRecorder()
	h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		NewPages().StaleForm(w, r, "/tf/devices?q=a")
	}))
	h.ServeHTTP(w, httptest.NewRequest("POST", "/tf/devices/bulk/command", nil))
	body := w.Body.String()
	if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status %d, Cache-Control %q: want 403, no-store", w.Code, w.Header().Get("Cache-Control"))
	}
	for _, want := range []string{
		`<html lang="en" data-theme-auto>`,
		"<title>This page is out of date - TasmoFleet</title>",
		`<div class="auth-card"><h1>This page is out of date</h1>`,
		`<p class="auth-text">Your sign-in changed since you opened it. Reload the page and try again.</p>`,
		`<div class="btn-row"><a class="btn btn-primary" href="/tf/devices?q=a">Reload</a><a class="btn" href="/tf/">Dashboard</a></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "CSRF") || strings.Contains(body, `class="sn-in"`) {
		t.Errorf("technical words or navigation on the stale page: %s", body)
	}
}

// Why (#1084): the refused single sign-on card must lead on. Back to sign-in is its first way on;
// the sign-out at the identity provider is a plain link (TasmoFleet holds no session there), named
// after the provider like the sign-in button, and only drawn when auth handed one over.
func TestMessagePageOffersSignOutAtTheProvider(t *testing.T) {
	render := func(d auth.MessageData) string {
		w := httptest.NewRecorder()
		NewPages().Message(w, httptest.NewRequest("GET", "/oidc/callback", nil), 403, d)
		return w.Body.String()
	}
	end := "https://idp.example/end?client_id=tf&post_logout_redirect_uri=https%3A%2F%2Ffleet.example%2F"
	d := auth.MessageData{BasePath: "/tf", Title: "Access denied", Text: "x", LocalLogin: true, IdP: "Authentik", IdPSignOut: end}
	back := `<div class="btn-row"><a class="btn btn-primary" href="/tf/login">Back to sign-in</a>`
	idp := `<a class="btn" href="https://idp.example/end?client_id=tf&amp;post_logout_redirect_uri=https%3A%2F%2Ffleet.example%2F">Sign out of Authentik</a></div>`
	if body := render(d); !strings.Contains(body, back) || !strings.Contains(body, idp) || strings.Index(body, back) > strings.Index(body, idp) {
		t.Errorf("want %q, then %q in %s", back, idp, body)
	}
	d.IdP = "Single sign-on" // the default name: the button says SSO, like the sign-in button
	if body := render(d); !strings.Contains(body, ">Sign out of SSO</a>") {
		t.Errorf("default provider name: %s", body)
	}
	d.IdPSignOut = ""
	if body := render(d); strings.Contains(body, "Sign out") || !strings.Contains(body, back) {
		t.Errorf("without an end-session link only the way back is drawn: %s", body)
	}
}

// Why: the forced password change has one primary action, Save and continue. Sign out is its quiet way out: if it
// were as loud (a second primary button, #1085 made the no-role card's Sign out one), a person who only wants to
// leave would be as likely to press the wrong one, and the page would lose its one obvious next step.
func TestForcedChangeKeepsOnePrimaryActionAndAQuietSignOut(t *testing.T) {
	p := NewPages()
	w := httptest.NewRecorder()
	p.ChangePassword(w, httptest.NewRequest("GET", "/account/password", nil), 200, auth.PasswordPageData{Forced: true, Self: "adam", CSRFToken: "tok"})
	body := w.Body.String()
	if n := strings.Count(body, "btn-primary"); n != 1 {
		t.Errorf("the forced change must have exactly one primary button, got %d: %s", n, body)
	}
	if !strings.Contains(body, `<button type="submit" class="btn btn-link">Sign out</button>`) {
		t.Errorf("Sign out on the forced change is not the quiet link button: %s", body)
	}
}
