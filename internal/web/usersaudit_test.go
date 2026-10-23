package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

func renderUsersPage(t *testing.T, d auth.UsersPageData) string {
	t.Helper()
	pages := NewPages()
	mem := &memPrincipals{}
	setupWith(t, func(deps *Deps) { deps.Pages = pages; deps.Principals = mem })
	if err := mem.UpsertPrincipal(t.Context(), store.PrincipalRecord{Subject: "sso-1", DisplayName: "Sam", Kind: "sso"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), model.Principal{Subject: "local:admin", Name: "admin", Role: model.RoleAdmin, Method: "local"})
		pages.Users(w, r.WithContext(ctx), http.StatusOK, d)
	}))
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tf/users", nil))
	return rec.Body.String()
}

// allowed are the guards of a row whose every change the rules allow.
var allowed = auth.UserGuards{Role: auth.Guard{Allowed: true}, Disable: auth.Guard{Allowed: true}, Delete: auth.Guard{Allowed: true},
	Reset: auth.Guard{Allowed: true}, Profile: auth.Guard{Allowed: true}}

func usersFixture() auth.UsersPageData {
	self := auth.UserGuards{Role: auth.Guard{Reason: "You cannot lower your own role."}, Disable: auth.Guard{Reason: "You cannot disable your own account."},
		Delete: auth.Guard{Reason: "You cannot delete your own account."}, Reset: auth.Guard{Reason: "Use Change password for your own account."}}
	idp := auth.Guard{Reason: "Managed by the identity provider."}
	return auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", Users: []auth.UserRow{
		{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin", Guards: self},
		{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", MustChangePassword: true, Guards: allowed},
		{Subject: "sso-1", Name: "Sam", Method: auth.MethodSSO, Role: "operator", Guards: auth.UserGuards{Role: idp, Disable: idp, Delete: idp, Reset: idp, Profile: idp}},
	}}
}

// menuOf is the "⋯" menu of the row that names who.
func menuOf(t *testing.T, body, who string) string {
	t.Helper()
	i := strings.Index(body, ">"+who+"</span>")
	if i < 0 {
		t.Fatalf("no row for %q", who)
	}
	m := body[i:]
	m = m[strings.Index(m, `class="rowmenu`):]
	return m[:strings.Index(m, "</details>")]
}

// Why (design v2, P9): the prototype's list shows who, role and state, how they sign in and
// when last, so an admin sees the whole picture before opening a menu: the own row says "You",
// a pending password and a deactivated account are badges.
func TestUsersListShowsStateAsBadges(t *testing.T) {
	d := usersFixture()
	d.Users = append(d.Users, auth.UserRow{Subject: "local:old", Name: "old", Method: auth.MethodLocal, Role: "operator", Disabled: true, Guards: allowed})
	body := renderUsersPage(t, d)
	for _, want := range []string{`<span class="bdg t-neutral">You</span>`, `<span class="bdg t-warn">Must change password</span>`, `<span class="bdg t-neutral">Deactivated</span>`,
		`<span class="bdg t-accent">Admin</span>`, "Never signed in · No sessions", ">Add local user</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	if strings.Contains(body, "<table") {
		t.Error("the users page is the prototype's list, not a table")
	}
}

// Why: while changes need a fresh confirmation, every allowed action opens the "Confirm it's
// you" dialog first (posting the password to the step-up route, or the IdP re-authentication)
// instead of failing after the click; the list itself stays open.
func TestUsersActionsAskForTheStepUpFirst(t *testing.T) {
	for _, kind := range []string{auth.StepUpPassword, auth.StepUpOIDC} {
		d := usersFixture()
		d.NeedStepUp, d.StepUpKind = true, kind
		body := renderUsersPage(t, d)
		if !strings.Contains(menuOf(t, body, "ops"), `data-dialog="users-stepup"`) || !strings.Contains(body, `<dialog id="users-stepup"`) {
			t.Errorf("%s: the actions do not lead to the step-up dialog", kind)
		}
		want := `action="/tf/users/stepup"`
		if kind == auth.StepUpOIDC {
			want = `action="/tf/oidc/stepup"`
		}
		if !strings.Contains(body, want) || strings.Contains(body, `action="/tf/users/local/`) {
			t.Errorf("%s: step-up dialog posts elsewhere, or a change is offered before the step-up", kind)
		}
	}
}

