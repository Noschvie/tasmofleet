package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// stepUpOIDC runs the IdP re-authentication for alice's browser.
func stepUpOIDC(t *testing.T, b *browser) {
	t.Helper()
	if r := b.get(beginOIDCStepUp(t, b, alice)); r.code != http.StatusSeeOther {
		t.Fatalf("oidc step-up callback: %d %s", r.code, r.body)
	}
}

// capturePages keeps the data of the last Users page it rendered.
type capturePages struct {
	DefaultPages
	last UsersPageData
}

func (c *capturePages) Users(w http.ResponseWriter, r *http.Request, status int, d UsersPageData) {
	c.last = d
	c.DefaultPages.Users(w, r, status, d)
}

// Why: seeing who has access is not a secret from an admin and must not cost
// a password prompt (audit #17); only changes are gated. Locked controls must
// not even be offered, so a click never ends in a 403.
func TestUsersListIsOpenWithoutStepUp(t *testing.T) {
	e := localEnv(t)
	adm := e.login("admin", testPassword)
	r := adm.get("/users")
	if r.code != http.StatusOK {
		t.Fatalf("list: %d", r.code)
	}
	for _, want := range []string{"admin", "ops", "/users/stepup"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("page lacks %q: %s", want, r.body)
		}
	}
	if strings.Contains(r.body, "/users/local/") || strings.Contains(r.body, "New user") {
		t.Errorf("changes offered before the step-up: %s", r.body)
	}
}

// Why: the list is open, so the gate has to sit on every single change
// route, for local and for OIDC admins; nothing may change without it.
func TestEveryChangeNeedsStepUp(t *testing.T) {
	changes := func(sub string) []struct {
		path string
		form url.Values
	} {
		return []struct {
			path string
			form url.Values
		}{
			{"/users", url.Values{"username": {"mallory"}, "password": {"mallory password 1"}, "role": {"admin"}}},
			{"/users/local/ops", url.Values{"action": {"role"}, "role": {"admin"}}},
			{"/users/local/ops", url.Values{"action": {"password"}, "password": {"mallory password 1"}}},
			{"/users/local/ops", url.Values{"action": {"disable"}}},
			{"/users/local/ops", url.Values{"action": {"delete"}}},
			{"/users/sessions", url.Values{"subject": {sub}}},
		}
	}
	check := func(t *testing.T, e *env, b *browser, sub string) {
		t.Helper()
		before := len(e.sessions.all())
		for _, c := range changes(sub) {
			if r := b.post(c.path, c.form); r.code != http.StatusForbidden {
				t.Errorf("POST %s %v without step-up: %d, want 403", c.path, c.form, r.code)
			}
		}
		ops := e.users.users["ops"]
		if len(e.users.users) != 2 || ops.Role != model.RoleOperator || ops.Disabled || ops.MustChangePassword {
			t.Errorf("accounts changed without a step-up: %+v", e.users.users)
		}
		if len(e.sessions.all()) != before {
			t.Error("sessions ended without a step-up")
		}
	}
	t.Run("local admin", func(t *testing.T) {
		e := localEnv(t)
		check(t, e, e.login("admin", testPassword), "local:ops")
	})
	t.Run("oidc admin", func(t *testing.T) {
		e, b := oidcAdmin(t)
		e.users.cli(func(m map[string]store.LocalUser) {
			m["ops"] = store.LocalUser{Username: "ops", PasswordHash: testHash, UpdatedAt: e.clock.Now(), Role: model.RoleOperator}
		})
		bb := e.browser()
		bb.get(bb.startOIDC("/", bob))
		check(t, e, b, "sub-bob")
	})
}

