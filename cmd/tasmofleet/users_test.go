package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

const cliPassword = "a long enough password"

// cli runs one tasmofleet command against dir like an operator on the host.
func cli(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("TASMOFLEET_DATA_DIR", dir)
	t.Setenv("TASMOFLEET_AUTH_MODE", "local")
	t.Setenv("TASMOFLEET_ALLOW_INSECURE_HTTP", "true")
	t.Setenv("TASMOFLEET_DEVICE_NETWORKS", "192.0.2.0/24")
	var out bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func mustCLI(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	out, err := cli(t, dir, stdin, args...)
	if err != nil {
		t.Fatalf("tasmofleet %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// db opens the CLI's database for assertions (the CLI has exited, so no lock).
func db(t *testing.T, dir string) *store.SQLite {
	t.Helper()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // temp DB
	return s
}

// Why: review finding 1: the old command wrote a role-less row, which the
// store refuses, so no account could be created from the host at all.
func TestUserAddCreatesALoginReadyAccount(t *testing.T) {
	dir := t.TempDir()
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "--role", "admin", "Alice")
	u, err := db(t, dir).GetLocalUser(context.Background(), "alice")
	if err != nil {
		t.Fatalf("the name must be normalised, or the user can never log in: %v", err)
	}
	if u.Role != model.RoleAdmin || u.Disabled || u.MustChangePassword || auth.VerifyPassword(u.PasswordHash, cliPassword) != nil {
		t.Fatalf("alice = %+v", u)
	}
	if _, err := cli(t, dir, cliPassword+"\n", "user", "add", "alice"); !errors.Is(err, auth.ErrUserExists) {
		t.Fatalf("adding an existing user must not overwrite it: %v", err)
	}
	if out := mustCLI(t, dir, cliPassword+"\n", "user", "add", "--must-change", "bob"); !strings.Contains(out, "operator") {
		t.Fatalf("default role: %q", out)
	}
	if u, _ := db(t, dir).GetLocalUser(context.Background(), "bob"); u.Role != model.RoleOperator || !u.MustChangePassword {
		t.Fatalf("bob = %+v", u)
	}
	list := mustCLI(t, dir, "", "user", "list")
	for _, want := range []string{"alice", "admin", "bob", "operator", "must change password"} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
}

// Why: passwords never come from arguments (they would show in ps and shell
// history), only from stdin.
func TestUserCommandsRejectBadInput(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"user"}, {"user", "bogus"}, {"user", "add"}, {"user", "add", "a", "b"}, {"user", "role", "a"}, {"user", "role", "a", "viewer"},
		{"user", "add", "--role", "viewer", "a"}, {"user", "add", "--create", "a"},
	} {
		if _, err := cli(t, dir, cliPassword+"\n", args...); err == nil {
			t.Errorf("tasmofleet %s was accepted", strings.Join(args, " "))
		}
	}
	if _, err := cli(t, dir, "short\n", "user", "add", "weak"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Errorf("weak password: %v", err)
	}
	if _, err := cli(t, dir, "", "user", "add", "empty"); err == nil {
		t.Error("an empty password was accepted")
	}
}

// Why: changing a password must not undo an admin's decisions (a disabled
// account stays disabled, the role stays) and must end the old sessions.
func TestUserPasswdKeepsStateAndRevokesSessions(t *testing.T) {
	dir := t.TempDir()
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "--role", "admin", "root")
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "ops")
	mustCLI(t, dir, "", "user", "disable", "ops")
	s := db(t, dir)
	ctx := context.Background()
	now := time.Now()
	for _, sub := range []string{"local:ops", "local:root"} {
		if err := s.CreateSession(ctx, store.Session{IDHash: sub, Principal: model.Principal{Subject: sub}, CSRFToken: "c", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close() //nolint:errcheck // reopened by the CLI below
	mustCLI(t, dir, "another long password\n", "user", "passwd", "ops")
	s = db(t, dir)
	u, _ := s.GetLocalUser(ctx, "ops")
	if !u.Disabled || u.Role != model.RoleOperator || auth.VerifyPassword(u.PasswordHash, "another long password") != nil {
		t.Fatalf("ops = %+v: passwd must only change the password", u)
	}
	if _, err := s.GetSession(ctx, "local:root"); err != nil {
		t.Errorf("another user's session was revoked: %v", err)
	}
	if _, err := s.GetSession(ctx, "local:ops"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the user's own session survived the password change: %v", err)
	}
	if _, err := cli(t, dir, cliPassword+"\n", "user", "passwd", "ghost"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("passwd on an unknown user without --create: %v", err)
	}
}

// Why: the host administrator is the last resort when nobody can sign in.
// "user passwd --create --role admin" always yields a working admin.
func TestBreakGlassRecovery(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// Nobody exists: set-password creates an admin.
	mustCLI(t, dir, cliPassword+"\n", "user", "passwd", "--create", "--role", "admin", "rescue")
	if u, err := db(t, dir).GetLocalUser(ctx, "rescue"); err != nil || u.Role != model.RoleAdmin || u.Disabled {
		t.Fatalf("rescue = %+v, %v", u, err)
	}
	// Only an operator exists, no admin at all: set-password promotes it.
	dir2 := t.TempDir()
	mustCLI(t, dir2, cliPassword+"\n", "user", "add", "--role", "operator", "ops")
	mustCLI(t, dir2, "recovered password 1\n", "user", "passwd", "--create", "--role", "admin", "ops")
	if u, _ := db(t, dir2).GetLocalUser(ctx, "ops"); u.Role != model.RoleAdmin || auth.VerifyPassword(u.PasswordHash, "recovered password 1") != nil {
		t.Fatalf("ops = %+v", u)
	}
	// The only admin is disabled: a new admin can be created, and enable works.
	dir3 := t.TempDir()
	mustCLI(t, dir3, cliPassword+"\n", "user", "add", "--role", "admin", "root")
	mustCLI(t, dir3, cliPassword+"\n", "user", "add", "--role", "admin", "second")
	mustCLI(t, dir3, "", "user", "disable", "root")
	mustCLI(t, dir3, "", "user", "enable", "root")
	if u, _ := db(t, dir3).GetLocalUser(ctx, "root"); u.Disabled {
		t.Fatal("enable did not enable")
	}
	mustCLI(t, dir3, "", "user", "delete", "second")
}

// Why: a typo must not lock everybody out (review finding 4): the last
// enabled admin cannot be demoted, disabled or deleted, from the CLI either.
func TestCLIProtectsTheLastAdmin(t *testing.T) {
	dir := t.TempDir()
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "--role", "admin", "root")
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "ops")
	for _, args := range [][]string{{"user", "role", "root", "operator"}, {"user", "disable", "root"}, {"user", "delete", "root"}} {
		if _, err := cli(t, dir, "", args...); !errors.Is(err, auth.ErrLastAdmin) {
			t.Errorf("tasmofleet %s: %v, want ErrLastAdmin", strings.Join(args, " "), err)
		}
	}
	mustCLI(t, dir, "", "user", "role", "ops", "admin")
	mustCLI(t, dir, "", "user", "disable", "root") // ops is a second admin now
	if _, err := cli(t, dir, "", "user", "delete", "ops"); !errors.Is(err, auth.ErrLastAdmin) {
		t.Errorf("a disabled admin must not count: %v", err)
	}
}