// Why: a row menu holds action items only, each confirmed in a dialog; an action the rules
// forbid stays visible, disabled, with the reason, so nobody wonders where it went (self,
// managed by the identity provider). The role item offers the role the user does not have.
func TestUsersMenuExplainsDisabledActions(t *testing.T) {
	body := renderUsersPage(t, usersFixture())
	ops := menuOf(t, body, "ops")
	if !strings.Contains(ops, "Make admin") || strings.Contains(ops, "Make operator") || strings.Contains(ops, "<select") || strings.Contains(ops, "<input") {
		t.Errorf("ops menu: %s", ops)
	}
	if !strings.Contains(body, `<dialog id="user-1-role-admin"`) || !strings.Contains(body, `action="/tf/users/local/ops"`) {
		t.Error("the role change has no confirmation dialog posting to the account")
	}
	own := menuOf(t, body, "admin")
	for _, why := range []string{"You cannot delete your own account", "You cannot disable your own account", "Use Sign out for your own session"} {
		if !strings.Contains(own, `<span class="menu-why">`+why+`</span>`) {
			t.Errorf("own row: no disabled item saying %q", why)
		}
	}
	if strings.Contains(body, `action="/tf/users/local/admin"`) {
		t.Error("the own row offers account changes")
	}
	if sso := menuOf(t, body, "Sam"); !strings.Contains(sso, "Managed by the identity provider") || !strings.Contains(sso, "No active sessions") {
		t.Errorf("SSO row: %s", sso)
	}
}

// Why: the own account page is reached from the account menu only; it has the trail of every
// other page, the profile, and for a local account the password form.
func TestAccountPageShowsProfileAndPasswordForm(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.ChangePassword(w, r, http.StatusOK, auth.PasswordPageData{BasePath: "/tf", CSRFToken: "tok", Local: true, DisplayName: "Ada Admin", Email: "ada@example.org"})
	})
	for _, want := range []string{`aria-label="Breadcrumb"`, "<h1>Your account</h1>", "Ada Admin", "ada@example.org", "Local password", `action="/tf/account/password"`, `action="/tf/account/profile"`} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
}

// Why: a person who saves the Backups tab must see the values they just saved, not the first tab.
func TestSystemSettingsSaveReturnsToTheSavedTab(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	r, _ := e.do(t, "POST", "/settings/system?tab=backups", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}})
	if loc := r.Header.Get("Location"); r.StatusCode != 303 || !strings.Contains(loc, "tab=backups") || !strings.Contains(loc, "ok=saved") {
		t.Fatalf("status %d, back to %q", r.StatusCode, loc)
	}
	r, _ = e.do(t, "POST", "/settings/system?tab=x%22%3E%3Cscript", "admin", url.Values{"csrf": {"tok"}, "v.BACKUP_KEEP": {"4"}})
	if loc := r.Header.Get("Location"); strings.ContainsAny(loc, `"<>`) || strings.Contains(loc, "tab=") {
		t.Fatalf("an invented tab reached the redirect: %q", loc)
	}
}

// Why: every field carries its visible label, also on the Network tab; read-only settings use
// the same label pattern as editable ones and say why they are read-only. The read-only
// key/value table has no Columns chooser, and the metrics switch sits on the Metrics tab, not
// with the audit retention.
func TestSystemSettingsTabsAreConsistent(t *testing.T) {
	e, _, _, _ := sysEnvWith(t, true, "TASMOFLEET_OIDC_CLIENT_ID=envid")
	_, body := e.do(t, "GET", "/settings/system?tab=network", "admin", nil)
	if !strings.Contains(body, `<span class="ufield-label">Device networks</span>`) {
		t.Error("the Network tab has no visible label for its field")
	}
	_, body = e.do(t, "GET", "/settings/system?tab=signin", "admin", nil)
	if !strings.Contains(body, `<span class="ufield-label">Client ID</span> <input class="mono" readonly value="envid">`) || !strings.Contains(body, "Set by environment (OIDC_CLIENT_ID)") {
		t.Error("a read-only setting does not use the label pattern of the editable ones, or hides why")
	}
	_, body = e.do(t, "GET", "/settings/system?tab=advanced", "admin", nil)
	if strings.Contains(body, `data-table="keyvalue"`) || strings.Contains(body, `class="colmenu"`) {
		t.Error("the read-only key/value table carries a Columns chooser")
	}
	general := tabBody(t, body, "general")
	if strings.Contains(general, "METRICS_DEVICE_LABELS") || !strings.Contains(tabBody(t, body, "metrics"), `name="v.METRICS_DEVICE_LABELS"`) {
		t.Error("METRICS_DEVICE_LABELS is not on its own Metrics tab")
	}
}

// tabBody is the markup of one tab of the settings form.
func tabBody(t *testing.T, body, slug string) string {
	t.Helper()
	i := strings.Index(body, `id="sec-`+slug+`"`)
	if i < 0 {
		t.Fatalf("no tab %s", slug)
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], `<div class="sys-tab"`); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

