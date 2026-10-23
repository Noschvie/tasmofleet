package store

import (
	"context"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the audit trail stores the stable subject; the shown name must follow
// the identity provider (a renamed user), so a second sign-in replaces the
// name and never adds a second row for the same subject.
func TestPrincipalNameFollowsTheLastSignIn(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	sub := "5f2c0e9a7b1d4c3e8a6f0b2d9c1e7a43b8d5f0a2c4e6b8d0f1a3c5e7b9d1f3a5"
	for _, name := range []string{"alice", "Alice K."} {
		if err := s.UpsertPrincipal(ctx, PrincipalRecord{Subject: sub, DisplayName: name, Kind: "sso", UpdatedAt: time.Unix(1700000000, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertPrincipal(ctx, PrincipalRecord{Subject: "local:admin", DisplayName: "admin", Kind: "local"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListPrincipals(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for _, p := range got {
		if p.Subject == sub && (p.DisplayName != "Alice K." || p.Kind != "sso") {
			t.Fatalf("%+v", p)
		}
	}
}

// Why: the audit user filter accepts a display name, which can stand for
// several subjects; the store must match any of them.
func TestAuditFilterMatchesAnyOfSeveralActors(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	for _, a := range []string{"sub-a", "sub-b", "sub-c"} {
		if err := s.AppendAudit(ctx, model.AuditEvent{At: time.Unix(1700000000, 0), Actor: a, Action: "x", Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAudit(ctx, AuditFilter{Actors: []string{"sub-a", "sub-c"}})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
}

// Why: the Users page shows the role and time of the LAST sign-in, so a second
// login replaces both; the name-only upsert the web layer does on every
// request must never wipe them.
func TestRecordLoginKeepsRoleAndTimeAcrossNameUpserts(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	first, second := time.UnixMilli(1700000000000), time.UnixMilli(1700000900000)
	rec := PrincipalRecord{Subject: "sub-1", DisplayName: "alice", Kind: "sso", Role: "operator", UpdatedAt: first}
	if err := s.RecordLogin(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec.Role, rec.UpdatedAt = "admin", second
	if err := s.RecordLogin(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPrincipal(ctx, PrincipalRecord{Subject: "sub-1", DisplayName: "Alice A.", Kind: "sso", UpdatedAt: second.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPrincipal(ctx, PrincipalRecord{Subject: "old", DisplayName: "bob", Kind: "sso"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListPrincipals(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for _, p := range got {
		switch p.Subject {
		case "sub-1":
			if p.Role != "admin" || !p.LastLogin.Equal(second) || p.DisplayName != "Alice A." {
				t.Fatalf("%+v", p)
			}
		case "old":
			if p.Role != "" || !p.LastLogin.IsZero() {
				t.Fatalf("a row that never logged in since 0016 has no login: %+v", p)
			}
		}
	}
}

// Why: "active sessions" must not count sessions that are dead but not yet
// swept (absolute expiry passed, or idle too long), and must be per subject.
func TestCountSessionsCountsOnlyLiveOnesPerSubject(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	now := time.UnixMilli(1700000000000)
	add := func(id, sub string, seen, exp time.Time) {
		t.Helper()
		err := s.CreateSession(ctx, Session{IDHash: id, Principal: model.Principal{Subject: sub, Method: "oidc", Role: model.RoleOperator},
			CSRFToken: "c", CreatedAt: seen, LastSeen: seen, ExpiresAt: exp})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "sub-a", now.Add(-time.Minute), now.Add(time.Hour))
	add("a2", "sub-a", now.Add(-time.Minute), now.Add(time.Hour))
	add("a-idle", "sub-a", now.Add(-3*time.Hour), now.Add(time.Hour))
	add("a-expired", "sub-a", now.Add(-time.Minute), now.Add(-time.Second))
	add("b1", "sub-b", now.Add(-time.Minute), now.Add(time.Hour))
	got, err := s.CountSessions(ctx, now, 2*time.Hour)
	if err != nil || len(got) != 2 || got["sub-a"] != 2 || got["sub-b"] != 1 {
		t.Fatalf("%v %v", got, err)
	}
}
