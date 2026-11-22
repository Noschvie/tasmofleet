package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/a-h/templ"

	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// DeviceNames records the admin's "Keep" against a device's DeviceName (store.SQLite). Optional:
// without it the notice has no Keep button.
type DeviceNames interface {
	// DismissDeviceName remembers that deviceName was declined; store.ErrConflict when the device
	// reports another name now, store.ErrNotFound for an unknown device.
	DismissDeviceName(ctx context.Context, id model.DeviceID, deviceName string) error
}

// BuildMemory keeps the chip build a person explicitly chose for a device (store.SQLite, #186).
// Optional: without it the preview remembers nothing.
type BuildMemory interface {
	// SetBuildMemory remembers build ("s2" or "s2cdc"); "" forgets it.
	SetBuildMemory(ctx context.Context, id model.DeviceID, build string) error
}

// tasmotaDefaultName is the DeviceName of a Tasmota nobody named; offering it would nag on every
// such device (the prober treats it the same way).
const tasmotaDefaultName = "Tasmota"

// nameOffer is the DeviceName the Identity card offers instead of the stored name (#125), "" when
// there is nothing to decide: the device reports none, the names agree, it is Tasmota's default,
// or the admin already answered Keep to exactly this name. A stored name that is still the
// device's own follows it on refresh and never gets here.
func nameOffer(d model.Device) string {
	n := d.DeviceName
	if n == "" || n == d.Name || n == tasmotaDefaultName || n == d.KeptDeviceName {
		return ""
	}
	return n
}

// nameVals is the hx-vals of an answer: the DeviceName the admin saw, so a newer one is never
// answered unseen.
func nameVals(name string) string {
	b, _ := json.Marshal(map[string]string{"name": name}) // a map of strings cannot fail
	return string(b)
}

// useNameAction is the offer banner's one action, "Use device name": an htmx post that swaps the Identity card, which
// then shows the stored name and no offer. Only an admin answers (nil for the others: the banner says the name, no more).
func (c identCard) useNameAction() *ui.Btn {
	if !c.CanAnswer {
		return nil
	}
	return &ui.Btn{Label: "Use device name", Attrs: c.answerAttrs("/name/adopt")}
}

// keepNameAction is "Keep", the other answer. It is no second action of the banner (the design system allows one): it
// sits in the Device name row of the card, beside the stored name it keeps, while the offer stands, and only where Keep
// can be saved (KeepOK). nil when there is nothing to answer or the viewer cannot.
func (c identCard) keepNameAction() *ui.Btn {
	if c.NameOffer == "" || !c.CanAnswer || !c.KeepOK {
		return nil
	}
	return &ui.Btn{Label: "Keep", Kind: "link", Attrs: c.answerAttrs("/name/keep")}
}

func (c identCard) answerAttrs(path string) templ.Attributes {
	return templ.Attributes{"hx-post": c.Base + devPath(c.ID, path), "hx-vals": nameVals(c.NameOffer), "hx-target": "#identity", "hx-swap": "outerHTML", "hx-disabled-elt": "this"}
}

const nameStale = "The device reports another name now. Check it below."

// deviceNameAdopt is POST /devices/{id}/name/adopt: "Use device name". The stored name becomes the
// DeviceName the page showed, through the inventory (validation, audit, events like any rename).
func (h *handler) deviceNameAdopt(w http.ResponseWriter, r *http.Request) {
	h.nameAnswer(w, r, func(d model.Device, seen string) string {
		if d.DeviceName != seen || nameOffer(d) == "" {
			return nameStale
		}
		cas, ok := h.d.Inventory.(interface {
			UpdateIf(ctx context.Context, actor string, expected, next model.Device) error
		})
		if !ok {
			h.log.ErrorContext(r.Context(), "adopt device name: the inventory has no compare-and-set update")
			return "The name could not be changed. The server log has the details."
		}
		next := d
		next.Name = seen
		// Written only if the device is still the one the page showed: an edit made meanwhile is not lost.
		switch err := cas.UpdateIf(r.Context(), actor(r), d, next); {
		case errors.Is(err, inventory.ErrConflict):
			return "The device changed meanwhile. Check it below."
		case err != nil:
			h.log.WarnContext(r.Context(), "adopt device name", "device", d.ID, "err", err)
			return "The name could not be changed. The server log has the details."
		}
		return ""
	})
}

// deviceNameKeep is POST /devices/{id}/name/keep: "Keep". The notice stays away until the device
// reports another DeviceName.
func (h *handler) deviceNameKeep(w http.ResponseWriter, r *http.Request) {
	h.nameAnswer(w, r, func(d model.Device, seen string) string {
		switch err := h.d.DeviceNames.DismissDeviceName(r.Context(), d.ID, seen); {
		case errors.Is(err, store.ErrConflict):
			return nameStale
		case err != nil:
			h.log.WarnContext(r.Context(), "keep device name", "device", d.ID, "err", err)
			return "The answer could not be saved. The server log has the details."
		}
		h.recordFirmware(r, "device.name_kept", "device:"+strconv.FormatInt(int64(d.ID), 10),
			map[string]any{"device_name": seen, "name": d.Name}, "ok")
		return ""
	})
}

// nameAnswer runs one answer and swaps the Identity card (and, when the name changed, the header
// with it, as after "Refresh identity").
func (h *handler) nameAnswer(w http.ResponseWriter, r *http.Request, answer func(d model.Device, seen string) string) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	failure := answer(d, r.PostFormValue("name"))
	if fresh, err := h.d.Inventory.Get(r.Context(), d.ID); err == nil {
		d = fresh
	} else {
		h.log.WarnContext(r.Context(), "name answer: reload device", "err", err)
	}
	v := h.view(r, "", "", nil)
	hl, running := h.deviceState(r, d)
	pf := h.pageFactsOf(r.Context(), d)
	tabs := h.headWith(v, d, hl, running, "overview", pf.Facts, pf.Paths, pf.PathsOK)
	c := h.identityWithHint(v, d, running != nil, tabs.offline(), h.factsOf(r.Context(), d), pf.Conflicts)
	c.Why = tabs.readsAPIOff()
	c.Error = failure
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	parts := []templ.Component{identityCard(c)}
	if failure == "" {
		parts = append(parts, deviceHeadSwap(tabs, trailOf(v, "device", deviceCrumbs(d)...)))
	}
	if err := templ.Join(parts...).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "name answer: render", "err", err)
	}
}
