package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// namePages keeps the data the account page is given, so a test can see what the web UI shows.
type namePages struct {
	DefaultPages
	account PasswordPageData
}

func (p *namePages) ChangePassword(w http.ResponseWriter, r *http.Request, status int, d PasswordPageData) {
	p.account = d
	p.DefaultPages.ChangePassword(w, r, status, d)
}

func ssoWithNamePages(t *testing.T) (*env, *browser, *namePages) {
	t.Helper()
	pages := &namePages{}
	e := newEnv(t, ModeOIDC, func(o *Options) { o.Pages = pages })
	b := e.browser()
	if r := b.get(b.startOIDC("/", alice)); r.code != http.StatusSeeOther {
		t.Fatalf("oidc login: %d", r.code)
	}
	return e, b, pages
}

func setName(b *browser, name string) resp {
	return b.do(http.MethodPost, "/account/name", url.Values{"csrf": {b.csrfToken()}, "display_name": {name}}, nil)
}

// Why (#283): the name an identity provider sends is rewritten at every sign-in. A person who chose a
// friendlier one must not lose it to the next login, and clearing it must bring the provider name back.
func TestSSODisplayNameSurvivesTheNextSignInAndClearingFallsBack(t *testing.T) {
	e, b, pages := ssoWithNamePages(t)
	if r := setName(b, "  Alice Example  "); r.code != http.StatusSeeOther {
		t.Fatalf("set: %d %s", r.code, r.body)
	}
	if got := e.dir.rows["sub-alice"].Custom; got != "Alice Example" {
		t.Fatalf("stored %q, want the trimmed name", got)
	}
	// A second sign-in, with the provider now sending another name.
	clear(e.sessions.m) // the first browser's session ended; csrfToken() reads the only live one
	again := e.browser()
	renamed := alice
	renamed.Name = "Alice From The IdP"
	if r := again.get(again.startOIDC("/", renamed)); r.code != http.StatusSeeOther {
		t.Fatalf("second login: %d", r.code)
	}
	again.get("/account")
	if d := pages.account; d.DisplayName != "Alice Example" || d.ProviderName != "Alice From The IdP" || !d.NameForm {
		t.Fatalf("a sign-in wiped the chosen name: %+v", d)
	}
	if r := setName(again, ""); r.code != http.StatusSeeOther {
		t.Fatalf("clear: %d", r.code)
	}
	again.get("/account")
	if d := pages.account; d.DisplayName != "" || d.ProviderName != "Alice From The IdP" {
		t.Fatalf("after clearing the provider name applies: %+v", d)
	}
}

// Why (#283): a display name is presentation. The audit trail must keep naming the actor by the
// stable subject, with no display name in the row, or a rename could pass as another person.
func TestSettingADisplayNameAuditsTheStableIdentity(t *testing.T) {
	e, b, _ := ssoWithNamePages(t)
	if r := setName(b, "Somebody Else"); r.code != http.StatusSeeOther {
		t.Fatalf("set: %d", r.code)
	}
	a := e.audit.last()
	if a.Action != "account.display_name" || a.Actor != "sub-alice" || strings.Contains(a.Detail, "Somebody") {
		t.Fatalf("audit %+v", a)
	}
}

// Why: the name is shown on every page next to other people's names, so invisible characters (zero-width,
// bidi override), control characters and a very long
// value are refused at the door, with the typed text kept in the form.
func TestDisplayNameValidation(t *testing.T) {
	e, b, pages := ssoWithNamePages(t)
	for name, typed := range map[string]string{"control": "a\x00b", "newline": "a\nb", "too long": strings.Repeat("x", 65), "zero width": "Alice\u200b", "bidi override": "\u202eecilA"} {
		if r := setName(b, typed); r.code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, r.code)
		}
		if pages.account.ErrField != "display_name" || pages.account.DisplayName != typed {
			t.Errorf("%s: form data %+v", name, pages.account)
		}
	}
	if got := e.dir.rows["sub-alice"].Custom; got != "" {
		t.Errorf("a refused name was stored: %q", got)
	}
}

// Why: local accounts already own a profile form (and keep their login name); the SSO route must
// not become a second way to write them, and nobody can set another person's name.
func TestLocalAccountsDoNotUseTheSSONameRoute(t *testing.T) {
	e := localEnv(t)
	b := e.login("admin", testPassword)
	r := b.post("/account/name", url.Values{"display_name": {"Boss"}})
	if r.code != http.StatusForbidden {
		t.Fatalf("local POST /account/name: %d, want 403", r.code)
	}
}