func auditEnvWith(t *testing.T, evs ...model.AuditEvent) *env {
	t.Helper()
	e := setupWith(t, func(d *Deps) { d.Principals = &memPrincipals{} })
	e.aud.events = append(e.aud.events, evs...)
	return e
}

// Why: the User column reads the same for every source (a chip for the kind), actions have
// readable names, a subject nobody recorded is not shown as a piece of its id, and the
// "Clear the filter" action exists once on screen, as a 40 px button on a phone.
func TestAuditReadsConsistently(t *testing.T) {
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	hash := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	e := auditEnvWith(t,
		model.AuditEvent{ID: 1, At: at, Actor: "local:admin", Action: "sysconfig.mqtt_test", Target: "settings:system", Result: "failed"},
		model.AuditEvent{ID: 2, At: at, Actor: hash, Action: "device.update", Target: "device:1", Result: "ok"},
	)
	_, body := e.do(t, "GET", "/audit?since=all", "admin", nil)
	if !strings.Contains(body, `>admin <span class="tag">Local</span>`) || strings.Contains(body, "admin (local)") {
		t.Error("a local user has no Local chip")
	}
	if !strings.Contains(body, ">MQTT test<") || strings.Contains(body, "Sysconfig mqtt test") {
		t.Error("the MQTT test action shows its internal name")
	}
	if !strings.Contains(body, "Unknown user") || strings.Contains(body, "user 9f86d081") {
		t.Error("an unrecorded subject shows a piece of its id")
	}

	_, body = e.do(t, "GET", "/audit?since=all&action=device.update", "admin", nil)
	if n := strings.Count(body, "Clear the filter"); n != 1 {
		t.Errorf("with results the filter row offers %d clear actions, want 1", n)
	}
	_, body = e.do(t, "GET", "/audit?since=all&target=nothing", "admin", nil)
	if n := strings.Count(body, "Clear the filter"); n != 1 || strings.Contains(body, `class="button ghost" href="/tf/audit"`) {
		t.Errorf("the empty state and the filter row both offer a clear action (%d)", n)
	}
	css, err := os.ReadFile("static/app.css")
	if err != nil || !strings.Contains(string(css), ".filters a.button") {
		t.Error("the clear link is not a full-width button on a phone")
	}
	if !strings.Contains(string(css), "table td.rowacts:empty") {
		t.Error("an empty Actions cell shows its label on a phone")
	}
}

// renderUserPage renders one of the users v2 pages for the local admin "admin" and returns the
// whole response (the OTP page's headers matter).
func renderUserPage(t *testing.T, f func(p *Pages, w http.ResponseWriter, r *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	pages := NewPages()
	mem := &memPrincipals{}
	setupWith(t, func(deps *Deps) { deps.Pages = pages; deps.Principals = mem })
	if err := mem.UpsertPrincipal(t.Context(), store.PrincipalRecord{Subject: "sso/1 x?y", DisplayName: "Sam", Kind: "sso"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h := httpx.Proxy{BasePath: "/tf"}.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), model.Principal{Subject: "local:admin", Name: "admin", Role: model.RoleAdmin, Method: "local"})
		f(pages, w, r.WithContext(ctx))
	}))
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tf/users/x", nil))
	return rec
}

func userDetailBody(t *testing.T, d auth.UserDetailData) string {
	t.Helper()
	return renderUserPage(t, func(p *Pages, w http.ResponseWriter, r *http.Request) { p.UserDetail(w, r, http.StatusOK, d) }).Body.String()
}

// Why (N4): an admin ends one stolen or forgotten session of a user without signing them out
// everywhere; the session the admin is using right now is ended with Sign out, never from here
// (the handler refuses it too), so it has no End session button.
func TestUserDetailEndsOnlyOtherSessions(t *testing.T) {
	now := time.Now()
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User:   auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator"},
		Guards: allowed,
		Sessions: []auth.SessionRow{
			{ID: "own-session-hash", CreatedAt: now.Add(-time.Hour), LastSeen: now, Current: true},
			{ID: "other-session-hash", CreatedAt: now.Add(-48 * time.Hour), LastSeen: now.Add(-2 * time.Hour)},
		}})
	if n := strings.Count(body, `action="/tf/users/sessions"`); n != 1 {
		t.Fatalf("want exactly one End session form, got %d", n)
	}
	if !strings.Contains(body, `<input type="hidden" name="session" value="other-session-hash">`) || !strings.Contains(body, `<input type="hidden" name="subject" value="local:ops">`) {
		t.Error("End session does not post the other session's id and the subject")
	}
	if strings.Contains(body, "own-session-hash") {
		t.Error("the viewer's own session can be ended from the detail page")
	}
	if !strings.Contains(body, "this session") {
		t.Error("the own session is not marked")
	}
}

