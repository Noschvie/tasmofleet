package store

import (
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the user detail page shows who created an account and when, and its
// profile; the values must survive the round trip through the database, and
// an account created by an admin must name that admin.
func TestLocalUserProfileAndCreationAreStored(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if err := s.AddLocalUser(ctx, LocalUser{Username: "anna", PasswordHash: "h", UpdatedAt: at, Role: model.RoleAdmin,
		DisplayName: "Anna A.", Email: "anna@example.org", CreatedAt: at, CreatedBy: "cli"}, Actor{}); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetLocalUser(ctx, "anna")
	if err != nil || u.DisplayName != "Anna A." || u.Email != "anna@example.org" || !u.CreatedAt.Equal(at) || u.CreatedBy != "cli" || !u.OTPExpiresAt.IsZero() {
		t.Fatalf("anna = %+v, %v", u, err)
	}
	if err := s.SetLocalProfile(ctx, "anna", "Anna", "", Actor{}); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.GetLocalUser(ctx, "anna"); u.DisplayName != "Anna" || u.Email != "" {
		t.Errorf("profile not replaced: %+v", u)
	}
}

// Why: a one-time password is valid for a limited time only. Any later
// password change (by the owner or an admin) must end that limit, or a
// regular password would expire 24 h after an old reset.
func TestOneTimePasswordExpiryIsClearedByEveryOtherPasswordChange(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	if err := s.AddLocalUser(ctx, LocalUser{Username: "bob", PasswordHash: "h", UpdatedAt: at, Role: model.RoleOperator}, Actor{}); err != nil {
		t.Fatal(err)
	}
	otp := func() {
		t.Helper()
		if err := s.SetLocalOneTimePassword(ctx, "bob", "otp-hash", at, at.Add(24*time.Hour), Actor{}); err != nil {
			t.Fatal(err)
		}
		u, _ := s.GetLocalUser(ctx, "bob")
		if !u.MustChangePassword || !u.OTPExpiresAt.Equal(at.Add(24*time.Hour)) || u.PasswordHash != "otp-hash" {
			t.Fatalf("after OTP: %+v", u)
		}
	}
	otp()
	if err := s.SetLocalPassword(ctx, "bob", "typed", at, true, Actor{}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetLocalUser(ctx, "bob"); !u.OTPExpiresAt.IsZero() {
		t.Errorf("admin reset kept the OTP expiry: %+v", u)
	}
	otp()
	u, _ := s.GetLocalUser(ctx, "bob")
	if err := s.ChangeOwnPassword(ctx, "bob", u.PasswordGen, "own", at); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetLocalUser(ctx, "bob"); !u.OTPExpiresAt.IsZero() || u.MustChangePassword {
		t.Errorf("own change kept the OTP expiry: %+v", u)
	}
	if err := s.SetLocalOneTimePassword(ctx, "bob", "x", at, time.Time{}, Actor{}); err == nil {
		t.Error("a one-time password without expiry was accepted")
	}
}

// Why: profile edits follow the same authority rules as every other account
// change: an admin who lost the role meanwhile changes nothing, and the
// owner's edit applies only to the account state the session was checked
// against.
func TestProfileEditsCheckTheirAuthority(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	for _, u := range []LocalUser{
		{Username: "root", PasswordHash: "h", Role: model.RoleAdmin},
		{Username: "ops", PasswordHash: "h", Role: model.RoleOperator},
	} {
		if err := s.AddLocalUser(ctx, u, Actor{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetLocalProfile(ctx, "ops", "x", "", Actor{Username: "root", PasswordGen: 2}); !errors.Is(err, ErrStaleActor) {
		t.Errorf("stale admin: %v, want ErrStaleActor", err)
	}
	if err := s.SetLocalProfile(ctx, "nobody", "x", "", Actor{Username: "root", PasswordGen: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing account: %v, want ErrNotFound", err)
	}
	if err := s.SetOwnProfile(ctx, "ops", 5, "x", ""); !errors.Is(err, ErrStaleActor) {
		t.Errorf("own edit at a stale generation: %v, want ErrStaleActor", err)
	}
	if err := s.SetOwnProfile(ctx, "ops", 1, "Ops", "ops@example.org"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetLocalUser(ctx, "ops"); u.DisplayName != "Ops" || u.Email != "ops@example.org" {
		t.Errorf("own edit lost: %+v", u)
	}
}

// Why: "End session" on the user detail page ends exactly one session of
// exactly that user; the list shows only sessions that are still alive.
func TestSubjectSessionsListAndEndOne(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	now := time.UnixMilli(1700000000000)
	add := func(id, sub string, seen, exp time.Time) {
		t.Helper()
		if err := s.CreateSession(ctx, Session{IDHash: id, Principal: model.Principal{Subject: sub, Method: "oidc", Role: model.RoleOperator},
			CSRFToken: "c", CreatedAt: seen, LastSeen: seen, ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	add("a-old", "sub-a", now.Add(-time.Hour), now.Add(time.Hour))
	add("a-new", "sub-a", now.Add(-time.Minute), now.Add(time.Hour))
	add("a-expired", "sub-a", now.Add(-time.Minute), now.Add(-time.Second))
	add("b1", "sub-b", now.Add(-time.Minute), now.Add(time.Hour))
	list, err := s.ListSubjectSessions(ctx, "sub-a", now, 2*time.Hour)
	if err != nil || len(list) != 2 || list[0].IDHash != "a-new" || list[1].IDHash != "a-old" {
		t.Fatalf("list = %+v, %v (want the two live ones, newest first)", list, err)
	}
	if ok, err := s.DeleteSubjectSession(ctx, "sub-a", "b1"); ok || err != nil {
		t.Errorf("ended another user's session: %v %v", ok, err)
	}
	if ok, err := s.DeleteSubjectSession(ctx, "sub-a", "a-old"); !ok || err != nil {
		t.Errorf("end a-old: %v %v", ok, err)
	}
	if _, err := s.GetSession(ctx, "a-new"); err != nil {
		t.Errorf("the other session of the user ended too: %v", err)
	}
	if _, err := s.GetSession(ctx, "b1"); err != nil {
		t.Errorf("b1 gone: %v", err)
	}
}

// Why: the detail page of an SSO user shows the e-mail of the last sign-in.
func TestPrincipalEmailIsRecorded(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	if err := s.RecordLogin(ctx, PrincipalRecord{Subject: "oidc:x", DisplayName: "X", Kind: "sso", Email: "x@example.org"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListPrincipals(ctx)
	if err != nil || len(rows) != 1 || rows[0].Email != "x@example.org" {
		t.Fatalf("%+v %v", rows, err)
	}
}

// Why (N12): the setup link is valid only while the first-start admin is
// marked InitialSetup. The mark must be stored, never appear by default (an
// account added without it, or one from before the column), and end with
// every change that moves the password generation, so no reset, own change,
// disable or seed can bring an account back into setup.
func TestInitialSetupMarkEndsWithEveryGenerationChange(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	add := func(name string, mark bool) {
		t.Helper()
		if err := s.AddLocalUser(ctx, LocalUser{Username: name, PasswordHash: "h", UpdatedAt: at, Role: model.RoleAdmin,
			MustChangePassword: true, InitialSetup: mark}, Actor{}); err != nil {
			t.Fatal(err)
		}
		if u, _ := s.GetLocalUser(ctx, name); u.InitialSetup != mark {
			t.Fatalf("%s: InitialSetup = %v, want %v", name, u.InitialSetup, mark)
		}
	}
	add("plain", false)
	changes := map[string]func(name string) error{
		"admin reset": func(n string) error { return s.SetLocalPassword(ctx, n, "x", at, true, Actor{}) },
		"one-time":    func(n string) error { return s.SetLocalOneTimePassword(ctx, n, "x", at, at.Add(time.Hour), Actor{}) },
		"own change":  func(n string) error { return s.ChangeOwnPassword(ctx, n, 1, "x", at) },
		"disable":     func(n string) error { return s.SetLocalDisabled(ctx, n, true, Actor{}) },
		"seed": func(n string) error {
			return s.SetLocalUser(ctx, LocalUser{Username: n, PasswordHash: "x", Role: model.RoleAdmin})
		},
	}
	for what, change := range changes {
		add(what, true)
		if err := change(what); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if u, _ := s.GetLocalUser(ctx, what); u.InitialSetup {
			t.Errorf("%s kept the initial setup mark: %+v", what, u)
		}
	}
}
