package store

import (
	"context"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: preferences must follow the user, never leak to another one, and a
// second save replaces the first (one row per user and key).
func TestUserPrefsAreKeyedPerUser(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetUserPref(ctx, "local:alice", "table.devices", `{"w":{"name":20}}`))
	must(s.SetUserPref(ctx, "local:alice", "table.devices", `{"w":{"name":30}}`))
	must(s.SetUserPref(ctx, "local:alice", "pager", `{"size":50}`))
	must(s.SetUserPref(ctx, "oidc-sub-1", "pager", `{"size":100}`))

	got, err := s.UserPrefs(ctx, "local:alice")
	must(err)
	if len(got) != 2 || got["table.devices"] != `{"w":{"name":30}}` || got["pager"] != `{"size":50}` {
		t.Fatalf("alice = %v", got)
	}
	other, err := s.UserPrefs(ctx, "oidc-sub-1")
	must(err)
	if len(other) != 1 || other["pager"] != `{"size":100}` {
		t.Fatalf("other user = %v", other)
	}

	must(s.DeleteUserPref(ctx, "local:alice", "pager"))
	must(s.DeleteUserPref(ctx, "local:alice", "pager")) // idempotent: reset twice is fine
	got, _ = s.UserPrefs(ctx, "local:alice")
	if _, ok := got["pager"]; ok || len(got) != 1 {
		t.Fatalf("after delete: %v", got)
	}
	if none, _ := s.UserPrefs(ctx, "nobody"); len(none) != 0 {
		t.Fatalf("unknown user has prefs: %v", none)
	}
}

// Why: a later account with the same name must not inherit the table layout of a
// removed one, and the preferences of a removed account are dead data.
func TestDeletingALocalUserDeletesItsPrefs(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := t.Context()
	addUser(t, s, "root", model.RoleAdmin)
	addUser(t, s, "ops", model.RoleOperator)
	for _, u := range []string{"local:ops", "local:root"} {
		if err := s.SetUserPref(ctx, u, "pager", `{"size":50}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteLocalUser(ctx, "ops", Actor{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserPrefs(ctx, "local:ops"); len(got) != 0 {
		t.Errorf("prefs of the removed account remain: %v", got)
	}
	if got, _ := s.UserPrefs(ctx, "local:root"); len(got) != 1 {
		t.Errorf("another account's prefs must stay: %v", got)
	}
	// A refused delete (last admin) keeps the prefs.
	if err := s.DeleteLocalUser(ctx, "root", Actor{}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last admin: %v", err)
	}
	if got, _ := s.UserPrefs(ctx, "local:root"); len(got) != 1 {
		t.Errorf("a refused delete must keep the prefs: %v", got)
	}
}
