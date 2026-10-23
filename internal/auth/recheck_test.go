package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/testutil/oidcmock"
)

// refreshGate wraps the IdP client: it counts refresh-token grants and can
// make them fail, like an IdP that revoked the user's grant.
type refreshGate struct {
	next    http.RoundTripper
	fail    atomic.Bool
	refresh atomic.Int32
	strip   atomic.Bool // drop refresh_token from the code-exchange response (an IdP without offline_access)
}

func (g *refreshGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && r.Body != nil && peekGrant(r) == "refresh_token" {
		g.refresh.Add(1)
		if g.fail.Load() {
			return nil, errors.New("idp unreachable (test)")
		}
	}
	resp, err := g.next.RoundTrip(r)
	if err != nil || !g.strip.Load() || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/token/") && !strings.Contains(r.URL.Path, "token") {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	if cerr := resp.Body.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return nil, rerr
	}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		delete(m, "refresh_token")
		body, _ = json.Marshal(m) //nolint:errcheck // a map[string]any always marshals
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp, nil
}

func recheckEnv(t *testing.T, opts ...envOpt) (*env, *refreshGate) {
	t.Helper()
	g := &refreshGate{}
	e := newEnv(t, ModeOIDC, append([]envOpt{func(o *Options) {
		c := *o.OIDC.HTTPClient
		g.next = c.Transport
		c.Transport = g
		o.OIDC.HTTPClient = &c
	}}, opts...)...)
	return e, g
}

func loginAlice(t *testing.T, e *env, u oidcmock.User) *browser {
	t.Helper()
	b := e.browser()
	if r := b.get(b.startOIDC("/", u)); r.code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", r.code, r.body)
	}
	return b
}

// Why: without a re-check, a user removed from the admin group at the IdP
// keeps admin rights until SESSION_MAX (spec §15.1).
func TestRecheckAppliesGroupChangesFromTheIdP(t *testing.T) {
	e, g := recheckEnv(t)
	b := loginAlice(t, e, alice)
	if s := e.sessions.all(); len(s) != 1 || s[0].RefreshToken == "" || s[0].RecheckedAt.IsZero() {
		t.Fatalf("the login must keep the refresh token: %+v", s)
	}
	// Within the interval nothing is asked of the IdP.
	e.clock.Advance(14 * time.Minute)
	if r := b.post("/admin-action", nil); r.code != http.StatusOK || g.refresh.Load() != 0 {
		t.Fatalf("before the interval: %d, refreshes %d", r.code, g.refresh.Load())
	}
	// At the IdP alice is demoted to the operator group.
	e.idp.TamperNextIDToken(func(c map[string]any) { c["groups"] = []any{"fleet-ops"} })
	e.clock.Advance(2 * time.Minute)
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("operator may still read: %d", r.code)
	}
	if g.refresh.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", g.refresh.Load())
	}
	if r := b.post("/admin-action", nil); r.code != http.StatusForbidden {
		t.Fatalf("the role downgrade was not applied: %d", r.code)
	}
	if s := e.sessions.all()[0]; s.Principal.Role != model.RoleOperator || !s.RecheckedAt.Equal(e.clock.Now()) {
		t.Fatalf("session after re-check: %+v", s)
	}
	// Losing every mapped group ends the session.
	e.idp.TamperNextIDToken(func(c map[string]any) { c["groups"] = []any{"family"} })
	e.clock.Advance(16 * time.Minute)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("a user without a mapped group kept the session")
	}
	if n := len(e.sessions.all()); n != 0 {
		t.Fatalf("the ended session was not deleted (%d left)", n)
	}
	if a := e.audit.last(); a.Action != "auth.session.recheck" || a.Result != "denied: no matching group" {
		t.Errorf("audit %+v", a)
	}
}

// Why: if the IdP cannot confirm the user any more (grant revoked, account
// deleted), trusting the stale groups is what the re-check exists to avoid.
func TestRecheckFailureEndsTheSession(t *testing.T) {
	e, g := recheckEnv(t)
	b := loginAlice(t, e, alice)
	g.fail.Store(true)
	e.clock.Advance(16 * time.Minute)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("the session survived a failed refresh")
	}
	if len(e.sessions.all()) != 0 {
		t.Fatal("session not deleted")
	}
}

