package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

const (
	opsPassword = "operator password 1"
	newPassword = "brand new password 2"
)

// localEnv is a standalone installation: AUTH_MODE=local, no PUBLIC_URL,
// no IdP. Accounts: admin (testPassword) and ops (operator, opsPassword).
func localEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := newEnv(t, ModeLocal, func(o *Options) {
		o.Proxy.PublicURL = ""
		o.DataDir = dir
	})
	opsHash, err := HashPassword(opsPassword)
	if err != nil {
		t.Fatal(err)
	}
	e.users.cli(func(m map[string]store.LocalUser) {
		m["ops"] = store.LocalUser{Username: "ops", PasswordHash: opsHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator}
	})
	return e
}

var nextIP = 10

// login signs a fresh browser in as user. Each browser comes from its own
// address, so the tests do not trip over each other's rate limits.
func (e *env) login(user, pass string) *browser {
	e.t.Helper()
	nextIP++
	b := e.browser()
	r := postLocal(b, "192.0.2."+strconv.Itoa(nextIP), user, pass)
	if r.code != http.StatusSeeOther {
		e.t.Fatalf("login %s: %d %s", user, r.code, r.body)
	}
	return b
}

// token is the CSRF token of this browser's session.
func (b *browser) token() string {
	b.t.Helper()
	s, err := b.e.sessions.GetSession(b.t.Context(), hashID(b.cookie(CookieInsecure)))
	if err != nil {
		b.t.Fatalf("no session for this browser: %v", err)
	}
	return s.CSRFToken
}

func (b *browser) post(path string, form url.Values) resp {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", b.token())
	return b.do(http.MethodPost, path, form, nil)
}

func (b *browser) stepUp(pass string) {
	b.t.Helper()
	if r := b.post("/users/stepup", url.Values{"current": {pass}}); r.code != http.StatusSeeOther {
		b.t.Fatalf("step-up: %d %s", r.code, r.body)
	}
}

// Why: a standalone installation (no proxy, no IdP) must be usable and safe
// from the first start: the generated password is not in the log, the file
// holding it is private, the forced change blocks every other page, and the
// file disappears once the password is changed.
func TestStandaloneFirstRun(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := localEnv(t)
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	dir := e.svc.o.DataDir
	created, err := e.svc.EnsureInitialAdmin(t.Context(), "")
	if err != nil || !created {
		t.Fatalf("EnsureInitialAdmin = %v, %v", created, err)
	}
	path := filepath.Join(dir, InitialPasswordFile)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("password file: %v, %v (want mode 0600)", fi, err)
	}
	raw, _ := os.ReadFile(path)
	pw := strings.TrimSpace(string(raw))
	if len(pw) != generatedPasswordLen {
		t.Fatalf("generated password has %d characters", len(pw))
	}
	if strings.Contains(logs.String(), pw) {
		t.Fatalf("the generated password is in the log (logs are shipped and kept): %s", logs.String())
	}
	if !strings.Contains(logs.String(), path) {
		t.Errorf("the log should name the file: %s", logs.String())
	}
	// A second start does not create another account or overwrite the file.
	if again, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil || again {
		t.Fatalf("second start: %v, %v", again, err)
	}

	b := e.browser()
	r := postLocal(b, "192.0.2.50", "admin", pw)
	if r.code != http.StatusSeeOther || r.loc != "/account" {
		t.Fatalf("first login: %d %q, want a redirect to the forced change", r.code, r.loc)
	}
	// Every other page is blocked until the password is changed.
	if r := b.get("/viewer"); r.code != http.StatusSeeOther || r.loc != "/account" {
		t.Errorf("GET /viewer: %d %q", r.code, r.loc)
	}
	if r := b.post("/admin-action", url.Values{}); r.code != http.StatusForbidden {
		t.Errorf("POST /admin-action: %d", r.code)
	}
	if r := b.get("/users"); r.code != http.StatusSeeOther || r.loc != "/account" {
		t.Errorf("GET /users: %d %q", r.code, r.loc)
	}
	if r := b.get("/account"); r.code != http.StatusOK || !strings.Contains(r.body, "must choose a new password") {
		t.Fatalf("change form: %d %s", r.code, r.body)
	}
	// A second browser signed in with the same account is ended by the change.
	other := e.browser()
	if r := postLocal(other, "192.0.2.51", "admin", pw); r.code != http.StatusSeeOther {
		t.Fatal(r.code)
	}
	if r := b.post("/account/password", url.Values{"current": {pw}, "new": {newPassword}, "confirm": {newPassword}}); r.code != http.StatusSeeOther || r.loc != "/" {
		t.Fatalf("change: %d %q %s", r.code, r.loc, r.body)
	}
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Errorf("after the change the user is in: %d", r.code)
	}
	if r := other.get("/viewer"); r.code == http.StatusOK {
		t.Error("the other browser's session survived the password change")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the initial password file survived the change: %v", err)
	}
	if r := postLocal(e.browser(), "192.0.2.52", "admin", pw); r.code != http.StatusUnauthorized {
		t.Errorf("the generated password still works: %d", r.code)
	}
}

