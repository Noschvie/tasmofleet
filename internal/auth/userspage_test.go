package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
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

// Why (#1203, L12): an OIDC admin has no local account to check, so ending one
// session must work for it with the zero Actor (no per-actor condition) and
// end exactly that session; the actor check added for local admins must not
// lock the IdP-authenticated admin out of this action.
func TestOIDCAdminEndsOneSessionOfAnSSOUser(t *testing.T) {
	e, alicesBrowser := oidcAdmin(t)
	bob1, bob2 := e.browser(), e.browser()
	bob1.get(bob1.startOIDC("/", bob))
	bob2.get(bob2.startOIDC("/", bob))
	stepUpOIDC(t, alicesBrowser)
	id1 := hashID(bob1.cookie(CookieInsecure))
	if r := alicesBrowser.post("/users/sessions", url.Values{"subject": {"sub-bob"}, "session": {id1}}); r.code != http.StatusOK {
		t.Fatalf("end one session: %d %s", r.code, r.body)
	}
	if bob1.get("/viewer").code == http.StatusOK {
		t.Error("the ended session still works")
	}
	if bob2.get("/viewer").code != http.StatusOK {
		t.Error("the user's other session ended too")
	}
}

// Why: End sessions reaches users whose sessions exist (SSO and local). A
// forged subject must not end an unknown account's sessions, a header
// principal's, or the admin's own session (Sign out exists for that).
func TestEndSessionsRefusesOtherTargets(t *testing.T) {
	e, b := oidcAdmin(t)
	stepUpOIDC(t, b)
	for sub, want := range map[string]int{"local:nobody": http.StatusNotFound, "nobody": http.StatusNotFound, "sub-alice": http.StatusBadRequest} {
		if r := b.post("/users/sessions", url.Values{"subject": {sub}}); r.code != want {
			t.Errorf("subject %q: %d, want %d", sub, r.code, want)
		}
	}
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Error("an admin ended their own session")
	}
	_ = e
}

func countSessions(e *env, subject string) int {
	n := 0
	for _, s := range e.sessions.all() {
		if s.Principal.Subject == subject {
			n++
		}
	}
	return n
}

// Why (#1162): a local user who is signed in on two devices and should be
// signed out (lost phone, shared computer) had no lever short of Deactivate.
// Ending sessions must hit exactly that user: neither another user nor the
// admin's own session, and the user can sign in again at once with the same
// password, because the password generation and the device cookies stay (that
// is what separates it from Deactivate).
func TestEndSessionsEndsExactlyOneLocalUser(t *testing.T) {
	e, _ := hookedEnv(t)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	ops1, ops2 := e.login("ops", opsPassword), e.login("ops", opsPassword)
	boss := e.login("boss2", testPassword)
	genBefore := e.users.users["ops"].PasswordGen
	if countSessions(e, "local:ops") != 2 {
		t.Fatalf("setup: ops has %d sessions", countSessions(e, "local:ops"))
	}
	r := adm.post("/users/sessions", url.Values{"subject": {"local:ops"}})
	if r.code != http.StatusOK || !strings.Contains(r.body, "Signed out 2 session(s). ops can sign in again with the same password.") {
		t.Fatalf("end sessions: %d %s", r.code, r.body)
	}
	if countSessions(e, "local:ops") != 0 || countSessions(e, "local:boss2") != 1 || countSessions(e, "local:admin") != 1 {
		t.Fatalf("sessions after: ops %d boss2 %d admin %d", countSessions(e, "local:ops"), countSessions(e, "local:boss2"), countSessions(e, "local:admin"))
	}
	for _, b := range []*browser{ops1, ops2} {
		if r := b.get("/viewer"); r.code == http.StatusOK {
			t.Error("a session of ops survived")
		}
	}
	for name, b := range map[string]*browser{"boss2": boss, "admin": adm} {
		if r := b.get("/viewer"); r.code != http.StatusOK {
			t.Errorf("%s was signed out: %d", name, r.code)
		}
	}
	if a := e.audit.last(); a.Result != "ok" || a.Actor != "local:admin" || !strings.Contains(a.Detail, `"subject":"local:ops"`) || !strings.Contains(a.Detail, `"sessions":"2"`) {
		t.Errorf("audit %+v", a)
	}
	if got := e.users.users["ops"].PasswordGen; got != genBefore {
		t.Errorf("password generation %d -> %d: ending sessions must not revoke the password", genBefore, got)
	}
	if again := e.login("ops", opsPassword); again.get("/viewer").code != http.StatusOK {
		t.Error("ops cannot sign in again with the same password")
	}
}

