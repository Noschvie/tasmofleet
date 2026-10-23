package auth

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/oauth2"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// OIDC step-up: an OIDC admin proves presence again by authenticating at the
// IdP a second time (prompt=login, max_age=0). It is a flow of its own and
// never creates or replaces a session: the callback only grants a 5 minute
// step-up for the session that started it.
//
// prompt and max_age are requests the IdP may ignore, so the callback checks
// the result: same subject, same session, the auth_time claim present and not
// older than the moment the flow started (minus a small clock skew), and the
// groups still map to the admin role. An IdP that sends no auth_time makes the
// step-up fail closed.
const (
	flowStepUp   = "stepup"
	stepUpSkew   = time.Minute
	stepUpFailed = "Confirmation failed"
)

var (
	errStepUpSession  = errors.New("session changed during the step-up")
	errStepUpSubject  = errors.New("a different account authenticated")
	errNoAuthTime     = errors.New("the identity provider sent no auth_time claim")
	errStaleAuthTime  = errors.New("the authentication is older than the step-up request")
	errStepUpNotAdmin = errors.New("the account is no longer an admin")
)

// oidcStepUpStart is POST /oidc/stepup (form field next: local path to return to).
func (s *Service) oidcStepUpStart(w http.ResponseWriter, r *http.Request) {
	ci := s.resolve(r)
	if ci.err != nil || ci.p.Method != ModeOIDC || ci.p.Role < model.RoleAdmin || ci.sessionID == "" {
		subject := "anonymous"
		if ci.err == nil {
			subject = ci.p.Subject
		}
		s.record(r, subject, "account.stepup", "denied: oidc step-up needs an OIDC admin session", nil)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.beginFlow(w, r, flow{
		Next: httpx.LocalPath(r.PostFormValue("next")), Purpose: flowStepUp,
		Session: ci.sessionID, Subject: ci.p.Subject, Started: s.o.Clock.Now().Unix(),
	}, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
}

// stepUpCallback finishes a step-up flow; state, PKCE and nonce are verified
// by callback already.
func (s *Service) stepUpCallback(w http.ResponseWriter, r *http.Request, f flow, subject string, claims map[string]any) {
	ci := s.resolve(r)
	deny := func(status int, reason error, text string) {
		actor := f.Subject
		s.record(r, actor, "account.stepup", "denied: "+reason.Error(), map[string]string{"method": "oidc"})
		s.o.Pages.Message(w, r, status, MessageData{BasePath: httpx.BasePath(r.Context()), Title: stepUpFailed, Text: text})
	}
	now := s.o.Clock.Now()
	switch {
	case ci.err != nil || ci.sessionID == "" || ci.sessionID != f.Session || ci.p.Subject != f.Subject:
		deny(http.StatusForbidden, errStepUpSession, "Your session changed while you confirmed your identity. Please start again.")
		return
	case subject != f.Subject:
		deny(http.StatusForbidden, errStepUpSubject, "A different account signed in. Please start again with your own account.")
		return
	}
	at, ok := claims["auth_time"].(float64)
	switch {
	case !ok:
		deny(http.StatusForbidden, errNoAuthTime, "The identity provider did not say when you signed in (auth_time), so the confirmation cannot be trusted. Ask the administrator to enable it, or use the command line.")
		return
	case time.Unix(int64(at), 0).Before(time.Unix(f.Started, 0).Add(-stepUpSkew)) || time.Unix(int64(at), 0).After(now.Add(stepUpSkew)):
		deny(http.StatusForbidden, errStaleAuthTime, "The identity provider reused an earlier sign-in instead of asking again. Please try again.")
		return
	case s.o.Roles.Role(groupsClaim(claims[s.o.OIDC.GroupsClaim])) < model.RoleAdmin:
		deny(http.StatusForbidden, errStepUpNotAdmin, "Your account is no longer an administrator.")
		return
	}
	s.oidcSteps.grant(ci.sessionID, 0, now)
	s.record(r, ci.p.Subject, "account.stepup", "ok", map[string]string{"method": "oidc"})
	s.redirectLocal(w, r, f.Next)
}
