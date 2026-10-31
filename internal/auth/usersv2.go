package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Users v2 (design v2, N4): the user detail page, one-time passwords, the
// profile fields and ending a single session.

// AuditLister reads the audit log (the user detail page shows the last
// entries of a user). *store.SQLite implements it.
type AuditLister interface {
	ListAudit(ctx context.Context, f store.AuditFilter) ([]model.AuditEvent, error)
}

// detailAuditRows is how many audit entries the user detail page shows.
const detailAuditRows = 10

// SessionRow is one live session on the user detail page.
type SessionRow struct {
	// ID is the session's store key (the SHA-256 of the cookie, never the
	// cookie): the form field "session" of POST /users/sessions.
	ID        string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	Current   bool // the viewer's own session: ended with Sign out, not here
}

// Guard says whether an action is offered and, if not, why (the reason is
// shown on the disabled menu item).
type Guard struct {
	Allowed bool
	Reason  string
}

// UserGuards are the actions of the user detail page. Role is lowering the
// role (raising is always allowed); Disable also stands for Enable when the
// account is disabled; Reset covers both a typed and a one-time password.
type UserGuards struct {
	Role, Disable, Delete, Reset, Profile Guard
}

// UserDetailData is rendered by UserPages.UserDetail (GET /users/{name} for a
// local account, GET /users/principal/{subject} for an SSO or proxy user).
type UserDetailData struct {
	BasePath   string
	CSRFToken  string
	Self       string // the viewer's subject
	Notice     string
	Error      string
	ErrField   string // "display_name", "email" or "password"; empty: a page-level error
	NeedStepUp bool   // as on the Users page: changes are locked until the viewer confirms
	StepUpKind string
	ViewOnly   bool
	IdP        string // the identity provider's name with AUTH_MODE=oidc (method line, info banner, step-up button)

	User         UserRow
	DisplayName  string // local: the profile field; SSO/proxy: the name of the last sign-in
	Email        string // local: the profile field; SSO/proxy: the address of the last sign-in
	CreatedAt    time.Time
	CreatedBy    string    // subject, CreatedByCLI or CreatedByFirstStart; empty: unknown
	OTPExpiresAt time.Time // set while the password is a one-time password
	OTPExpired   bool
	Sessions     []SessionRow
	Audit        []model.AuditEvent // the last detailAuditRows entries by this user, newest first
	Guards       UserGuards
}

// OTPPageData is rendered by UserPages.OneTimePassword: the only time the
// clear text of a generated password is shown. The page must not be cached
// (the built-in renderer sends Cache-Control: no-store).
type OTPPageData struct {
	BasePath  string
	CSRFToken string
	User      string // username
	Password  string
	ExpiresAt time.Time
	Created   bool // a new account (else: a reset)
}

// UserPages renders the users v2 pages. Pages implementations may implement
// it; otherwise the built-in pages are used.
type UserPages interface {
	UserDetail(w http.ResponseWriter, r *http.Request, status int, d UserDetailData)
	OneTimePassword(w http.ResponseWriter, r *http.Request, status int, d OTPPageData)
}

func (s *Service) userPages() UserPages {
	if p, ok := s.o.Pages.(UserPages); ok {
		return p
	}
	return DefaultPages{}
}

// MsgLastAdmin is the guard reason of a change that would leave no enabled admin (the web
// pages recognise it to say "the last active admin").
const MsgLastAdmin = "This would leave no enabled administrator."

// localGuards decides the actions offered for local account u: the viewer
// cannot lock themselves out (self), and the last enabled admin stays
// (enabledAdmins counts the enabled local admins, u included). The handlers
// enforce the same rules; this only explains them.
func localGuards(u store.LocalUser, self bool, enabledAdmins int) UserGuards {
	g := UserGuards{
		Role: Guard{Allowed: true}, Disable: Guard{Allowed: true}, Delete: Guard{Allowed: true},
		Reset: Guard{Allowed: true}, Profile: Guard{Allowed: true},
	}
	if u.Role == model.RoleAdmin && !u.Disabled && enabledAdmins <= 1 {
		g.Role = Guard{Reason: MsgLastAdmin}
		g.Disable = Guard{Reason: MsgLastAdmin}
		g.Delete = Guard{Reason: MsgLastAdmin}
	}
	if self {
		g.Role = Guard{Reason: "You cannot lower your own role."}
		g.Disable = Guard{Reason: "You cannot disable your own account."}
		g.Delete = Guard{Reason: "You cannot delete your own account."}
		g.Reset = Guard{Reason: "Use Change password for your own account."}
		g.Profile = Guard{Reason: "Edit your own profile on the Account page."}
	}
	return g
}

// principalGuards: an SSO or proxy user is managed where it comes from.
func principalGuards(kind string) UserGuards {
	why := "Managed by the identity provider."
	if kind == MethodHeader {
		why = "Managed by the authenticating proxy."
	}
	no := Guard{Reason: why}
	return UserGuards{Role: no, Disable: no, Delete: no, Reset: no, Profile: no}
}

