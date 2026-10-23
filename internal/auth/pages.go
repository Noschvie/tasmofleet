package auth

import (
	"html/template"
	"log/slog"
	"net/http"
)

// LoginData is rendered by Pages.Login: the one sign-in page, with the
// single sign-on button (SSO) and/or the local form (Local).
type LoginData struct {
	BasePath  string
	CSRFToken string // form field "csrf"
	Next      string // form field "next", a local path
	Error     string
	Notice    string // neutral hint above the form (signed out, sign-in needed to continue)
	Username  string // keeps what was typed after a failed attempt (never the password)
	Local     bool   // show the username/password form (LOCAL_LOGIN on and a local user exists)
	SSO       string // IdP name for the "Sign in with" button; empty: no button (OIDC not configured)
	// Expired: the previous session ran out (banner "session expired", N11).
	Expired bool
	// Remember keeps the "Keep me signed in on this device" choice (form
	// field "remember") after a failed attempt. Unchecked, the session cookie
	// ends with the browser.
	Remember bool
	// ShowAttemptsLeft: a failed attempt; AttemptsLeft is how many more
	// attempts are possible before sign-in from here is delayed (N11).
	ShowAttemptsLeft bool
	AttemptsLeft     int
}

// MessageData is rendered by Pages.Message (403, 503, failed sign-in).
type MessageData struct {
	BasePath   string
	Title      string
	Text       string
	LocalLogin bool   // offer the link back to the sign-in page (local sign-in is possible there)
	SignOut    bool   // offer a Sign out form: the user holds a session but no page of the app is open to them
	CSRFToken  string // the token that form carries
}

// Pages renders the few HTML pages auth needs. Package web may supply its
// own templates; DefaultPages is a plain fallback.
type Pages interface {
	Login(w http.ResponseWriter, r *http.Request, status int, d LoginData)
	Message(w http.ResponseWriter, r *http.Request, status int, d MessageData)
}

// DefaultPages renders minimal pages without inline script or style.
type DefaultPages struct{}

