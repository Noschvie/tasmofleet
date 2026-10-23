package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func TestLocalUserRolesAndFlags(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, u := range []LocalUser{
		{Username: "zed", PasswordHash: "h", UpdatedAt: at, Role: model.RoleOperator, MustChangePassword: true},
		{Username: "anna", PasswordHash: "h", UpdatedAt: at, Role: model.RoleAdmin, Disabled: true},
	} {
		if err := s.SetLocalUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetLocalUser(ctx, "anna")
	if err != nil || got.Role != model.RoleAdmin || !got.Disabled || got.MustChangePassword {
		t.Fatalf("anna = %+v, %v", got, err)
	}
	list, err := s.ListLocalUsers(ctx)
	if err != nil || len(list) != 2 || list[0].Username != "anna" || list[1].Username != "zed" || !list[1].MustChangePassword {
		t.Fatalf("list = %+v, %v (want sorted by name with flags)", list, err)
	}
}

// Why: a user without a valid role must never exist. Only operator and
// admin are roles; a zero Role stored by mistake would silently be a
// locked-out or, worse, wrongly mapped account.
func TestSetLocalUserRejectsRolesOtherThanOperatorAndAdmin(t *testing.T) {
	s, _, _ := newStore(t)
	for _, r := range []model.Role{model.RoleNone, model.Role(7)} {
		if err := s.SetLocalUser(t.Context(), LocalUser{Username: "x", PasswordHash: "h", Role: r}); err == nil {
			t.Errorf("role %d was accepted", r)
		}
	}
}

// Why: the OIDC refresh token is a long-lived credential for the IdP. A
// copy of the database file must not reveal it (spec §15.1).
func TestSessionRefreshTokenIsSealedAtRest(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sess := Session{IDHash: "h1", Principal: model.Principal{Subject: "sub-1", Method: "oidc"}, CSRFToken: "c",
		RefreshToken: "refresh-secret-value", RecheckedAt: base, CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT refresh_token FROM sessions WHERE id_hash='h1'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || raw == "refresh-secret-value" {
		t.Fatalf("raw column = %q, want a sealed value", raw)
	}
	got, err := s.GetSession(ctx, "h1")
	if err != nil || got.RefreshToken != "refresh-secret-value" || !got.RecheckedAt.Equal(base) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A session without a token stores no ciphertext of an empty string.
	sess.IDHash, sess.RefreshToken = "h2", ""
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, "h2"); got.RefreshToken != "" {
		t.Fatalf("empty token came back as %q", got.RefreshToken)
	}
}

func TestUpdateSessionRecheck(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sess := Session{IDHash: "h1", Principal: model.Principal{Subject: "s", Groups: []string{"a"}, Role: model.RoleAdmin, Method: "oidc"},
		CSRFToken: "c", IDToken: "old", RefreshToken: "r1", RecheckedAt: base, CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.Principal.Groups, sess.Principal.Role, sess.IDToken, sess.RefreshToken = []string{"b"}, model.RoleOperator, "new", "r2"
	sess.RecheckedAt = base.Add(15 * time.Minute)
	if err := s.UpdateSessionRecheck(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSession(ctx, "h1")
	if got.RefreshToken != "r2" || got.IDToken != "new" || got.Principal.Role != model.RoleOperator || got.Principal.Groups[0] != "b" || !got.RecheckedAt.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("got %+v", got)
	}
	if !got.CreatedAt.Equal(base) || got.CSRFToken != "c" {
		t.Fatalf("recheck changed identity fields: %+v", got)
	}
	sess.IDHash = "nope"
	if err := s.UpdateSessionRecheck(ctx, sess); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

// Why: a password change must end every session of exactly that user and
// nobody else's.
func TestDeleteUserSessions(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for id, sub := range map[string]string{"a1": "local:anna", "a2": "local:anna", "b1": "local:bob"} {
		if err := s.CreateSession(ctx, Session{IDHash: id, Principal: model.Principal{Subject: sub}, CSRFToken: "c", CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteUserSessions(ctx, "local:anna")
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if _, err := s.GetSession(ctx, "b1"); err != nil {
		t.Fatalf("bob's session gone: %v", err)
	}
	if _, err := s.GetSession(ctx, "a1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("anna's session survived: %v", err)
	}
}

func TestDeviceCookies(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "anna", model.RoleOperator)
	addUser(t, s, "bob", model.RoleOperator)
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := s.PutDeviceCookie(ctx, "c1", "anna", base, 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasDeviceCookie(ctx, "c1", "anna", 1); err != nil || !ok {
		t.Fatalf("has = %v, %v", ok, err)
	}
	// Bound to the user: a cookie id issued for anna says nothing about bob.
	if ok, _ := s.HasDeviceCookie(ctx, "c1", "bob", 1); ok {
		t.Fatal("cookie valid for another user")
	}
	if err := s.PutDeviceCookie(ctx, "c2", "anna", base, 1); err != nil {
		t.Fatal(err)
	}
	// Per-cookie revocation leaves the other one alone.
	if err := s.DeleteDeviceCookie(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasDeviceCookie(ctx, "c1", "anna", 1); ok {
		t.Fatal("revoked cookie still valid")
	}
	if ok, _ := s.HasDeviceCookie(ctx, "c2", "anna", 1); !ok {
		t.Fatal("sibling cookie was revoked")
	}
	if err := s.PutDeviceCookie(ctx, "b1", "bob", base, 1); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteUserDeviceCookies(ctx, "anna"); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if ok, _ := s.HasDeviceCookie(ctx, "b1", "bob", 1); !ok {
		t.Fatal("bob's cookie was revoked with anna's")
	}
	if n, err := s.DeleteDeviceCookiesBefore(ctx, base.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
}

// Why: every login issues a cookie; without a cap a script that logs in
// in a loop could grow the table without bound.
func TestDeviceCookiesCappedPerUser(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "anna", model.RoleOperator)
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for i := range MaxDeviceCookiesPerUser + 10 {
		if err := s.PutDeviceCookie(ctx, "id"+string(rune('A'+i%26))+string(rune('a'+i/26)), "anna", base.Add(time.Duration(i)*time.Second), 1); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM device_cookies WHERE username='anna'").Scan(&n); err != nil || n != MaxDeviceCookiesPerUser {
		t.Fatalf("rows = %d, %v; want %d", n, err, MaxDeviceCookiesPerUser)
	}
	// The oldest were evicted, the newest kept.
	last := "id" + string(rune('A'+(MaxDeviceCookiesPerUser+9)%26)) + string(rune('a'+(MaxDeviceCookiesPerUser+9)/26))
	if ok, _ := s.HasDeviceCookie(ctx, last, "anna", 1); !ok {
		t.Fatal("newest cookie was evicted")
	}
	if ok, _ := s.HasDeviceCookie(ctx, "idAa", "anna", 1); ok {
		t.Fatal("oldest cookie survived the cap")
	}
}

func addUser(t *testing.T, s *SQLite, name string, role model.Role) {
	t.Helper()
	if err := s.AddLocalUser(t.Context(), LocalUser{Username: name, PasswordHash: "h", Role: role}, Actor{}); err != nil {
		t.Fatal(err)
	}
}

// Why: Add must never overwrite an existing account (role, password), even
// when the web admin and the CLI add the same name at the same moment.
func TestAddLocalUserNeverOverwrites(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "anna", model.RoleAdmin)
	err := s.AddLocalUser(ctx, LocalUser{Username: "anna", PasswordHash: "other", Role: model.RoleOperator}, Actor{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second add: %v", err)
	}
	if u, _ := s.GetLocalUser(ctx, "anna"); u.PasswordHash != "h" || u.Role != model.RoleAdmin || u.PasswordGen != 1 {
		t.Fatalf("anna = %+v", u)
	}
	if err := s.AddLocalUser(ctx, LocalUser{Username: "x", PasswordHash: "h"}, Actor{}); err == nil {
		t.Fatal("add without a role")
	}
	var wg sync.WaitGroup
	var won atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.AddLocalUser(ctx, LocalUser{Username: "race", PasswordHash: "h", Role: model.RoleOperator}, Actor{}) == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d concurrent adds succeeded", won.Load())
	}
}

// Why: the generation is what kills a login that raced a password change or
// a disable (sessions and device cookies carry the generation they verified).
func TestPasswordGenerationBumps(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "root", model.RoleAdmin)
	addUser(t, s, "ops", model.RoleOperator)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gen := func() int64 { u, _ := s.GetLocalUser(ctx, "ops"); return u.PasswordGen }
	if gen() != 1 {
		t.Fatalf("new accounts start at generation 1, got %d", gen())
	}
	if err := s.SetLocalPassword(ctx, "ops", "h2", at, true, Actor{}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetLocalUser(ctx, "ops"); u.PasswordGen != 2 || u.PasswordHash != "h2" || !u.MustChangePassword || !u.UpdatedAt.Equal(at) {
		t.Fatalf("after password change: %+v", u)
	}
	if err := s.SetLocalRole(ctx, "ops", model.RoleAdmin, Actor{}); err != nil || gen() != 2 {
		t.Fatalf("a role change must not end sessions: %v, gen %d", err, gen())
	}
	if err := s.SetLocalDisabled(ctx, "ops", true, Actor{}); err != nil || gen() != 3 {
		t.Fatalf("disable: %v, gen %d", err, gen())
	}
	if err := s.SetLocalDisabled(ctx, "ops", false, Actor{}); err != nil || gen() != 3 {
		t.Fatalf("enable keeps the generation: %v, gen %d", err, gen())
	}
	if err := s.SetLocalPassword(ctx, "ghost", "h", at, false, Actor{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

// Why: revocation must not depend on deleting rows (a concurrent login can
// insert one right after the delete). A local session or device cookie is
// stored only if, in the same statement, its account is enabled and still at
// the generation the login verified; and it is valid on every request only
// while that generation is current.
func TestSessionAndDeviceCookieAreBoundToTheGeneration(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	addUser(t, s, "a", model.RoleOperator) // generation 1
	local := func(id string, gen int64) Session {
		return Session{IDHash: id, Principal: model.Principal{Subject: "local:a", Name: "a", Method: "local"}, CSRFToken: "c",
			PasswordGen: gen, CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
	}
	if err := s.CreateSession(ctx, local("ok", 1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(ctx, "ok"); got.PasswordGen != 1 {
		t.Fatalf("session generation %d", got.PasswordGen)
	}
	if err := s.PutDeviceCookie(ctx, "d", "a", base, 1); err != nil {
		t.Fatal(err)
	}
	// The password changes between the login's credential check and its write.
	if err := s.SetLocalPassword(ctx, "a", "h2", base, false, Actor{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, local("late", 1)); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("a session that verified the old password was stored: %v", err)
	}
	if err := s.PutDeviceCookie(ctx, "late", "a", base, 1); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("a device cookie for the old password was stored: %v", err)
	}
	if ok, _ := s.HasDeviceCookie(ctx, "d", "a", 2); ok {
		t.Fatal("a cookie of an older generation is accepted at the current one")
	}
	if err := s.CreateSession(ctx, local("new", 2)); err != nil {
		t.Fatalf("the current generation: %v", err)
	}
	// Disabled accounts get no new sessions or cookies, at any generation.
	if err := s.SetLocalDisabled(ctx, "a", true, Actor{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, local("dis", 3)); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("session for a disabled account: %v", err)
	}
	if err := s.PutDeviceCookie(ctx, "dis", "a", base, 3); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("cookie for a disabled account: %v", err)
	}
	// Sessions of other methods carry no generation and are unaffected.
	if err := s.CreateSession(ctx, Session{IDHash: "oidc", Principal: model.Principal{Subject: "sub", Method: "oidc"}, CSRFToken: "c", CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}); err != nil {
		t.Fatalf("oidc session: %v", err)
	}
}

// Why: the last enabled admin must survive every path, and two admins
// removing each other at the same moment must not both succeed.
func TestLastAdminRule(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "root", model.RoleAdmin)
	addUser(t, s, "ops", model.RoleOperator)
	if err := s.SetLocalRole(ctx, "root", model.RoleOperator, Actor{}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote: %v", err)
	}
	if err := s.SetLocalDisabled(ctx, "root", true, Actor{}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disable: %v", err)
	}
	if err := s.DeleteLocalUser(ctx, "root", Actor{}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteLocalUser(ctx, "ghost", Actor{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if err := s.DeleteLocalUser(ctx, "ops", Actor{}); err != nil {
		t.Errorf("deleting an operator: %v", err)
	}
	// A disabled admin does not count as a remaining admin.
	addUser(t, s, "second", model.RoleAdmin)
	if err := s.SetLocalDisabled(ctx, "second", true, Actor{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLocalDisabled(ctx, "root", true, Actor{}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disabled admins must not count: %v", err)
	}
	if err := s.SetLocalDisabled(ctx, "second", false, Actor{}); err != nil {
		t.Fatal(err)
	}
}

// Two admins disable each other at the same moment (web admin and CLI): the
// check and the change are one statement, so exactly one succeeds.
func TestConcurrentAdminRemovalLeavesOneAdmin(t *testing.T) {
	for round := range 15 {
		s, _, _ := newStore(t)
		ctx := t.Context()
		addUser(t, s, "a", model.RoleAdmin)
		addUser(t, s, "b", model.RoleAdmin)
		var wg sync.WaitGroup
		var ok atomic.Int32
		for _, op := range []func() error{
			func() error { return s.SetLocalDisabled(ctx, "a", true, Actor{}) },
			func() error { return s.DeleteLocalUser(ctx, "b", Actor{}) },
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if op() == nil {
					ok.Add(1)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 1 {
			t.Fatalf("round %d: %d removals succeeded, want exactly 1", round, ok.Load())
		}
		users, _ := s.ListLocalUsers(ctx)
		enabled := 0
		for _, u := range users {
			if !u.Disabled {
				enabled++
			}
		}
		if enabled != 1 {
			t.Fatalf("round %d: %d enabled admins left", round, enabled)
		}
	}
}

// Why: the web admin's authority is checked when the request starts and
// again, in the same statement, when the change is written. A demotion,
// disable, deletion or password change of the actor in between must stop the
// write (review: stale identity), for every kind of mutation.
func TestActorMustStillBeAnEnabledAdminAtWriteTime(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (*SQLite, Actor) {
		s, _, _ := newStore(t)
		addUser(t, s, "boss", model.RoleAdmin)
		addUser(t, s, "other", model.RoleAdmin)
		addUser(t, s, "ops", model.RoleOperator)
		return s, Actor{Username: "boss", PasswordGen: 1}
	}
	mutations := map[string]func(s *SQLite, by Actor) error{
		"add": func(s *SQLite, by Actor) error {
			return s.AddLocalUser(context.Background(), LocalUser{Username: "new", PasswordHash: "h", Role: model.RoleOperator}, by)
		},
		"password": func(s *SQLite, by Actor) error {
			return s.SetLocalPassword(context.Background(), "ops", "h2", at, true, by)
		},
		"role": func(s *SQLite, by Actor) error {
			return s.SetLocalRole(context.Background(), "ops", model.RoleAdmin, by)
		},
		"disable": func(s *SQLite, by Actor) error { return s.SetLocalDisabled(context.Background(), "ops", true, by) },
		"delete":  func(s *SQLite, by Actor) error { return s.DeleteLocalUser(context.Background(), "ops", by) },
	}
	ruins := map[string]func(s *SQLite){
		"demoted":          func(s *SQLite) { must(t, s.SetLocalRole(context.Background(), "boss", model.RoleOperator, Actor{})) },
		"disabled":         func(s *SQLite) { must(t, s.SetLocalDisabled(context.Background(), "boss", true, Actor{})) },
		"password changed": func(s *SQLite) { must(t, s.SetLocalPassword(context.Background(), "boss", "new", at, false, Actor{})) },
		"deleted":          func(s *SQLite) { must(t, s.DeleteLocalUser(context.Background(), "boss", Actor{})) },
	}
	for ruin, doRuin := range ruins {
		for name, mutate := range mutations {
			t.Run(ruin+"/"+name, func(t *testing.T) {
				s, boss := setup(t)
				doRuin(s)
				if err := mutate(s, boss); !errors.Is(err, ErrStaleActor) {
					t.Fatalf("%s after the actor was %s: %v, want ErrStaleActor", name, ruin, err)
				}
				if u, err := s.GetLocalUser(context.Background(), "ops"); err != nil || u.Role != model.RoleOperator || u.Disabled || u.PasswordHash != "h" {
					t.Fatalf("a refused change was applied: %+v, %v", u, err)
				}
				if _, err := s.GetLocalUser(context.Background(), "new"); !errors.Is(err, ErrNotFound) {
					t.Fatal("a refused add created the account")
				}
			})
		}
	}
	t.Run("valid actor", func(t *testing.T) {
		s, boss := setup(t)
		for _, name := range []string{"add", "password", "role", "disable", "delete"} { // delete last
			if err := mutations[name](s, boss); err != nil {
				t.Fatalf("%s by a valid admin: %v", name, err)
			}
		}
	})
	t.Run("never an operator or an unknown name", func(t *testing.T) {
		s, _ := setup(t)
		ctx := context.Background()
		if err := s.SetLocalRole(ctx, "ops", model.RoleAdmin, Actor{Username: "ops", PasswordGen: 1}); !errors.Is(err, ErrStaleActor) {
			t.Fatalf("operator as actor: %v", err)
		}
		if err := s.DeleteLocalUser(ctx, "ops", Actor{Username: "ghost", PasswordGen: 1}); !errors.Is(err, ErrStaleActor) {
			t.Fatalf("unknown actor: %v", err)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Why: the owner's own password change is a compare-and-set on the
// generation the session was validated against; a change by someone else in
// between (reset by an admin, change from another device) wins and this one
// is refused instead of silently overwriting it.
func TestChangeOwnPasswordIsCompareAndSet(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	addUser(t, s, "ops", model.RoleOperator)
	if err := s.SetLocalPassword(ctx, "ops", "h1", at, true, Actor{}); err != nil { // gen 2, must change
		t.Fatal(err)
	}
	if err := s.ChangeOwnPassword(ctx, "ops", 1, "mine", at); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("stale generation: %v", err)
	}
	if err := s.ChangeOwnPassword(ctx, "ops", 2, "mine", at); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetLocalUser(ctx, "ops")
	if u.PasswordHash != "mine" || u.PasswordGen != 3 || u.MustChangePassword {
		t.Fatalf("ops = %+v", u)
	}
	if err := s.SetLocalDisabled(ctx, "ops", true, Actor{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeOwnPassword(ctx, "ops", 4, "again", at); !errors.Is(err, ErrStaleActor) {
		t.Fatalf("a disabled account must not change its password: %v", err)
	}
}
