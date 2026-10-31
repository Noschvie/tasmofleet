package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// flow is the state of one login attempt, kept in a short-lived HttpOnly
// cookie bound to the browser that started it.
type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
	// Step-up flows (oidcstepup.go) only: the purpose, the session (hash) and
	// the subject they were started for, and the start time auth_time must reach.
	Purpose string `json:"p,omitempty"`
	Session string `json:"h,omitempty"`
	Subject string `json:"u,omitempty"`
	Started int64  `json:"t,omitempty"`
}

const flowTTL = 10 * time.Minute

func (s *Service) flowCookie(r *http.Request) string {
	if s.sessions.secure(r) {
		return CookieSecure + "-oidc"
	}
	return CookieInsecure + "-oidc"
}

func (s *Service) setFlowCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the request scheme (spec §15.2)
		Name: s.flowCookie(r), Value: value, Path: s.sessions.cookiePath(r), MaxAge: maxAge,
		HttpOnly: true, Secure: s.sessions.secure(r), SameSite: http.SameSiteLaxMode,
	})
}

// flowMAC authenticates the flow cookie with the session-purpose key.
func (s *Service) flowMAC(body string) string {
	m := hmac.New(sha256.New, s.sessionKey)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func random32() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error (Go >= 1.24)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) startOIDC(w http.ResponseWriter, r *http.Request, next string) {
	s.beginFlow(w, r, flow{Next: next})
}

// beginFlow starts an authorization code flow from the preset fields of f
// (Next and the step-up fields); opts add authorization request parameters.
func (s *Service) beginFlow(w http.ResponseWriter, r *http.Request, f flow, opts ...oauth2.AuthCodeOption) {
	s.mu.RLock()
	cfg := s.oauth
	s.mu.RUnlock()
	if cfg == nil {
		s.o.Pages.Message(w, r, http.StatusServiceUnavailable, MessageData{
			BasePath: httpx.BasePath(r.Context()), Title: "Sign-in unavailable",
			Text: "The identity provider is not reachable yet.", LocalLogin: s.o.LocalLogin,
		})
		return
	}
	f.State, f.Nonce, f.Verifier = random32(), random32(), oauth2.GenerateVerifier()
	f.Expires = s.o.Clock.Now().Add(flowTTL).Unix()
	b, err := json.Marshal(f)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	s.setFlowCookie(w, r, body+"."+s.flowMAC(body), int(flowTTL.Seconds()))
	opts = append(opts, oauth2.S256ChallengeOption(f.Verifier), oidc.Nonce(f.Nonce))
	http.Redirect(w, r, cfg.AuthCodeURL(f.State, opts...), http.StatusFound)
}

// Errors of the callback; the user sees a generic page, the log the reason.
var (
	errFlowMissing  = errors.New("login attempt expired or unknown")
	errStateInvalid = errors.New("state mismatch")
	errNonceInvalid = errors.New("nonce mismatch")
)

func (s *Service) readFlow(r *http.Request) (flow, error) {
	c, err := r.Cookie(s.flowCookie(r))
	if err != nil || c.Value == "" {
		return flow{}, errFlowMissing
	}
	body, mac, ok := cutLast(c.Value, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(s.flowMAC(body))) {
		return flow{}, errFlowMissing // not issued by this server (or a different key)
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return flow{}, errFlowMissing
	}
	var f flow
	if err := json.Unmarshal(b, &f); err != nil || f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return flow{}, errFlowMissing
	}
	if s.o.Clock.Now().Unix() > f.Expires {
		return flow{}, errFlowMissing
	}
	return f, nil
}