func TestSessionsRevokeForOneUser(t *testing.T) {
	dir := t.TempDir()
	mustCLI(t, dir, cliPassword+"\n", "user", "add", "--role", "admin", "root")
	s := db(t, dir)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		sub := "local:root"
		if id == "b" {
			sub = "oidc-sub"
		}
		if err := s.CreateSession(ctx, store.Session{IDHash: id, Principal: model.Principal{Subject: sub}, CSRFToken: "c", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close() //nolint:errcheck // reopened by the CLI below
	if out := mustCLI(t, dir, "", "sessions", "revoke", "--user", "Root"); !strings.Contains(out, "revoked 1 sessions of root") {
		t.Fatalf("output %q", out)
	}
	s = db(t, dir)
	if _, err := s.GetSession(ctx, "a"); !errors.Is(err, store.ErrNotFound) {
		t.Error("root's session survived")
	}
	if _, err := s.GetSession(ctx, "b"); err != nil {
		t.Error("another subject's session was revoked")
	}
	s.Close() //nolint:errcheck // reopened by the CLI below
	if out := mustCLI(t, dir, "", "sessions", "revoke"); !strings.Contains(out, "revoked 1 sessions") {
		t.Fatalf("revoke all: %q", out)
	}
	if _, err := cli(t, dir, "", "sessions", "revoke", "extra"); err == nil {
		t.Error("stray argument accepted")
	}
}