var pagesTmpl = template.Must(template.New("").Parse(`
{{define "login"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sign in</title></head><body>
<h1>Sign in</h1>
{{with .Notice}}<p role="status">{{.}}</p>{{end}}
{{with .Error}}<p role="alert">{{.}}</p>{{end}}
{{if .SSO}}<p><a href="{{.BasePath}}/login/sso?next={{.Next}}">Sign in with {{.SSO}}</a></p>{{end}}
{{if .Local}}<form method="post" action="{{.BasePath}}/login/local">
<input type="hidden" name="csrf" value="{{.CSRFToken}}"><input type="hidden" name="next" value="{{.Next}}">
<label>Username <input name="username" autocomplete="username" required></label>
<label>Password <input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button></form>{{end}}
{{if not (or .SSO .Local)}}<p>Sign-in is disabled.</p>{{end}}
</body></html>{{end}}
{{define "password"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Change password</title></head><body>
<h1>Change password</h1>
{{if .Forced}}<p>You must choose a new password before you can continue.</p>{{end}}
{{with .Error}}<p role="alert">{{.}}</p>{{end}}
{{if not .Local}}<p>Your password is managed where you sign in.</p>{{else}}<form method="post" action="{{.BasePath}}/account/password">
<input type="hidden" name="csrf" value="{{.CSRFToken}}">
<label>Current password <input name="current" type="password" autocomplete="current-password" required></label>
<label>New password <input name="new" type="password" autocomplete="new-password" minlength="12" required></label>
<label>Repeat <input name="confirm" type="password" autocomplete="new-password" minlength="12" required></label>
<button type="submit">Change password</button></form>{{end}}
</body></html>{{end}}
{{define "users"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Users</title></head><body>
<h1>Users</h1>
{{with .Notice}}<p role="status">{{.}}</p>{{end}}
{{with .Error}}<p role="alert">{{.}}</p>{{end}}
{{if .ViewOnly}}<p>View only: behind a header-authenticating proxy there is nothing to confirm a change with.</p>
{{else if .NeedStepUp}}{{if eq .StepUpKind "oidc"}}
<form method="post" action="{{.BasePath}}/oidc/stepup">
<input type="hidden" name="csrf" value="{{.CSRFToken}}"><input type="hidden" name="next" value="/users">
<button type="submit">Sign in again to make changes</button></form>
{{else}}
<form method="post" action="{{.BasePath}}/users/stepup">
<input type="hidden" name="csrf" value="{{.CSRFToken}}">
<label>Confirm your password <input name="current" type="password" autocomplete="current-password" required></label>
<button type="submit">Continue</button></form>{{end}}{{end}}
<table><thead><tr><th>User</th><th>Method</th><th>Role</th><th>State</th><th>Last login</th><th>Sessions</th><th>Actions</th></tr></thead><tbody>
{{range .Users}}<tr><td>{{.Name}}</td><td>{{.Method}}</td><td>{{.Role}}</td><td>{{if .Disabled}}disabled{{else}}enabled{{end}}{{if .MustChangePassword}}, must change password{{end}}</td><td>{{if .LastLogin.IsZero}}-{{else}}{{.LastLogin.Format "2006-01-02 15:04"}}{{end}}</td><td>{{.Sessions}}</td><td>
{{if or $.ViewOnly $.NeedStepUp}}{{else if eq .Method "local"}}
<form method="post" action="{{$.BasePath}}/users/local/{{.Name}}"><input type="hidden" name="csrf" value="{{$.CSRFToken}}">
<select name="role"><option value="operator"{{if eq .Role "operator"}} selected{{end}}>operator</option><option value="admin"{{if eq .Role "admin"}} selected{{end}}>admin</option></select>
<button name="action" value="role">Set role</button>
<input name="password" type="password" autocomplete="new-password" placeholder="new password"><button name="action" value="password">Reset password</button>
{{if .Disabled}}<button name="action" value="enable">Enable</button>{{else}}<button name="action" value="disable">Disable</button>{{end}}
<button name="action" value="delete">Delete</button></form>
{{else if and (eq .Method "sso") (gt .Sessions 0) (ne .Subject $.Self)}}
<form method="post" action="{{$.BasePath}}/users/sessions"><input type="hidden" name="csrf" value="{{$.CSRFToken}}"><input type="hidden" name="subject" value="{{.Subject}}">
<button type="submit">End sessions</button></form>{{end}}</td></tr>{{end}}
</tbody></table>
{{if not (or .ViewOnly .NeedStepUp)}}<h2>New user</h2>
<form method="post" action="{{.BasePath}}/users"><input type="hidden" name="csrf" value="{{.CSRFToken}}">
<label>Username <input name="username" required></label>
<label>Password <input name="password" type="password" autocomplete="new-password" minlength="12" required></label>
<label>Role <select name="role"><option value="operator">operator</option><option value="admin">admin</option></select></label>
<button type="submit">Create</button></form>{{end}}
</body></html>{{end}}
{{define "userdetail"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>{{.User.Name}}</title></head><body>
<h1>{{.User.Name}}</h1>
{{with .Notice}}<p role="status">{{.}}</p>{{end}}
{{with .Error}}<p role="alert">{{.}}</p>{{end}}
<dl><dt>Method</dt><dd>{{.User.Method}}</dd><dt>Role</dt><dd>{{.User.Role}}</dd>
<dt>Name</dt><dd>{{.DisplayName}}</dd><dt>E-mail</dt><dd>{{.Email}}</dd>
{{if not .CreatedAt.IsZero}}<dt>Created</dt><dd>{{.CreatedAt.Format "2006-01-02 15:04"}}{{with .CreatedBy}} by {{.}}{{end}}</dd>{{end}}
{{if not .OTPExpiresAt.IsZero}}<dt>One-time password</dt><dd>{{if .OTPExpired}}expired{{else}}valid until {{.OTPExpiresAt.Format "2006-01-02 15:04"}}{{end}}</dd>{{end}}</dl>
<h2>Sessions</h2><ul>{{range .Sessions}}<li>{{.LastSeen.Format "2006-01-02 15:04"}}{{if .Current}} (this session){{else if not (or $.ViewOnly $.NeedStepUp)}}
<form method="post" action="{{$.BasePath}}/users/sessions"><input type="hidden" name="csrf" value="{{$.CSRFToken}}">
<input type="hidden" name="subject" value="{{$.User.Subject}}"><input type="hidden" name="session" value="{{.ID}}">
<button type="submit">End session</button></form>{{end}}</li>{{end}}</ul>
{{if and (eq .User.Method "local") (not (or .ViewOnly .NeedStepUp)) .Guards.Reset.Allowed}}
<form method="post" action="{{.BasePath}}/users/local/{{.User.Name}}"><input type="hidden" name="csrf" value="{{.CSRFToken}}">
<input type="hidden" name="from" value="detail"><button name="action" value="otp">Generate a one-time password</button></form>{{end}}
<h2>Recent activity</h2><ul>{{range .Audit}}<li>{{.At.Format "2006-01-02 15:04"}} {{.Action}} {{.Result}}</li>{{end}}</ul>
<p><a href="{{.BasePath}}/users">All users</a></p>
</body></html>{{end}}
{{define "otp"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>One-time password</title></head><body>
<h1>One-time password for {{.User}}</h1>
<p>Give this password to {{.User}}. It is shown only now, is valid until {{.ExpiresAt.Format "2006-01-02 15:04"}} and must be changed at the first sign-in.</p>
<p><code>{{.Password}}</code></p>
<p><a href="{{.BasePath}}/users/{{.User}}">Back to {{.User}}</a></p>
</body></html>{{end}}
{{define "setupaccount"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Create the first admin account</title></head><body>
<h1>Create the first admin account</h1>
{{if .NeedToken}}<p>{{if .TokenInvalid}}This setup link is not valid (any more). {{end}}Open the setup link from the container log: the line that reports the created admin account contains it.</p>
{{else}}{{with .Error}}<p role="alert">{{.}}</p>{{end}}
<form method="post" action="{{.BasePath}}/setup/account">
<input type="hidden" name="csrf" value="{{.CSRFToken}}"><input type="hidden" name="token" value="{{.Token}}">
<p>Username: {{.Username}}</p>
<label>Name <input name="display_name" value="{{.DisplayName}}"></label>
<label>E-mail <input name="email" type="email" value="{{.Email}}"></label>
<label>Password <input name="password" type="password" autocomplete="new-password" minlength="12" required></label>
<label>Repeat <input name="confirm" type="password" autocomplete="new-password" minlength="12" required></label>
<button type="submit">Create account</button></form>{{end}}
</body></html>{{end}}
{{define "message"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><title>{{.Title}}</title></head><body>
<h1>{{.Title}}</h1><p>{{.Text}}</p>
{{if .LocalLogin}}<p><a href="{{.BasePath}}/login">Back to sign-in</a></p>{{end}}
{{if .SignOut}}<form method="post" action="{{.BasePath}}/logout"><input type="hidden" name="csrf" value="{{.CSRFToken}}"><button type="submit">Sign out</button></form>{{end}}
</body></html>{{end}}`))

