package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

func newAccounts(t *testing.T) (*Accounts, *fakeUsers, *fakeSessions, *fakeCookies, *fakeClock) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	u := &fakeUsers{t: t, users: map[string]store.LocalUser{}}
	s := &fakeSessions{m: map[string]store.Session{}}
	c := &fakeCookies{m: map[string]string{}}
	return &Accounts{Users: u, Sessions: s, Cookies: c, Clock: clk}, u, s, c, clk
}

func TestAccountsAddValidates(t *testing.T) {
	a, users, _, _, _ := newAccounts(t)
	ctx := t.Context()
	if err := a.Add(ctx, " Anna ", testPassword, model.RoleOperator, true); err != nil {
		t.Fatal(err)
	}
	u := users.users["anna"]
	if u.Role != model.RoleOperator || !u.MustChangePassword || u.PasswordHash == "" {
		t.Fatalf("stored %+v (names are normalised to lower case)", u)
	}
	for name, err := range map[string]error{
		"Anna":   ErrUserExists, // normalised duplicate
		"":       ErrBadUsername,
		"-x":     ErrBadUsername,
		"a b":    ErrBadUsername,
		"a/../b": ErrBadUsername,
	} {
		if got := a.Add(ctx, name, testPassword, model.RoleOperator, false); !errors.Is(got, err) {
			t.Errorf("Add(%q) = %v, want %v", name, got, err)
		}
	}
	// Only operator and admin exist as roles.
	if err := a.Add(ctx, "bob", testPassword, model.RoleNone, false); !errors.Is(err, ErrBadRole) {
		t.Errorf("role none: %v", err)
	}
	if err := a.Add(ctx, "bob", "short", model.RoleOperator, false); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("weak password: %v", err)
	}
	if _, err := ParseRole("viewer"); !errors.Is(err, ErrBadRole) {
		t.Errorf("viewer role must not parse: %v", err)
	}
}

