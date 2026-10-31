package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// PasswordPageData is rendered by AccountPages.ChangePassword.
type PasswordPageData struct {
	BasePath  string
	CSRFToken string
	Error     string
	ErrField  string // input the Error belongs to ("current", "new", "confirm"); empty: a page-level error
	Forced    bool   // first login or admin reset: nothing else is reachable before this
	Self      string // who is signed in, shown on the forced change (no app shell there)
	// Profile (N4, POST /account/profile, fields "display_name" and "email"):
	// the current values, or what was typed when saving failed (Error and
	// ErrField "display_name" / "email" then belong to the profile form).
	DisplayName  string
	Email        string
	ProfileSaved bool // GET with ?profile=saved after a successful save
	// Local: the viewer has a local account, so the page offers the password
	// and profile forms. SSO and proxy users see their profile only (GET
	// /account is open to every signed-in user; the forms stay local-only).
	Local bool
	IdP   string // the identity provider's name with AUTH_MODE=oidc ("Sign-in: SSO (IdP)")
	// NameForm: an SSO or proxy user, who can set a display name of their own (POST /account/name,
	// field "display_name"; #283). Then DisplayName is that chosen name (empty: none) and
	// ProviderName the name of the last sign-in, which shows again when it is cleared.
	NameForm     bool
	ProviderName string
}

// Sign-in methods of a UserRow.
const (
	MethodLocal  = "local"
	MethodSSO    = "sso"
	MethodHeader = "header"
)

// StepUpKind values of UsersPageData: how the viewer confirms before a change.
const (
	StepUpPassword = "password" // local admin: type the password again
	StepUpOIDC     = "oidc"     // OIDC admin: authenticate at the IdP again
)

// UserRow is one principal on the Users page: a local account or someone
// seen at an SSO or trusted-proxy sign-in.
type UserRow struct {
	Subject            string // the stable key (audit rows store it); "local:<name>" for local accounts
	Name               string // local: the username, others: the name captured at the last sign-in
	Method             string // MethodLocal, MethodSSO or MethodHeader
	Role               string // "admin", "operator"; empty when unknown (SSO user not seen since the column exists)
	Disabled           bool
	MustChangePassword bool
	LastLogin          time.Time // zero: never or unknown
	Sessions           int       // live web sessions
	DisplayName        string    // local: profile field; others: empty (Name is the display name)
	Email              string    // local: profile field; SSO/proxy: address of the last sign-in
	// Guards say which changes the rules allow for this row and why not (self, last admin,
	// managed elsewhere); the Users page shows them on disabled menu items. The handlers
	// enforce the same rules.
	Guards UserGuards
}

// UsersPageData is rendered by AccountPages.Users. The list is always
// complete; NeedStepUp only locks the changes.
type UsersPageData struct {
	BasePath   string
	CSRFToken  string
	Self       string // the viewer's subject
	Error      string
	ErrField   string // input the Error belongs to ("current", "username", "password", "role"); empty: a page-level error
	Username   string // typed username, kept when creating a user failed
	Role       string // chosen role, kept likewise
	Notice     string
	NeedStepUp bool   // changes are locked until the viewer confirms
	StepUpKind string // how: StepUpPassword or StepUpOIDC; empty with ViewOnly
	ViewOnly   bool   // header principals have no step-up and cannot change anything
	Users      []UserRow
	// New: render the add-user page (GET /users/new, or a refused POST /users)
	// instead of the list. DisplayName, Email and OTP keep what was typed.
	New         bool
	DisplayName string
	Email       string
	OTP         bool   // "Generate a one-time password" was chosen
	IdP         string // the identity provider's name with AUTH_MODE=oidc (source column, step-up button)
}

// AccountPages renders the account pages. Pages implementations may also
// implement it; otherwise the built-in pages are used.
type AccountPages interface {
	ChangePassword(w http.ResponseWriter, r *http.Request, status int, d PasswordPageData)
	Users(w http.ResponseWriter, r *http.Request, status int, d UsersPageData)
}

func (s *Service) accountPages() AccountPages {
	if p, ok := s.o.Pages.(AccountPages); ok {
		return p
	}
	return DefaultPages{}
}