// Why (#1162, L6): ending sessions is an account mutation. An admin demoted,
// disabled or re-keyed after its page loaded must not delete anything; the
// check sits in the same store statement, so no request can slip through.
func TestEndLocalSessionsRefusedWhenActorChangedMidRequest(t *testing.T) {
	ruins := map[string]func(m map[string]store.LocalUser){
		"demoted":          func(m map[string]store.LocalUser) { u := m["admin"]; u.Role = model.RoleOperator; m["admin"] = u },
		"disabled":         func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = true; m["admin"] = u },
		"password changed": func(m map[string]store.LocalUser) { u := m["admin"]; u.PasswordGen++; m["admin"] = u },
	}
	for ruin, doRuin := range ruins {
		t.Run(ruin, func(t *testing.T) {
			e, h := hookedEnv(t)
			adm := e.login("admin", testPassword)
			adm.stepUp(testPassword)
			e.login("ops", opsPassword)
			h.before = func() { e.users.cli(doRuin) }
			r := adm.post("/users/sessions", url.Values{"subject": {"local:ops"}})
			if r.code != http.StatusForbidden {
				t.Fatalf("after the admin was %s: %d, want 403", ruin, r.code)
			}
			if countSessions(e, "local:ops") != 1 {
				t.Fatal("the refused request deleted the sessions")
			}
			if a := e.audit.last(); a.Result != "denied: acting account changed" {
				t.Errorf("audit %+v", a)
			}
		})
	}
}

// Why (#1203, L6): ending ONE session of a local user is an account mutation
// as much as ending all of them. An admin demoted, disabled or re-keyed after
// its page loaded must not delete anything; the check sits in the same store
// statement, so no request can slip through between check and delete.
func TestEndOneSessionRefusedWhenActorChangedMidRequest(t *testing.T) {
	ruins := map[string]func(m map[string]store.LocalUser){
		"demoted":          func(m map[string]store.LocalUser) { u := m["admin"]; u.Role = model.RoleOperator; m["admin"] = u },
		"disabled":         func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = true; m["admin"] = u },
		"password changed": func(m map[string]store.LocalUser) { u := m["admin"]; u.PasswordGen++; m["admin"] = u },
	}
	for ruin, doRuin := range ruins {
		t.Run(ruin, func(t *testing.T) {
			e, h := hookedEnv(t)
			adm := e.login("admin", testPassword)
			adm.stepUp(testPassword)
			ops := e.login("ops", opsPassword)
			id := hashID(ops.cookie(CookieInsecure))
			h.before = func() { e.users.cli(doRuin) }
			r := adm.post("/users/sessions", url.Values{"subject": {"local:ops"}, "session": {id}})
			if r.code != http.StatusForbidden {
				t.Fatalf("after the admin was %s: %d, want 403", ruin, r.code)
			}
			if countSessions(e, "local:ops") != 1 {
				t.Fatal("the refused request ended the session")
			}
			if a := e.audit.last(); a.Action != "account.session.end" || a.Result != "denied: acting account changed" {
				t.Errorf("audit %+v", a)
			}
		})
	}
}