var errNoSuchUser = errors.New("auth: no such user")

// userDetail builds the detail page of a local account (name != "") or of
// an SSO/proxy principal (subject).
func (s *Service) userDetail(r *http.Request, ci ctxInfo, name, subject string) (UserDetailData, error) {
	ctx := r.Context()
	list, err := s.usersData(r, ci)
	if err != nil {
		return UserDetailData{}, err
	}
	d := UserDetailData{BasePath: list.BasePath, CSRFToken: list.CSRFToken, Self: list.Self,
		NeedStepUp: list.NeedStepUp, StepUpKind: list.StepUpKind, ViewOnly: list.ViewOnly, IdP: list.IdP}
	if name != "" {
		subject = SubjectOf(NormalizeUsername(name))
	}
	found := false
	for _, row := range list.Users {
		if row.Subject == subject && (name != "") == (row.Method == MethodLocal) {
			d.User, found = row, true
		}
	}
	if !found {
		return d, errNoSuchUser
	}
	now := s.o.Clock.Now()
	if name != "" {
		u, err := s.accounts.Get(ctx, name)
		if err != nil {
			return d, err
		}
		admins := 0
		users, err := s.accounts.List(ctx)
		if err != nil {
			return d, err
		}
		for _, o := range users {
			if o.Role == model.RoleAdmin && !o.Disabled {
				admins++
			}
		}
		d.DisplayName, d.Email, d.CreatedAt, d.CreatedBy = u.DisplayName, u.Email, u.CreatedAt, u.CreatedBy
		d.OTPExpiresAt, d.OTPExpired = u.OTPExpiresAt, OTPExpired(u, now)
		d.Guards = localGuards(u, ci.p.Method == ModeLocal && ci.p.Name == u.Username, admins)
	} else {
		if dir := s.o.Directory; dir != nil {
			rows, err := dir.ListPrincipals(ctx)
			if err != nil {
				return d, err
			}
			for _, p := range rows {
				if p.Subject == subject {
					d.DisplayName, d.Email = p.DisplayName, p.Email
				}
			}
		}
		d.Guards = principalGuards(d.User.Method)
	}
	if dir := s.o.Directory; dir != nil {
		sessions, err := dir.ListSubjectSessions(ctx, subject, now, s.o.Idle)
		if err != nil {
			return d, err
		}
		for _, sess := range sessions {
			d.Sessions = append(d.Sessions, SessionRow{ID: sess.IDHash, CreatedAt: sess.CreatedAt, LastSeen: sess.LastSeen,
				ExpiresAt: sess.ExpiresAt, Current: sess.IDHash == ci.sessionID})
		}
	}
	if s.o.AuditLog != nil {
		if d.Audit, err = s.o.AuditLog.ListAudit(ctx, store.AuditFilter{Actor: subject, Limit: detailAuditRows}); err != nil {
			return d, err
		}
	}
	return d, nil
}