// Why: when the account already changed its password (or was deleted) a
// leftover file would keep a dead secret on disk.
func TestInitialPasswordFileRemovedAtStartupWhenSpent(t *testing.T) {
	e := localEnv(t)
	path := filepath.Join(e.svc.o.DataDir, InitialPasswordFile)
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "admin" exists and has no pending change.
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file kept although the account changed its password: %v", err)
	}
}

func TestInitialAdminFromEnvPasswordNeedsNoFile(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	e := localEnv(t)
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if created, err := e.svc.EnsureInitialAdmin(t.Context(), "from the environment 1"); err != nil || !created {
		t.Fatal(created, err)
	}
	u := e.users.users[InitialAdminName]
	if !u.MustChangePassword || u.Role != model.RoleAdmin || VerifyPassword(u.PasswordHash, "from the environment 1") != nil {
		t.Fatalf("initial admin = %+v", u)
	}
	if _, err := os.Stat(filepath.Join(e.svc.o.DataDir, InitialPasswordFile)); !os.IsNotExist(err) {
		t.Errorf("a password file was written for an operator-chosen password: %v", err)
	}
	if strings.Contains(logs.String(), "from the environment") {
		t.Error("INITIAL_ADMIN_PASSWORD is in the log")
	}
	// Too short is refused loudly instead of creating a weak admin.
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if _, err := e.svc.EnsureInitialAdmin(t.Context(), "short"); err == nil {
		t.Fatal("a short INITIAL_ADMIN_PASSWORD was accepted")
	}
}

// Only local mode (or an explicit INITIAL_ADMIN_PASSWORD) seeds an account;
// an OIDC installation must not get a surprise admin.
func TestNoSurpriseAdminInOIDCMode(t *testing.T) {
	e := newEnv(t, ModeOIDC)
	e.users.cli(func(m map[string]store.LocalUser) { clear(m) })
	if created, err := e.svc.EnsureInitialAdmin(t.Context(), ""); err != nil || created {
		t.Fatalf("created = %v, %v", created, err)
	}
}

// Why: an operator must not manage accounts, however the request is
// crafted (spec §15.3): not create, not raise, not reset another password.
func TestOperatorCannotManageAccounts(t *testing.T) {
	e := localEnv(t)
	ops := e.login("ops", opsPassword)
	probes := []struct {
		method, path string
		form         url.Values
	}{
		{"GET", "/users", nil},
		{"POST", "/users", url.Values{"username": {"mallory"}, "password": {"mallory password 1"}, "role": {"admin"}}},
		{"POST", "/users/stepup", url.Values{"current": {opsPassword}}},
		{"POST", "/users/local/ops", url.Values{"action": {"role"}, "role": {"admin"}}},
		{"POST", "/users/local/admin", url.Values{"action": {"password"}, "password": {"mallory password 1"}}},
		{"POST", "/users/local/admin", url.Values{"action": {"delete"}}},
		{"POST", "/users/local/admin", url.Values{"action": {"disable"}}},
		{"POST", "/users/sessions", url.Values{"subject": {"local:admin"}}},
	}
	for _, p := range probes {
		var r resp
		if p.method == "GET" {
			r = ops.get(p.path)
		} else {
			r = ops.post(p.path, p.form)
		}
		if r.code != http.StatusForbidden {
			t.Errorf("%s %s as operator: %d, want 403", p.method, p.path, r.code)
		}
	}
	if len(e.users.users) != 2 || e.users.users["ops"].Role != model.RoleOperator || e.users.users["admin"].Disabled {
		t.Fatalf("an operator changed the accounts: %+v", e.users.users)
	}
}

