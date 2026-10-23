package web

import "net/http"

// styleguide is /styleguide: the prototype's "UI states" screen and the gallery of every
// component (design v2, WP F4). It shows sample data only, so every signed-in user may open it
// (D17). It holds the event stream like the pages it documents, so the top bar's live indicator
// is the real one.
func (h *handler) styleguide(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Styleguide", "styleguide", nil)
	v.Live = true
	h.show(w, r, http.StatusOK, "styleguide", v, styleguidePage(sgView{Base: v.Base, CSRF: v.CSRF, Shell: shellData(v)}))
}
