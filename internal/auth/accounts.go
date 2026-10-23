package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Account errors.
var (
	ErrUserExists   = errors.New("auth: user already exists")
	ErrUserNotFound = errors.New("auth: user not found")
	ErrBadUsername  = errors.New("auth: username must be 1-64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	ErrBadRole      = errors.New("auth: role must be operator or admin")
	ErrBadName      = fmt.Errorf("auth: display name must be at most %d characters without control characters", maxDisplayName)
	ErrBadEmail     = errors.New("auth: e-mail must be a plain address like name@example.org")
	ErrLastAdmin    = store.ErrLastAdmin
	// ErrStaleActor: the acting account was changed, disabled or demoted while
	// the request ran.
	ErrStaleActor = store.ErrStaleActor
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NormalizeUsername lower-cases and trims name. Usernames are lower case so
// that "Admin" and "admin" cannot be two accounts.
func NormalizeUsername(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ParseRole maps "operator" and "admin" to a role; anything else (in
// particular "none" and "viewer") is ErrBadRole.
func ParseRole(s string) (model.Role, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "operator":
		return model.RoleOperator, nil
	case "admin":
		return model.RoleAdmin, nil
	}
	return model.RoleNone, ErrBadRole
}

// Accounts manages local accounts. The CLI and the admin web UI both use it,
// so the rules exist once. The invariants that must hold across processes
// (an existing account is never overwritten, the last enabled admin stays)
// are enforced by the store in single SQL statements, not here.
// LocalUser.UpdatedAt is the time of the last password change.
type Accounts struct {
	Users    store.LocalUserStore
	Sessions store.SessionStore
	Cookies  store.DeviceCookieStore
	Clock    clock.Clock // default clock.Real

	by store.Actor // authority of the caller; zero = trusted host CLI
}

// As returns a copy whose mutations run on the authority of the signed-in
// admin by: the store applies each change only if, in the same SQL
// statement, by is still an enabled admin at the same password generation.
// The web UI must use As for every mutation; the plain value is for the CLI.
func (a *Accounts) As(by store.Actor) *Accounts {
	c := *a
	c.by = by
	return &c
}

func (a *Accounts) now() clock.Clock {
	if a.Clock == nil {
		return clock.Real{}
	}
	return a.Clock
}

// SubjectOf is the session subject of a local account.
func SubjectOf(username string) string { return "local:" + username }

// mapStoreErr translates store errors into the account errors.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrUserNotFound
	case errors.Is(err, store.ErrConflict):
		return ErrUserExists
	}
	return err
}

// Values of LocalUser.CreatedBy that are not a subject.
const (
	CreatedByCLI        = "cli"
	CreatedByFirstStart = "first-start"
)

// OTPValidity is how long a generated one-time password can be used to sign in.
const OTPValidity = 24 * time.Hour

const (
	maxDisplayName = 64
	maxEmail       = 254
)

// Add creates an account from the CLI. mustChange forces a password change
// at the first login (always set when someone else chose the password).
func (a *Accounts) Add(ctx context.Context, name, password string, role model.Role, mustChange bool) error {
	if password == "" {
		// create would generate a one-time password, and Add has no way to
		// hand it back: the account would have a password nobody knows.
		return ErrWeakPassword
	}
	_, err := a.create(ctx, NewUser{Name: name, Role: role, Password: password}, mustChange, CreatedByCLI)
	return err
}

// NewUser is an account to create (Create). An empty Password means: generate
// a one-time password.
type NewUser struct {
	Name        string
	Role        model.Role
	DisplayName string
	Email       string
	Password    string
	// initialSetup marks the first-start admin (store.LocalUser.InitialSetup);
	// only EnsureInitialAdmin sets it.
	initialSetup bool
}

// Create creates an account for an admin (createdBy is the admin's subject).
// The new user must change the password at the first sign-in. With an empty
// nu.Password a one-time password is generated, valid for OTPValidity, and
// returned: it is the only copy of the clear text (shown once, never logged
// or audited).
func (a *Accounts) Create(ctx context.Context, nu NewUser, createdBy string) (otp string, err error) {
	return a.create(ctx, nu, true, createdBy)
}

func (a *Accounts) create(ctx context.Context, nu NewUser, mustChange bool, createdBy string) (otp string, err error) {
	name := NormalizeUsername(nu.Name)
	if !usernameRe.MatchString(name) {
		return "", ErrBadUsername
	}
	if nu.Role != model.RoleOperator && nu.Role != model.RoleAdmin {
		return "", ErrBadRole
	}
	display, email, err := CleanProfile(nu.DisplayName, nu.Email)
	if err != nil {
		return "", err
	}
	now := a.now().Now()
	u := store.LocalUser{Username: name, UpdatedAt: now, Role: nu.Role, MustChangePassword: mustChange,
		DisplayName: display, Email: email, CreatedAt: now, CreatedBy: createdBy, InitialSetup: nu.initialSetup}
	pw := nu.Password
	if pw == "" {
		if pw, err = GeneratePassword(generatedPasswordLen); err != nil {
			return "", err
		}
		otp, u.MustChangePassword, u.OTPExpiresAt = pw, true, now.Add(OTPValidity)
	}
	if u.PasswordHash, err = HashPassword(pw); err != nil {
		return "", err
	}
	if err := a.Users.AddLocalUser(ctx, u, a.by); err != nil {
		return "", mapStoreErr(err)
	}
	return otp, nil
}

// CleanProfile trims and checks the profile fields; both may be empty.
func CleanProfile(displayName, email string) (string, string, error) {
	displayName, email = strings.TrimSpace(displayName), strings.TrimSpace(email)
	if utf8.RuneCountInString(displayName) > maxDisplayName || strings.IndexFunc(displayName, unicode.IsControl) >= 0 {
		return "", "", ErrBadName
	}
	if email != "" {
		a, err := mail.ParseAddress(email)
		if err != nil || a.Address != email || a.Name != "" || len(email) > maxEmail {
			return "", "", ErrBadEmail
		}
	}
	return displayName, email, nil
}

// SetProfile sets display name and e-mail of an account (admin).
func (a *Accounts) SetProfile(ctx context.Context, name, displayName, email string) error {
	display, addr, err := CleanProfile(displayName, email)
	if err != nil {
		return err
	}
	return mapStoreErr(a.Users.SetLocalProfile(ctx, NormalizeUsername(name), display, addr, a.by))
}

// SetOwnProfile is SetProfile by the account's owner, applied only while the
// account is still at expectGen (the generation the session was checked against).
func (a *Accounts) SetOwnProfile(ctx context.Context, name string, expectGen int64, displayName, email string) error {
	display, addr, err := CleanProfile(displayName, email)
	if err != nil {
		return err
	}
	return mapStoreErr(a.Users.SetOwnProfile(ctx, NormalizeUsername(name), expectGen, display, addr))
}

// ResetOneTimePassword replaces the password with a generated one-time
// password (valid for OTPValidity, must be changed at the sign-in) and
// revokes the user's sessions and device cookies. The returned clear text is
// the only copy: shown once, never logged or audited.
func (a *Accounts) ResetOneTimePassword(ctx context.Context, name string) (string, error) {
	pw, err := GeneratePassword(generatedPasswordLen)
	if err != nil {
		return "", err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return "", err
	}
	name = NormalizeUsername(name)
	now := a.now().Now()
	if err := a.Users.SetLocalOneTimePassword(ctx, name, hash, now, now.Add(OTPValidity), a.by); err != nil {
		return "", mapStoreErr(err)
	}
	return pw, a.revoke(ctx, name)
}

// OTPExpired reports whether u signs in with a one-time password whose
// validity ended at now.
func OTPExpired(u store.LocalUser, now time.Time) bool {
	return !u.OTPExpiresAt.IsZero() && !now.Before(u.OTPExpiresAt)
}

// Get returns one account.
func (a *Accounts) Get(ctx context.Context, name string) (store.LocalUser, error) {
	u, err := a.Users.GetLocalUser(ctx, NormalizeUsername(name))
	return u, mapStoreErr(err)
}

// SetPassword replaces the password and revokes the user's sessions and
// device cookies. mustChange is set when an admin or the CLI chose the
// password for someone else. The store increments the password generation
// in the same statement, so a login that verified the old password and is
// still in flight ends up with a session that is already invalid.
func (a *Accounts) SetPassword(ctx context.Context, name, password string, mustChange bool) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	name = NormalizeUsername(name)
	if err := a.Users.SetLocalPassword(ctx, name, hash, a.now().Now(), mustChange, a.by); err != nil {
		return mapStoreErr(err)
	}
	return a.revoke(ctx, name)
}

