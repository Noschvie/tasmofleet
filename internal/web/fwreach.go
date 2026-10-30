package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// FWReachForgetter resets what is known about a device reaching the firmware server to
// "unknown" and audits it as actor (device.fwreach).
type FWReachForgetter interface {
	Forget(ctx context.Context, actor string, id model.DeviceID) error
}

// fwReachForget is POST /devices/{id}/fwreach/forget: an administrator drops a remembered "no"
// (for example after fixing a firewall) so the next update tries the download path again.
func (h *handler) fwReachForget(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	if err := h.d.FWReach.Forget(r.Context(), actor(r), d.ID); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/devices/"+strconv.FormatInt(int64(d.ID), 10))
}