func TestOnlyTheOwnNameIsWritable(t *testing.T) {
	e, b, _ := ssoWithNamePages(t)
	e.dir.rows["sub-bob"] = e.dir.rows["sub-alice"]
	bob := e.dir.rows["sub-bob"]
	bob.Subject, bob.DisplayName = "sub-bob", "bob"
	e.dir.rows["sub-bob"] = bob
	// A crafted "subject" field must be ignored: the name is always the signed-in person's.
	b.do(http.MethodPost, "/account/name", url.Values{"csrf": {b.csrfToken()}, "display_name": {"Mallory"}, "subject": {"sub-bob"}}, nil)
	if got := e.dir.rows["sub-bob"].Custom; got != "" {
		t.Fatalf("another person's name was written: %q", got)
	}
	if got := e.dir.rows["sub-alice"].Custom; got != "Mallory" {
		t.Fatalf("own name %q", got)
	}
}

// Why (#283): the identity provider's full name (the "name" claim) is the default display name at every
// sign-in, ahead of the user name, and follows a rename at the provider.
func TestSSOSignInTakesTheNameClaimAsDisplayName(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	b := e.browser()
	full := alice
	full.Name = "Alice Example"
	if r := b.get(b.startOIDC("/", full)); r.code != http.StatusSeeOther {
		t.Fatalf("login: %d", r.code)
	}
	if got := e.dir.rows["sub-alice"]; got.DisplayName != "Alice Example" || got.Subject != "sub-alice" {
		t.Fatalf("recorded %+v, want the name claim under the unchanged subject", got)
	}
	if s := e.sessions.all(); len(s) != 1 || s[0].Principal.Name != "Alice Example" {
		t.Fatalf("the session shows %+v", s)
	}
}

// Why (#283): without a name claim the user name is the fallback, as before.
func TestSSOSignInFallsBackToTheUsernameWithoutANameClaim(t *testing.T) {
	e := newEnv(t, ModeOIDC, func(o *Options) { o.OIDC.NameClaim = "no_such_claim" })
	b := e.browser()
	if r := b.get(b.startOIDC("/", alice)); r.code != http.StatusSeeOther {
		t.Fatalf("login: %d", r.code)
	}
	if got := e.dir.rows["sub-alice"].DisplayName; got != "alice" {
		t.Fatalf("recorded %q, want the user name", got)
	}
}

// Why (#283): a name an identity provider or proxy sends is shown on every page and in the audit labels,
// so it follows the same rules as a typed one. One with a zero-width or control character, or an absurd
// length, is dropped and the login name shows instead; the sign-in itself is not refused.
func TestIdPNamesFollowTheDisplayNameRules(t *testing.T) {
	for name, claim := range map[string]string{"zero width": "Alice\u200bX", "bidi": "\u202eecilA", "control": "A\x07B", "too long": strings.Repeat("n", 65)} {
		e := newEnv(t, ModeOIDC)
		b := e.browser()
		u := alice
		u.Name = claim
		if r := b.get(b.startOIDC("/", u)); r.code != http.StatusSeeOther {
			t.Fatalf("%s: login refused: %d", name, r.code)
		}
		if got := e.dir.rows["sub-alice"].DisplayName; got != "alice" {
			t.Errorf("%s: recorded %q, want the login name", name, got)
		}
	}
	e := newEnv(t, ModeHeader)
	hdr := map[string]string{"X-authentik-username": "boss", "X-authentik-name": "Bo\u200bss", "X-authentik-groups": "fleet-admins"}
	e.browser().do(http.MethodGet, "/account", nil, hdr)
	if got := e.dir.rows["boss"].DisplayName; got != "boss" {
		t.Errorf("header: recorded %q, want the login name", got)
	}
	hdr["X-authentik-name"] = "  Bo Ss  "
	e.clock.Advance(headerSeenEvery + time.Minute)
	e.browser().do(http.MethodGet, "/account", nil, hdr)
	if got := e.dir.rows["boss"].DisplayName; got != "Bo Ss" {
		t.Errorf("header: recorded %q, want the trimmed name", got)
	}
}