func render(w http.ResponseWriter, r *http.Request, status int, name string, d any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := pagesTmpl.ExecuteTemplate(w, name, d); err != nil {
		slog.ErrorContext(r.Context(), "render auth page", "page", name, "err", err)
	}
}

// Login renders the sign-in page.
func (DefaultPages) Login(w http.ResponseWriter, r *http.Request, status int, d LoginData) {
	render(w, r, status, "login", d)
}

// Message renders a short message page.
func (DefaultPages) Message(w http.ResponseWriter, r *http.Request, status int, d MessageData) {
	render(w, r, status, "message", d)
}

// ChangePassword renders the own-password form.
func (DefaultPages) ChangePassword(w http.ResponseWriter, r *http.Request, status int, d PasswordPageData) {
	render(w, r, status, "password", d)
}

// Users renders the account administration page.
func (DefaultPages) Users(w http.ResponseWriter, r *http.Request, status int, d UsersPageData) {
	render(w, r, status, "users", d)
}

// UserDetail renders the user detail page.
func (DefaultPages) UserDetail(w http.ResponseWriter, r *http.Request, status int, d UserDetailData) {
	render(w, r, status, "userdetail", d)
}

// OneTimePassword renders the generated password once.
func (DefaultPages) OneTimePassword(w http.ResponseWriter, r *http.Request, status int, d OTPPageData) {
	render(w, r, status, "otp", d)
}

// SetupAccount renders setup step 1.
func (DefaultPages) SetupAccount(w http.ResponseWriter, r *http.Request, status int, d SetupAccountData) {
	render(w, r, status, "setupaccount", d)
}
