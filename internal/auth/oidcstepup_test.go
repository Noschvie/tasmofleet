package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

// beginOIDCStepUp posts the step-up start and follows the IdP redirect; it
// returns the callback URL (the IdP's answer for user u).
func beginOIDCStepUp(t *testing.T, b *browser, u oidcmock.User) string {
	t.Helper()
	b.e.idp.NextLogin(u)
	r := b.do(http.MethodPost, "/oidc/stepup", url.Values{"csrf": {b.token()}, "next": {"/import/tasmoadmin"}}, nil)
	if r.code != http.StatusFound {
		t.Fatalf("start: %d %s", r.code, r.body)
	}
	r = b.get(r.loc)
	if r.code != http.StatusFound || !strings.HasPrefix(r.loc, b.e.srv.URL+"/oidc/callback") {
		t.Fatalf("authorize: %d %s", r.code, r.loc)
	}
	return r.loc
}

func oidcAdmin(t *testing.T) (*env, *browser) {
	t.Helper()
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	b.get(b.startOIDC("/", alice))
	return e, b
}

func stepped(b *browser) bool { return strings.Contains(b.post("/stepped", nil).body, "stepped=true") }

// Why: the OIDC step-up is the IdP-side twin of retyping the password. A
// fresh authentication must grant it, ask the IdP to really re-authenticate
// (prompt=login, max_age=0) and leave the existing session untouched.
func TestOIDCStepUpGrantsAfterFreshAuthentication(t *testing.T) {
	e, b := oidcAdmin(t)
	if stepped(b) {
		t.Fatal("fresh session must not be stepped up")
	}
	sess := b.cookie(CookieInsecure)
	r := b.get(beginOIDCStepUp(t, b, alice))
	if r.code != http.StatusSeeOther || r.loc != "/import/tasmoadmin" {
		t.Fatalf("callback: %d %q %s", r.code, r.loc, r.body)
	}
	reqs := e.idp.AuthorizeRequests()
	last := reqs[len(reqs)-1]
	if last.Get("prompt") != "login" || last.Get("max_age") != "0" {
		t.Fatalf("authorization request lacks re-authentication: %v", last)
	}
	if !stepped(b) {
		t.Fatal("no step-up after a fresh authentication")
	}
	if b.cookie(CookieInsecure) != sess || len(e.sessions.all()) != 1 {
		t.Fatal("a step-up must not create or replace the session")
	}
	e.clock.Advance(6 * time.Minute)
	if stepped(b) {
		t.Fatal("step-up must expire after 5 minutes")
	}
}

// Why: prompt and max_age are only requests. If the IdP ignores them, the
// callback must still refuse: missing, stale or foreign authentications, a
// different account or a demoted group never grant a step-up.
func TestOIDCStepUpRefusesUntrustedAuthentications(t *testing.T) {
	cases := []struct {
		name   string
		user   oidcmock.User
		tamper func(map[string]any)
		want   string
	}{
		{"no auth_time", alice, func(c map[string]any) { delete(c, "auth_time") }, "auth_time"},
		{"stale auth_time", alice, func(c map[string]any) {
			c["auth_time"] = float64(time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC).Unix())
		}, "reused"},
		{"other account", oidcmock.User{Subject: "sub-mallory", Username: "mallory", Groups: []string{"fleet-admins"}}, nil, "different account"},
		{"demoted", oidcmock.User{Subject: alice.Subject, Username: "alice", Groups: []string{"fleet-ops"}}, nil, "no longer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, b := oidcAdmin(t)
			loc := beginOIDCStepUp(t, b, tc.user)
			if tc.tamper != nil {
				e.idp.TamperNextIDToken(tc.tamper)
			}
			r := b.get(loc)
			if r.code != http.StatusForbidden || !strings.Contains(r.body, tc.want) {
				t.Fatalf("%d %s", r.code, r.body)
			}
			if stepped(b) || len(e.sessions.all()) != 1 {
				t.Fatal("refused step-up granted something or changed the sessions")
			}
			if a := e.audit.last(); !strings.HasPrefix(a.Result, "denied") {
				t.Fatalf("refusal not audited: %+v", a)
			}
		})
	}
}

// Why: the flow cookie must be useless in any other session (a step-up
// started in one browser cannot be completed by or for another).
func TestOIDCStepUpFlowIsBoundToTheSession(t *testing.T) {
	e, b := oidcAdmin(t)
	loc := beginOIDCStepUp(t, b, alice)
	flowCookie := b.cookie(e.flowName())
	other := e.browser()
	other.get(other.startOIDC("/", alice)) // a second session of the same account
	other.setCookie(e.flowName(), flowCookie)
	if r := other.get(loc); r.code != http.StatusForbidden || stepped(other) {
		t.Fatalf("foreign session completed the step-up: %d", r.code)
	}
}

// Why: only OIDC admins may start the flow; operators and local accounts have
// no business at the IdP step-up, and local sessions must not get one.
func TestOIDCStepUpStartRequiresOIDCAdmin(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	op := e.browser()
	op.get(op.startOIDC("/", bob))
	if r := op.do(http.MethodPost, "/oidc/stepup", url.Values{"csrf": {op.token()}}, nil); r.code != http.StatusForbidden {
		t.Fatalf("operator: %d", r.code)
	}
	if r := e.browser().do(http.MethodPost, "/oidc/stepup", url.Values{}, nil); r.code != http.StatusForbidden {
		t.Fatalf("anonymous: %d", r.code)
	}
	loc := e.login("admin", testPassword)
	if r := loc.do(http.MethodPost, "/oidc/stepup", url.Values{"csrf": {loc.token()}}, nil); r.code != http.StatusForbidden {
		t.Fatalf("local account in OIDC mode: %d", r.code)
	}
}
