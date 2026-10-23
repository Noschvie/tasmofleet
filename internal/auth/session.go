package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Sessions manages server-side web sessions (spec §15.2). The cookie holds
// 32 random bytes; the store keeps only their SHA-256.
type Sessions struct {
	Store store.SessionStore
	Clock clock.Clock
	Idle  time.Duration // default 2h
	Max   time.Duration // default 12h
	// NoRefreshLife, if > 0, caps the lifetime of an OIDC session whose IdP
	// issued no refresh token: its groups cannot be re-checked, so it lives
	// no longer than one re-check interval and the user re-authenticates.
	NoRefreshLife time.Duration
	// Cookies, if set, is pruned of expired device cookies by RunCleanup.
	Cookies store.DeviceCookieStore
}

// Session cookie names.
const (
	CookieSecure   = "__Host-tasmofleet"
	CookieInsecure = "tasmofleet"
	touchEvery     = time.Minute
)

func (s *Sessions) idle() time.Duration {
	if s.Idle > 0 {
		return s.Idle
	}
	return 2 * time.Hour
}

func (s *Sessions) max() time.Duration {
	if s.Max > 0 {
		return s.Max
	}
	return 12 * time.Hour
}

// secure reports whether cookies for r get the Secure flag and the __Host-
// name: this request arrived over HTTPS (own TLS listener, or
// X-Forwarded-Proto from a trusted proxy). PUBLIC_URL does not decide it:
// plain-HTTP requests to the container's own address (break-glass access)
// never get a Secure cookie, the browser would drop it and login would fail.
func (s *Sessions) secure(r *http.Request) bool {
	return httpx.Scheme(r.Context()) == "https"
}

func (s *Sessions) cookieNameFor(r *http.Request) string {
	if s.secure(r) {
		return CookieSecure
	}
	return CookieInsecure
}

func (s *Sessions) cookiePath(r *http.Request) string {
	if s.secure(r) { // __Host- requires Path=/
		return "/"
	}
	if b := httpx.BasePath(r.Context()); b != "" {
		return b // path-match: "/tf" covers "/tf" and "/tf/...", not "/tfx"
	}
	return "/"
}

func hashID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create starts a session for p. Any session the request already carries is
// deleted first, so the ID always changes at login (no fixation).
func (s *Sessions) Create(w http.ResponseWriter, r *http.Request, p model.Principal, idToken string) (store.Session, error) {
	return s.create(w, r, p, idToken, "", 0, 0, true)
}

// CreateLocal is Create for a local login: passwordGen is the generation of
// the account's password that the login verified; the session is valid only
// while that generation is current. persistent is "Keep me signed in on this
// device" (N11): without it the cookie is a browser-session cookie (no
// Max-Age) and ends when the browser closes; the server-side limits are the
// same either way.
func (s *Sessions) CreateLocal(w http.ResponseWriter, r *http.Request, p model.Principal, passwordGen int64, persistent bool) (store.Session, error) {
	return s.create(w, r, p, "", "", passwordGen, 0, persistent)
}

// CreateOIDC is Create for an OIDC login: refreshToken (may be empty) is
// kept, sealed by the store, for the periodic group re-check.
func (s *Sessions) CreateOIDC(w http.ResponseWriter, r *http.Request, p model.Principal, idToken, refreshToken string) (store.Session, error) {
	var life time.Duration
	if refreshToken == "" {
		life = s.NoRefreshLife
	}
	return s.create(w, r, p, idToken, refreshToken, 0, life, true)
}