// seedSSOUser puts an SSO user into the directory with n live sessions, the
// state an OIDC sign-in leaves behind, without running an OIDC flow in a
// ModeLocal env.
func seedSSOUser(e *env, subject string, n int) {
	e.dir.rows[subject] = store.PrincipalRecord{Subject: subject, DisplayName: subject, Kind: MethodSSO, UpdatedAt: e.clock.Now()}
	for i := 0; i < n; i++ {
		e.sessions.m[subject+"-"+strconv.Itoa(i)] = store.Session{
			IDHash:    subject + "-" + strconv.Itoa(i),
			Principal: model.Principal{Method: MethodSSO, Subject: subject, Name: subject, Role: model.RoleOperator},
			CreatedAt: e.clock.Now(), LastSeen: e.clock.Now(), ExpiresAt: e.clock.Now().Add(time.Hour),
		}
	}
}

// Why (#1227, L6): ending all sessions of an SSO user is an account mutation
// like ending them for a local user. A local admin demoted, disabled or
// re-keyed after its page loaded must not delete anything; the check has to
// sit in the same store statement as the delete, so no request can slip
// through between check and delete. The hook ruins the admin right before the
// delete runs.
func TestEndSSOSessionsRefusedWhenActorChangedMidRequest(t *testing.T) {
	ruins := map[string]func(m map[string]store.LocalUser){
		"demoted":          func(m map[string]store.LocalUser) { u := m["admin"]; u.Role = model.RoleOperator; m["admin"] = u },
		"disabled":         func(m map[string]store.LocalUser) { u := m["admin"]; u.Disabled = true; m["admin"] = u },
		"password changed": func(m map[string]store.LocalUser) { u := m["admin"]; u.PasswordGen++; m["admin"] = u },
	}
	for ruin, doRuin := range ruins {
		t.Run(ruin, func(t *testing.T) {
			e, h := hookedEnv(t)
			adm := e.login("admin", testPassword)
			adm.stepUp(testPassword)
			seedSSOUser(e, "sub-bob", 2)
			seedSSOUser(e, "sub-carol", 1)
			h.before = func() { e.users.cli(doRuin) }
			r := adm.post("/users/sessions", url.Values{"subject": {"sub-bob"}})
			if r.code != http.StatusForbidden {
				t.Fatalf("after the admin was %s: %d, want 403", ruin, r.code)
			}
			if !strings.Contains(r.body, "Your account changed while the request ran. Sign in again.") {
				t.Errorf("the 403 lacks the fixed wording: %s", r.body)
			}
			if countSessions(e, "sub-bob") != 2 || countSessions(e, "sub-carol") != 1 {
				t.Fatalf("the refused request deleted sessions: bob %d carol %d", countSessions(e, "sub-bob"), countSessions(e, "sub-carol"))
			}
			if a := e.audit.last(); a.Action != "account.sessions.end" || a.Result != "denied: acting account changed" {
				t.Errorf("audit %+v", a)
			}
		})
	}
}

// Why (#1227), a guard against over-locking (it passes without the fix, the
// interleave test above is the one that fails): routing the SSO branch through
// the actor check must not cost a valid local admin anything. It still ends
// exactly the sessions of that subject and the audit row carries the count.
func TestLocalAdminEndsAllSessionsOfAnSSOUser(t *testing.T) {
	e, _ := hookedEnv(t)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	e.login("ops", opsPassword)
	seedSSOUser(e, "sub-bob", 2)
	seedSSOUser(e, "sub-carol", 1)
	if r := adm.post("/users/sessions", url.Values{"subject": {"sub-bob"}}); r.code != http.StatusOK {
		t.Fatalf("valid admin: %d %s", r.code, r.body)
	}
	if countSessions(e, "sub-bob") != 0 || countSessions(e, "sub-carol") != 1 || countSessions(e, "local:ops") != 1 || countSessions(e, "local:admin") != 1 {
		t.Fatalf("sessions after: bob %d carol %d ops %d admin %d", countSessions(e, "sub-bob"), countSessions(e, "sub-carol"), countSessions(e, "local:ops"), countSessions(e, "local:admin"))
	}
	if a := e.audit.last(); a.Action != "account.sessions.end" || a.Result != "ok" || !strings.Contains(a.Detail, "sub-bob") || !strings.Contains(a.Detail, "2") {
		t.Errorf("audit %+v", a)
	}
}

