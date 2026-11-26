package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
)

// The sign-in, setup and temporary password pages of #1111: one first action, the words a person
// can act on, and one name for the temporary password.

func signInBody(t *testing.T, d auth.LoginData) string {
	t.Helper()
	w := httptest.NewRecorder()
	NewPages().Login(w, httptest.NewRequest(http.MethodGet, "/login", nil), http.StatusOK, d)
	return w.Body.String()
}

// primaryButtons are the primary buttons and links of a page, in page order.
var primaryButtons = regexp.MustCompile(`<(?:a|button)\b[^>]*class="btn btn-primary[^"]*"[^>]*>[^<]*`)

// Why (#1111, A7-023): with single sign-on on, local accounts are the break-glass way in. The page
// used to put the username and password first and the single sign-on button below an "or" rule, so
// everyone saw the break-glass form as the way in. Single sign-on is now the one primary action,
// named after the provider (or "single sign-on", never the bare abbreviation), and the local
// Sign in is a default button: one page, one primary.
func TestSignInPutsSingleSignOnFirst(t *testing.T) {
	for name, label := range map[string]string{"Authentik": "Sign in with Authentik", "Single sign-on": "Sign in with single sign-on"} {
		body := signInBody(t, auth.LoginData{BasePath: "/tf", Local: true, SSO: name, CSRFToken: "tok"})
		primaries := primaryButtons.FindAllString(body, -1)
		if len(primaries) != 1 {
			t.Fatalf("%q: want exactly one primary button, got %q", name, primaries)
		}
		if !strings.Contains(primaries[0], `id="sso"`) || !strings.HasSuffix(primaries[0], ">"+label) {
			t.Errorf("%q: the primary must be the single sign-on link %q, got %q", name, label, primaries[0])
		}
		sso, user := strings.Index(body, `id="sso"`), strings.Index(body, `name="username"`)
		if user < 0 || sso > user {
			t.Errorf("%q: single sign-on must come before the local form (sso at %d, username at %d)", name, sso, user)
		}
		if !strings.Contains(body, `<button type="submit" class="btn btn-tall">Sign in</button>`) {
			t.Errorf("%q: the local Sign in must be a default button", name)
		}
		if strings.Contains(body, "SSO") || strings.Contains(body, "or-rule") {
			t.Errorf("%q: no bare SSO and no or rule on the sign-in page: %s", name, body)
		}
	}
	// single sign-on alone: its button is the primary, under the new name
	only := primaryButtons.FindAllString(signInBody(t, auth.LoginData{BasePath: "/tf", SSO: "Authentik"}), -1)
	if len(only) != 1 || !strings.HasSuffix(only[0], ">Sign in with Authentik") {
		t.Errorf("single sign-on only: %q", only)
	}
	// local alone: today's form, its Sign in the primary, no fold
	local := signInBody(t, auth.LoginData{BasePath: "/tf", Local: true, CSRFToken: "tok"})
	if p := primaryButtons.FindAllString(local, -1); len(p) != 1 || !strings.HasSuffix(p[0], ">Sign in") {
		t.Errorf("local only: the local Sign in must be the one primary, got %q", p)
	}
	if strings.Contains(local, "<details") || strings.Contains(local, `id="sso"`) {
		t.Errorf("local only: no fold and no single sign-on: %s", local)
	}
}

// localFold is the opening tag of the fold that holds the local form.
var localFold = regexp.MustCompile(`<details class="fold auth-local"[^>]*>`)

