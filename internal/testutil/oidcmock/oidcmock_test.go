package oidcmock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const redirect = "http://127.0.0.1:1/oidc/callback"

func start(t *testing.T) *Server {
	t.Helper()
	s, err := Start(Options{RedirectURIs: []string{redirect}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

type result struct {
	StatusCode int
	Header     http.Header
}

// get does a GET without following redirects.
func get(t *testing.T, c *http.Client, u string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirect(c).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return result{resp.StatusCode, resp.Header}
}

func noRedirect(c *http.Client) *http.Client {
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

// authorize runs the authorization request and returns the callback query.
func authorize(t *testing.T, s *Server, cfg oauth2.Config, opts ...oauth2.AuthCodeOption) url.Values {
	t.Helper()
	resp := get(t, s.Client(), cfg.AuthCodeURL("st", opts...))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func setup(t *testing.T) (*Server, *oidc.Provider, oauth2.Config, context.Context) {
	s := start(t)
	ctx := oidc.ClientContext(t.Context(), s.Client())
	p, err := oidc.NewProvider(ctx, s.Issuer())
	if err != nil {
		t.Fatalf("discovery must work with go-oidc like against Authentik: %v", err)
	}
	cfg := oauth2.Config{ClientID: "tasmofleet", ClientSecret: "mock-secret", Endpoint: p.Endpoint(),
		RedirectURL: redirect, Scopes: []string{oidc.ScopeOpenID, "profile"}}
	return s, p, cfg, ctx
}

// The relying party's tests rely on the mock behaving like Authentik: a valid
// code flow yields a verifiable RS256 token with the groups list.
func TestCodeFlowIssuesVerifiableTokenWithGroups(t *testing.T) {
	s, p, cfg, ctx := setup(t)
	if !strings.HasSuffix(s.Issuer(), "/application/o/tasmofleet/") {
		t.Fatalf("issuer %q does not follow Authentik's per-application path", s.Issuer())
	}
	s.NextLogin(User{Subject: "u1", Username: "alice", Groups: []string{"admins", "x"}})
	v := oauth2.GenerateVerifier()
	q := authorize(t, s, cfg, oauth2.S256ChallengeOption(v), oidc.Nonce("n1"))
	if q.Get("state") != "st" {
		t.Fatalf("state not echoed: %v", q)
	}
	tok, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(v))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := p.Verifier(&oidc.Config{ClientID: "tasmofleet"}).Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Nonce    string   `json:"nonce"`
		Username string   `json:"preferred_username"`
		Groups   []string `json:"groups"`
	}
	if err := idt.Claims(&c); err != nil {
		t.Fatal(err)
	}
	if c.Nonce != "n1" || c.Username != "alice" || len(c.Groups) != 2 {
		t.Fatalf("claims %+v", c)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil || meta.EndSession == "" {
		t.Fatalf("end_session_endpoint missing (RP-initiated logout needs it): %v", err)
	}

	// Refresh tokens work like Authentik's.
	if _, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: tok.RefreshToken}).Token(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// A code is single use: replaying it must fail.
	if _, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(v)); err == nil {
		t.Fatal("authorization code accepted twice")
	}
}

// PKCE S256 is enforced so the relying party's PKCE handling is actually tested.
func TestPKCEEnforced(t *testing.T) {
	s, _, cfg, ctx := setup(t)

	s.NextLogin(User{Subject: "u1"})
	if q := authorize(t, s, cfg); q.Get("error") != "invalid_request" {
		t.Fatalf("authorization without code_challenge accepted: %v", q)
	}

	s.NextLogin(User{Subject: "u1"})
	q := authorize(t, s, cfg, oauth2.S256ChallengeOption(oauth2.GenerateVerifier()))
	if _, err := cfg.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(oauth2.GenerateVerifier())); err == nil {
		t.Fatal("token issued for a wrong code_verifier")
	}
}

// An unregistered redirect_uri must never be redirected to.
func TestUnregisteredRedirectRefused(t *testing.T) {
	s, _, cfg, _ := setup(t)
	cfg.RedirectURL = "https://evil.example/cb"
	resp := get(t, s.Client(), cfg.AuthCodeURL("st", oauth2.S256ChallengeOption("x")))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestEndSessionRecordedAndRedirects(t *testing.T) {
	s := start(t)
	resp := get(t, s.Client(), s.Issuer()+"end-session/?id_token_hint=abc&post_logout_redirect_uri="+url.QueryEscape(redirect))
	if resp.Header.Get("Location") != redirect {
		t.Fatalf("location %q", resp.Header.Get("Location"))
	}
	calls := s.EndSessionCalls()
	if len(calls) != 1 || calls[0].Get("id_token_hint") != "abc" {
		b, _ := json.Marshal(calls)
		t.Fatalf("calls %s", b)
	}
}
