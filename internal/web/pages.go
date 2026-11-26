package web

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Pages renders the pages of package auth in the UI's look (design v2, WP P9): sign-in, the
// forced password change and messages as standalone cards, the account and user pages in the
// app shell.
type Pages struct {
	h *handler // set by Handler: the account pages then join the app shell
}

var (
	_ auth.Pages        = (*Pages)(nil)
	_ auth.AccountPages = (*Pages)(nil)
	_ auth.UserPages    = (*Pages)(nil)
	_ auth.SetupPages   = (*Pages)(nil)
)

// NewPages returns the auth pages.
func NewPages() *Pages { return &Pages{} }

// frame is the frame of a standalone page: version and host for the card's footer.
func (p *Pages) frame(r *http.Request, title, base string) authFrame {
	f := authFrame{Title: title, Base: base, Host: r.Host, Asset: asset}
	if p.h != nil {
		f.Version = p.h.d.Version
	}
	return f
}

// write renders a standalone page; the pages carry credentials forms, so nothing is cached.
func (p *Pages) write(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		slog.ErrorContext(r.Context(), "render auth page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writePage(w, status, buf.Bytes())
}

// Login renders the sign-in page.
func (p *Pages) Login(w http.ResponseWriter, r *http.Request, status int, d auth.LoginData) {
	p.write(w, r, status, loginPage(p.frame(r, "Sign in", d.BasePath), d))
}

// Message renders a short message page.
func (p *Pages) Message(w http.ResponseWriter, r *http.Request, status int, d auth.MessageData) {
	// A signed-in user who may use the app keeps the navigation and gets the error page in the shell. A user
	// without a role (#1085) has no page of the app to go to: every link of the shell would answer 403 again, so
	// the card stands alone with Sign out, and nothing about devices, jobs or the broker is read for it.
	if pr, ok := auth.FromContext(r.Context()); ok && p.h != nil && !d.LocalLogin && pr.Role >= model.RoleOperator {
		p.h.deniedPage(w, r, status, d)
		return
	}
	p.write(w, r, status, messagePage(p.frame(r, d.Title, d.BasePath), d))
}

// StaleForm is the answer to a form whose CSRF token no longer matches (#1084: the sign-in changed
// since the page was opened), for a browser that loads it as a page. It is the standalone card: the
// check runs before any role is decided, so it draws no navigation. reload is the local path of the
// page the form was on (the caller checked it; the base path when it is not known). The answer stays
// a refusal: 403.
func (p *Pages) StaleForm(w http.ResponseWriter, r *http.Request, reload string) {
	base := httpx.BasePath(r.Context())
	p.write(w, r, http.StatusForbidden, stalePage(p.frame(r, staleTitle, base), base, reload))
}

// ChangePassword renders the account page, or the forced change before first use.
func (p *Pages) ChangePassword(w http.ResponseWriter, r *http.Request, status int, d auth.PasswordPageData) {
	// The forced change stays a centred card: nothing else is reachable yet.
	if p.h == nil || d.Forced {
		p.write(w, r, status, forcedPasswordPage(p.frame(r, "Choose a new password", d.BasePath), d))
		return
	}
	v := p.h.view(r, "Account", "account", nil)
	if d.NameForm && d.DisplayName != "" {
		v.User.Name = d.DisplayName // fresh from the save, the cached directory may lag a few seconds
	}
	p.h.show(w, r, status, "account", v, accountPage(accountViewOf(v, d)))
}

// accountViewOf is the profile the account page shows for the signed-in user.
func accountViewOf(v view, d auth.PasswordPageData) accountView {
	a := accountView{D: d, Username: v.User.Name, Name: d.DisplayName, Role: capital(v.RoleName)}
	if a.Name == "" {
		a.Name = v.User.Name
	}
	a.SignIn = signInMethod(v.User.Method, d.IdP)
	return a
}

// signInMethod says how a user signs in: "Local password", "SSO (Authentik)", "Proxy header".
func signInMethod(method, idp string) string {
	switch method {
	case auth.MethodLocal:
		return "Local password"
	case auth.MethodHeader:
		return "Proxy header"
	}
	if idp != "" {
		return "SSO (" + idp + ")"
	}
	return "SSO"
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Users renders the account administration: the list, or the add-user page.
func (p *Pages) Users(w http.ResponseWriter, r *http.Request, status int, d auth.UsersPageData) {
	if p.h == nil {
		auth.DefaultPages{}.Users(w, r, status, d)
		return
	}
	if d.New {
		v := p.h.view(r, "Add local user", "users", nil)
		v.Crumbs = []ui.Crumb{{Label: "Add local user"}}
		a := userAddView{D: d, StepUp: stepUpOf(d, p.h.d.Clock.Now(), "Adding a user")}
		p.h.show(w, r, status, "useradd", v, userAddPage(a))
		return
	}
	v := p.h.view(r, "Users", "users", nil)
	p.h.show(w, r, status, "users", v, usersPage(usersViewOf(d, p.h.d.Clock.Now(), p.h.pr.Text)))
}

// usersViewOf builds the user list: the viewer first, then by name; each row with its menu.
// nameOf names an SSO or proxy user like the audit log does (two people called "Sam" get a
// subject suffix each).
func usersViewOf(d auth.UsersPageData, now time.Time, nameOf func(subject string) string) usersView {
	users := slices.Clone(d.Users)
	slices.SortStableFunc(users, func(a, b auth.UserRow) int {
		switch {
		case a.Subject == d.Self && b.Subject != d.Self:
			return -1
		case b.Subject == d.Self && a.Subject != d.Self:
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	v := usersView{D: d, StepUp: stepUpOf(d, now, "Changing users")}
	if v.StepUp != nil {
		for _, u := range users {
			if u.Subject == d.Self {
				v.StepUp.Sub = u.Name + " · last sign-in " + lastSignIn(now, u.LastLogin)
			}
		}
	}
	for i, u := range users {
		row := userRowView{
			Name: u.Name, Avatar: u.Name, AvatarKey: u.Subject, Self: u.Subject == d.Self,
			Role: capital(u.Role), Admin: u.Role == "admin", MustChange: u.MustChangePassword,
			Deactivated: u.Disabled, Sessions: sessionsText(u.Sessions),
			MenuOpensUp: i >= 2 && i >= len(users)-2,
		}
		username := u.Name
		switch u.Method {
		case auth.MethodLocal:
			row.Source = "Local"
			if u.DisplayName != "" {
				row.Name, row.Avatar = u.DisplayName, u.DisplayName
			}
		case auth.MethodHeader:
			row.Source = "Proxy header"
			row.Name, username = nameOf(u.Subject), u.Subject
			row.Name = humanName(row.Name, u)
		default:
			row.Name = humanName(nameOf(u.Subject), u)
			row.Source = "SSO"
			if d.IdP != "" {
				row.Source += " · " + d.IdP
			}
		}
		if longToken.MatchString(username) && len(username) >= 32 {
			// a raw subject is no label: the address (or a dash) is the line, the subject the tooltip
			row.SubTitle = username
			row.Sub = orDash(u.Email)
		} else {
			row.Sub = username + " · " + orDash(u.Email)
		}
		switch {
		case u.Disabled:
			row.Last = "Deactivated"
		case u.LastLogin.IsZero():
			row.Last = "Never signed in"
		case u.Sessions > 0 && now.Sub(u.LastLogin) < 5*time.Minute:
			row.Last, row.LastAt = "Active now", u.LastLogin.UTC().Format(time.RFC3339)
		default:
			// a date, not an age: a last sign-in can be older than today (#419)
			row.Last, row.LastAt, row.LastDay = ui.StampRelativeText(now, u.LastLogin), u.LastLogin.UTC().Format(time.RFC3339), true
		}
		if !u.Disabled {
			switch u.Role {
			case "admin":
				v.Admins++
			case "operator":
				v.Operators++
			}
		}
		row.Menu, row.Dialogs = userMenu(d, u, i)
		if d.ViewOnly { // every action is disabled: nothing to open a menu for
			row.Detail = row.Menu[0].Href // "View details" is the first entry
			row.Menu, row.Dialogs = nil, nil
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// humanName keeps a readable name; when no name is recorded ("Unknown user"), the
// address of the last sign-in is the better label.
func humanName(name string, u auth.UserRow) string {
	if (strings.HasPrefix(name, "user ") || name == "Unknown user") && u.Email != "" {
		return u.Email
	}
	return name
}

// lastSignIn is the step-up dialog's "last sign-in 1h ago" ("just now", "unknown").
func lastSignIn(now, t time.Time) string {
	switch s := agoWords(now, t); s {
	case "now":
		return "just now"
	case "never":
		return "unknown"
	default:
		return s
	}
}

func sessionsText(n int) string {
	switch n {
	case 0:
		return "No sessions"
	case 1:
		return "1 session"
	}
	return strconv.Itoa(n) + " sessions"
}

// stepUpDialogID is the id of the users pages' "Confirm it's you" dialog.
const stepUpDialogID = "users-stepup"

// passwordChangeDue is the badge of an account that has to choose its own password at the next
// sign-in: one name on the users list, the user's page and the style guide.
const passwordChangeDue = "Password change due"

// stepUpOf is the step-up dialog of the users pages while changes are locked; nil when the
// viewer may change users now, or can never confirm (header principals, view only).
func stepUpOf(d auth.UsersPageData, now time.Time, what string) *ui.StepUpData {
	if !d.NeedStepUp || d.ViewOnly {
		return nil
	}
	s := &ui.StepUpData{ID: stepUpDialogID, What: what, CSRF: d.CSRFToken}
	for _, u := range d.Users {
		if u.Subject == d.Self {
			s.Name = u.Name
			s.Sub = u.Name + " · last sign-in " + lastSignIn(now, u.LastLogin)
		}
	}
	if d.StepUpKind == auth.StepUpOIDC {
		idp := d.IdP
		if idp == "" {
			idp = "your identity provider"
		}
		s.IdP, s.Action = idp, d.BasePath+"/oidc/stepup"
		s.Fields = []ui.KVRow{{Key: "next", Value: "/users"}}
	} else {
		s.Local, s.PasswordName, s.Action = true, "current", d.BasePath+"/users/stepup"
		if d.ErrField == "current" {
			s.Error = d.Error
		}
	}
	return s
}

// userMenu is the "⋯" menu of a user row and the confirmation dialogs of its actions. An action
// the rules forbid stays in the menu, disabled, with the reason (self, last admin, managed by the
// identity provider); while changes need a step-up an allowed action opens that dialog first.
// Without JavaScript every action leads to the user's detail page.
func userMenu(d auth.UsersPageData, u auth.UserRow, i int) ([]ui.MenuItem, []ui.DialogData) {
	base := d.BasePath
	detail := base + "/users/" + url.PathEscape(u.Name)
	if u.Method != auth.MethodLocal {
		detail = base + "/users/principal/" + url.PathEscape(u.Subject)
	}
	g := u.Guards
	self := u.Subject == d.Self
	var items []ui.MenuItem
	var dialogs []ui.DialogData
	// danger is the tone of the confirming button; the menu entry itself reads red only for the last one, after its rule
	// (Delete). Deactivate is a default entry here and a default button on the user page, and danger-solid in its dialog.
	add := func(key, label string, guard auth.Guard, dlg ui.DialogData, danger, sep bool) {
		it := ui.MenuItem{Label: label, Href: detail, Danger: danger && sep, Sep: sep}
		switch {
		case d.ViewOnly:
			it.Disabled, it.Why = true, "View only behind the proxy"
		case !guard.Allowed:
			it.Disabled, it.Why = true, strings.TrimSuffix(guard.Reason, ".")
		case d.NeedStepUp:
			it.Dialog = stepUpDialogID
		default:
			dlg.ID = "user-" + strconv.Itoa(i) + "-" + key
			dlg.CSRF, dlg.Danger = d.CSRFToken, danger
			it.Dialog = dlg.ID
			dialogs = append(dialogs, dlg)
		}
		items = append(items, it)
	}
	items = append(items, ui.MenuItem{Label: "View details", Href: detail})
	who := u.Name
	if u.DisplayName != "" {
		who = u.DisplayName
	}
	local := base + "/users/local/" + url.PathEscape(u.Name)
	if u.Role == "admin" {
		add("role-op", "Make operator", g.Role, ui.DialogData{Title: "Make " + who + " an operator?", Body: "Operators can switch, restart and back up devices, but cannot manage firmware, policy, users or settings.", Action: local, Verb: "Make operator",
			Fields: []ui.KVRow{{Key: "action", Value: "role"}, {Key: "role", Value: "operator"}}}, false, false)
	} else {
		raise := auth.Guard{Allowed: u.Method == auth.MethodLocal, Reason: g.Role.Reason} // raising a role is never a lock-out
		add("role-admin", "Make admin", raise, ui.DialogData{Title: "Make " + who + " an admin?", Body: "Admins change everything, including users and how people sign in.", Action: local, Verb: "Make admin",
			Fields: []ui.KVRow{{Key: "action", Value: "role"}, {Key: "role", Value: "admin"}}}, false, false)
	}
	add("otp", "Reset password", g.Reset, ui.DialogData{Title: "Reset the password of " + who + "?", Body: "A one-time password is generated and shown once. The current password stops working; " + who + " chooses a new one at the next sign-in.", Action: local, Verb: "Reset password",
		Fields: []ui.KVRow{{Key: "action", Value: "otp"}}}, false, false)
	if u.Disabled {
		add("enable", "Reactivate", g.Disable, ui.DialogData{Title: "Reactivate " + who + "?", Body: who + " can sign in again.", Action: local, Verb: "Reactivate",
			Fields: []ui.KVRow{{Key: "action", Value: "enable"}}}, false, false)
	} else {
		add("disable", "Deactivate", g.Disable, ui.DialogData{Title: "Deactivate " + who + "?", Body: who + " is signed out at once and cannot sign in until an admin reactivates the account.", Action: local, Verb: "Deactivate user",
			Fields: []ui.KVRow{{Key: "action", Value: "disable"}}}, true, false)
	}
	end := auth.Guard{Allowed: (u.Method == auth.MethodSSO || u.Method == auth.MethodLocal) && u.Sessions > 0 && !self}
	switch {
	case self:
		end.Reason = "Use Sign out for your own session"
	case u.Method == auth.MethodHeader:
		end.Reason = endHeaderWhy
	case u.Sessions == 0:
		end.Reason = "No active sessions"
	}
	endBody := who + " is signed out everywhere and can sign in again; the role still comes from the identity provider."
	if u.Method == auth.MethodLocal {
		endBody = who + " is signed out everywhere and can sign in again with the same password."
	}
	add("sessions", "End all sessions", end, ui.DialogData{Title: "End all sessions of " + who + "?", Body: endBody, Action: base + "/users/sessions", Verb: "End sessions",
		Fields: []ui.KVRow{{Key: "subject", Value: u.Subject}}}, false, false)
	add("delete", "Delete", g.Delete, ui.DialogData{Title: "Delete " + who + "?", Body: "The account is removed for good. The audit log keeps its entries.", Action: local, Verb: "Delete user",
		Fields: []ui.KVRow{{Key: "action", Value: "delete"}}}, true, true)
	return items, dialogs
}

// errorPageIn writes an error page in the shell (design v2 screens e404, e403, eoff) under a navigation entry, so a 404 below /devices keeps Devices
// highlighted and the trail says where the missing page would have been. short is the last step of the
// trail ("Not found"); the page's own title, and so the browser tab's, is its heading (#1085).
func (h *handler) errorPageIn(w http.ResponseWriter, r *http.Request, status int, short, nav string, e errView) {
	h.show(w, r, status, "error", h.errorView(r, short, nav, e), errPage(e))
}

// errorView is the view of an error page: titled like its heading, the trail ends in short.
func (h *handler) errorView(r *http.Request, short, nav string, e errView) view {
	v := h.view(r, e.Title, nav, nil)
	v.Crumbs = []ui.Crumb{{Label: short}}
	return v
}

// wayButtons are the two ways on of a refused or switched-off page: back to the place it belongs to first, the
// dashboard second. A page that belongs to no group (the dashboard's own) leads back to the dashboard, then to the devices.
func wayButtons(v view, base string) (primary, secondary ui.Btn) {
	label, path := shellData(v).WayBack()
	if label == "Dashboard" {
		return ui.Btn{Label: "Back to dashboard", Kind: "primary", Href: base + "/"}, ui.Btn{Label: "All devices", Href: base + "/devices"}
	}
	return ui.Btn{Label: "Back to " + label, Kind: "primary", Href: base + path}, ui.Btn{Label: "Go to dashboard", Href: base + "/"}
}

// deniedPage is an answer of the auth guard (a refusal, an unknown user) for a person who may use the app: the
// error page in the shell, in the place the address belongs to (#1085). A refused GET below /devices/{id}/ stays
// inside that device's header with its tabs, when the device exists; every other address lights the group
// navOfPath names. The device header is drawn only for a page the person reads (a GET, no htmx): a refused POST or
// swap needs no read of the device, the jobs or the router. The caller has checked that the principal is an
// operator or more: a person without a role never gets here.
func (h *handler) deniedPage(w http.ResponseWriter, r *http.Request, status int, d auth.MessageData) {
	base := httpx.BasePath(r.Context())
	code, short := strconv.Itoa(status), d.Title
	switch status {
	case http.StatusForbidden:
		code, short = code+" · Not allowed", "Not allowed"
	case http.StatusNotFound:
		code, short = code+" · Not found", "Not found"
	}
	e := errView{Code: code, Title: d.Title, Body: d.Text, Path: base + r.URL.Path}
	if dev, ok := h.deniedDevice(r); ok {
		v := h.deviceMessageView(r, dev, e.Title, nil)
		v.Crumbs[len(v.Crumbs)-1].Label = short
		e.Primary = ui.Btn{Label: "Back to " + dev.Name, Kind: "primary", Href: base + devPath(dev.ID, "")}
		e.Secondary = ui.Btn{Label: "All devices", Href: base + "/devices"}
		h.show(w, r, status, "error", v, deniedDevicePage(h.tabsOf(r, v, dev, ""), e))
		return
	}
	v := h.errorView(r, short, navOfPath(r.URL.Path), e)
	e.Primary, e.Secondary = wayButtons(v, base)
	h.show(w, r, status, "error", v, errPage(e))
}

// deniedDevice is the device a refused page of the person's reading belongs to: a GET without htmx below
// /devices/{id}/, whose device exists. A failed read only costs the header (the page falls back to the Devices group).
func (h *handler) deniedDevice(r *http.Request) (model.Device, bool) {
	if r.Method != http.MethodGet || r.Header.Get("HX-Request") != "" {
		return model.Device{}, false
	}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 3 || segs[0] != "devices" {
		return model.Device{}, false
	}
	id, err := strconv.ParseInt(segs[1], 10, 64)
	if err != nil || id <= 0 {
		return model.Device{}, false
	}
	dev, err := h.d.Inventory.Get(r.Context(), model.DeviceID(id))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.log.WarnContext(r.Context(), "refused page: device unavailable", "device", id, "err", err)
		}
		return model.Device{}, false
	}
	return dev, true
}

// navOfPath is the navigation entry an error page belongs to, by the address of the page (the router has
// already stripped the base path from r.URL.Path); "" for the rest. The auth guard answers an admin-only page with
// a 403 before any handler runs, so this is the one place that knows which entry that page would have lit:
// TestDeniedPageLightsTheAdminPagesGroup compares it with the pages themselves.
func navOfPath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	second := ""
	if len(segs) > 1 {
		second = segs[1]
	}
	switch segs[0] {
	case "devices":
		switch second {
		case "new":
			return "discovery"
		case "backups":
			return "backups"
		}
		return "devices"
	case "discovery", "import":
		return "discovery"
	case "backups", "restores":
		return "backups"
	case "updates":
		return "updates"
	case "settings":
		if len(segs) > 2 && segs[2] == "snapshots" {
			return "snapshots"
		}
		return "system"
	case "transactions", "firmware", "policy", "users", "audit", "about":
		return segs[0]
	}
	return ""
}

// UserDetail renders the page of one user (design v2 screens user-u1..u5, N4): who, the actions
// the rules allow (the others disabled with the reason), details, live sessions and the user's
// recent audit entries.
func (p *Pages) UserDetail(w http.ResponseWriter, r *http.Request, status int, d auth.UserDetailData) {
	if p.h == nil {
		auth.DefaultPages{}.UserDetail(w, r, status, d)
		return
	}
	nameOf := p.h.pr.Text
	u := userDetailViewOf(d, p.h.d.Clock.Now(), nameOf)
	v := p.h.view(r, u.Title, "users", nil)
	v.Crumbs = []ui.Crumb{{Label: u.Title}}
	if u.StepUp != nil {
		u.StepUp.Name = v.User.Name
		u.StepUp.Sub = v.User.Name + " · " + signInMethod(v.User.Method, d.IdP)
	}
	p.h.show(w, r, status, "users", v, userDetailPage(u))
}

// OneTimePassword renders the generated password, the only time it is shown (screen userotp).
// The page is never cached (writePage sends Cache-Control: no-store).
func (p *Pages) OneTimePassword(w http.ResponseWriter, r *http.Request, status int, d auth.OTPPageData) {
	if p.h == nil {
		auth.DefaultPages{}.OneTimePassword(w, r, status, d)
		return
	}
	v := p.h.view(r, "One-time password", "users", nil)
	v.Crumbs = []ui.Crumb{{Label: "One-time password"}}
	o := otpView{D: d, Detail: d.BasePath + "/users/" + url.PathEscape(d.User),
		Expires: d.ExpiresAt.In(p.h.d.Clock.Now().Location()).Format("2 Jan 2006, 15:04")}
	p.h.show(w, r, status, "users", v, otpPage(o))
}

// SetupAccount renders setup step 1 (screens setup, setup-v-tokenerr, setup-v-error, N12): without
// a valid token where to find the link, with it the first admin's password form. The token is
// only ever in the hidden form field; package auth sends Referrer-Policy: origin.
func (p *Pages) SetupAccount(w http.ResponseWriter, r *http.Request, status int, d auth.SetupAccountData) {
	title := "Create the first admin account"
	if d.NeedToken {
		title = "Open the setup link"
	}
	p.write(w, r, status, setupAccountPage(p.frame(r, title, d.BasePath), d))
}

// userDetailView is the user detail page.
type userDetailView struct {
	D            auth.UserDetailData
	Title        string // display name, or the username
	Sub          string // "maren · SSO · Authentik"
	Avatar       string // the name the initials come from
	Self         bool
	Role         string // "Admin", "Operator", "" (unknown)
	Method       string // "Local", "SSO · Authentik", "Proxy header"
	Username     string
	Actions      []ui.MenuItem     // the buttons: Make admin/operator, Reset password, Deactivate/Reactivate, End all sessions, Delete…
	Dialogs      []ui.DialogData   // their confirmations
	Why          string            // why buttons are disabled (local accounts; principals have the banner)
	Reasons      []reasonGroup     // one line per reason of a disabled action button (reasonID names the line of a button)
	ReasonIDs    map[string]string // button label -> id of the element that says why it is disabled (a line, or the info banner)
	Info         string            // the banner of an SSO or proxy user
	Reactivate   *ui.MenuItem      // the deactivated banner's button
	Facts        []ui.KVRow
	Sessions     []sessionView
	Activity     []activityView
	AuditHref    string
	LocalAction  string // POST /users/local/{name}
	ProfileOpen  bool
	PasswordOpen bool
	StepUp       *ui.StepUpData
}

// userActionKind is the kind of a user page button: the danger one for the menu's red entry, default for the rest.
func userActionKind(a ui.MenuItem) string {
	if a.Danger {
		return "danger"
	}
	return ""
}

// reactivateAction is the deactivated banner's one button, a link that opens the Reactivate dialog; nil when the
// viewer cannot reactivate the account.
func (v userDetailView) reactivateAction() *ui.Btn {
	if v.Reactivate == nil {
		return nil
	}
	return &ui.Btn{Label: "Reactivate", Href: v.Reactivate.Href, Dialog: v.Reactivate.Dialog}
}

// udReasonID is the id of the n-th line that says why an action button of the user page is disabled.
func udReasonID(n int) string { return "ud-why-" + strconv.Itoa(n) }

// udInfoID is the id of the info banner of an SSO or proxy user: it says why a button it already explains is off.
const udInfoID = "ud-info"

// endHeaderWhy is why End all sessions is off for a proxy user (the info banner of that user says the same).
const endHeaderWhy = "The proxy signs this user in on every request"

// reasonID is the id of the line that says why the button labelled label is disabled.
func (v userDetailView) reasonID(label string) string { return v.ReasonIDs[label] }

type sessionView struct {
	ID, Started, StartedAt, Seen, SeenAt string
	Current                              bool
}

type activityView struct {
	When, At, Action, Summary, Status string
	Tone                              ui.Tone
}

// userDetailViewOf builds the page. The actions are the list's row menu (userMenu) as buttons, each
// confirmation posting from=detail so the result comes back on this page.
func userDetailViewOf(d auth.UserDetailData, now time.Time, nameOf func(subject string) string) userDetailView {
	u := d.User
	u.Guards = d.Guards
	v := userDetailView{D: d, Self: u.Subject == d.Self, Role: capital(u.Role)}
	v.Title, v.Avatar, v.Username = u.Name, u.Name, u.Name
	switch u.Method {
	case auth.MethodLocal:
		v.Method = "Local"
		if d.DisplayName != "" {
			v.Title, v.Avatar = d.DisplayName, d.DisplayName
		}
	case auth.MethodHeader:
		v.Method, v.Username = "Proxy header", u.Subject
		v.Title, v.Avatar = nameOf(u.Subject), nameOf(u.Subject)
		v.Info = "Signed in by the authenticating proxy on every request. No password is stored for this user; the role comes from the proxy’s groups."
	default:
		v.Method = "SSO"
		if d.IdP != "" {
			v.Method += " · " + d.IdP
		}
		v.Title, v.Avatar = nameOf(u.Subject), nameOf(u.Subject)
		idp := d.IdP
		if idp == "" {
			idp = "the identity provider"
		}
		v.Info = "Name, email and password are managed in " + idp + ". The role comes from its group mapping."
	}
	v.Sub = v.Username + " · " + v.Method
	list := auth.UsersPageData{BasePath: d.BasePath, CSRFToken: d.CSRFToken, Self: d.Self, ViewOnly: d.ViewOnly,
		NeedStepUp: d.NeedStepUp, StepUpKind: d.StepUpKind, IdP: d.IdP}
	items, dialogs := userMenu(list, u, 0)
	v.Actions = items[1:] // without "View details": this is the page
	v.LocalAction = d.BasePath + "/users/local/" + url.PathEscape(u.Name)
	for i := range dialogs {
		if dialogs[i].Action == v.LocalAction {
			dialogs[i].Fields = append(dialogs[i].Fields, ui.KVRow{Key: "from", Value: "detail"})
		}
	}
	v.Dialogs = dialogs
	if u.Disabled {
		for i := range v.Actions {
			if v.Actions[i].Label == "Reactivate" && !v.Actions[i].Disabled {
				it := v.Actions[i]
				v.Reactivate = &it
			}
		}
	}
	if u.Method == auth.MethodLocal {
		v.Why = guardText(v.Self, d.Guards)
	}
	// A disabled button is explained by the info banner when that says it already (managed elsewhere, signed in by
	// the proxy), else by a line of its own reason, which reads as a sentence.
	v.ReasonIDs = map[string]string{}
	managed := strings.TrimSuffix(d.Guards.Role.Reason, ".")
	var blocked []blockedAction
	for _, a := range v.Actions {
		switch {
		case !a.Disabled || a.Why == "":
		case v.Info != "" && u.Method != auth.MethodLocal && (a.Why == managed || a.Why == endHeaderWhy):
			v.ReasonIDs[a.Label] = udInfoID
		default:
			blocked = append(blocked, blockedAction{Label: a.Label, Why: strings.TrimSuffix(a.Why, ".") + "."})
		}
	}
	v.Reasons = groupReasons(blocked)
	for i, g := range v.Reasons {
		for _, l := range g.Labels {
			v.ReasonIDs[l] = udReasonID(i)
		}
	}
	if s := stepUpOf(list, now, "Changing "+possessive(v.Title)+" account"); s != nil {
		if s.IdP != "" {
			s.Fields = []ui.KVRow{{Key: "next", Value: detailPath(u)}}
		}
		v.StepUp = s
	}
	v.Facts = []ui.KVRow{{Key: "Role", Value: orDash(v.Role)}, {Key: "Sign-in method", Value: v.Method}, {Key: "Username", Value: v.Username},
		{Key: "Email", Value: orDash(d.Email)}}
	if !d.CreatedAt.IsZero() {
		v.Facts = append(v.Facts, ui.KVRow{Key: "Created", Value: d.CreatedAt.In(now.Location()).Format("2 Jan 2006") + createdBy(d.CreatedBy, nameOf), Mask: true})
	}
	last := ui.KVRow{Key: "Last sign-in", Value: "Never"}
	if !u.LastLogin.IsZero() {
		// Today/Yesterday are decided in the browser's zone, like every other relative day
		last = ui.KVRow{Key: "Last sign-in", Value: ui.StampRelativeText(now, u.LastLogin), Mask: true, At: u.LastLogin, Stamp: ui.StampRelative}
	}
	v.Facts = append(v.Facts, last)
	if !d.OTPExpiresAt.IsZero() {
		otp := "Valid until " + ui.StampRelativeText(now, d.OTPExpiresAt)
		if d.OTPExpired {
			otp = "Expired: reset the password again"
		}
		v.Facts = append(v.Facts, ui.KVRow{Key: "One-time password", Value: otp, Mask: !d.OTPExpired})
	}
	for _, s := range d.Sessions {
		v.Sessions = append(v.Sessions, sessionView{ID: s.ID, Current: s.Current,
			Started: ui.StampRelativeText(now, s.CreatedAt), StartedAt: s.CreatedAt.UTC().Format(time.RFC3339),
			Seen: agoWords(now, s.LastSeen), SeenAt: s.LastSeen.UTC().Format(time.RFC3339)})
	}
	for _, e := range d.Audit {
		a := activityView{When: ui.StampRelativeText(now, e.At), At: e.At.UTC().Format(time.RFC3339), Action: e.Action, Summary: auditShort(e)}
		a.Tone, a.Status = auditBadge(auditStatus(e))
		v.Activity = append(v.Activity, a)
	}
	v.AuditHref = d.BasePath + "/audit?since=all&actor=" + url.QueryEscape(u.Subject)
	v.ProfileOpen = d.ErrField == "display_name" || d.ErrField == "email"
	v.PasswordOpen = d.ErrField == "password"
	return v
}

// detailPath is the local path of a user's detail page (the step-up's way back).
func detailPath(u auth.UserRow) string {
	if u.Method == auth.MethodLocal {
		return "/users/" + url.PathEscape(u.Name)
	}
	return "/users/principal/" + url.PathEscape(u.Subject)
}

// guardText is the line under the buttons of a local account: why some are disabled. The own
// account and the last admin are said in one sentence, as the prototype does.
func guardText(self bool, g auth.UserGuards) string {
	last := false
	for _, x := range []auth.Guard{g.Role, g.Disable, g.Delete} {
		last = last || x.Reason == auth.MsgLastAdmin
	}
	switch {
	case self && last:
		return "That’s you, and you’re the last active admin."
	case self:
		return "That’s you: change your own name, email and password on the Account page."
	case last:
		return "This is the last active admin: make another admin first."
	}
	return ""
}

func possessive(name string) string {
	if strings.HasSuffix(name, "s") {
		return name + "’"
	}
	return name + "’s"
}

// createdBy is the creator after the date: " by maren", " from the command line", " at first start".
func createdBy(by string, nameOf func(string) string) string {
	switch by {
	case "":
		return ""
	case auth.CreatedByCLI:
		return " from the command line"
	case auth.CreatedByFirstStart:
		return " at first start"
	}
	if n, ok := strings.CutPrefix(by, "local:"); ok {
		return " by " + n
	}
	return " by " + nameOf(by)
}

// otpView is the one-time password page.
type otpView struct {
	D       auth.OTPPageData
	Detail  string // the user's detail page (Done)
	Expires string
}