func (s *Service) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base := httpx.BasePath(ctx)
	fail := func(status int, reason error, text string) {
		s.record(r, "anonymous", "auth.login.oidc", reason.Error(), nil)
		s.o.Pages.Message(w, r, status, MessageData{BasePath: base, Title: "Sign-in failed", Text: text, LocalLogin: s.o.LocalLogin})
	}
	f, err := s.readFlow(r)
	// The flow cookie is single use: clearing it first makes a replayed
	// callback URL (state + code) fail even if the IdP accepted the code twice.
	s.setFlowCookie(w, r, "", -1)
	if err != nil {
		fail(http.StatusBadRequest, err, "The sign-in attempt expired. Please start again.")
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 {
		fail(http.StatusBadRequest, errStateInvalid, "The sign-in attempt is invalid. Please start again.")
		return
	}
	if e := q.Get("error"); e != "" {
		fail(http.StatusForbidden, fmt.Errorf("idp error: %.64s", e), "The identity provider refused the sign-in.")
		return
	}
	s.mu.RLock()
	cfg, verifier := s.oauth, s.verifier
	s.mu.RUnlock()
	if cfg == nil {
		fail(http.StatusServiceUnavailable, errors.New("oidc not ready"), "The identity provider is not reachable yet.")
		return
	}
	octx := oidc.ClientContext(ctx, s.o.OIDC.HTTPClient)
	tok, err := cfg.Exchange(octx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		fail(http.StatusBadGateway, fmt.Errorf("code exchange: %w", err), "The identity provider did not confirm the sign-in.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(octx, raw)
	if err != nil {
		fail(http.StatusBadGateway, fmt.Errorf("id token: %w", err), "The identity provider sent an invalid token.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		fail(http.StatusBadRequest, errNonceInvalid, "The sign-in attempt is invalid. Please start again.")
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		fail(http.StatusBadGateway, fmt.Errorf("claims: %w", err), "The identity provider sent an invalid token.")
		return
	}
	if f.Purpose == flowStepUp {
		s.stepUpCallback(w, r, f, idt.Subject, claims)
		return
	}
	if reservedSubject(idt.Subject) {
		s.record(r, idt.Subject, "auth.login.oidc", "denied: reserved subject", nil)
		s.o.Pages.Message(w, r, http.StatusForbidden, reservedMessage(base))
		return
	}
	p := model.Principal{
		Subject: idt.Subject, Name: str(claims[s.o.OIDC.UsernameClaim]), Email: str(claims["email"]),
		Groups: groupsClaim(claims[s.o.OIDC.GroupsClaim]), Method: ModeOIDC,
	}
	if p.Name == "" {
		p.Name = str(claims["name"])
	}
	p.Role = s.o.Roles.Role(p.Groups)
	if p.Role == model.RoleNone {
		// No matching group: 403 and no session (spec §15.1).
		s.record(r, p.Subject, "auth.login.oidc", "denied: no matching group", map[string]string{"user": p.Name})
		s.o.Pages.Message(w, r, http.StatusForbidden, MessageData{
			BasePath: base, Title: "Access denied",
			Text: "You are signed in at the identity provider, but none of your groups grants access to TasmoFleet.",
		})
		return
	}
	if tok.RefreshToken == "" {
		s.noRefreshOnce.Do(func() {
			slog.InfoContext(ctx, "the IdP issued no refresh token (grant the offline_access scope): OIDC sessions are capped to the re-check interval so group changes still apply",
				"recheck", s.o.Recheck)
		})
	}
	if _, err := s.sessions.CreateOIDC(w, r, p, raw, tok.RefreshToken); err != nil {
		fail(http.StatusInternalServerError, err, "The session could not be created.")
		return
	}
	s.noteLogin(ctx, p)
	s.record(r, p.Subject, "auth.login.oidc", "ok", map[string]string{"user": p.Name, "role": roleName(p.Role)})
	s.redirectLocal(w, r, f.Next)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// groupsClaim accepts a list of names (Authentik) or a single string.
func groupsClaim(v any) []string {
	switch g := v.(type) {
	case string:
		return []string{g}
	case []any:
		out := make([]string, 0, len(g))
		for _, x := range g {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func roleName(r model.Role) string {
	switch r {
	case model.RoleAdmin:
		return "admin"
	case model.RoleOperator:
		return "operator"
	case model.RoleNone:
		return "none"
	}
	return "unknown"
}
