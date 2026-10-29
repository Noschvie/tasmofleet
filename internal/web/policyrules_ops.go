package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// List operations of the merged policy page (design v2, N3): reorder (the
// order is the precedence), duplicate (a disabled copy below), enable/disable,
// and adding a rule from a template. Each is a plain form POST that names the
// version the person saw; on success it redirects back to the rules, a stale
// version answers 409 like the other rule changes.

func (h *handler) registerPolicyRuleOps(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	route("POST "+rulesPath+"/order", adm, h.rulesOrder)
	route("POST "+rulesPath+"/duplicate", adm, h.rulesDuplicate)
	route("POST "+rulesPath+"/enable", adm, func(w http.ResponseWriter, r *http.Request) { h.rulesSetEnabled(w, r, true) })
	route("POST "+rulesPath+"/disable", adm, func(w http.ResponseWriter, r *http.Request) { h.rulesSetEnabled(w, r, false) })
	route("POST "+rulesPath+"/template", adm, h.rulesFromTemplate)
}

// opForm reads version and, when withIdx, idx of a list operation.
func opForm(w http.ResponseWriter, r *http.Request, single []string, withIdx bool) (f url.Values, ver int64, idx int, ok bool) {
	f, ok = strictForm(w, r, single, nil)
	if !ok {
		return nil, 0, 0, false
	}
	ver, err := strconv.ParseInt(f.Get("version"), 10, 64)
	bad := err != nil || ver < 0 || !versionRe.MatchString(f.Get("version"))
	if withIdx {
		var e2 error
		idx, e2 = strconv.Atoi(f.Get("idx"))
		bad = bad || e2 != nil || idx < 0 || !idxRe.MatchString(f.Get("idx"))
	}
	if bad {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, 0, 0, false
	}
	return f, ver, idx, true
}

// rulesOpDone answers a list operation: back to the policy page with its confirmation, the
// rule at edit (>= 0) open in the editor.
func (h *handler) rulesOpDone(w http.ResponseWriter, r *http.Request, err error, ok string, edit int) {
	h.rulesOpBack(w, r, err, ok, edit, "")
}

// rulesOpBack is rulesOpDone for a form that may say where to return: back=rule returns to the
// rule page of the rule at edit (the rule page's own buttons), anything else to the policy page.
func (h *handler) rulesOpBack(w http.ResponseWriter, r *http.Request, err error, ok string, edit int, back string) {
	switch {
	case err == nil && back == "rule" && edit >= 0:
		st, serr := h.d.PolicyRules.State(r.Context())
		if serr != nil || edit >= len(st.Specs) {
			redirect(w, r, policyPath+"?ok="+ok)
			return
		}
		redirect(w, r, rulesPath+"/"+st.Specs[edit].ID+"?ok="+ok)
	case err == nil:
		to := policyPath + "?ok=" + ok
		if edit >= 0 {
			to += "&edit=" + strconv.Itoa(edit)
		}
		redirect(w, r, to)
	case errors.Is(err, sysconfig.ErrStale):
		h.rulesStale(w, r)
	case errors.Is(err, sysconfig.ErrNoSuchRule):
		h.notFound(w, r)
	default:
		h.rulesFail(w, r, err)
	}
}

// rulesOrder stores a new order. Two forms: "order" is the full list of old
// positions in their new order, comma separated (drag and drop); "idx" with
// "move" = up|down moves one rule by one place (the buttons without JS).
func (h *handler) rulesOrder(w http.ResponseWriter, r *http.Request) {
	f, ok := strictForm(w, r, []string{"csrf", "version", "order", "idx", "move"}, nil)
	if !ok {
		return
	}
	ver, err := strconv.ParseInt(f.Get("version"), 10, 64)
	if err != nil || !versionRe.MatchString(f.Get("version")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var order []int
	switch {
	case f.Get("order") != "" && f.Get("move") == "":
		parts := strings.Split(f.Get("order"), ",")
		if len(parts) > 1000 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			order = append(order, n)
		}
	case f.Get("order") == "" && (f.Get("move") == "up" || f.Get("move") == "down"):
		idx, err := strconv.Atoi(f.Get("idx"))
		if err != nil || !idxRe.MatchString(f.Get("idx")) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		st, err := h.d.PolicyRules.State(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if ver != st.VersionID {
			h.rulesStale(w, r)
			return
		}
		if order, ok = moveOrder(len(st.Specs), idx, f.Get("move") == "up"); !ok {
			h.notFound(w, r)
			return
		}
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err = h.d.PolicyRules.Reorder(r.Context(), actor(r), ver, order, h.d.Recorder)
	h.rulesOpDone(w, r, err, "saved", -1)
}

// moveOrder is the permutation that moves the rule at idx one place up or
// down in a list of n. Moving the first rule up or the last one down keeps the
// order (the service then answers "nothing changed").
func moveOrder(n, idx int, up bool) ([]int, bool) {
	if idx < 0 || idx >= n {
		return nil, false
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	to := idx + 1
	if up {
		to = idx - 1
	}
	if to >= 0 && to < n {
		order[idx], order[to] = order[to], order[idx]
	}
	return order, true
}

func (h *handler) rulesDuplicate(w http.ResponseWriter, r *http.Request) {
	f, ver, idx, ok := opForm(w, r, []string{"csrf", "version", "idx", "back"}, true)
	if !ok {
		return
	}
	_, at, err := h.d.PolicyRules.Duplicate(r.Context(), actor(r), ver, idx, h.d.Recorder)
	h.rulesOpBack(w, r, err, "duplicated", at, f.Get("back"))
}

func (h *handler) rulesSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	f, ver, idx, ok := opForm(w, r, []string{"csrf", "version", "idx", "back"}, true)
	if !ok {
		return
	}
	_, err := h.d.PolicyRules.SetEnabled(r.Context(), actor(r), ver, idx, enabled, h.d.Recorder)
	done := "disabled"
	if enabled {
		done = "enabled"
	}
	h.rulesOpBack(w, r, err, done, idx, f.Get("back"))
}

// rulesFromTemplate appends the template named by "template" to the rules.
func (h *handler) rulesFromTemplate(w http.ResponseWriter, r *http.Request) {
	f, ver, _, ok := opForm(w, r, []string{"csrf", "version", "template"}, false)
	if !ok {
		return
	}
	tpls := policyTemplates(h.brokerHost())
	i := slices.IndexFunc(tpls, func(t ruleTemplate) bool { return t.ID == f.Get("template") })
	if i < 0 {
		h.notFound(w, r)
		return
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ver != st.VersionID {
		h.rulesStale(w, r)
		return
	}
	list := append(slices.Clone(st.Specs), tpls[i].Spec)
	_, err = h.d.PolicyRules.Save(r.Context(), actor(r), "from template: "+tpls[i].Title, list, h.d.Recorder)
	h.rulesOpDone(w, r, err, "template", len(list)-1)
}
