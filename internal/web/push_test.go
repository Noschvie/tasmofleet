package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// pushWebEnv is the web handler with the real push channel on a temporary database.
func pushWebEnv(t *testing.T) (*env, *store.SQLite, *notify.Push) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() }) //nolint:errcheck // cleanup of a temp DB
	key, err := notify.LoadVAPIDKey(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := notify.NewPush(st, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return setupWith(t, func(d *Deps) { d.Push = p }), st, p
}

// pushCall sends a JSON body with any method as role.
func (e *env) pushCall(t *testing.T, method, path, role, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, e.srv.URL+"/tf"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Test-Role", role)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func subscriptionJSON(t *testing.T, endpoint string) string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth) // never fails (crypto/rand)
	b, _ := json.Marshal(map[string]any{"endpoint": endpoint, "expirationTime": nil,
		"keys": map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(auth)}})
	return string(b)
}

const testEndpoint = "https://fcm.googleapis.com/fcm/send/secret-capability-123"

// Why (#610): notifications are for admins (#540); an operator must not be able to read the key, add a
// browser that then receives every event about the fleet, or remove an admin's subscription.
func TestPushRoutesAreForAdminsOnly(t *testing.T) {
	e, st, _ := pushWebEnv(t)
	sub := subscriptionJSON(t, testEndpoint)
	for _, role := range []string{"operator", ""} {
		want := map[string]int{"operator": http.StatusForbidden, "": http.StatusUnauthorized}[role]
		for _, m := range []string{"GET", "POST", "DELETE"} {
			path := sysPath + "/push/subscription"
			if m == "GET" {
				path = sysPath + "/push/key"
			}
			if code, _ := e.pushCall(t, m, path, role, sub); code != want {
				t.Errorf("%s %s as %q: %d, want %d", m, path, role, code, want)
			}
		}
	}
	if subs, _ := st.ListPushSubscriptions(t.Context()); len(subs) != 0 {
		t.Fatalf("a subscription was stored for a non-admin: %+v", subs)
	}
}

// Why: the subscription is stored for the admin who made it (the role is checked again at every push),
// and the audit names who did it and the push service, never the endpoint: its path is the browser's
// capability to receive pushes.
func TestPushSubscribeStoresTheOwnerAndAuditsTheHostOnly(t *testing.T) {
	e, st, _ := pushWebEnv(t)
	if code, body := e.pushCall(t, "POST", sysPath+"/push/subscription", "admin", subscriptionJSON(t, testEndpoint)); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	subs, err := st.ListPushSubscriptions(t.Context())
	if err != nil || len(subs) != 1 || subs[0].Endpoint != testEndpoint || subs[0].OwnerSubject != "test:admin" {
		t.Fatalf("stored %+v %v", subs, err)
	}
	if code, _ := e.pushCall(t, "DELETE", sysPath+"/push/subscription", "admin", `{"endpoint":"`+testEndpoint+`"}`); code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d", code)
	}
	if code, _ := e.pushCall(t, "DELETE", sysPath+"/push/subscription", "admin", `{"endpoint":"`+testEndpoint+`"}`); code != http.StatusNotFound {
		t.Fatalf("second unsubscribe: %d, want 404", code)
	}
	e.aud.mu.Lock()
	defer e.aud.mu.Unlock()
	var actions []string
	for _, ev := range e.aud.events {
		actions = append(actions, ev.Action)
		if strings.Contains(ev.Target+ev.Detail+ev.Result, "secret-capability") {
			t.Errorf("audit carries the endpoint: %+v", ev)
		}
		if ev.Target != "push:fcm.googleapis.com" || ev.Actor != "test:admin" {
			t.Errorf("audit %+v", ev)
		}
	}
	if strings.Join(actions, ",") != "push.subscribe,push.unsubscribe" {
		t.Errorf("audit actions %v", actions)
	}
}

// Why: a subscription that is not an https push service with a valid browser key is refused with a
// reason, and nothing is stored (it would only fail at every event, or POST somewhere it should not).
func TestPushSubscribeRejectsBadEndpointAndKeys(t *testing.T) {
	e, st, _ := pushWebEnv(t)
	for name, body := range map[string]string{
		"plain http":   subscriptionJSON(t, "http://push.example/x"),
		"loopback":     subscriptionJSON(t, "https://127.0.0.1/x"),
		"not json":     "endpoint=https://push.example/x",
		"missing keys": `{"endpoint":"https://push.example/x"}`,
		"too large":    `{"endpoint":"https://push.example/` + strings.Repeat("x", 9000) + `"}`,
	} {
		if code, _ := e.pushCall(t, "POST", sysPath+"/push/subscription", "admin", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if subs, _ := st.ListPushSubscriptions(t.Context()); len(subs) != 0 {
		t.Fatalf("stored %+v", subs)
	}
}

// Why: the browser needs the public key to subscribe; the private key signs every push and must never
// reach a page (#610 acceptance: never in HTML).
func TestPublicKeyEndpointNeverHasPrivateKey(t *testing.T) {
	e, st, p := pushWebEnv(t)
	code, body := e.pushCall(t, "GET", sysPath+"/push/key", "admin", "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil || len(m) != 1 || m["key"] != p.PublicKey() {
		t.Fatalf("body %q: %v", body, err)
	}
	priv, err := st.VAPIDKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, priv) {
		t.Fatal("the private key is in the answer")
	}
}