// ChangeOwnPassword is the account owner's change: it applies only if the
// account is still at expectGen, the generation the owner's session was
// validated against (compare-and-set), so a concurrent reset or change by
// someone else is never silently overwritten. It revokes all sessions and
// device cookies of the user, like SetPassword.
func (a *Accounts) ChangeOwnPassword(ctx context.Context, name string, expectGen int64, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	name = NormalizeUsername(name)
	if err := a.Users.ChangeOwnPassword(ctx, name, expectGen, hash, a.now().Now()); err != nil {
		return mapStoreErr(err)
	}
	return a.revoke(ctx, name)
}

// SetRole changes the role. It takes effect on the user's next request;
// sessions stay valid because the role is read on every request.
func (a *Accounts) SetRole(ctx context.Context, name string, role model.Role) error {
	if role != model.RoleOperator && role != model.RoleAdmin {
		return ErrBadRole
	}
	return mapStoreErr(a.Users.SetLocalRole(ctx, NormalizeUsername(name), role, a.by))
}

// SetDisabled disables or enables an account. Disabling revokes sessions
// and device cookies.
func (a *Accounts) SetDisabled(ctx context.Context, name string, disabled bool) error {
	name = NormalizeUsername(name)
	if err := a.Users.SetLocalDisabled(ctx, name, disabled, a.by); err != nil {
		return mapStoreErr(err)
	}
	if disabled {
		return a.revoke(ctx, name)
	}
	return nil
}