// Why: an operator may change only their own password, and only with the
// current one; extra fields must not smuggle in a role or another account.
func TestOwnPasswordChange(t *testing.T) {
	e := localEnv(t)
	ops := e.login("ops", opsPassword)
	laptop := e.login("ops", opsPassword)
	adminOldHash := e.users.users["admin"].PasswordHash

	cases := []struct {
		name string
		form url.Values
		want int
	}{
		{"wrong current password", url.Values{"current": {"not the password"}, "new": {newPassword}, "confirm": {newPassword}}, http.StatusForbidden},
		{"no current password", url.Values{"new": {newPassword}, "confirm": {newPassword}}, http.StatusForbidden},
		{"confirmation differs", url.Values{"current": {opsPassword}, "new": {newPassword}, "confirm": {"something else 123"}}, http.StatusBadRequest},
		{"same password", url.Values{"current": {opsPassword}, "new": {opsPassword}, "confirm": {opsPassword}}, http.StatusBadRequest},
		{"too short", url.Values{"current": {opsPassword}, "new": {"short"}, "confirm": {"short"}}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if r := ops.post("/account/password", c.form); r.code != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.code, c.want)
		}
	}
	if VerifyPassword(e.users.users["ops"].PasswordHash, opsPassword) != nil {
		t.Fatal("a refused change altered the password")
	}
	r := ops.post("/account/password", url.Values{
		"current": {opsPassword}, "new": {newPassword}, "confirm": {newPassword},
		"role": {"admin"}, "username": {"admin"}, "action": {"role"}, // crafted extras are ignored
	})
	if r.code != http.StatusSeeOther {
		t.Fatalf("change: %d %s", r.code, r.body)
	}
	if u := e.users.users["ops"]; u.Role != model.RoleOperator || VerifyPassword(u.PasswordHash, newPassword) != nil {
		t.Fatalf("ops = %+v", u)
	}
	if e.users.users["admin"].PasswordHash != adminOldHash {
		t.Fatal("another account's password changed")
	}
	if r := laptop.get("/viewer"); r.code == http.StatusOK {
		t.Error("the other device stayed signed in after a password change")
	}
	if r := ops.get("/viewer"); r.code != http.StatusOK {
		t.Errorf("this browser was signed out: %d", r.code)
	}
	if r := ops.post("/admin-action", url.Values{}); r.code != http.StatusForbidden {
		t.Errorf("operator became admin: %d", r.code)
	}
}

// Why: the current password is a guessing oracle for anyone holding a
// session, so it is rate limited like a login.
func TestOwnPasswordChangeIsRateLimited(t *testing.T) {
	e := localEnv(t)
	ops := e.login("ops", opsPassword)
	var limited bool
	for range 8 {
		r := ops.post("/account/password", url.Values{"current": {"wrong guess 123"}, "new": {newPassword}, "confirm": {newPassword}})
		if r.code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("8 wrong current passwords were all checked")
	}
	if r := ops.post("/account/password", url.Values{"current": {opsPassword}, "new": {newPassword}, "confirm": {newPassword}}); r.code != http.StatusTooManyRequests {
		t.Errorf("even the right password waits while limited: %d", r.code)
	}
}