// Why (#1203): the check must not cost a valid admin anything. With an
// unchanged actor the one session still ends, and a session that is already
// gone is still the 404 "already ended", not a 403.
func TestEndOneSessionByAValidActorStillWorks(t *testing.T) {
	e, _ := hookedEnv(t)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	ops := e.login("ops", opsPassword)
	id := hashID(ops.cookie(CookieInsecure))
	form := url.Values{"subject": {"local:ops"}, "session": {id}}
	if r := adm.post("/users/sessions", form); r.code != http.StatusOK || countSessions(e, "local:ops") != 0 {
		t.Fatalf("valid admin: %d, ops sessions %d", r.code, countSessions(e, "local:ops"))
	}
	if r := adm.post("/users/sessions", form); r.code != http.StatusNotFound {
		t.Errorf("session already gone: %d, want 404", r.code)
	}
}

// Why (#1162): the guards around the local branch. The own account is refused
// in every spelling of its subject (a case-folded "local:ADMIN" is the same
// account), an unknown name is a 404 and deletes nothing, and a missing
// step-up refuses before anything is looked at.
func TestEndLocalSessionsGuards(t *testing.T) {
	e, _ := hookedEnv(t)
	adm := e.login("admin", testPassword)
	e.login("ops", opsPassword)
	if r := adm.post("/users/sessions", url.Values{"subject": {"local:ops"}}); r.code == http.StatusOK || countSessions(e, "local:ops") != 1 {
		t.Fatalf("without a step-up: %d, ops sessions %d", r.code, countSessions(e, "local:ops"))
	}
	adm.stepUp(testPassword)
	for sub, want := range map[string]int{"local:admin": http.StatusBadRequest, "local:ADMIN": http.StatusBadRequest, "local: Admin ": http.StatusBadRequest, "local:nobody": http.StatusNotFound} {
		if r := adm.post("/users/sessions", url.Values{"subject": {sub}}); r.code != want {
			t.Errorf("subject %q: %d, want %d", sub, r.code, want)
		}
	}
	if r := adm.get("/viewer"); r.code != http.StatusOK {
		t.Error("the admin ended its own session")
	}
	if countSessions(e, "local:ops") != 1 {
		t.Error("a refused request ended ops sessions")
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

// Why (#1162): a store that fails while looking the account up or deleting its rows must not look like
// success, leak the cause into the page, or leave the admin guessing: 500, a generic body, an audit row
// "error" with the same {subject} detail shape as every other outcome of the action, and nothing ended.
func TestEndLocalSessionsStoreFailureIsAnErrorNotASuccess(t *testing.T) {
	boom := errors.New("disk exploded: secret-detail")
	for name, break_ := range map[string]func(h *hookedUsers, hs *hookedSessions){
		"account lookup": func(h *hookedUsers, _ *hookedSessions) { h.readErrFor, h.readErr = "ops", boom },
		"delete":         func(_ *hookedUsers, hs *hookedSessions) { hs.deleteErr = boom },
	} {
		t.Run(name, func(t *testing.T) {
			e, h := hookedEnv(t)
			adm := e.login("admin", testPassword)
			adm.stepUp(testPassword)
			e.login("ops", opsPassword)
			break_(h, e.svc.o.Sessions.(*hookedSessions))
			r := adm.post("/users/sessions", url.Values{"subject": {"local:ops"}})
			if r.code != http.StatusInternalServerError || strings.Contains(r.body, "secret-detail") || strings.Contains(r.body, "Signed out") {
				t.Fatalf("%d %s", r.code, r.body)
			}
			if a := e.audit.last(); a.Action != "account.sessions.end" || a.Result != "error" || !strings.Contains(a.Detail, `"subject":"local:ops"`) {
				t.Errorf("audit %+v", a)
			}
			if countSessions(e, "local:ops") != 1 {
				t.Error("a failed request ended the sessions")
			}
		})
	}
}