// accountRoutes are the only routes that change credentials. GET /account is
// the account page of every signed-in user; changing the password (POST
// /account/password) needs a local account. /users is for admins of any sign-in method: the list
// needs no step-up, every change needs a fresh one (local password, or the
// IdP for OIDC admins); header admins have none and can only look.
var accountRoutePatterns = []string{
	"GET /account",
	"POST /account/password",
	"GET /users",
	"GET /users/new",
	"POST /users",
	"POST /users/stepup",
	"POST /users/local/{name}",
	"POST /users/sessions",
	"GET /users/{name}",
	"GET /users/principal/{subject...}",
	"POST /account/profile",
	"POST /account/name",
	"GET /setup/account",
	"POST /setup/account",
}

// MountPaths are the paths to mount Routes() on (ServeMux patterns).
func (s *Service) MountPaths() []string {
	return []string{"/login", "/login/sso", "/login/local", "/oidc/callback", "/oidc/stepup", "/logout", "/account", "/account/password",
		"/account/profile", "/account/name", "/users", "/users/", "/setup/account"}
}

// localOnly refuses principals that are not local accounts.
func (s *Service) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, _ := FromContext(r.Context()); p.Method != "local" {
			s.o.Pages.Message(w, r, http.StatusForbidden, MessageData{
				BasePath: httpx.BasePath(r.Context()), Title: "Not available",
				Text: "Only local accounts are managed here. Your identity comes from the identity provider.",
			})
			return
		}
		next(w, r)
	}
}

func (s *Service) accountHandlers() map[string]http.Handler {
	member := func(h http.HandlerFunc) http.Handler { return s.require(model.RoleOperator, true)(s.localOnly(h)) }
	anyone := func(h http.HandlerFunc) http.Handler { return s.require(model.RoleOperator, true)(h) }
	settled := func(h http.HandlerFunc) http.Handler { return s.require(model.RoleOperator, false)(s.localOnly(h)) }
	admin := func(h http.HandlerFunc) http.Handler { return s.require(model.RoleAdmin, false)(h) }
	return map[string]http.Handler{
		"GET /account":                      anyone(s.passwordForm),
		"POST /account/password":            member(s.passwordSubmit),
		"GET /users":                        admin(s.usersPage),
		"GET /users/new":                    admin(s.userNewPage),
		"POST /users":                       admin(s.userCreate),
		"POST /users/stepup":                admin(s.stepUpSubmit),
		"POST /users/local/{name}":          admin(s.userAction),
		"POST /users/sessions":              admin(s.endSessions),
		"GET /users/{name}":                 admin(s.localDetailPage),
		"GET /users/principal/{subject...}": admin(s.principalDetailPage),
		"POST /account/profile":             settled(s.profileSubmit),
		"POST /account/name":                anyone(s.nameSubmit),
		"GET /setup/account":                http.HandlerFunc(s.setupAccountPage),
		"POST /setup/account":               http.HandlerFunc(s.setupAccountSubmit),
	}
}

// verifyCurrent checks the signed-in user's current password for a
// credential operation (own password change, step-up). Guessing it is
// limited like a login, in its own budget (the "pw:" scope), and the KDF
// slots are shared with the login. ok is false with limited set when the
// limiter refused.
func (s *Service) verifyCurrent(r *http.Request, name, pass string) (ok, limited bool) {
	ctx := r.Context()
	src, scope := limitKey(clientIP(r)), "pw:"+name
	if allowed, _ := s.o.Limiter.Allow(s.o.Clock.Now(), src, scope, true); !allowed {
		return false, true
	}
	u, err := s.o.LocalUsers.GetLocalUser(ctx, name)
	hash := dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	verr := s.verifyBounded(ctx, hash, pass)
	if err != nil || verr != nil {
		return false, false
	}
	s.o.Limiter.Success(src, scope)
	return true, false
}

func (s *Service) passwordForm(w http.ResponseWriter, r *http.Request) {
	ci := s.resolve(r)
	d := PasswordPageData{
		BasePath: httpx.BasePath(r.Context()), CSRFToken: ci.csrf, Forced: ci.mustChange, Self: ci.p.Name,
		ProfileSaved: r.URL.Query().Get("profile") == "saved", Local: ci.p.Method == ModeLocal, IdP: s.idpName(),
	}
	if d.Local {
		if u, err := s.accounts.Get(r.Context(), ci.p.Name); err == nil {
			d.DisplayName, d.Email = u.DisplayName, u.Email
		}
	} else if dir := s.o.Directory; dir != nil {
		// SSO and proxy users: the address of their last sign-in, the name of it and the name they chose.
		d.NameForm = true
		if seen, err := dir.ListPrincipals(r.Context()); err == nil {
			for _, p := range seen {
				if p.Subject == ci.p.Subject {
					d.DisplayName, d.ProviderName, d.Email = p.Custom, p.Provider, p.Email
				}
			}
		}
	}
	s.accountPages().ChangePassword(w, r, http.StatusOK, d)
}

