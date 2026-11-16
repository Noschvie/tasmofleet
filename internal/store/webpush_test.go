package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/secretbox"
)

const fakeVAPID = "fake-vapid-private-key-NOT-REAL-0123456789"

// Why (#610): the VAPID private key signs every push; whoever holds it can push to every subscribed
// browser. It must be sealed with secret.key like the other secrets, so a database file or a snapshot
// alone does not give it away.
func TestVAPIDKeySealedAtRest(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.VAPIDKey(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: %v, want ErrNotFound (the first start makes the key)", err)
	}
	if err := s.ReplaceVAPIDKey(ctx, fakeVAPID); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT private_key FROM push_vapid").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, fakeVAPID) {
		t.Fatalf("the stored column holds the key in clear: %q", raw)
	}
	if k, err := s.VAPIDKey(ctx); err != nil || k != fakeVAPID {
		t.Fatalf("VAPIDKey = %q, %v", k, err)
	}
}

// Why: a key that changed at every start would silently break every subscription (the push services
// refuse a push signed with another key than the one the browser subscribed with).
func TestVAPIDKeySurvivesReopen(t *testing.T) {
	s, path, box := newStore(t)
	if err := s.ReplaceVAPIDKey(ctx, fakeVAPID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() }) //nolint:errcheck // cleanup of a temp DB
	if k, err := s2.VAPIDKey(ctx); err != nil || k != fakeVAPID {
		t.Fatalf("after reopen: %q, %v", k, err)
	}
}

// Why: with another secret.key the sealed key cannot be opened. That must be reported as such (not as
// "no key yet" and not as a database failure), and replacing it must drop every subscription in the same
// step: they were made for the old public key and would be refused by the push services from now on.
func TestUnreadableVAPIDKeyAndReplacingItDropsEverySubscription(t *testing.T) {
	s, path, _ := newStore(t)
	if err := s.ReplaceVAPIDKey(ctx, fakeVAPID); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePushSubscription(ctx, PushSubscription{Endpoint: "https://push.example/a", P256dh: "p", Auth: "a", OwnerMethod: "local", OwnerSubject: "local:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := secretbox.LoadOrCreate(filepath.Join(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() }) //nolint:errcheck // cleanup of a temp DB
	if _, err := s2.VAPIDKey(ctx); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("VAPIDKey with another secret.key: %v, want ErrUnreadable", err)
	}
	if err := s2.ReplaceVAPIDKey(ctx, "new-fake-key"); err != nil {
		t.Fatal(err)
	}
	if k, err := s2.VAPIDKey(ctx); err != nil || k != "new-fake-key" {
		t.Fatalf("after replace: %q, %v", k, err)
	}
	if subs, err := s2.ListPushSubscriptions(ctx); err != nil || len(subs) != 0 {
		t.Fatalf("subscriptions after a key replace: %+v %v, want none", subs, err)
	}
}

// Why: one browser has one endpoint. Subscribing again (new keys, or another admin signed in on the same
// browser) must replace its row, never add a second one that would deliver every event twice.
func TestSubscriptionUpsertByEndpointAndResult(t *testing.T) {
	s, _, _ := newStore(t)
	at := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	a := PushSubscription{Endpoint: "https://push.example/a", P256dh: "p1", Auth: "a1", OwnerMethod: "local", OwnerSubject: "local:alice"}
	if err := s.SavePushSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPushResult(ctx, a.Endpoint, at, "HTTP 400"); err != nil {
		t.Fatal(err)
	}
	a.P256dh, a.Auth, a.OwnerMethod, a.OwnerSubject = "p2", "a2", "oidc", "sso-bob"
	if err := s.SavePushSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePushSubscription(ctx, PushSubscription{Endpoint: "https://push.example/b", P256dh: "p", Auth: "a", OwnerMethod: "local", OwnerSubject: "local:alice"}); err != nil {
		t.Fatal(err)
	}
	subs, err := s.ListPushSubscriptions(ctx)
	if err != nil || len(subs) != 2 {
		t.Fatalf("subs %+v %v, want two rows", subs, err)
	}
	got := subs[0]
	if got.Endpoint != a.Endpoint || got.P256dh != "p2" || got.Auth != "a2" || got.OwnerMethod != "oidc" || got.OwnerSubject != "sso-bob" || !got.CreatedAt.Equal(at) {
		t.Fatalf("re-subscribed row %+v", got)
	}
	if got.LastError != "" {
		t.Fatalf("a new subscription starts without the old row's error: %q", got.LastError)
	}
	later := at.Add(time.Minute)
	if err := s.RecordPushResult(ctx, a.Endpoint, later, ""); err != nil {
		t.Fatal(err)
	}
	subs, _ = s.ListPushSubscriptions(ctx)
	if !subs[0].LastOKAt.Equal(later) || subs[0].LastError != "" {
		t.Fatalf("after a delivered push: %+v", subs[0])
	}
	if ok, err := s.DeletePushSubscription(ctx, a.Endpoint); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := s.DeletePushSubscription(ctx, a.Endpoint); err != nil || ok {
		t.Fatalf("second delete: %v %v, want not found", ok, err)
	}
	// a result for a row that is gone (unsubscribed while the push was on its way) is no error
	if err := s.RecordPushResult(ctx, a.Endpoint, later, "HTTP 500"); err != nil {
		t.Fatal(err)
	}
}