// renderDetail renders the detail page with a notice or error; a user that
// no longer exists (deleted just now) falls back to the Users list.
func (s *Service) renderDetail(w http.ResponseWriter, r *http.Request, status int, name, subject, notice, errMsg, errField string) {
	ci := s.resolve(r)
	d, err := s.userDetail(r, ci, name, subject)
	switch {
	case errors.Is(err, errNoSuchUser), errors.Is(err, ErrUserNotFound):
		s.renderUsers(w, r, status, notice, errMsg)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "user detail", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	d.Notice, d.Error, d.ErrField = notice, errMsg, errField
	s.userPages().UserDetail(w, r, status, d)
}

func (s *Service) notFoundUser(w http.ResponseWriter, r *http.Request) {
	s.o.Pages.Message(w, r, http.StatusNotFound, MessageData{BasePath: httpx.BasePath(r.Context()), Title: "No such user",
		Text: "This user does not exist (any more)."})
}

// localDetailPage is GET /users/{name}: open to admins without step-up, like the list.
func (s *Service) localDetailPage(w http.ResponseWriter, r *http.Request) {
	s.detailPage(w, r, r.PathValue("name"), "")
}

// principalDetailPage is GET /users/principal/{subject...}.
func (s *Service) principalDetailPage(w http.ResponseWriter, r *http.Request) {
	s.detailPage(w, r, "", r.PathValue("subject"))
}

func (s *Service) detailPage(w http.ResponseWriter, r *http.Request, name, subject string) {
	d, err := s.userDetail(r, s.resolve(r), name, subject)
	switch {
	case errors.Is(err, errNoSuchUser), errors.Is(err, ErrUserNotFound):
		s.notFoundUser(w, r)
	case err != nil:
		slog.ErrorContext(r.Context(), "user detail", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		s.userPages().UserDetail(w, r, http.StatusOK, d)
	}
}

// fromDetail reports whether a change was posted from the detail page (form
// field from=detail): its result is then shown there, not on the list.
func fromDetail(r *http.Request) bool { return r.PostFormValue("from") == "detail" }

// showOTP renders the one-time password result page.
func (s *Service) showOTP(w http.ResponseWriter, r *http.Request, ci ctxInfo, user, otp string, created bool) {
	s.userPages().OneTimePassword(w, r, http.StatusOK, OTPPageData{
		BasePath: httpx.BasePath(r.Context()), CSRFToken: ci.csrf, User: user, Password: otp,
		ExpiresAt: s.o.Clock.Now().Add(OTPValidity), Created: created,
	})
}

// profileSubmit is POST /account/profile: the signed-in local user sets the
// own display name and e-mail.
func (s *Service) profileSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ci := s.resolve(r)
	display, email := r.PostFormValue("display_name"), r.PostFormValue("email")
	err := s.accounts.SetOwnProfile(ctx, ci.p.Name, ci.passwordGen, display, email)
	d := PasswordPageData{BasePath: httpx.BasePath(ctx), CSRFToken: ci.csrf, Self: ci.p.Name, DisplayName: display, Email: email, Local: true}
	switch {
	case err == nil:
		s.record(r, ci.p.Subject, "account.profile", "ok", nil)
		s.redirectLocal(w, r, "/account?profile=saved")
	case errors.Is(err, ErrBadName), errors.Is(err, ErrBadEmail):
		d.ErrField, d.Error = inputProblem(err)
		s.accountPages().ChangePassword(w, r, http.StatusBadRequest, d)
	case errors.Is(err, ErrStaleActor):
		s.record(r, ci.p.Subject, "account.profile", "denied: account changed meanwhile", nil)
		d.Error = "Your account was changed while you were typing. Sign in again."
		s.accountPages().ChangePassword(w, r, http.StatusConflict, d)
	default:
		slog.ErrorContext(ctx, "set own profile", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// nameSubmit is POST /account/name: an SSO or proxy user sets, or with an empty value clears, the
// display name shown instead of the name their identity provider sends. It is presentation only:
// the subject that audit rows store and the role do not change, and a later sign-in does not
// overwrite it (store.SetCustomName). Local accounts have the profile form instead.
func (s *Service) nameSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ci := s.resolve(r)
	if ci.p.Method == ModeLocal || s.o.Directory == nil {
		s.o.Pages.Message(w, r, http.StatusForbidden, MessageData{
			BasePath: httpx.BasePath(ctx), Title: "Not available",
			Text: "Local accounts set their name in the profile form.",
		})
		return
	}
	typed := r.PostFormValue("display_name")
	name, _, err := CleanProfile(typed, "")
	if err == nil {
		err = s.o.Directory.SetCustomName(ctx, ci.p.Subject, name)
	}
	switch {
	case err == nil:
		action := "account.display_name"
		if name == "" {
			action = "account.display_name.clear"
		}
		s.record(r, ci.p.Subject, action, "ok", nil)
		s.redirectLocal(w, r, "/account?profile=saved")
	case errors.Is(err, ErrBadName):
		d := PasswordPageData{BasePath: httpx.BasePath(ctx), CSRFToken: ci.csrf, Self: ci.p.Name, IdP: s.idpName(), NameForm: true, DisplayName: typed}
		d.ErrField, d.Error = inputProblem(err)
		s.accountPages().ChangePassword(w, r, http.StatusBadRequest, d)
	default:
		slog.ErrorContext(ctx, "set display name", "err", err) // includes store.ErrNotFound: a subject that never signed in cannot be here
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// endOneSession ends the single session (form field "session") of subject.
// The caller has checked the step-up.
func (s *Service) endOneSession(w http.ResponseWriter, r *http.Request, ci ctxInfo, subject, id string) {
	detail := map[string]string{"subject": subject}
	name := ""
	if local, ok := strings.CutPrefix(subject, "local:"); ok {
		name = local
	}
	back := func(status int, notice, errMsg string) {
		if name != "" {
			s.renderDetail(w, r, status, name, "", notice, errMsg, "")
			return
		}
		s.renderDetail(w, r, status, "", subject, notice, errMsg, "")
	}
	if id == ci.sessionID {
		s.record(r, ci.p.Subject, "account.session.end", "denied: own session", detail)
		back(http.StatusBadRequest, "", "Use Sign out to end your own session.")
		return
	}
	if s.o.Directory == nil {
		http.NotFound(w, r)
		return
	}
	ok, err := s.o.Directory.DeleteSubjectSession(r.Context(), subject, id)
	if err != nil {
		slog.ErrorContext(r.Context(), "end session", "err", err)
		s.record(r, ci.p.Subject, "account.session.end", "error", detail)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.record(r, ci.p.Subject, "account.session.end", "denied: no such session", detail)
		back(http.StatusNotFound, "", "That session has already ended.")
		return
	}
	s.record(r, ci.p.Subject, "account.session.end", "ok", detail)
	back(http.StatusOK, "Session ended. The user can sign in again.", "")
}
