package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// localEnvWith is localEnv with captured pages.
func localEnvWith(t *testing.T, pages Pages) *env {
	t.Helper()
	e := newEnv(t, ModeLocal, func(o *Options) {
		o.Proxy.PublicURL = ""
		o.DataDir = t.TempDir()
		o.Pages = pages
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

// captureAccountPages keeps the data of the last password and users page.
type captureAccountPages struct {
	DefaultPages
	pw    PasswordPageData
	users UsersPageData
}

func (c *captureAccountPages) ChangePassword(w http.ResponseWriter, r *http.Request, status int, d PasswordPageData) {
	c.pw = d
	c.DefaultPages.ChangePassword(w, r, status, d)
}

func (c *captureAccountPages) Users(w http.ResponseWriter, r *http.Request, status int, d UsersPageData) {
	c.users = d
	c.DefaultPages.Users(w, r, status, d)
}

// Why: each rejected password change must point at the input the person has to
// fix (so the page can mark it and move the focus there), and must say it in
// words: the Go error chain "auth: password shorter than 12 characters" is for
// the CLI and the log, not for a browser.
func TestPasswordChangeNamesTheFailingField(t *testing.T) {
	pages := &captureAccountPages{}
	e := localEnvWith(t, pages)
	ops := e.login("ops", opsPassword)
	cases := []struct {
		name, field string
		form        url.Values
	}{
		{"wrong current", "current", url.Values{"current": {"not the password"}, "new": {newPassword}, "confirm": {newPassword}}},
		{"mismatch", "confirm", url.Values{"current": {opsPassword}, "new": {newPassword}, "confirm": {"something else 123"}}},
		{"same", "new", url.Values{"current": {opsPassword}, "new": {opsPassword}, "confirm": {opsPassword}}},
		{"too short", "new", url.Values{"current": {opsPassword}, "new": {"short"}, "confirm": {"short"}}},
	}
	for _, c := range cases {
		if r := ops.post("/account/password", c.form); r.code == http.StatusSeeOther {
			t.Fatalf("%s: accepted", c.name)
		}
		if pages.pw.ErrField != c.field || pages.pw.Error == "" {
			t.Errorf("%s: field %q (want %q), error %q", c.name, pages.pw.ErrField, c.field, pages.pw.Error)
		}
		if strings.Contains(pages.pw.Error, "auth:") {
			t.Errorf("%s: internal error text shown: %q", c.name, pages.pw.Error)
		}
	}
}

// Why: a rate-limited attempt belongs to no input; it stays a page message.
func TestPasswordChangeRateLimitHasNoField(t *testing.T) {
	pages := &captureAccountPages{}
	e := localEnvWith(t, pages)
	ops := e.login("ops", opsPassword)
	for range 30 {
		ops.post("/account/password", url.Values{"current": {"x"}, "new": {newPassword}, "confirm": {newPassword}})
		if pages.pw.Error == "Too many attempts. Wait a minute and try again." {
			if pages.pw.ErrField != "" {
				t.Fatalf("rate limit bound to field %q", pages.pw.ErrField)
			}
			return
		}
	}
	t.Fatal("the rate limit did not trip in 30 attempts")
}

// Why: when creating a user fails, the admin must not retype the username,
// and must read what is wrong with the password in plain words at the field.
func TestUserCreateKeepsInputAndNamesTheField(t *testing.T) {
	pages := &captureAccountPages{}
	e := localEnvWith(t, pages)
	adm := e.login("admin", testPassword)
	adm.stepUp(testPassword)
	r := adm.post("/users", url.Values{"username": {"newbie"}, "password": {"short"}, "role": {"admin"}})
	if r.code != http.StatusBadRequest {
		t.Fatalf("status %d", r.code)
	}
	d := pages.users
	if d.ErrField != "password" || d.Username != "newbie" || d.Role != "admin" {
		t.Fatalf("field %q username %q role %q", d.ErrField, d.Username, d.Role)
	}
	if d.Error != "Use at least 12 characters." {
		t.Fatalf("error %q", d.Error)
	}
	r = adm.post("/users", url.Values{"username": {"ops"}, "password": {newPassword}, "role": {"operator"}})
	if r.code != http.StatusConflict || pages.users.ErrField != "username" || strings.Contains(pages.users.Error, "auth:") {
		t.Fatalf("duplicate: %d field %q error %q", r.code, pages.users.ErrField, pages.users.Error)
	}
	// A row action (reset password, role) repeats per user: it stays a page message.
	r = adm.post("/users/local/ops", url.Values{"action": {"password"}, "password": {"short"}})
	if r.code != http.StatusBadRequest || pages.users.ErrField != "" || !strings.Contains(pages.users.Error, "12 characters") {
		t.Fatalf("reset: %d field %q error %q", r.code, pages.users.ErrField, pages.users.Error)
	}
}

// Why: the wrong step-up password is an error of the one password input.
func TestStepUpWrongPasswordNamesTheField(t *testing.T) {
	pages := &captureAccountPages{}
	e := localEnvWith(t, pages)
	adm := e.login("admin", testPassword)
	adm.post("/users/stepup", url.Values{"current": {"wrong"}})
	if pages.users.ErrField != "current" {
		t.Fatalf("field %q", pages.users.ErrField)
	}
}

// Why: errors that the admin cannot fix at an input keep their text but never
// leak a package prefix.
func TestInputProblemHasNoPackagePrefix(t *testing.T) {
	for _, err := range []error{ErrWeakPassword, ErrBadUsername, ErrUserExists, ErrBadRole, ErrUserNotFound, ErrLastAdmin, errSelfDelete} {
		_, msg := inputProblem(err)
		if msg == "" || strings.Contains(msg, "auth:") || strings.Contains(msg, "store:") {
			t.Errorf("%v -> %q", err, msg)
		}
	}
	if f, _ := inputProblem(errors.New("x")); f != "" {
		t.Errorf("unknown error bound to %q", f)
	}
}
