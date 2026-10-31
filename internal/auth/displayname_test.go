package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
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
	renamed.Username = "alice.new"
	if r := again.get(again.startOIDC("/", renamed)); r.code != http.StatusSeeOther {
		t.Fatalf("second login: %d", r.code)
	}
	again.get("/account")
	if d := pages.account; d.DisplayName != "Alice Example" || d.ProviderName != "alice.new" || !d.NameForm {
		t.Fatalf("a sign-in wiped the chosen name: %+v", d)
	}
	if r := setName(again, ""); r.code != http.StatusSeeOther {
		t.Fatalf("clear: %d", r.code)
	}
	again.get("/account")
	if d := pages.account; d.DisplayName != "" || d.ProviderName != "alice.new" {
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

// Why: the name is shown on every page next to other people's names, so characters that make one name
// look like another (zero-width, bidi override, odd spaces), control characters and a very long
// value are refused at the door, with the typed text kept in the form.
func TestDisplayNameValidation(t *testing.T) {
	e, b, pages := ssoWithNamePages(t)
	for name, typed := range map[string]string{"control": "a\x00b", "newline": "a\nb", "too long": strings.Repeat("x", 65),
		"zero width": "Alice\u200b", "bidi override": "\u202eecilA", "no-break space": "a\u00a0b", "ideographic space": "a\u3000b", "line separator": "a\u2028b"} {
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
