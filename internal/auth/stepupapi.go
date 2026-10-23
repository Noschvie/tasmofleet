package auth

import "net/http"

// SteppedUp reports whether the session of r confirmed its identity within the
// last 5 minutes: local accounts by typing the password again (StepUp), OIDC
// sessions by authenticating at the IdP again (POST /oidc/stepup). The two
// are kept apart, so an OIDC re-authentication never counts where the local
// password is meant (/users). Header principals have no session and are
// never stepped up; features decide for themselves whether they accept that.
func (s *Service) SteppedUp(r *http.Request) bool {
	ci := s.resolve(r)
	if ci.err != nil || ci.sessionID == "" {
		return false
	}
	switch ci.p.Method {
	case ModeLocal:
		return s.steps.valid(ci.sessionID, ci.passwordGen, s.o.Clock.Now())
	case ModeOIDC:
		return s.oidcSteps.valid(ci.sessionID, 0, s.o.Clock.Now())
	}
	return false
}

// SessionKey identifies the login session of r (the stored session id hash,
// not the cookie value); empty for header principals.
func (s *Service) SessionKey(r *http.Request) string {
	ci := s.resolve(r)
	if ci.err != nil {
		return ""
	}
	return ci.sessionID
}

// StepUp checks the current password of the local user behind r (same rate
// limit and audit as the account pages) and, if it is right, starts a step-up
// for the session. limited reports a rate-limit refusal. Other principals
// always get (false, false); OIDC admins use POST /oidc/stepup.
func (s *Service) StepUp(r *http.Request, password string) (ok, limited bool) {
	ci := s.resolve(r)
	if ci.err != nil || ci.sessionID == "" || ci.p.Method != ModeLocal {
		return false, false
	}
	ok, limited = s.verifyCurrent(r, ci.p.Name, password)
	switch {
	case limited:
		s.record(r, ci.p.Subject, "account.stepup", "denied: rate limited", nil)
	case !ok:
		s.record(r, ci.p.Subject, "account.stepup", "denied: wrong password", nil)
	default:
		s.steps.grant(ci.sessionID, ci.passwordGen, s.o.Clock.Now())
		s.record(r, ci.p.Subject, "account.stepup", "ok", nil)
	}
	return ok, limited
}