// Why: audit #18. An OIDC admin has no local password, so the IdP
// re-authentication is their step-up; it must open exactly the changes (here:
// resetting a local account's password and creating one), be audited under
// their subject, and a local password step-up must stay unavailable to them.
func TestOIDCAdminStepUpAllowsLocalAccountChanges(t *testing.T) {
	e, b := oidcAdmin(t)
	e.users.cli(func(m map[string]store.LocalUser) {
		m["ops"] = store.LocalUser{Username: "ops", PasswordHash: testHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator}
	})
	if r := b.get("/users"); r.code != http.StatusOK || !strings.Contains(r.body, "/oidc/stepup") || strings.Contains(r.body, "/users/local/") {
		t.Fatalf("an OIDC admin sees the list and is offered the IdP prompt: %d %s", r.code, r.body)
	}
	if r := b.post("/users/stepup", url.Values{"current": {testPassword}}); r.code != http.StatusForbidden {
		t.Fatalf("a password step-up is for local accounts only: %d", r.code)
	}
	stepUpOIDC(t, b)
	oldHash := e.users.users["ops"].PasswordHash
	if r := b.post("/users/local/ops", url.Values{"action": {"password"}, "password": {"reset by oidc admin 1"}}); r.code != http.StatusOK {
		t.Fatalf("reset: %d %s", r.code, r.body)
	}
	if u := e.users.users["ops"]; u.PasswordHash == oldHash || !u.MustChangePassword {
		t.Fatalf("password not reset or not forced to change: %+v", u)
	}
	if a := e.audit.last(); a.Action != "account.password" || a.Actor != "sub-alice" || a.Result != "ok" {
		t.Errorf("audit %+v", a)
	}
	if r := b.post("/users", url.Values{"username": {"newbie"}, "password": {newPassword}, "role": {"operator"}}); r.code != http.StatusOK {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	if _, ok := e.users.users["newbie"]; !ok {
		t.Fatal("account not created")
	}
	// The step-up is time-limited like the local one.
	e.clock.Advance(6 * time.Minute)
	if r := b.post("/users/local/ops", url.Values{"action": {"delete"}}); r.code != http.StatusForbidden {
		t.Fatalf("expired step-up: %d", r.code)
	}
}

// Why: the "last admin" rule protects the installation from lock-out; it must
// hold for an OIDC admin too, who bypasses the per-actor check.
func TestLastLocalAdminSurvivesAnOIDCAdmin(t *testing.T) {
	e, b := oidcAdmin(t)
	stepUpOIDC(t, b)
	for _, form := range []url.Values{{"action": {"delete"}}, {"action": {"disable"}}, {"action": {"role"}, "role": {"operator"}}} {
		if r := b.post("/users/local/admin", form); r.code != http.StatusConflict {
			t.Errorf("%v on the last admin: %d, want 409", form, r.code)
		}
	}
	if u := e.users.users["admin"]; u.Role != model.RoleAdmin || u.Disabled {
		t.Fatalf("last admin changed: %+v", u)
	}
}

// Why: End sessions is the one lever over an SSO user (roles stay with the
// IdP). It must hit exactly that subject: not other SSO users, not local
// sessions, not the admin's own.
func TestEndSessionsHitsExactlyOneSubject(t *testing.T) {
	e, alicesBrowser := oidcAdmin(t)
	bob1, bob2 := e.browser(), e.browser()
	bob1.get(bob1.startOIDC("/", bob))
	bob2.get(bob2.startOIDC("/", bob))
	local := e.login("admin", testPassword)
	count := func(sub string) int {
		n := 0
		for _, s := range e.sessions.all() {
			if s.Principal.Subject == sub {
				n++
			}
		}
		return n
	}
	if r := alicesBrowser.get("/users"); !strings.Contains(r.body, "bob") || strings.Contains(r.body, "End sessions") {
		t.Fatalf("the SSO user is listed, the End sessions control only after the step-up: %s", r.body)
	}
	stepUpOIDC(t, alicesBrowser)
	r := alicesBrowser.get("/users")
	if !strings.Contains(r.body, `value="sub-bob"`) || strings.Contains(r.body, `value="sub-alice"`) {
		t.Fatalf("End sessions for bob only, never for yourself: %s", r.body)
	}
	if r := alicesBrowser.post("/users/sessions", url.Values{"subject": {"sub-bob"}}); r.code != http.StatusOK {
		t.Fatalf("end sessions: %d %s", r.code, r.body)
	}
	if count("sub-bob") != 0 || count("sub-alice") != 1 || count("local:admin") != 1 {
		t.Fatalf("sessions after: bob %d alice %d local %d", count("sub-bob"), count("sub-alice"), count("local:admin"))
	}
	if r := bob1.get("/viewer"); r.code == http.StatusOK {
		t.Error("bob's session survived")
	}
	if r := local.get("/viewer"); r.code != http.StatusOK {
		t.Errorf("the local admin was signed out: %d", r.code)
	}
	if a := e.audit.last(); a.Action != "account.sessions.end" || a.Actor != "sub-alice" || !strings.Contains(a.Detail, "sub-bob") {
		t.Errorf("audit %+v", a)
	}
}

// Why: End sessions is for SSO users. A forged subject must not end a local
// account's sessions (that has its own, stronger actions), an unknown one, or
// the admin's own session (Sign out exists for that).
func TestEndSessionsRefusesOtherTargets(t *testing.T) {
	e, b := oidcAdmin(t)
	local := e.login("admin", testPassword)
	stepUpOIDC(t, b)
	for sub, want := range map[string]int{"local:admin": http.StatusNotFound, "nobody": http.StatusNotFound, "sub-alice": http.StatusBadRequest} {
		if r := b.post("/users/sessions", url.Values{"subject": {sub}}); r.code != want {
			t.Errorf("subject %q: %d, want %d", sub, r.code, want)
		}
	}
	if r := local.get("/viewer"); r.code != http.StatusOK {
		t.Error("a forged subject ended the local admin's session")
	}
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Error("an admin ended their own session")
	}
}