// Why: step-up limits what a hijacked admin session can do within minutes
// (spec §15.1): every change needs the password typed again, valid 5 min.
func TestAdminStepUp(t *testing.T) {
	e := localEnv(t)
	adm := e.login("admin", testPassword)
	create := url.Values{"username": {"Newbie"}, "password": {newPassword}, "role": {"operator"}}

	if r := adm.get("/users"); r.code != http.StatusOK || !strings.Contains(r.body, "ops") || !strings.Contains(r.body, "/users/stepup") || strings.Contains(r.body, "/users/local/") {
		t.Fatalf("without step-up the list is open, the changes are locked behind the password prompt: %d %s", r.code, r.body)
	}
	if r := adm.post("/users", create); r.code != http.StatusForbidden || len(e.users.users) != 2 {
		t.Fatalf("create without step-up: %d, users %d", r.code, len(e.users.users))
	}
	if r := adm.post("/users/stepup", url.Values{"current": {"wrong"}}); r.code != http.StatusForbidden {
		t.Fatalf("wrong step-up password: %d", r.code)
	}
	adm.stepUp(testPassword)
	if r := adm.get("/users"); r.code != http.StatusOK || !strings.Contains(r.body, "/users/local/ops") || strings.Contains(r.body, "/users/stepup") {
		t.Fatalf("list after step-up offers the changes: %d %s", r.code, r.body)
	}
	e.clock.Advance(4 * time.Minute)
	if r := adm.post("/users", create); r.code != http.StatusOK {
		t.Fatalf("create within 5 minutes: %d %s", r.code, r.body)
	}
	u := e.users.users["newbie"]
	if u.Role != model.RoleOperator || !u.MustChangePassword {
		t.Fatalf("newbie = %+v: names are lower-cased and an admin-chosen password must be changed", u)
	}
	e.clock.Advance(2 * time.Minute) // 6 minutes since the step-up
	if r := adm.post("/users/local/ops", url.Values{"action": {"role"}, "role": {"admin"}}); r.code != http.StatusForbidden {
		t.Fatalf("step-up must expire after 5 minutes: %d", r.code)
	}
	if e.users.users["ops"].Role != model.RoleOperator {
		t.Fatal("role changed after the step-up expired")
	}
	// The step-up belongs to one session: a second admin session has none.
	other := e.login("admin", testPassword)
	if r := other.post("/users", url.Values{"username": {"x1"}, "password": {newPassword}, "role": {"operator"}}); r.code != http.StatusForbidden {
		t.Fatalf("step-up leaked to another session: %d", r.code)
	}
}

func TestAdminManagesAccounts(t *testing.T) {
	e := localEnv(t)
	adm := e.login("admin", testPassword)
	ops := e.login("ops", opsPassword)
	adm.stepUp(testPassword)

	// A role change applies at once, without a new login.
	if r := ops.post("/admin-action", url.Values{}); r.code != http.StatusForbidden {
		t.Fatalf("operator before: %d", r.code)
	}
	if r := adm.post("/users/local/ops", url.Values{"action": {"role"}, "role": {"admin"}}); r.code != http.StatusOK {
		t.Fatalf("promote: %d %s", r.code, r.body)
	}
	if r := ops.post("/admin-action", url.Values{}); r.code != http.StatusOK {
		t.Fatalf("promotion did not apply to the running session: %d", r.code)
	}
	if r := adm.post("/users/local/ops", url.Values{"action": {"role"}, "role": {"viewer"}}); r.code != http.StatusBadRequest {
		t.Fatalf("a viewer role was accepted: %d", r.code)
	}
	if r := adm.post("/users/local/ops", url.Values{"action": {"role"}, "role": {"operator"}}); r.code != http.StatusOK {
		t.Fatal(r.code)
	}
	// Disabling signs the user out at once and blocks the next login.
	if r := adm.post("/users/local/ops", url.Values{"action": {"disable"}}); r.code != http.StatusOK {
		t.Fatalf("disable: %d", r.code)
	}
	if r := ops.get("/viewer"); r.code == http.StatusOK {
		t.Error("a disabled user's session still works")
	}
	if r := postLocal(e.browser(), "192.0.2.90", "ops", opsPassword); r.code != http.StatusUnauthorized {
		t.Errorf("disabled user logged in: %d", r.code)
	}
	if a := e.audit.last(); a.Result == "ok" && a.Action == "auth.login.local" {
		t.Errorf("audit %+v", a)
	}
	if r := adm.post("/users/local/ops", url.Values{"action": {"enable"}}); r.code != http.StatusOK {
		t.Fatal(r.code)
	}
	// An admin-set password must be changed at the next login.
	if r := adm.post("/users/local/ops", url.Values{"action": {"password"}, "password": {"temporary password 1"}}); r.code != http.StatusOK {
		t.Fatalf("reset: %d %s", r.code, r.body)
	}
	b := e.browser()
	if r := postLocal(b, "192.0.2.91", "ops", "temporary password 1"); r.code != http.StatusSeeOther || r.loc != "/account" {
		t.Fatalf("login with the temporary password: %d %q", r.code, r.loc)
	}
	// Own password goes through /account/password, not the admin form.
	if r := adm.post("/users/local/admin", url.Values{"action": {"password"}, "password": {newPassword}}); r.code != http.StatusBadRequest {
		t.Errorf("admin reset of own password: %d", r.code)
	}
	if r := adm.post("/users/local/admin", url.Values{"action": {"delete"}}); r.code != http.StatusBadRequest {
		t.Errorf("deleting the own (and last) admin account: %d, the self rule refuses first", r.code)
	}
	if r := adm.post("/users/local/ghost", url.Values{"action": {"delete"}}); r.code != http.StatusNotFound {
		t.Errorf("unknown user: %d", r.code)
	}
	if r := adm.post("/users/local/ops", url.Values{"action": {"delete"}}); r.code != http.StatusOK {
		t.Fatalf("delete: %d", r.code)
	}
	if _, ok := e.users.users["ops"]; ok {
		t.Fatal("ops still exists")
	}
	if a := e.audit.last(); a.Action != "account.delete" || a.Actor != "local:admin" {
		t.Errorf("audit %+v", a)
	}
}

