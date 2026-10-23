package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// recheck re-evaluates an OIDC session at the IdP with its refresh token
// (spec §15.1, "group revocation"). The refreshed ID token carries the
// current groups: a role change applies at once, losing every mapped group
// ends the session. Any failure to refresh ends the session too, because the
// alternative, trusting stale groups, is exactly what this check prevents.
// The only exception is a request whose own context ended: that says
// nothing about the IdP, and the session is kept.
//
// Rechecks run one at a time and re-read the session under the lock:
// refresh tokens may rotate, so two concurrent requests must not both spend
// the same token (the second would fail and wrongly end the session).
func (s *Service) recheck(ctx context.Context, sess store.Session) (store.Session, error) {
	s.recheckMu.Lock()
	defer s.recheckMu.Unlock()
	cur, err := s.o.Sessions.GetSession(ctx, sess.IDHash)
	if errors.Is(err, store.ErrNotFound) {
		return sess, ErrUnauthenticated
	}
	if err != nil {
		return sess, fmt.Errorf("auth: reload session: %w", err)
	}
	if s.o.Clock.Now().Sub(cur.RecheckedAt) < s.o.Recheck {
		return cur, nil // another request did it while we waited
	}
	s.mu.RLock()
	cfg, verifier := s.oauth, s.verifier
	s.mu.RUnlock()
	if cfg == nil {
		return cur, errors.New("auth: oidc discovery not finished, cannot re-check session")
	}
	end := func(reason string, err error) (store.Session, error) {
		if ctx.Err() != nil {
			return cur, ctx.Err() // the client went away, not the IdP's verdict
		}
		slog.InfoContext(ctx, "oidc session ended by re-check", "user", cur.Principal.Name, "reason", reason, "err", err)
		s.o.Audit.Record(ctx, model.AuditEvent{At: s.o.Clock.Now(), Actor: cur.Principal.Subject,
			Action: "auth.session.recheck", Result: "denied: " + reason})
		return cur, ErrUnauthenticated
	}
	octx := oidc.ClientContext(ctx, s.o.OIDC.HTTPClient)
	tok, err := cfg.TokenSource(octx, &oauth2.Token{RefreshToken: cur.RefreshToken}).Token()
	if err != nil {
		return end("refresh failed", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return end("no id_token in refresh response", nil)
	}
	idt, err := verifier.Verify(octx, raw)
	if err != nil {
		return end("invalid id_token", err)
	}
	if idt.Subject != cur.Principal.Subject {
		return end("subject changed", nil)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return end("invalid claims", err)
	}
	p := cur.Principal
	p.Groups = groupsClaim(claims[s.o.OIDC.GroupsClaim])
	if n := str(claims[s.o.OIDC.UsernameClaim]); n != "" {
		p.Name = n
	}
	if e := str(claims["email"]); e != "" {
		p.Email = e
	}
	p.Role = s.o.Roles.Role(p.Groups)
	if p.Role == model.RoleNone {
		return end("no matching group", nil)
	}
	cur.Principal, cur.IDToken, cur.RecheckedAt = p, raw, s.o.Clock.Now()
	if tok.RefreshToken != "" {
		cur.RefreshToken = tok.RefreshToken // rotated
	}
	if err := s.o.Sessions.UpdateSessionRecheck(ctx, cur); err != nil {
		return cur, fmt.Errorf("auth: store re-check: %w", err)
	}
	if p.Role != sess.Principal.Role {
		s.o.Audit.Record(ctx, model.AuditEvent{At: cur.RecheckedAt, Actor: p.Subject, Action: "auth.session.recheck",
			Result: "role changed", Detail: `{"role":"` + roleName(p.Role) + `"}`})
	}
	return cur, nil
}