// Why (#1111, A7-023): the local form sits in a fold "Sign in with a local account" while single
// sign-on is on. A closed fold would hide the answer to a local attempt: the wrong password with its
// attempts line, the lockout, or what the person typed. So the fold is open whenever the page
// answers a local attempt, and closed on the first view (with nothing focused inside it, since
// autofocus in a closed fold goes nowhere). Only local sign-in fills LoginData.Error: a failed
// single sign-on ends on the message page, so an error here is always a local answer.
func TestSignInLocalFoldOpensForALocalAnswer(t *testing.T) {
	base := auth.LoginData{BasePath: "/tf", Local: true, SSO: "Authentik", CSRFToken: "tok"}
	first := signInBody(t, base)
	fold := localFold.FindString(first)
	if fold == "" || strings.Contains(fold, " open") || !strings.Contains(first, "<summary>Sign in with a local account</summary>") {
		t.Fatalf("first view: want a closed fold \"Sign in with a local account\", got %q in %s", fold, first)
	}
	if strings.Contains(first, "autofocus") {
		t.Error("first view: nothing in the closed fold may take the focus")
	}

	wrong := base
	wrong.Username, wrong.Error, wrong.ShowAttemptsLeft, wrong.AttemptsLeft = "maren", "Wrong username or password.", true, 4
	body := signInBody(t, wrong)
	if !strings.Contains(localFold.FindString(body), " open") {
		t.Errorf("a wrong password: the fold must be open: %s", body)
	}
	if !strings.Contains(inputTag(t, body, "password"), "aria-invalid") || strings.Contains(body, "banner t-danger") {
		t.Error("a wrong password: the error belongs under Password, not in a banner")
	}
	if !strings.Contains(inputTag(t, body, "password"), "autofocus") || strings.Count(body, "autofocus") != 1 {
		t.Error("a wrong password: the focus goes to the password, and only there")
	}

	locked := base
	locked.Error, locked.ShowAttemptsLeft, locked.AttemptsLeft = "Too many attempts. Wait a minute and try again.", true, 0
	body = signInBody(t, locked)
	if !strings.Contains(localFold.FindString(body), " open") {
		t.Errorf("a lockout: the fold must be open: %s", body)
	}
	if b, sso := strings.Index(body, `role="alert"`), strings.Index(body, `id="sso"`); b < 0 || b > sso {
		t.Errorf("a lockout: its banner stands above the single sign-on button (banner at %d, sso at %d)", b, sso)
	}

	typed := base
	typed.Username = "maren"
	if !strings.Contains(localFold.FindString(signInBody(t, typed)), " open") {
		t.Error("a typed username: the fold must be open")
	}
}

// Why (#1111, A7-023): there is no reset by email on purpose; an admin resets a local password. A
// person who forgot it found nothing on the page that said so. The line belongs to local accounts:
// a page with single sign-on only has no password to forget.
func TestSignInSaysWhoResetsAForgottenPassword(t *testing.T) {
	const forgot = "Forgot your password? Ask an admin to reset it."
	for name, d := range map[string]auth.LoginData{
		"local":            {BasePath: "/tf", Local: true},
		"local and SSO":    {BasePath: "/tf", Local: true, SSO: "Authentik"},
		"single sign-on":   {BasePath: "/tf", SSO: "Authentik"},
		"sign-in disabled": {BasePath: "/tf"},
	} {
		if got, want := strings.Count(signInBody(t, d), forgot), map[bool]int{true: 1}[d.Local]; got != want {
			t.Errorf("%s: the forgot line appears %d times, want %d", name, got, want)
		}
	}
}

// Why (#1111, A7-019): the first screen of the product asked for "docker logs <container> ...", a
// placeholder the person had to fill in, and assumed the Docker command line. The page now says
// where the link is in one sentence and names both ways to read the log, the Docker command with a
// Copy button. That button is hidden until copy.js shows it, so the standalone card must load
// copy.js, or Copy never appears.
func TestSetupLinkPageNamesTheLogWithoutPlaceholders(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		body := setupAccountBody(t, auth.SetupAccountData{BasePath: "/tf", NeedToken: true, TokenInvalid: invalid, Username: "admin"})
		for _, bad := range []string{"<container>", "&lt;container&gt;", "grep", "$ "} {
			if strings.Contains(body, bad) {
				t.Errorf("invalid=%v: the page still shows the shell placeholder %q", invalid, bad)
			}
		}
		for _, want := range []string{
			"Open the setup link from the TasmoFleet log. It is printed once at start.",
			`docker logs tasmofleet`,
			`data-copy-text="docker logs tasmofleet"`,
			"open the container’s log",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("invalid=%v: missing %q", invalid, want)
			}
		}
		if n := strings.Count(body, "data-copy-"); n != 1 {
			t.Errorf("invalid=%v: want one Copy button (the Docker command; the Unraid line has nothing to copy), got %d", invalid, n)
		}
		if !regexp.MustCompile(`<script src="/tf/static/copy\.js(\?v=\w+)?" defer></script>`).MatchString(body) {
			t.Errorf("invalid=%v: the card does not load copy.js, so Copy stays hidden: %s", invalid, body)
		}
	}
}