// Why: a stolen session or device cookie must stop working when the
// password changes (spec §15.2); the time stamp alone is the backstop.
func TestSetPasswordRevokesSessionsAndDeviceCookies(t *testing.T) {
	a, users, sessions, cookies, clk := newAccounts(t)
	ctx := t.Context()
	if err := a.Add(ctx, "anna", testPassword, model.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(ctx, "bob", testPassword, model.RoleOperator, false); err != nil {
		t.Fatal(err)
	}
	sessions.m["s1"] = store.Session{IDHash: "s1", Principal: model.Principal{Subject: "local:anna"}}
	sessions.m["s2"] = store.Session{IDHash: "s2", Principal: model.Principal{Subject: "local:bob"}}
	cookies.m["c1"], cookies.m["c2"] = "anna", "bob"
	clk.Advance(time.Hour)
	if err := a.SetPassword(ctx, "anna", "another long password", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := sessions.m["s1"]; ok {
		t.Error("anna's session survived a password change")
	}
	if _, ok := cookies.m["c1"]; ok {
		t.Error("anna's device cookie survived a password change")
	}
	if _, ok := sessions.m["s2"]; !ok || cookies.m["c2"] != "bob" {
		t.Error("another user's sessions or cookies were revoked")
	}
	if u := users.users["anna"]; !u.UpdatedAt.Equal(clk.Now()) || !u.MustChangePassword || VerifyPassword(u.PasswordHash, "another long password") != nil {
		t.Errorf("anna = %+v", u)
	}
}

// Why: a role change must not log the user out (they would lose work) but
// must apply at once; so UpdatedAt (password time) stays put and sessions stay.
func TestSetRoleKeepsSessionsAndPasswordTime(t *testing.T) {
	a, users, sessions, _, clk := newAccounts(t)
	ctx := t.Context()
	_ = a.Add(ctx, "root", testPassword, model.RoleAdmin, false)
	_ = a.Add(ctx, "anna", testPassword, model.RoleAdmin, false)
	sessions.m["s1"] = store.Session{IDHash: "s1", Principal: model.Principal{Subject: "local:anna"}}
	before := users.users["anna"].UpdatedAt
	clk.Advance(time.Hour)
	if err := a.SetRole(ctx, "anna", model.RoleOperator); err != nil {
		t.Fatal(err)
	}
	if users.users["anna"].Role != model.RoleOperator || !users.users["anna"].UpdatedAt.Equal(before) {
		t.Fatalf("anna = %+v", users.users["anna"])
	}
	if len(sessions.m) != 1 {
		t.Fatal("role change revoked sessions")
	}
	if err := a.SetRole(ctx, "anna", model.RoleNone); !errors.Is(err, ErrBadRole) {
		t.Fatalf("role none accepted: %v", err)
	}
}

// Why: the last admin cannot be locked out by a typo or a click.
func TestLastAdminIsProtected(t *testing.T) {
	a, _, _, _, _ := newAccounts(t)
	ctx := t.Context()
	_ = a.Add(ctx, "root", testPassword, model.RoleAdmin, false)
	_ = a.Add(ctx, "ops", testPassword, model.RoleOperator, false)
	for name, f := range map[string]func() error{
		"demote":  func() error { return a.SetRole(ctx, "root", model.RoleOperator) },
		"disable": func() error { return a.SetDisabled(ctx, "root", true) },
		"delete":  func() error { return a.Delete(ctx, "root") },
	} {
		if err := f(); !errors.Is(err, ErrLastAdmin) {
			t.Errorf("%s the last admin: %v", name, err)
		}
	}
	// With a second enabled admin it works; a disabled admin does not count.
	_ = a.Add(ctx, "second", testPassword, model.RoleAdmin, false)
	if err := a.SetDisabled(ctx, "second", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetDisabled(ctx, "root", true); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabled admins must not count: %v", err)
	}
	if err := a.SetDisabled(ctx, "second", false); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "root"); err != nil {
		t.Fatal(err)
	}
}

func TestDisableRevokesAndEnableDoesNotResurrect(t *testing.T) {
	a, users, sessions, cookies, _ := newAccounts(t)
	ctx := t.Context()
	_ = a.Add(ctx, "root", testPassword, model.RoleAdmin, false)
	_ = a.Add(ctx, "ops", testPassword, model.RoleOperator, false)
	sessions.m["s"] = store.Session{IDHash: "s", Principal: model.Principal{Subject: "local:ops"}}
	cookies.m["c"] = "ops"
	if err := a.SetDisabled(ctx, "ops", true); err != nil {
		t.Fatal(err)
	}
	if len(sessions.m) != 0 || cookies.count() != 0 || !users.users["ops"].Disabled {
		t.Fatal("disable did not revoke")
	}
	if err := a.SetDisabled(ctx, "ops", false); err != nil {
		t.Fatal(err)
	}
	if len(sessions.m) != 0 {
		t.Fatal("enable resurrected a session")
	}
	if err := a.Delete(ctx, "nobody"); !errors.Is(err, ErrUserNotFound) {
		t.Fatal(err)
	}
}

// racingUsers runs hook right after the next lookup of target, like another
// request changing the account while a login is hashing the password.
type racingUsers struct {
	*fakeUsers
	target string
	hook   func()
}

func (r *racingUsers) GetLocalUser(ctx context.Context, name string) (store.LocalUser, error) {
	u, err := r.fakeUsers.GetLocalUser(ctx, name)
	if name == r.target && r.hook != nil {
		h := r.hook
		r.hook = nil
		h()
	}
	return u, err
}

// Why: "change the password to kick the attacker out" must hold even when
// the attacker's login read the old hash before the change and finishes
// after it: the session and device cookie are bound to the verified
// password generation (review finding 2).
func TestLoginInFlightDuringPasswordChangeIsDead(t *testing.T) {
	for name, change := range map[string]func(a *Accounts) error{
		"password change": func(a *Accounts) error { return a.SetPassword(context.Background(), "ops", newPassword, false) },
		"disable":         func(a *Accounts) error { return a.SetDisabled(context.Background(), "ops", true) },
	} {
		t.Run(name, func(t *testing.T) {
			var racing *racingUsers
			e := newEnv(t, ModeLocal, func(o *Options) {
				o.Proxy.PublicURL = ""
				racing = &racingUsers{fakeUsers: o.LocalUsers.(*fakeUsers)}
				o.LocalUsers = racing
			})
			hash, err := HashPassword(opsPassword)
			if err != nil {
				t.Fatal(err)
			}
			e.users.cli(func(m map[string]store.LocalUser) {
				m["ops"] = store.LocalUser{Username: "ops", PasswordHash: hash, UpdatedAt: e.clock.Now().Add(-time.Hour), Role: model.RoleOperator, PasswordGen: 1}
			})
			racing.target = "ops"
			racing.hook = func() {
				if err := change(e.svc.accounts); err != nil {
					t.Error(err)
				}
			}
			b := e.browser()
			r := postLocal(b, "192.0.2.77", "ops", opsPassword)
			// The store refuses the session: the generation the login verified is gone.
			if r.code != http.StatusUnauthorized {
				t.Fatalf("a login that raced the change must be refused: %d", r.code)
			}
			if n := len(e.sessions.all()); n != 0 {
				t.Fatalf("%d sessions exist", n)
			}
			if e.cookies.count() != 0 {
				t.Fatal("a device cookie was recorded for the old password")
			}
			if a := e.audit.last(); a.Result != "denied: account changed during login" {
				t.Errorf("audit %+v", a)
			}
			if r := b.get("/viewer"); r.code == http.StatusOK {
				t.Fatal("a working session exists")
			}
		})
	}
}

// Why: revocation must also hold when a row exists anyway (a delete that
// raced an insert, a restored backup): the generation is compared on EVERY
// request, not only when the session is created.
func TestStaleGenerationSessionIsRejectedOnEveryRequest(t *testing.T) {
	e := localEnv(t)
	b := e.login("ops", opsPassword)
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatal(r.code)
	}
	// Someone changes the password; the session row is (wrongly) still there.
	e.users.cli(func(m map[string]store.LocalUser) {
		u := m["ops"]
		u.PasswordGen++
		m["ops"] = u
	})
	if len(e.sessions.all()) != 1 {
		t.Fatal("setup: the session row must still exist")
	}
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("a session of an older password generation was accepted")
	}
	// Same for the device cookie.
	dev := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	dev.AddCookie(&http.Cookie{Name: CookieInsecure + "-device", Value: b.cookie(CookieInsecure + "-device")})
	if e.svc.validDeviceCookie(dev, "ops") {
		t.Fatal("a device cookie of an older generation counts as recognised")
	}
}

