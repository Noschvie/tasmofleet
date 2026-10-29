package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// List operations of the policy page and the rule page: reorder (the order is the precedence),
// duplicate (a disabled copy below) and enable/disable. A rule is named by its id (never by its
// position). Each is a plain form POST that names the version the person saw; on success it
// redirects back, a stale version answers 409 like the other rule changes.

func (h *handler) registerPolicyRuleOps(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	route("POST "+rulesPath+"/order", adm, h.rulesOrder)
	route("POST "+rulesPath+"/duplicate", adm, h.rulesDuplicate)
	route("POST "+rulesPath+"/enable", adm, func(w http.ResponseWriter, r *http.Request) { h.rulesSetEnabled(w, r, true) })
	route("POST "+rulesPath+"/disable", adm, func(w http.ResponseWriter, r *http.Request) { h.rulesSetEnabled(w, r, false) })
}

// opForm reads the version and the rule id of a list operation and finds the rule's position in
// the stored list. A stale version answers 409 before an unknown id answers 404.
func (h *handler) opForm(w http.ResponseWriter, r *http.Request, single []string) (f url.Values, ver int64, idx int, ok bool) {
	f, ok = strictForm(w, r, single, nil)
	if !ok {
		return nil, 0, 0, false
	}
	ver, err := strconv.ParseInt(f.Get("version"), 10, 64)
	if err != nil || ver < 0 || !versionRe.MatchString(f.Get("version")) || !policy.RuleIDRe.MatchString(f.Get("id")) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, 0, 0, false
	}
	st, err := h.d.PolicyRules.State(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return nil, 0, 0, false
	}
	if ver != st.VersionID {
		h.rulesStale(w, r)
		return nil, 0, 0, false
	}
	if idx = indexOfRule(st.Specs, f.Get("id")); idx < 0 {
		h.notFound(w, r)
		return nil, 0, 0, false
	}
	return f, ver, idx, true
}

// rulesOpBack answers a list operation. On success: back=rule returns to the rule page of rule
// id (the rule page's own buttons), anything else to the policy page with the confirmation.
func (h *handler) rulesOpBack(w http.ResponseWriter, r *http.Request, err error, ok, id, back string) {
	switch {
	case err == nil && back == "rule" && id != "":
		redirect(w, r, rulesPath+"/"+id+"?ok="+ok)
	case err == nil:
		redirect(w, r, policyPath+"?ok="+ok)
	case errors.Is(err, sysconfig.ErrStale):
		h.rulesStale(w, r)
	case errors.Is(err, sysconfig.ErrNoSuchRule):
		h.notFound(w, r)
	default:
		h.rulesFail(w, r, err)
	}
}

// rulesOrder stores a new order. Two forms: "order" is the full list of old positions in their
// new order, comma separated (drag and drop); "id" with "move" = up|down moves one rule by one
// place (the menu entries without JS).
func (h *handler) rulesOrder(w http.ResponseWriter, r *http.Request) {
	f, ok := strictForm(w, r, []string{"csrf", "version", "order", "id", "move"}, nil)
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
	case f.Get("order") != "" && f.Get("move") == "" && f.Get("id") == "":
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
	case f.Get("order") == "" && (f.Get("move") == "up" || f.Get("move") == "down") && policy.RuleIDRe.MatchString(f.Get("id")):
		st, err := h.d.PolicyRules.State(r.Context())
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if ver != st.VersionID {
			h.rulesStale(w, r)
			return
		}
		if order, ok = moveOrder(len(st.Specs), indexOfRule(st.Specs, f.Get("id")), f.Get("move") == "up"); !ok {
			h.notFound(w, r)
			return
		}
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err = h.d.PolicyRules.Reorder(r.Context(), actor(r), ver, order, h.d.Recorder)
	h.rulesOpBack(w, r, err, "saved", "", "")
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

// rulesDuplicate adds a disabled copy below the rule and opens the copy's rule page.
func (h *handler) rulesDuplicate(w http.ResponseWriter, r *http.Request) {
	_, ver, idx, ok := h.opForm(w, r, []string{"csrf", "version", "id"})
	if !ok {
		return
	}
	_, at, err := h.d.PolicyRules.Duplicate(r.Context(), actor(r), ver, idx, h.d.Recorder)
	if err != nil {
		h.rulesOpBack(w, r, err, "duplicated", "", "")
		return
	}
	st, serr := h.d.PolicyRules.State(r.Context())
	if serr != nil || at < 0 || at >= len(st.Specs) {
		redirect(w, r, policyPath+"?ok=duplicated")
		return
	}
	redirect(w, r, rulesPath+"/"+st.Specs[at].ID+"?ok=duplicated")
}

func (h *handler) rulesSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	f, ver, idx, ok := h.opForm(w, r, []string{"csrf", "version", "id", "back"})
	if !ok {
		return
	}
	if b := f.Get("back"); b != "" && b != "rule" && b != "list" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err := h.d.PolicyRules.SetEnabled(r.Context(), actor(r), ver, idx, enabled, h.d.Recorder)
	done := "disabled"
	if enabled {
		done = "enabled"
	}
	h.rulesOpBack(w, r, err, done, f.Get("id"), f.Get("back"))
}