// Why (#1111, A7-021): the forced change listed three rules with hollow circles that looked like a
// live checklist and never ticked. The rule is now the help line under New password, built from
// the one minimum the server checks, and the secret has one name: the temporary password.
func TestForcedChangeExplainsTheRuleOnce(t *testing.T) {
	w := httptest.NewRecorder()
	NewPages().ChangePassword(w, httptest.NewRequest(http.MethodGet, "/account/password", nil), http.StatusOK, auth.PasswordPageData{Forced: true, Self: "ali", CSRFToken: "tok"})
	body := w.Body.String()
	if strings.Contains(body, "pw-checks") || strings.Contains(body, "Both entries match") {
		t.Errorf("the static checklist is still there: %s", body)
	}
	help := "Use at least " + strconv.Itoa(auth.PasswordMinLength) + " characters, different from the temporary one."
	if strings.Count(body, help) != 1 {
		t.Errorf("the help line %q must appear once: %s", help, body)
	}
	if !strings.Contains(body, ">Temporary password</") || strings.Contains(strings.ToLower(body), "one-time") {
		t.Errorf("the current secret must be labelled Temporary password: %s", body)
	}
}

// Why (#1111, A7-022): "one-time" and "temporary" named the same generated secret on the admin's
// and on the user's side. It is "temporary password" on every page an admin sees it on: the add
// form, the reset dialog, the user's facts (valid and expired) and the page that shows it. The add
// form's "Set an initial password" names the other option, a password the admin chooses, and stays.
func TestTemporaryPasswordHasOneName(t *testing.T) {
	user := auth.UserRow{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", MustChangePassword: true}
	detail := func(expired bool) string {
		return userDetailBody(t, auth.UserDetailData{BasePath: "/tf", CSRFToken: "tok", Self: "local:admin", User: user, Guards: allowed,
			OTPExpiresAt: time.Now().Add(time.Hour), OTPExpired: expired})
	}
	pages := map[string]string{
		"add user":   renderUsersPage(t, auth.UsersPageData{BasePath: "/tf", CSRFToken: "tok", New: true, OTP: true}),
		"users list": renderUsersPage(t, usersFixture()),
		"user page":  detail(false),
		"expired":    detail(true),
		"otp page": renderUserPage(t, func(p *Pages, w http.ResponseWriter, r *http.Request) {
			p.OneTimePassword(w, r, http.StatusOK, auth.OTPPageData{BasePath: "/tf", User: "ops", Password: "otp-fake-1", ExpiresAt: time.Now().Add(time.Hour)})
		}).Body.String(),
	}
	for name, body := range pages {
		low := strings.ToLower(body)
		if strings.Contains(low, "one-time") {
			t.Errorf("%s still says one-time: %s", name, body)
		}
		if !strings.Contains(low, "temporary password") {
			t.Errorf("%s does not name the temporary password", name)
		}
	}
}

// Why (#1111, A7-033): the page that shows a generated password was titled "One-time password" in
// the tab and the trail while its heading said "Password reset for lena". Title and crumb now say
// what happened, like the heading. Checked on the helper the handler uses, not on a rendered
// trail: the trail's length rules (#1106) decide whether the crumb is drawn.
func TestTemporaryPasswordPageIsNamedLikeItsHeading(t *testing.T) {
	for _, c := range []struct {
		created bool
		want    string
	}{{false, "Password reset"}, {true, "Account created"}} {
		title, crumbs := otpPageNames(auth.OTPPageData{Created: c.created})
		if title != c.want || len(crumbs) != 1 || crumbs[0].Label != c.want || crumbs[0].Path != "" {
			t.Errorf("created=%v: title %q, crumbs %+v, want %q", c.created, title, crumbs, c.want)
		}
	}
	body := renderUserPage(t, func(p *Pages, w http.ResponseWriter, r *http.Request) {
		p.OneTimePassword(w, r, http.StatusOK, auth.OTPPageData{BasePath: "/tf", User: "lena", Password: "otp-fake-2", ExpiresAt: time.Now().Add(time.Hour)})
	}).Body.String()
	if !strings.Contains(body, "<title>Password reset - TasmoFleet</title>") {
		t.Errorf("the tab is not named Password reset: %s", body)
	}
}