// Why: an action the rules forbid stays visible, disabled, and the page says why (P9's rule for
// the row menu, now on the detail page): the own account and the last admin.
func TestUserDetailExplainsDisabledActions(t *testing.T) {
	last := auth.Guard{Reason: auth.MsgLastAdmin}
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User: auth.UserRow{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin"},
		Guards: auth.UserGuards{Role: last, Disable: last, Delete: last, Reset: auth.Guard{Reason: "Use Change password for your own account."},
			Profile: auth.Guard{Reason: "Edit your own profile on the Account page."}}})
	for _, want := range []string{`disabled title="This would leave no enabled administrator">Delete…</button>`, `disabled title="Use Change password for your own account">Reset password</button>`,
		"That’s you, and you’re the last active admin.", `<span class="bdg t-neutral">You</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if strings.Contains(body, `action="/tf/users/local/admin"`) {
		t.Error("the own account offers a change it cannot make")
	}
}

// Why: the confirmations post from=detail, so the result (or the refusal) comes back on this page
// instead of the list; the profile form is the N4 profile action.
func TestUserDetailActionsReturnToTheDetailPage(t *testing.T) {
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin",
		User: auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", Disabled: true}, Guards: allowed,
		DisplayName: "Olli Ops", ErrField: "email", Error: "Enter a valid email address."})
	form := body[strings.Index(body, `<dialog id="user-0-enable"`):]
	form = form[:strings.Index(form, "</dialog>")]
	if !strings.Contains(form, `name="from" value="detail"`) || !strings.Contains(form, `name="action" value="enable"`) {
		t.Errorf("Reactivate does not come back to the detail page: %s", form)
	}
	if !strings.Contains(body, `data-dialog="user-0-enable">Reactivate</a></div>`) {
		t.Error("the deactivated banner has no Reactivate")
	}
	if !strings.Contains(body, `<details class="fold ud-edit" open>`) || !strings.Contains(body, "Enter a valid email address.") || !strings.Contains(body, `name="action" value="profile"`) {
		t.Error("a refused profile change does not reopen its form with the error")
	}
}

// Why: an SSO subject is an arbitrary string from the identity provider; every link that carries
// it must be escaped, or "a/b?c" opens another page or another user's audit trail.
func TestUserDetailEscapesThePrincipalSubject(t *testing.T) {
	idp := auth.Guard{Reason: "Managed by the identity provider."}
	body := userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", NeedStepUp: true, StepUpKind: auth.StepUpOIDC, IdP: "Authentik",
		User:     auth.UserRow{Subject: "sso/1 x?y", Name: "Sam", Method: auth.MethodSSO, Role: "operator", Sessions: 1},
		Guards:   auth.UserGuards{Role: idp, Disable: idp, Delete: idp, Reset: idp, Profile: idp},
		Sessions: []auth.SessionRow{{ID: "s1", CreatedAt: time.Now(), LastSeen: time.Now()}}})
	for _, want := range []string{`href="/tf/audit?since=all&amp;actor=sso%2F1+x%3Fy"`, `name="next" value="/users/principal/sso%2F1%20x%3Fy"`,
		"Name, email and password are managed in Authentik.", "Sam · SSO · Authentik"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if strings.Contains(body, "/users/principal/sso/1") {
		t.Error("an unescaped subject in a link")
	}
}

// Why (N4): a generated password is shown exactly once: never cached by the browser or a proxy,
// only once in the page (the copy button reads it from the page, it is no second copy in the
// markup), and never written to the log by the renderer.
func TestOneTimePasswordIsShownOnceAndNotCached(t *testing.T) {
	const pw = "otp-fake-Hx7q-82Lk-9wPe"
	var logs strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	rec := renderUserPage(t, func(p *Pages, w http.ResponseWriter, r *http.Request) {
		p.OneTimePassword(w, r, http.StatusOK, auth.OTPPageData{BasePath: "/tf", CSRFToken: "tok", User: "lena", Password: pw, ExpiresAt: time.Now().Add(24 * time.Hour)})
	})
	body := rec.Body.String()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if n := strings.Count(body, pw); n != 1 {
		t.Errorf("the password appears %d times, want 1", n)
	}
	if strings.Contains(logs.String(), pw) {
		t.Error("the password reached the log")
	}
	for _, want := range []string{"Password reset for lena", `data-copy-from="#otp-value"`, `<a class="btn btn-primary" href="/tf/users/lena">Done</a>`, "can’t be shown again"} {
		if !strings.Contains(body, want) {
			t.Errorf("OTP page lacks %q", want)
		}
	}
}