// Delete removes the account with its sessions and device cookies.
func (a *Accounts) Delete(ctx context.Context, name string) error {
	name = NormalizeUsername(name)
	if err := a.Users.DeleteLocalUser(ctx, name, a.by); err != nil {
		return mapStoreErr(err)
	}
	return a.revoke(ctx, name)
}

// List returns all accounts ordered by name.
func (a *Accounts) List(ctx context.Context) ([]store.LocalUser, error) {
	return a.Users.ListLocalUsers(ctx)
}

// RevokeSessions ends all sessions of one local user and returns the count.
func (a *Accounts) RevokeSessions(ctx context.Context, name string) (int64, error) {
	n, err := a.Sessions.DeleteUserSessions(ctx, SubjectOf(NormalizeUsername(name)))
	if err != nil {
		return 0, fmt.Errorf("auth: revoke sessions: %w", err)
	}
	return n, nil
}

func (a *Accounts) revoke(ctx context.Context, name string) error {
	if _, err := a.Sessions.DeleteUserSessions(ctx, SubjectOf(name)); err != nil {
		return fmt.Errorf("auth: revoke sessions: %w", err)
	}
	if a.Cookies != nil {
		if _, err := a.Cookies.DeleteUserDeviceCookies(ctx, name); err != nil {
			return fmt.Errorf("auth: revoke device cookies: %w", err)
		}
	}
	return nil
}

// msgWeakPassword is what people read when a new password is too short.
var msgWeakPassword = fmt.Sprintf("Use at least %d characters.", PasswordMinLength)

// inputProblem turns an error that is the admin's to fix into a sentence for
// the page. field names the create-user input it belongs to; it is empty for
// errors about no single input. The Go error chains ("auth: ...") are for logs
// and the CLI, never for the browser.
func inputProblem(err error) (field, msg string) {
	switch {
	case errors.Is(err, ErrWeakPassword):
		return "password", msgWeakPassword
	case errors.Is(err, ErrBadUsername):
		return "username", "Use 1 to 64 letters, digits, dots, underscores or hyphens, starting with a letter or digit."
	case errors.Is(err, ErrUserExists):
		return "username", "A user with this name already exists."
	case errors.Is(err, ErrBadRole):
		return "role", "Choose operator or admin."
	case errors.Is(err, ErrUserNotFound):
		return "", "That user no longer exists."
	case errors.Is(err, ErrLastAdmin):
		return "", "This would leave no enabled administrator."
	case errors.Is(err, ErrBadName):
		return "display_name", fmt.Sprintf("Use at most %d characters.", maxDisplayName)
	case errors.Is(err, ErrBadEmail):
		return "email", "Enter an address like name@example.org, or leave it empty."
	case errors.Is(err, errConfirmMismatch):
		return "confirm", "Passwords don’t match."
	}
	return "", err.Error()
}