// Why: a proxy admin has no session and nothing to confirm with, so the page
// is view only for them (documented). They must still see everything, and
// not a single change may go through, including the password step-up.
func TestHeaderAdminIsViewOnly(t *testing.T) {
	e := newEnv(t, ModeHeader)
	hdr := map[string]string{"X-authentik-username": "boss", "X-authentik-name": "The Boss", "X-authentik-groups": "fleet-admins"}
	r := e.browser().do(http.MethodGet, "/users", nil, hdr)
	if r.code != http.StatusOK || !strings.Contains(r.body, "View only") || !strings.Contains(r.body, "The Boss") || strings.Contains(r.body, "/users/local/") {
		t.Fatalf("view: %d %s", r.code, r.body)
	}
	csrf := e.svc.headerToken("boss")
	for _, c := range []struct {
		path string
		form url.Values
	}{
		{"/users", url.Values{"username": {"mallory"}, "password": {"mallory password 1"}, "role": {"admin"}}},
		{"/users/local/admin", url.Values{"action": {"delete"}}},
		{"/users/stepup", url.Values{"current": {testPassword}}},
		{"/users/sessions", url.Values{"subject": {"boss"}}},
	} {
		c.form.Set("csrf", csrf)
		if r := e.browser().do(http.MethodPost, c.path, c.form, hdr); r.code != http.StatusForbidden {
			t.Errorf("header admin POST %s: %d, want 403", c.path, r.code)
		}
	}
	if len(e.users.users) != 1 || e.users.users["admin"].Disabled {
		t.Fatalf("a header admin changed the accounts: %+v", e.users.users)
	}
}