// Why: only local accounts own a password. OIDC and header admins see their
// account page (who they are, how they sign in) but get no password form, and
// a crafted POST cannot set a password for them, whatever their role.
func TestExternalAdminsHaveNoPasswordForm(t *testing.T) {
	e := newEnv(t, ModeHeader)
	hdr := map[string]string{"X-authentik-username": "boss", "X-authentik-groups": "fleet-admins"}
	if r := e.browser().do(http.MethodGet, "/account", nil, hdr); r.code != http.StatusOK || strings.Contains(r.body, `action="/account/password"`) {
		t.Errorf("header admin GET /account: %d, want 200 without a password form: %s", r.code, r.body)
	}
	o := newEnv(t, ModeOIDC)
	b := o.browser()
	if r := b.get(b.startOIDC("/", alice)); r.code != http.StatusSeeOther {
		t.Fatalf("oidc login: %d", r.code)
	}
	if r := b.get("/account"); r.code != http.StatusOK || strings.Contains(r.body, `action="/account/password"`) {
		t.Errorf("OIDC admin GET /account: %d, want 200 without a password form", r.code)
	}
	r := b.do(http.MethodPost, "/account/password", url.Values{"csrf": {b.csrfToken()}, "current": {"x"}, "new": {newPassword}, "confirm": {newPassword}}, nil)
	if r.code != http.StatusForbidden {
		t.Errorf("OIDC admin POST /account/password: %d, want 403", r.code)
	}
}

func TestDisabledOrRolelessAccountCannotSignIn(t *testing.T) {
	e := localEnv(t)
	e.users.cli(func(m map[string]store.LocalUser) {
		u := m["ops"]
		u.Disabled = true
		m["ops"] = u
	})
	r := postLocal(e.browser(), "192.0.2.70", "ops", opsPassword)
	if r.code != http.StatusUnauthorized || !strings.Contains(r.body, "Wrong username or password") {
		t.Fatalf("disabled account: %d (must look like a wrong password)", r.code)
	}
	if a := e.audit.last(); a.Result != "denied: account disabled" {
		t.Errorf("audit %+v", a)
	}
}

// Why: the role comes from the account, not from a constant: an operator who
// signs in must not be an admin (the old break-glass code granted admin to all).
func TestLoginUsesTheAccountsRole(t *testing.T) {
	e := localEnv(t)
	ops := e.login("ops", opsPassword)
	if r := ops.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("operator may read: %d", r.code)
	}
	if r := ops.post("/admin-action", url.Values{}); r.code != http.StatusForbidden {
		t.Fatalf("operator may not do admin actions: %d", r.code)
	}
	if s := e.sessions.all(); len(s) == 0 {
		t.Fatal("no session")
	}
}