func TestRecheckRejectsOtherSubject(t *testing.T) {
	e, _ := recheckEnv(t)
	b := loginAlice(t, e, alice)
	e.idp.TamperNextIDToken(func(c map[string]any) { c["sub"] = "someone-else" })
	e.clock.Advance(16 * time.Minute)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("a refresh that returns another subject was accepted")
	}
}

// Why: rotating refresh tokens are single use; two parallel requests must
// not both spend the token, or the loser would end a healthy session.
func TestRecheckRunsOncePerInterval(t *testing.T) {
	e, g := recheckEnv(t)
	b := loginAlice(t, e, alice)
	e.clock.Advance(16 * time.Minute)
	done := make(chan int, 4)
	for range 4 {
		go func() { done <- b.get("/viewer").code }()
	}
	for range 4 {
		if code := <-done; code != http.StatusOK {
			t.Errorf("parallel request: %d", code)
		}
	}
	if g.refresh.Load() != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", g.refresh.Load())
	}
}

// Why: an IdP that issues no refresh token cannot be re-checked; that must
// not lock everybody out (the session limits still bound it), but it must be
// visible: no RecheckedAt, no refresh traffic.
func TestNoRefreshTokenMeansNoRecheck(t *testing.T) {
	e, g := recheckEnv(t)
	b := loginAlice(t, e, alice)
	e.sessions.mu.Lock()
	for k, s := range e.sessions.m {
		s.RefreshToken = ""
		e.sessions.m[k] = s
	}
	e.sessions.mu.Unlock()
	e.clock.Advance(time.Hour)
	if r := b.get("/viewer"); r.code != http.StatusOK || g.refresh.Load() != 0 {
		t.Fatalf("%d, refreshes %d", r.code, g.refresh.Load())
	}
}

// The same path against the real store: the token round-trips through the
// sealed column and rotates on every re-check.
func TestRecheckWithSQLiteStore(t *testing.T) {
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
	e, g := recheckEnv(t, func(o *Options) { o.Sessions = db })
	b := loginAlice(t, e, alice)
	e.idp.TamperNextIDToken(func(c map[string]any) { c["groups"] = []any{"fleet-ops"} })
	e.clock.Advance(16 * time.Minute)
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("viewer: %d", r.code)
	}
	if r := b.post2(db, "/admin-action"); r.code != http.StatusForbidden {
		t.Fatalf("downgrade through SQLite: %d", r.code)
	}
	if g.refresh.Load() != 1 {
		t.Fatalf("refreshes = %d", g.refresh.Load())
	}
}

// post2 posts with the CSRF token read from a real store (the env's fake
// holds no sessions then).
func (b *browser) post2(db *store.SQLite, path string) resp {
	b.t.Helper()
	s, err := db.GetSession(b.t.Context(), hashID(b.cookie(CookieInsecure)))
	if err != nil {
		b.t.Fatal(err)
	}
	return b.do(http.MethodPost, path, map[string][]string{"csrf": {s.CSRFToken}}, nil)
}

// Why: an IdP that issues no refresh token cannot be re-checked. Trusting
// the stored groups for 12 h would defeat the group revocation, so such a
// session lives one re-check interval and the user signs in again (silently,
// if the IdP session is alive).
func TestSessionWithoutRefreshTokenIsCappedToTheRecheckInterval(t *testing.T) {
	e, g := recheckEnv(t)
	g.strip.Store(true)
	b := loginAlice(t, e, alice)
	s := e.sessions.all()
	if len(s) != 1 || s[0].RefreshToken != "" {
		t.Fatalf("sessions %+v", s)
	}
	if got := s[0].ExpiresAt.Sub(s[0].CreatedAt); got != 15*time.Minute {
		t.Fatalf("session lifetime %v, want the 15 min re-check interval", got)
	}
	e.clock.Advance(14 * time.Minute)
	if r := b.get("/viewer"); r.code != http.StatusOK {
		t.Fatalf("within the interval: %d", r.code)
	}
	e.clock.Advance(2 * time.Minute)
	if r := b.get("/viewer"); r.code == http.StatusOK {
		t.Fatal("a session without refresh token outlived the re-check interval")
	}
	// With a refresh token the session keeps its normal lifetime.
	g.strip.Store(false)
	b = loginAlice(t, e, alice)
	if got := e.sessions.all()[0].ExpiresAt.Sub(e.sessions.all()[0].CreatedAt); got != 12*time.Hour {
		t.Fatalf("normal lifetime %v", got)
	}
	_ = b
}
