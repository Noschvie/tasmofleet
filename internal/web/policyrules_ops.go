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

// ruleTemplate is one entry of the empty state's "Start from a template".
type ruleTemplate struct {
	ID    string
	Title string // shown on the card and used as the rule name
	Text  string // one-line description
	Spec  sysconfig.RuleSpec
}

// policyTemplates are the four templates of the prototype. They are added as
// enabled Advisory rules for the whole fleet (the prototype's choice): a hint
// first, the admin makes them Required. The broker template needs the
// configured broker host and is left out without one.
func policyTemplates(brokerHost string) []ruleTemplate {
	t := []ruleTemplate{
		{ID: "telemetry", Title: "Telemetry every 5 minutes", Text: "Devices publish their state every 5 minutes.",
			Spec: sysconfig.RuleSpec{Command: "TelePeriod", Want: "300"}},
		{ID: "poweron", Title: "Restore power after an outage", Text: "Relays come back the way they were.",
			Spec: sysconfig.RuleSpec{Command: "PowerOnState", Want: "3"}},
	}
	if brokerHost != "" {
		t = append(t, ruleTemplate{ID: "broker", Title: "Use one MQTT broker", Text: "Every device talks to the same broker.",
			Spec: sysconfig.RuleSpec{Command: "MqttHost", Want: brokerHost}})
	}
	t = append(t, ruleTemplate{ID: "timezone", Title: "Local timezone", Text: "Daylight-saving rules from the device location (Timezone 99).",
		Spec: sysconfig.RuleSpec{Command: "Timezone", Want: "99"}})
	for i := range t {
		t[i].Spec.Name, t[i].Spec.Severity = t[i].Title, "info"
	}
	return t
}

// brokerHost is the host of the configured MQTT broker, "" without one.
func (h *handler) brokerHost() string {
	if h.d.SysConfig == nil {
		return ""
	}
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "MQTT_URL" && f.Value != "" {
			if u, err := url.Parse(f.Value); err == nil {
				return u.Hostname()
			}
		}
	}
	return ""
}

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
	switch {
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
	_, ver, idx, ok := opForm(w, r, []string{"csrf", "version", "idx"}, true)
	if !ok {
		return
	}
	_, at, err := h.d.PolicyRules.Duplicate(r.Context(), actor(r), ver, idx, h.d.Recorder)
	h.rulesOpDone(w, r, err, "duplicated", at)
}

func (h *handler) rulesSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	_, ver, idx, ok := opForm(w, r, []string{"csrf", "version", "idx"}, true)
	if !ok {
		return
	}
	_, err := h.d.PolicyRules.SetEnabled(r.Context(), actor(r), ver, idx, enabled, h.d.Recorder)
	done := "disabled"
	if enabled {
		done = "enabled"
	}
	h.rulesOpDone(w, r, err, done, idx)
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
