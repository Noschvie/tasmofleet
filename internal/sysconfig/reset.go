package sysconfig

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// ResetRequest is "Reset settings…" on the Advanced tab (design v2, N9).
type ResetRequest struct {
	Actor  string
	Via    string
	StepUp StepUp
	Audit  audit.Recorder
}

// Reset puts every setting of the page back to its default by removing its
// stored row. It always needs a confirmation (step-up), whatever the fields
// are: it changes the sign-in settings, the broker and the device networks in
// one go. Only rows of registered fields go; the internal rows that share the
// table stay: the setup marker (the first-run setup must not start again), the
// policy live check pause (a pause survives until someone resumes it) and the
// marker of the one-time environment import (the environment must not be
// imported again). Fields the environment locks are not stored and stay as
// they are. The change is audited as sysconfig.reset with every field that
// changed; subscribers apply the defaults live like after a save.
func (s *Service) Reset(ctx context.Context, req ResetRequest) (Result, error) {
	r := Request{Actor: req.Actor, Via: req.Via, StepUp: req.StepUp, Audit: req.Audit, reset: true}
	if !req.StepUp.Done {
		s.deny(ctx, r, "confirmation required", nil)
		return Result{}, &StepUpRequiredError{Possible: req.StepUp.Possible}
	}
	s.mu.Lock()
	for k := range s.stored {
		if _, field := Lookup(k); field && !s.cur.IsLocked(k) {
			r.Clear = append(r.Clear, k)
		}
	}
	s.mu.Unlock()
	sort.Strings(r.Clear)
	if len(r.Clear) == 0 { // nothing stored: still on record that someone reset
		d, _ := json.Marshal(withVia(map[string]any{"changes": []any{}}, r.Via))
		s.rec(r.Audit).Record(ctx, model.AuditEvent{Actor: r.Actor, Action: r.action(), Target: "settings:system", Detail: string(d), Result: "ok"})
		return Result{}, nil
	}
	return s.Apply(ctx, r)
}