// Why: "last login" and "role" of SSO and proxy users exist only because the
// sign-in writes them down; the role is the one from the groups at that
// moment. A proxy user has no login event, so it is refreshed on use, but not
// on every request.
func TestSignInsAreRecordedForThePage(t *testing.T) {
	e, _ := oidcAdmin(t)
	bb := e.browser()
	bb.get(bb.startOIDC("/", bob))
	rec := e.dir.rows["sub-bob"]
	if rec.Kind != "sso" || rec.Role != "operator" || rec.DisplayName != "bob" || !rec.LastLogin.Equal(e.clock.Now()) {
		t.Fatalf("sso record %+v", rec)
	}
	if got := e.dir.rows["sub-alice"].Role; got != "admin" {
		t.Errorf("alice role %q", got)
	}
	l := e.login("admin", testPassword)
	if r := l.get("/users"); r.code != http.StatusOK {
		t.Fatal(r.code)
	}
	if rec := e.dir.rows["local:admin"]; rec.Kind != "local" || rec.Role != "admin" || rec.LastLogin.IsZero() {
		t.Fatalf("local record %+v", rec)
	}

	h := newEnv(t, ModeHeader)
	hdr := map[string]string{"X-authentik-username": "boss", "X-authentik-groups": "fleet-ops"}
	h.browser().do(http.MethodGet, "/viewer", nil, hdr)
	first := h.dir.rows["boss"]
	if first.Kind != "header" || first.Role != "operator" || first.LastLogin.IsZero() {
		t.Fatalf("header record %+v", first)
	}
	h.clock.Advance(time.Minute)
	h.browser().do(http.MethodGet, "/viewer", nil, hdr)
	if !h.dir.rows["boss"].LastLogin.Equal(first.LastLogin) {
		t.Error("a header user's row was written again within the throttle window")
	}
	h.clock.Advance(headerSeenEvery)
	h.browser().do(http.MethodGet, "/viewer", nil, hdr)
	if !h.dir.rows["boss"].LastLogin.After(first.LastLogin) {
		t.Error("a header user's last login was never refreshed")
	}
}

// Why: the page's columns come from three sources (account table, sign-in
// records, session store); each value has to land on the right row.
func TestUsersPageRowsCarryMethodRoleLoginAndSessions(t *testing.T) {
	pages := &capturePages{}
	e := newEnv(t, ModeOIDC, func(o *Options) { o.Pages = pages })
	for range 2 { // two sessions of alice
		b := e.browser()
		b.get(b.startOIDC("/", alice))
	}
	e.clock.Advance(time.Hour)
	local := e.login("admin", testPassword)
	if r := local.get("/users"); r.code != http.StatusOK {
		t.Fatalf("%d %s", r.code, r.body)
	}
	got := map[string]UserRow{}
	for _, u := range pages.last.Users {
		got[u.Subject] = u
	}
	if len(got) != 2 {
		t.Fatalf("rows %+v", pages.last.Users)
	}
	a, l := got["sub-alice"], got["local:admin"]
	if a.Method != MethodSSO || a.Role != "admin" || a.Sessions != 2 || a.Name != "alice" || !a.LastLogin.Equal(e.clock.Now().Add(-time.Hour)) {
		t.Errorf("sso row %+v", a)
	}
	if l.Method != MethodLocal || l.Role != "admin" || l.Sessions != 1 || l.Name != "admin" || !l.LastLogin.Equal(e.clock.Now()) {
		t.Errorf("local row %+v", l)
	}
}

// Why: the old route moved and does not linger as a shim (no backward
// compatibility layers); it must not reach any handler.
func TestOldAdminUsersRouteIsGone(t *testing.T) {
	e := localEnv(t)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	for _, p := range []string{"/admin/users", "/admin/users/stepup", "/admin/users/ops"} {
		if r := adm.get(p); r.code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.code)
		}
	}
}