func (s *Service) passwordSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ci := s.resolve(r)
	p := ci.p
	d := PasswordPageData{BasePath: httpx.BasePath(ctx), CSRFToken: ci.csrf, Forced: ci.mustChange, Self: p.Name, Local: true}
	fail := func(status int, field, msg string) {
		d.Error, d.ErrField = msg, field
		s.accountPages().ChangePassword(w, r, status, d)
	}
	// Only the three named fields are read: a crafted POST cannot smuggle in
	// a role or another username.
	current, next, confirm := r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("confirm")
	ok, limited := s.verifyCurrent(r, p.Name, current)
	switch {
	case limited:
		s.record(r, p.Subject, "account.password.change", "denied: rate limited", nil)
		fail(http.StatusTooManyRequests, "", "Too many attempts. Wait a minute and try again.")
		return
	case !ok:
		s.record(r, p.Subject, "account.password.change", "denied: wrong current password", nil)
		fail(http.StatusForbidden, "current", "The current password is wrong.")
		return
	case next != confirm:
		fail(http.StatusBadRequest, "confirm", "The two new passwords differ.")
		return
	case next == current:
		fail(http.StatusBadRequest, "new", "The new password must differ from the current one.")
		return
	}
	// SetPassword revokes every session and device cookie of this user,
	// including the ones of other browsers; this browser then gets fresh ones.
	if err := s.accounts.ChangeOwnPassword(ctx, p.Name, ci.passwordGen, next); err != nil {
		switch {
		case errors.Is(err, ErrWeakPassword):
			fail(http.StatusBadRequest, "new", msgWeakPassword)
			return
		case errors.Is(err, ErrStaleActor):
			s.record(r, p.Subject, "account.password.change", "denied: account changed meanwhile", nil)
			fail(http.StatusConflict, "", "Your account was changed while you were typing. Sign in again.")
			return
		}
		slog.ErrorContext(ctx, "change password", "user", p.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.steps.revoke(ci.sessionID, ci.passwordGen)
	if p.Name == InitialAdminName {
		s.removeInitialPasswordFile(ctx) // the generated password is spent
	}
	s.record(r, p.Subject, "account.password.change", "ok", nil)
	fresh, err := s.accounts.Get(ctx, p.Name)
	if err == nil {
		// A browser-session cookie: the sign-in's "keep me signed in" choice is
		// not known here, and the shorter lifetime is the safe side.
		_, err = s.sessions.CreateLocal(w, r, p, fresh.PasswordGen, false)
	}
	if err != nil {
		slog.ErrorContext(ctx, "create session after password change", "err", err)
		http.Redirect(w, r, httpx.BasePath(ctx)+"/login", http.StatusSeeOther)
		return
	}
	s.setDeviceCookie(w, r, p.Name, fresh.PasswordGen)
	s.redirectLocal(w, r, "/")
}

// --- admin ---

// stepUpKind is how the signed-in admin confirms a change: "" for header
// principals, who have no session and nothing to confirm with.
func stepUpKindOf(p model.Principal) string {
	switch p.Method {
	case ModeLocal:
		return StepUpPassword
	case ModeOIDC:
		return StepUpOIDC
	}
	return ""
}

// steppedUp reports whether the viewer confirmed within the last 5 minutes.
// A local password and an IdP re-authentication are kept apart (see SteppedUp).
func (s *Service) steppedUp(ci ctxInfo) bool {
	now := s.o.Clock.Now()
	switch ci.p.Method {
	case ModeLocal:
		return ci.sessionID != "" && s.steps.valid(ci.sessionID, ci.passwordGen, now)
	case ModeOIDC:
		return ci.sessionID != "" && s.oidcSteps.valid(ci.sessionID, 0, now)
	}
	return false
}

func (s *Service) usersData(r *http.Request, ci ctxInfo) (UsersPageData, error) {
	ctx := r.Context()
	d := UsersPageData{BasePath: httpx.BasePath(ctx), CSRFToken: ci.csrf, Self: ci.p.Subject, StepUpKind: stepUpKindOf(ci.p)}
	d.ViewOnly = d.StepUpKind == ""
	d.NeedStepUp = !d.ViewOnly && !s.steppedUp(ci)
	d.IdP = s.idpName()
	users, err := s.accounts.List(ctx)
	if err != nil {
		return d, err
	}
	var seen []store.PrincipalRecord
	counts := map[string]int{}
	if dir := s.o.Directory; dir != nil {
		if seen, err = dir.ListPrincipals(ctx); err != nil {
			return d, err
		}
		if counts, err = dir.CountSessions(ctx, s.o.Clock.Now(), s.o.Idle); err != nil {
			return d, err
		}
	}
	logins := map[string]time.Time{}
	for _, p := range seen {
		logins[p.Subject] = p.LastLogin
	}
	admins := 0
	for _, u := range users {
		if u.Role == model.RoleAdmin && !u.Disabled {
			admins++
		}
	}
	for _, u := range users {
		sub := SubjectOf(u.Username)
		d.Users = append(d.Users, UserRow{Subject: sub, Name: u.Username, Method: MethodLocal, Role: roleName(u.Role),
			Disabled: u.Disabled, MustChangePassword: u.MustChangePassword, LastLogin: logins[sub], Sessions: counts[sub],
			DisplayName: u.DisplayName, Email: u.Email, Guards: localGuards(u, sub == d.Self, admins)})
	}
	for _, p := range seen {
		if p.Kind == MethodLocal {
			continue // local accounts come from the account table; a deleted one has no row here
		}
		d.Users = append(d.Users, UserRow{Subject: p.Subject, Name: p.DisplayName, Method: p.Kind, Role: p.Role,
			LastLogin: p.LastLogin, Sessions: counts[p.Subject], Email: p.Email, Guards: principalGuards(p.Kind)})
	}
	return d, nil
}

func (s *Service) renderUsers(w http.ResponseWriter, r *http.Request, status int, notice, errMsg string) {
	s.renderUsersField(w, r, status, notice, errMsg, "", "", "")
}

// renderUsersField is renderUsers for an error that belongs to one input of
// the create-user form: errField names it, username and role refill the form.
func (s *Service) renderUsersField(w http.ResponseWriter, r *http.Request, status int, notice, errMsg, errField, username, role string) {
	ci := s.resolve(r)
	d, err := s.usersData(r, ci)
	if err != nil {
		slog.ErrorContext(r.Context(), "list users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.Notice, d.Error, d.ErrField, d.Username, d.Role = notice, errMsg, errField, username, role
	s.accountPages().Users(w, r, status, d)
}

func (s *Service) usersPage(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, http.StatusOK, "", "")
}

// renderNewUser is the add-user page again after POST /users was refused:
// what was typed stays (never the passwords), the error names its field.
func (s *Service) renderNewUser(w http.ResponseWriter, r *http.Request, status int, errMsg, errField string) {
	ci := s.resolve(r)
	d, err := s.usersData(r, ci)
	if err != nil {
		slog.ErrorContext(r.Context(), "list users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.New, d.Error, d.ErrField = true, errMsg, errField
	d.Username, d.Role = r.PostFormValue("username"), r.PostFormValue("role")
	d.DisplayName, d.Email, d.OTP = r.PostFormValue("display_name"), r.PostFormValue("email"), r.PostFormValue("otp") != ""
	s.accountPages().Users(w, r, status, d)
}

// idpName is the configured identity provider's display name; empty without OIDC.
func (s *Service) idpName() string {
	if s.o.Mode == ModeOIDC {
		return s.o.OIDC.DisplayName
	}
	return ""
}

// userNewPage is GET /users/new: the add-user form. Like the list it needs no
// step-up to look at; saving does.
func (s *Service) userNewPage(w http.ResponseWriter, r *http.Request) {
	ci := s.resolve(r)
	d, err := s.usersData(r, ci)
	if err != nil {
		slog.ErrorContext(r.Context(), "list users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.New, d.OTP = true, true // a generated one-time password is the default
	s.accountPages().Users(w, r, http.StatusOK, d)
}

// stepUpSubmit takes the password of a local admin. OIDC admins confirm at the
// IdP (POST /oidc/stepup), header principals cannot confirm at all.
func (s *Service) stepUpSubmit(w http.ResponseWriter, r *http.Request) {
	ci := s.resolve(r)
	if ci.p.Method != ModeLocal {
		s.record(r, ci.p.Subject, "account.stepup", "denied: password step-up needs a local account ("+ci.p.Method+")", nil)
		s.renderUsers(w, r, http.StatusForbidden, "", "A password confirmation is only available for local accounts.")
		return
	}
	ok, limited := s.verifyCurrent(r, ci.p.Name, r.PostFormValue("current"))
	switch {
	case limited:
		s.record(r, ci.p.Subject, "account.stepup", "denied: rate limited", nil)
		s.renderUsers(w, r, http.StatusTooManyRequests, "", "Too many attempts. Wait a minute and try again.")
	case !ok:
		s.record(r, ci.p.Subject, "account.stepup", "denied: wrong password", nil)
		s.renderUsersField(w, r, http.StatusForbidden, "", "The password is wrong.", "current", "", "")
	default:
		s.steps.grant(ci.sessionID, ci.passwordGen, s.o.Clock.Now())
		s.record(r, ci.p.Subject, "account.stepup", "ok", nil)
		http.Redirect(w, r, httpx.BasePath(r.Context())+"/users", http.StatusSeeOther)
	}
}

// stepped returns the admin's context when the step-up is still valid, else
// renders the page with the prompt (403) and returns false. Every change goes
// through it; the list does not.
func (s *Service) stepped(w http.ResponseWriter, r *http.Request) (ctxInfo, bool) {
	ci := s.resolve(r)
	if !s.steppedUp(ci) {
		msg := "Confirm your identity to change users."
		if stepUpKindOf(ci.p) == "" {
			msg = "Behind a header-authenticating proxy this page is view only: there is nothing to confirm with. Sign in as a local or SSO administrator to change users."
		}
		s.record(r, ci.p.Subject, "account.manage", "denied: step-up required", nil)
		s.renderUsers(w, r, http.StatusForbidden, "", msg)
		return ci, false
	}
	return ci, true
}

func (s *Service) userCreate(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.stepped(w, r)
	if !ok {
		return
	}
	name := NormalizeUsername(r.PostFormValue("username"))
	role, err := ParseRole(r.PostFormValue("role"))
	// "otp" asks for a generated one-time password instead of a typed one.
	wantOTP := r.PostFormValue("otp") != ""
	var otp string
	if err == nil {
		nu := NewUser{Name: name, Role: role, DisplayName: r.PostFormValue("display_name"), Email: r.PostFormValue("email"),
			Password: r.PostFormValue("password")}
		_, repeated := r.PostForm["confirm"] // the add-user page repeats the password; older forms do not
		switch {
		case wantOTP:
			nu.Password = ""
		case nu.Password == "":
			err = ErrWeakPassword // an empty typed password must not silently become a generated one
		case repeated && r.PostFormValue("confirm") != nu.Password:
			err = errConfirmMismatch
		}
		if err == nil {
			// The new user must change the password: the admin chose it.
			otp, err = s.acting(ci).Create(r.Context(), nu, ci.p.Subject)
		}
	}
	if err == nil && wantOTP {
		s.record(r, ci.p.Subject, "account.create", "ok", map[string]string{"user": name, "password": "one-time"})
		s.showOTP(w, r, ci, name, otp, true)
		return
	}
	s.finishAccountAction(w, r, ci, "account.create", name, "User "+name+" created. They must change the password at the first login.", err)
}

func (s *Service) userAction(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.stepped(w, r)
	if !ok {
		return
	}
	ctx, name := r.Context(), NormalizeUsername(r.PathValue("name"))
	action := r.PostFormValue("action")
	var err error
	var notice string
	// An admin cannot lock themselves out: the guard is here, the UI only hides the controls.
	self := ci.p.Method == ModeLocal && name == ci.p.Name
	switch action {
	case "role":
		var role model.Role
		if role, err = ParseRole(r.PostFormValue("role")); err == nil {
			if self && role != model.RoleAdmin {
				err = errSelfDemote
			} else {
				err = s.acting(ci).SetRole(ctx, name, role)
			}
		}
		notice = "Role of " + name + " changed."
	case "disable":
		if self {
			err = errSelfDisable
		} else {
			err = s.acting(ci).SetDisabled(ctx, name, true)
		}
		notice = "User " + name + " disabled and signed out."
	case "enable":
		err = s.acting(ci).SetDisabled(ctx, name, false)
		notice = "User " + name + " enabled."
	case "password":
		if ci.p.Method == ModeLocal && name == ci.p.Name {
			err = errSelfReset
		} else {
			err = s.acting(ci).SetPassword(ctx, name, r.PostFormValue("password"), true)
			if err == nil && name == InitialAdminName {
				s.removeInitialPasswordFile(ctx)
			}
		}
		notice = "Password of " + name + " reset; they must change it at the next login."
	case "otp":
		if self {
			err = errSelfReset
			break
		}
		otp, oerr := s.acting(ci).ResetOneTimePassword(ctx, name)
		if oerr == nil {
			if name == InitialAdminName {
				s.removeInitialPasswordFile(ctx)
			}
			s.record(r, ci.p.Subject, "account.otp", "ok", map[string]string{"user": name})
			s.showOTP(w, r, ci, name, otp, false)
			return
		}
		err = oerr
	case "profile":
		err = s.acting(ci).SetProfile(ctx, name, r.PostFormValue("display_name"), r.PostFormValue("email"))
		notice = "Profile of " + name + " saved."
	case "delete":
		if self {
			err = errSelfDelete
		} else {
			err = s.acting(ci).Delete(ctx, name)
		}
		notice = "User " + name + " deleted."
	default:
		err = errUnknownAction
	}
	s.finishAccountAction(w, r, ci, "account."+action, name, notice, err)
}

var (
	errSelfReset     = errors.New("use \"Change password\" for your own account")
	errSelfDisable   = errors.New("you cannot disable your own account")
	errSelfDelete    = errors.New("you cannot delete your own account")
	errSelfDemote    = errors.New("you cannot lower your own role")
	errUnknownAction = errors.New("unknown action")
	// errConfirmMismatch: the two password fields of the add-user page differ.
	errConfirmMismatch = errors.New("the passwords do not match")
)

// acting returns the account manager bound to the signed-in admin's
// authority: for a local admin every mutation re-checks, atomically with the
// write, that this admin is still an enabled admin at the same password
// generation. An OIDC admin has no local account to check; its authority is
// the session (role re-read on every request) plus the IdP step-up that
// stepped() demanded.
func (s *Service) acting(ci ctxInfo) *Accounts {
	if ci.p.Method != ModeLocal {
		return s.accounts
	}
	return s.accounts.As(store.Actor{Username: ci.p.Name, PasswordGen: ci.passwordGen})
}

// endSessions is POST /users/sessions (form field subject): it signs an SSO
// user out everywhere. Roles stay with the IdP groups, so this is the one
// change available for such a user. Exactly the sessions of that subject end.
func (s *Service) endSessions(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.stepped(w, r)
	if !ok {
		return
	}
	subject := r.PostFormValue("subject")
	if id := r.PostFormValue("session"); id != "" {
		s.endOneSession(w, r, ci, subject, id)
		return
	}
	detail := map[string]string{"subject": subject}
	known := false
	if s.o.Directory != nil {
		rows, err := s.o.Directory.ListPrincipals(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "list principals", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, p := range rows {
			known = known || (p.Subject == subject && p.Kind == MethodSSO)
		}
	}
	switch {
	case !known:
		s.record(r, ci.p.Subject, "account.sessions.end", "denied: not an SSO user", detail)
		s.renderUsers(w, r, http.StatusNotFound, "", "No such SSO user.")
		return
	case subject == ci.p.Subject:
		s.record(r, ci.p.Subject, "account.sessions.end", "denied: own account", detail)
		s.renderUsers(w, r, http.StatusBadRequest, "", "Use Sign out to end your own session.")
		return
	}
	n, err := s.o.Sessions.DeleteUserSessions(r.Context(), subject)
	if err != nil {
		slog.ErrorContext(r.Context(), "end sessions", "err", err)
		s.record(r, ci.p.Subject, "account.sessions.end", "error", detail)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	detail["sessions"] = strconv.FormatInt(n, 10)
	s.record(r, ci.p.Subject, "account.sessions.end", "ok", detail)
	s.renderUsers(w, r, http.StatusOK, "Signed out "+strconv.FormatInt(n, 10)+" session(s). The user can sign in again; roles come from the identity provider.", "")
}

func (s *Service) finishAccountAction(w http.ResponseWriter, r *http.Request, ci ctxInfo, action, target, notice string, err error) {
	detail := map[string]string{"user": target}
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrStaleActor):
			// The admin lost its rights while the request ran: nothing was
			// changed. Do not echo details; the session is dead already.
			s.record(r, ci.p.Subject, action, "denied: acting account changed", detail)
			s.renderUsers(w, r, http.StatusForbidden, "", "Your account changed while the request ran. Sign in again.")
			return
		case errors.Is(err, ErrUserNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrUserExists), errors.Is(err, ErrLastAdmin):
			status = http.StatusConflict
		case !isInputError(err):
			slog.ErrorContext(r.Context(), "account action", "action", action, "user", target, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			s.record(r, ci.p.Subject, action, "error", detail)
			return
		}
		s.record(r, ci.p.Subject, action, "denied: "+err.Error(), detail)
		field, msg := inputProblem(err)
		if fromDetail(r) {
			if field != "display_name" && field != "email" {
				field = ""
			}
			if msg == msgWeakPassword {
				msg = "The new password needs at least " + strconv.Itoa(PasswordMinLength) + " characters."
				field = "password"
			}
			s.renderDetail(w, r, status, target, "", "", msg, field)
			return
		}
		if action == "account.create" {
			s.renderNewUser(w, r, status, msg, field)
			return
		}
		// Row actions repeat per user: their errors stay in the banner.
		if msg == msgWeakPassword {
			msg = "The new password needs at least " + strconv.Itoa(PasswordMinLength) + " characters."
		}
		s.renderUsers(w, r, status, "", msg)
		return
	}
	s.record(r, ci.p.Subject, action, "ok", detail)
	if fromDetail(r) {
		s.renderDetail(w, r, http.StatusOK, target, "", notice, "", "")
		return
	}
	s.renderUsers(w, r, http.StatusOK, notice, "")
}

// isInputError reports errors that are the admin's to fix, with messages
// safe to show.
func isInputError(err error) bool {
	for _, e := range []error{ErrUserExists, ErrUserNotFound, ErrBadUsername, ErrBadRole, ErrLastAdmin, ErrWeakPassword, ErrBadName, ErrBadEmail, errSelfReset, errSelfDisable, errSelfDelete, errSelfDemote, errUnknownAction, errConfirmMismatch} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// headerSeenEvery bounds how often a header user's "last login" is written:
// they have no login event, only requests.
const headerSeenEvery = 10 * time.Minute

// noteLogin records a sign-in for the Users page: name, method, the role it
// resolved to and the time. A failure is logged and never blocks the sign-in.
func (s *Service) noteLogin(ctx context.Context, p model.Principal) {
	if s.o.Directory == nil || p.Subject == "" {
		return
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.Subject
	}
	if len(name) > 128 {
		name = name[:128]
	}
	kind := MethodHeader
	switch p.Method {
	case ModeLocal:
		kind = MethodLocal
	case ModeOIDC:
		kind = MethodSSO
	}
	email := strings.TrimSpace(p.Email)
	if len(email) > 254 {
		email = "" // not an address; never stored cut
	}
	rec := store.PrincipalRecord{Subject: p.Subject, DisplayName: name, Kind: kind, Role: roleName(p.Role), UpdatedAt: s.o.Clock.Now(), Email: email}
	if err := s.o.Directory.RecordLogin(ctx, rec); err != nil {
		slog.WarnContext(ctx, "auth: could not record a sign-in", "err", err)
	}
}

// noteHeaderSeen is noteLogin for a header principal, at most every
// headerSeenEvery per subject.
func (s *Service) noteHeaderSeen(ctx context.Context, p model.Principal) {
	now := s.o.Clock.Now()
	s.seenMu.Lock()
	if last, ok := s.seen[p.Subject]; ok && now.Sub(last) < headerSeenEvery {
		s.seenMu.Unlock()
		return
	}
	if len(s.seen) >= 1024 {
		clear(s.seen) // bounded; a cleared entry costs one extra write
	}
	s.seen[p.Subject] = now
	s.seenMu.Unlock()
	s.noteLogin(ctx, p)
}