// recordingPages renders like DefaultPages and keeps the last users-page data,
// so a test can check what the web UI is given (the add-user page, the guards).
type recordingPages struct {
	DefaultPages
	users UsersPageData
}

func (p *recordingPages) Users(w http.ResponseWriter, r *http.Request, status int, d UsersPageData) {
	p.users = d
	p.DefaultPages.Users(w, r, status, d)
}

// Why: adding a user is its own page (GET /users/new, design v2 screen useradd)
// for admins only, and a refused form comes back as that page with what was
// typed, never as the list where the input would be lost. A mistyped repeat of
// the initial password must not create an account with a password nobody knows.
func TestAddUserPage(t *testing.T) {
	rec := &recordingPages{}
	e := newEnv(t, ModeLocal, func(o *Options) { o.Proxy.PublicURL = ""; o.DataDir = t.TempDir(); o.Pages = rec })
	opsHash, err := HashPassword(opsPassword)
	if err != nil {
		t.Fatal(err)
	}
	e.users.cli(func(m map[string]store.LocalUser) {
		m["ops"] = store.LocalUser{Username: "ops", PasswordHash: opsHash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator}
	})
	if r := e.login("ops", opsPassword).get("/users/new"); r.code != http.StatusForbidden {
		t.Errorf("operator GET /users/new: %d, want 403", r.code)
	}
	adm := e.login("admin", testPassword)
	if r := adm.get("/users/new"); r.code != http.StatusOK || !rec.users.New || !rec.users.OTP {
		t.Fatalf("GET /users/new: %d, New=%v OTP=%v: the add page with a one-time password preselected", r.code, rec.users.New, rec.users.OTP)
	}
	adm.stepUp(testPassword)
	form := url.Values{"username": {"lena"}, "display_name": {"Lena Berg"}, "email": {"lena@example.org"}, "role": {"admin"},
		"password": {newPassword}, "confirm": {newPassword + "x"}}
	r := adm.post("/users", form)
	if r.code != http.StatusBadRequest || !rec.users.New || rec.users.ErrField != "confirm" {
		t.Fatalf("mismatched repeat: %d New=%v field=%q", r.code, rec.users.New, rec.users.ErrField)
	}
	if _, ok := e.users.users["lena"]; ok {
		t.Fatal("a user was created although the two passwords differ")
	}
	if d := rec.users; d.Username != "lena" || d.DisplayName != "Lena Berg" || d.Email != "lena@example.org" || d.Role != "admin" || d.OTP {
		t.Errorf("typed values lost on the refused form: %+v", d)
	}
	form.Set("confirm", newPassword)
	if r := adm.post("/users", form); r.code != http.StatusOK || rec.users.New {
		t.Fatalf("create: %d New=%v, want the list with the notice", r.code, rec.users.New)
	}
}

// Why: the Users page shows every change the rules forbid as a disabled menu
// item with the reason; the list must carry the same guards the handlers
// enforce (no self lock-out, the last admin stays, SSO users are managed at the
// identity provider).
func TestUsersListCarriesTheGuards(t *testing.T) {
	rec := &recordingPages{}
	e := newEnv(t, ModeLocal, func(o *Options) { o.Proxy.PublicURL = ""; o.DataDir = t.TempDir(); o.Pages = rec })
	adm := e.login("admin", testPassword)
	if r := adm.get("/users"); r.code != http.StatusOK {
		t.Fatalf("GET /users: %d", r.code)
	}
	var self *UserRow
	for i := range rec.users.Users {
		if rec.users.Users[i].Subject == rec.users.Self {
			self = &rec.users.Users[i]
		}
	}
	if self == nil {
		t.Fatalf("the viewer is not in the list: %+v", rec.users.Users)
	}
	if self.Guards.Delete.Allowed || self.Guards.Delete.Reason == "" || self.Guards.Disable.Allowed {
		t.Errorf("own account offered for delete/disable: %+v", self.Guards)
	}
}
