package web

import (
	"context"
	"net/http"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// PolicyLivePause is the pause of the automatic policy live check (design v2,
// N21, sysconfig.LivePause). Optional: without it there is no Pause / Resume.
type PolicyLivePause interface {
	State() sysconfig.LivePauseState
	Set(ctx context.Context, actor string, paused bool, rec audit.Recorder) (sysconfig.LivePauseState, bool, error)
}

// registerPolicyLivePause adds POST /policy/live/pause and /policy/live/resume
// (admin, CSRF like every POST). They answer with a redirect to the policy
// page; "Run now" (POST /policy/live) stays available while paused, it is an
// on-demand check independent of the background reads.
func (h *handler) registerPolicyLivePause(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Policy == nil || h.d.PolicyLivePause == nil {
		return
	}
	route("POST /policy/live/pause", adm, func(w http.ResponseWriter, r *http.Request) { h.policyLiveSet(w, r, true) })
	route("POST /policy/live/resume", adm, func(w http.ResponseWriter, r *http.Request) { h.policyLiveSet(w, r, false) })
}

func (h *handler) policyLiveSet(w http.ResponseWriter, r *http.Request, paused bool) {
	if _, ok := strictForm(w, r, []string{"csrf"}, nil); !ok {
		return
	}
	if _, _, err := h.d.PolicyLivePause.Set(r.Context(), actor(r), paused, h.d.Recorder); err != nil {
		h.fail(w, r, err)
		return
	}
	ok := "resumed"
	if paused {
		ok = "paused"
	}
	redirect(w, r, "/policy?ok="+ok)
}