// create stores the session; life > 0 shortens its absolute lifetime. A
// persistent cookie carries Max-Age, otherwise it is a browser-session cookie.
func (s *Sessions) create(w http.ResponseWriter, r *http.Request, p model.Principal, idToken, refreshToken string, passwordGen int64, life time.Duration, persistent bool) (store.Session, error) {
	ctx := r.Context()
	if c, err := r.Cookie(s.cookieNameFor(r)); err == nil && c.Value != "" {
		if err := s.deleteOwn(ctx, c.Value); err != nil {
			return store.Session{}, fmt.Errorf("auth: rotate session: %w", err)
		}
	}
	raw := rand.Text() + rand.Text() // 2x 128 bits: the cookie carries 256 bits of entropy
	now := s.Clock.Now()
	sess := store.Session{
		IDHash: hashID(raw), Principal: p, CSRFToken: rand.Text(), IDToken: idToken, RefreshToken: refreshToken,
		PasswordGen: passwordGen, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(s.max()),
	}
	maxAge := s.max()
	if life > 0 && life < maxAge {
		sess.ExpiresAt, maxAge = now.Add(life), life
	}
	if refreshToken != "" {
		sess.RecheckedAt = now
	}
	if err := s.Store.CreateSession(ctx, sess); err != nil {
		return store.Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	c := &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.cookieNameFor(r), Value: raw, Path: s.cookiePath(r),
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
	}
	if persistent {
		c.MaxAge = int(maxAge.Seconds())
	}
	http.SetCookie(w, c)
	return sess, nil
}

// Get returns the request's session or ErrUnauthenticated. Expired sessions
// (idle or absolute) are deleted and reported as ErrSessionExpired.
func (s *Sessions) Get(r *http.Request) (store.Session, error) {
	c, err := r.Cookie(s.cookieNameFor(r))
	if err != nil || c.Value == "" {
		return store.Session{}, ErrUnauthenticated
	}
	ctx := r.Context()
	id := hashID(c.Value)
	sess, err := s.Store.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Session{}, ErrUnauthenticated
	}
	if err != nil {
		return store.Session{}, fmt.Errorf("auth: load session: %w", err)
	}
	now := s.Clock.Now()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.CreatedAt.Add(s.max())) || !now.Before(sess.LastSeen.Add(s.idle())) {
		if err := s.Store.DeleteSession(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.ErrorContext(ctx, "delete expired session", "err", err)
		}
		return store.Session{}, ErrSessionExpired
	}
	if now.Sub(sess.LastSeen) >= touchEvery {
		if err := s.Store.TouchSession(ctx, id, now); err != nil {
			return store.Session{}, fmt.Errorf("auth: touch session: %w", err)
		}
		sess.LastSeen = now
	}
	return sess, nil
}

// deleteOwn deletes the session a cookie value names (already deleted is fine).
func (s *Sessions) deleteOwn(ctx context.Context, raw string) error {
	if err := s.Store.DeleteSession(ctx, hashID(raw)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// Destroy deletes the request's session and clears the cookie.
func (s *Sessions) Destroy(w http.ResponseWriter, r *http.Request) error {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.cookieNameFor(r), Value: "", Path: s.cookiePath(r), MaxAge: -1,
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
	})
	if c, cerr := r.Cookie(s.cookieNameFor(r)); cerr == nil && c.Value != "" {
		if err := s.deleteOwn(r.Context(), c.Value); err != nil {
			return fmt.Errorf("auth: delete session: %w", err)
		}
	}
	return nil
}

// RevokeAll deletes every session (for the CLI, e.g. after a suspected
// compromise or a group change that must take effect now).
func (s *Sessions) RevokeAll(ctx context.Context) (int64, error) {
	// A cut-off far beyond any ExpiresAt, even of sessions created under an
	// older, longer SESSION_MAX.
	n, err := s.Store.DeleteExpiredSessions(ctx, s.Clock.Now().AddDate(100, 0, 0))
	if err != nil {
		return 0, fmt.Errorf("auth: revoke all sessions: %w", err)
	}
	return n, nil
}

// RunCleanup deletes expired sessions every interval (1h if zero) until ctx
// ends. Idle-expired sessions are also deleted on their next use.
func (s *Sessions) RunCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(interval):
			n, err := s.Store.DeleteExpiredSessions(ctx, s.Clock.Now())
			if err != nil {
				slog.ErrorContext(ctx, "session cleanup", "err", err)
				continue
			}
			if n > 0 {
				slog.DebugContext(ctx, "session cleanup", "deleted", n)
			}
			if s.Cookies != nil {
				if _, err := s.Cookies.DeleteDeviceCookiesBefore(ctx, s.Clock.Now().Add(-deviceTTL)); err != nil {
					slog.ErrorContext(ctx, "device cookie cleanup", "err", err)
				}
			}
		}
	}
}