// Why: every route of the users page must sit behind the admin guard and the
// CSRF check, and the removal of /admin/users must leave no handler mounted
// without them: anonymous is sent to sign in, an operator gets 403, an admin
// without step-up may look but not change, and a POST without CSRF token never
// reaches a handler.
func TestUsersRoutesAreGuardedPerRoute(t *testing.T) {
	e := localEnv(t)
	anon, ops, adm := e.browser(), e.login("ops", opsPassword), e.login("admin", testPassword)
	routes := []struct {
		method, path string
		form         url.Values
		adminWithout int // admin without step-up
	}{
		{"GET", "/users", nil, http.StatusOK},
		{"POST", "/users", url.Values{"username": {"x1"}, "password": {newPassword}, "role": {"operator"}}, http.StatusForbidden},
		{"POST", "/users/stepup", url.Values{"current": {"wrong"}}, http.StatusForbidden},
		{"POST", "/users/local/ops", url.Values{"action": {"delete"}}, http.StatusForbidden},
		{"POST", "/users/sessions", url.Values{"subject": {"local:ops"}}, http.StatusForbidden},
	}
	for _, rt := range routes {
		do := func(b *browser, withCSRF bool) resp {
			if rt.method == "GET" {
				return b.get(rt.path)
			}
			if withCSRF {
				return b.post(rt.path, rt.form)
			}
			return b.do(http.MethodPost, rt.path, rt.form, nil)
		}
		if r := do(anon, false); r.code != http.StatusSeeOther && r.code != http.StatusUnauthorized && r.code != http.StatusForbidden {
			t.Errorf("anonymous %s %s: %d", rt.method, rt.path, r.code)
		}
		if r := do(ops, true); r.code != http.StatusForbidden {
			t.Errorf("operator %s %s: %d, want 403", rt.method, rt.path, r.code)
		}
		if r := do(adm, true); r.code != rt.adminWithout {
			t.Errorf("admin %s %s: %d, want %d", rt.method, rt.path, r.code, rt.adminWithout)
		}
		if rt.method == "POST" {
			if r := do(adm, false); r.code != http.StatusForbidden {
				t.Errorf("admin %s %s without CSRF token: %d, want 403", rt.method, rt.path, r.code)
			}
		}
	}
	if len(e.users.users) != 2 || e.users.users["ops"].Disabled {
		t.Fatalf("accounts changed: %+v", e.users.users)
	}
}

// Why (finding 88): an admin who disables, deletes or demotes their own account locks themselves
// out. The server refuses all three (a second admin exists, so this is the self rule, not the
// last-admin rule), nothing changes, and every refusal is audited. Demoting the last admin stays
// forbidden (TestLastLocalAdminSurvivesAnOIDCAdmin).
func TestAdminCannotDisableDeleteOrDemoteThemselves(t *testing.T) {
	e := localEnv(t)
	e.users.cli(func(m map[string]store.LocalUser) {
		u := m["ops"]
		u.Role = model.RoleAdmin // a second admin, so the last-admin rule is not what refuses
		m["ops"] = u
	})
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	for _, c := range []struct {
		name string
		form url.Values
		want string
	}{
		{"disable", url.Values{"action": {"disable"}}, "denied: you cannot disable your own account"},
		{"delete", url.Values{"action": {"delete"}}, "denied: you cannot delete your own account"},
		{"demote", url.Values{"action": {"role"}, "role": {"operator"}}, "denied: you cannot lower your own role"},
	} {
		if r := adm.post("/users/local/admin", c.form); r.code != http.StatusBadRequest {
			t.Errorf("%s own account: %d, want 400", c.name, r.code)
		}
		if a := e.audit.last(); a.Result != c.want {
			t.Errorf("%s: audit result %q, want %q", c.name, a.Result, c.want)
		}
		if u, ok := e.users.users["admin"]; !ok || u.Role != model.RoleAdmin || u.Disabled {
			t.Fatalf("%s changed the own account: %+v", c.name, u)
		}
	}
	// other accounts stay manageable, also by the same admin
	if r := adm.post("/users/local/ops", url.Values{"action": {"disable"}}); r.code != http.StatusOK {
		t.Errorf("disable another admin: %d", r.code)
	}
}