// Why: web admin and CLI are separate writers on one database (or two
// containers on one volume). Both go through Accounts and must neither
// create an account twice nor leave zero enabled admins.
func TestConcurrentWritersKeepAccountInvariants(t *testing.T) {
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // temp DB
	web := &Accounts{Users: db, Sessions: db, Cookies: db}
	cli := &Accounts{Users: db, Sessions: db, Cookies: db} // separate instance: no shared lock
	ctx := t.Context()

	var created atomic.Int32
	var wg sync.WaitGroup
	for _, a := range []*Accounts{web, cli, web, cli} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.Add(ctx, "dup", testPassword, model.RoleOperator, false) == nil {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("%d concurrent adds of one name succeeded", created.Load())
	}
	if err := web.Add(ctx, "a1", testPassword, model.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	if err := cli.Add(ctx, "a2", testPassword, model.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	var removed atomic.Int32
	for _, op := range []func() error{
		func() error { return web.SetDisabled(ctx, "a1", true) },
		func() error { return cli.Delete(ctx, "a2") },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := op(); err == nil {
				removed.Add(1)
			} else if !errors.Is(err, ErrLastAdmin) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if removed.Load() != 1 {
		t.Fatalf("%d admins removed concurrently, want exactly 1", removed.Load())
	}
}
