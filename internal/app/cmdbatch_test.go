package app

import (
	"context"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type fakeLocal map[string]store.LocalUser

func (f fakeLocal) GetLocalUser(_ context.Context, name string) (store.LocalUser, error) {
	if u, ok := f[name]; ok {
		return u, nil
	}
	return store.LocalUser{}, store.ErrNotFound
}

type fakeDir []store.PrincipalRecord

func (f fakeDir) UpsertPrincipal(context.Context, store.PrincipalRecord) error { return nil }
func (f fakeDir) ListPrincipals(context.Context) ([]store.PrincipalRecord, error) {
	return f, nil
}

// Why (security review): the subject alone does not say who someone is. A local
// admin and a header operator can spell the same subject; a job created by the
// operator must never run with the admin's role, and the admin's job must run
// as the admin whatever a header user of the same name is.
func TestResolverSeparatesSignInMethodsThatShareASubject(t *testing.T) {
	r := principalResolver{
		local: fakeLocal{"alice": {Username: "alice", Role: model.RoleAdmin}},
		dir: fakeDir{
			{Subject: "local:alice", Kind: "header", Role: "operator", DisplayName: "alice"}, // a header user claiming the local spelling
			{Subject: "alice", Kind: "header", Role: "operator"},
			{Subject: "sso-1", Kind: "sso", Role: "admin"},
		},
	}
	ctx := context.Background()

	// the header operator "local:alice" is never resolved to the local admin
	if p, err := r.Current(ctx, "header", "local:alice"); err == nil {
		t.Fatalf("a header user must not resolve under the local prefix: %+v", p)
	}
	// the local admin's own job runs as the local admin, not as the header record
	p, err := r.Current(ctx, "local", "local:alice")
	if err != nil || p.Role != model.RoleAdmin || p.Method != "local" {
		t.Fatalf("local admin: %+v %v", p, err)
	}
	// a plain header operator stays an operator (the engine then refuses the step)
	if p, err := r.Current(ctx, "header", "alice"); err != nil || p.Role != model.RoleOperator {
		t.Fatalf("header operator: %+v %v", p, err)
	}
	// the same subject claimed as another method is ambiguous: refused
	if _, err := r.Current(ctx, "oidc", "alice"); err == nil {
		t.Fatal("an OIDC job for a subject recorded as a header user must fail closed")
	}
	if _, err := r.Current(ctx, "header", "sso-1"); err == nil {
		t.Fatal("a header job for a subject recorded as SSO must fail closed")
	}
	if p, err := r.Current(ctx, "oidc", "sso-1"); err != nil || p.Role != model.RoleAdmin {
		t.Fatalf("sso admin: %+v %v", p, err)
	}
	// local method needs the local prefix; jobs from before the method was stored never resolve
	if _, err := r.Current(ctx, "local", "alice"); err == nil {
		t.Fatal("a local job needs the local: subject")
	}
	for _, m := range []string{"", "future"} {
		if _, err := r.Current(ctx, m, "local:alice"); err == nil {
			t.Fatalf("method %q must not resolve", m)
		}
	}
}

// Why: gone, disabled, no role and never signed in all fail closed.
func TestResolverFailsClosed(t *testing.T) {
	r := principalResolver{
		local: fakeLocal{"off": {Username: "off", Role: model.RoleAdmin, Disabled: true}},
		dir:   fakeDir{{Subject: "norole", Kind: "sso"}},
	}
	ctx := context.Background()
	for name, tc := range map[string]struct{ method, subject string }{
		"disabled":        {"local", "local:off"},
		"gone":            {"local", "local:nobody"},
		"no role":         {"oidc", "norole"},
		"never signed in": {"header", "stranger"},
	} {
		if _, err := r.Current(ctx, tc.method, tc.subject); err == nil {
			t.Errorf("%s resolved", name)
		}
	}
}
