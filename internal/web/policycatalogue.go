package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The command field of the rule editor learns from the Tasmota command catalogues (#269): what to
// suggest, what an index may be, what kind of value the command takes, whether the Tasmota
// versions of the fleet know the command, and a short help with a link to the docs. Nothing here
// is a table of its own; the data is internal/tasmotacmd's.

// suggestLimit is how many commands the suggestion list shows.
const suggestLimit = 8

// fleetDevices is the inventory as the catalogue check sees it.
func (h *handler) fleetDevices(ctx context.Context) []tasmotacmd.FleetDevice {
	devs, err := h.d.Inventory.List(ctx)
	if err != nil {
		return nil // no inventory, no versions: the check says nothing
	}
	out := make([]tasmotacmd.FleetDevice, 0, len(devs))
	for _, d := range devs {
		out = append(out, tasmotacmd.FleetDevice{
			Name: d.Name, Version: d.Identity.Version,
			Env: tasmotacmd.Env{Chip: string(d.Identity.Family), Variant: d.Identity.Variant},
		})
	}
	return out
}

// commandKeep is the suggestion filter: only commands a rule may name.
func commandKeep(name string) bool {
	if strings.EqualFold(name, "SetOption") {
		return true // SetOption<n>, the number is typed in its own field
	}
	if f, ok := settings.Lookup(name); ok {
		return !f.Secret
	}
	return commandRefusal(name) == ""
}

// checkRow is one firmware version of the fleet in the check banner.
type checkRow struct {
	Version string
	Devices string
	Result  string
	OK      bool
}

// checkView is the banner under the command field.
type checkView struct {
	Tone  ui.Tone
	Title string
	Rows  []checkRow
	Foot  string
}

// commandView is what the command field shows around the typed text.
type commandView struct {
	Command string
	// Indexed: the field is a name with a number box (SetOption [19]).
	Indexed   bool
	Base      string
	Index     string
	Lo, Hi    int
	Desc      string
	DocsURL   string
	Check     *checkView
	HasFleet  bool // some device reports a firmware version (suggestions come from those)
	FleetVers []string
}

// commandViewOf reads the catalogues for the typed command.
func (h *handler) commandViewOf(ctx context.Context, command string) commandView {
	cv := commandView{Command: command}
	command = strings.TrimSpace(command)
	if command == "" {
		return cv
	}
	set, err := tasmotacmd.Pinned()
	if err != nil {
		return cv
	}
	fleet := h.fleetDevices(ctx)
	res := set.Check(command, fleet)
	cv.Desc = tasmotacmd.Describe(command)
	cv.DocsURL, _ = tasmotacmd.DocsURL(command)
	if res.Name != "" && res.Range.OK {
		rest := ""
		if len(command) >= len(res.Name) && strings.EqualFold(command[:len(res.Name)], res.Name) {
			rest = command[len(res.Name):]
		}
		if _, err := strconv.Atoi(rest); err == nil || rest == "" {
			cv.Indexed, cv.Base, cv.Index, cv.Lo, cv.Hi = true, res.Name, rest, res.Range.Lo, res.Range.Hi
		}
	}
	cv.Check = buildCheck(command, res, len(fleet))
	return cv
}

// listNames is "a, b and c" or "a, b and 3 more".
func listNames(names []string) string {
	switch n := len(names); {
	case n == 0:
		return ""
	case n == 1:
		return names[0]
	case n <= 3:
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
	default:
		return names[0] + ", " + names[1] + " and " + strconv.Itoa(n-2) + " more"
	}
}

// buildCheck turns the catalogue check into the banner; nil when there is nothing to say.
func buildCheck(command string, res tasmotacmd.Result, fleetSize int) *checkView {
	if len(res.Versions) == 0 {
		return nil
	}
	typed := strings.Fields(command)[0]
	var missing, lacking, badIdx []string // versions
	var affected []string                 // devices
	needs := map[string]bool{}
	var needList []string
	for _, v := range res.Versions {
		switch {
		case v.Exists == tasmotacmd.False:
			missing = append(missing, v.Version)
			affected = append(affected, v.Devices...)
		case v.IndexBad:
			badIdx = append(badIdx, v.Version)
		}
		if len(v.Lacking) > 0 {
			lacking = append(lacking, v.Version)
			for _, l := range v.Lacking {
				affected = append(affected, l.Device)
				for _, n := range l.Needs {
					if !needs[n] {
						needs[n] = true
						needList = append(needList, n)
					}
				}
			}
		}
	}
	if len(missing) == 0 && len(lacking) == 0 && len(badIdx) == 0 {
		return nil
	}
	cv := &checkView{Tone: ui.ToneWarn}
	for _, v := range res.Versions {
		row := checkRow{Version: v.Version, Devices: plural(len(v.Devices), "device"), OK: true, Result: "command exists"}
		switch {
		case v.Exists == tasmotacmd.False:
			row.OK, row.Result = false, "not a command in Tasmota "+v.Pinned
			if v.Pinned != v.Version {
				row.Result = "not a command in Tasmota " + v.Pinned + " or before"
			}
		case len(v.Lacking) > 0:
			row.OK = false
			first := v.Lacking[0]
			row.Result = "needs " + strings.Join(first.Needs, ", ")
			if len(v.Lacking) < len(v.Devices) {
				row.Devices = strconv.Itoa(len(v.Lacking)) + " of " + plural(len(v.Devices), "device")
				row.Result = first.Device + ": " + row.Result
			}
		case v.IndexBad:
			row.OK = false
			row.Result = fmt.Sprintf("index %d is outside %d to %d", res.Range.Typed, res.Range.Lo, res.Range.Hi)
		}
		cv.Rows = append(cv.Rows, row)
	}
	nAff := len(dedupeNames(affected))
	switch {
	case res.Refuse:
		cv.Tone = ui.ToneDanger
		cv.Title = typed + " is not a command in any Tasmota version in your fleet"
		cv.Foot = "No device would know it, so the rule cannot be saved."
	case len(missing) > 0:
		cv.Title = typed + " is not a command in Tasmota " + strings.Join(missing, ", ") + " (" + plural(nAff, "device") + ")"
		cv.Foot = listNames(dedupeNames(affected)) + " show as “Not judged” for this rule. You can still save; the rule applies to the other " + plural(max(fleetSize-nAff, 0), "device") + "."
		if nAff > 3 {
			cv.Foot = plural(nAff, "device") + " show as “Not judged” for this rule. You can still save; the rule applies to the other " + plural(max(fleetSize-nAff, 0), "device") + "."
		}
	case len(lacking) > 0:
		cv.Title = typed + " needs the feature " + strings.Join(needList, ", ") + " (" + plural(nAff, "device") + ")"
		cv.Foot = "Their firmware was built without it, so it does not know the command. Those " + plural(nAff, "device") + " show as “Not judged”. Reflash with a build that has " + strings.Join(needList, ", ") + " to judge them."
		if nAff == 1 {
			cv.Foot = "Its firmware was built without it, so it does not know the command. That device shows as “Not judged”. Reflash with a build that has " + strings.Join(needList, ", ") + " to judge it."
		}
	default:
		cv.Title = fmt.Sprintf("%s: index %d is outside %d to %d", res.Name, res.Range.Typed, res.Range.Lo, res.Range.Hi)
		cv.Foot = "You can still save. The range is read from the Tasmota source and may be too narrow; nothing else changes."
		cv.Rows = nil
	}
	return cv
}

func dedupeNames(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// unknownCommand is the one reason a command is refused on save because of the catalogues: no
// Tasmota version present in the fleet has it. Curated settings are never refused here.
func (h *handler) unknownCommand(ctx context.Context, command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if _, ok := settings.Lookup(command); ok {
		return ""
	}
	set, err := tasmotacmd.Pinned()
	if err != nil {
		return ""
	}
	res := set.Check(command, h.fleetDevices(ctx))
	if !res.Refuse {
		return ""
	}
	var vs []string
	for _, v := range res.Versions {
		vs = append(vs, v.Version)
	}
	return typedName(command) + " is not a command in any Tasmota version in your fleet (" + strings.Join(vs, ", ") + ")."
}

func typedName(command string) string {
	if f := strings.Fields(command); len(f) > 0 {
		return f[0]
	}
	return command
}

// notJudgedNote says why a device cannot judge the command, from the catalogue of its own
// firmware: "not a command in 13.4.0" or "needs USE_SUNRISE". "" when nothing is known against it.
func notJudgedNote(command string, d model.Device) string {
	set, err := tasmotacmd.Pinned()
	if err != nil || strings.TrimSpace(command) == "" {
		return ""
	}
	res := set.Check(command, []tasmotacmd.FleetDevice{{
		Name: d.Name, Version: d.Identity.Version,
		Env: tasmotacmd.Env{Chip: string(d.Identity.Family), Variant: d.Identity.Variant},
	}})
	if len(res.Versions) == 0 {
		return ""
	}
	switch v := res.Versions[0]; {
	case v.Exists == tasmotacmd.False:
		return "not a command in " + v.Version
	case len(v.Lacking) > 0:
		return "needs " + strings.Join(v.Lacking[0].Needs, ", ")
	}
	return ""
}

// ---- suggestions ----

// suggestView is the listbox under the command field.
type suggestView struct {
	Query    string
	Starts   []tasmotacmd.Suggestion
	Contains []tasmotacmd.Suggestion
	Total    int
	Versions []string
	Shown    int
}

// commandSuggest is GET /policy/rules/command-suggest?command=: the listbox for the typed text.
func (h *handler) commandSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, vs := range q {
		if k != "command" || len(vs) != 1 {
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return
		}
	}
	query := strings.TrimSpace(q.Get("command"))
	if len(query) > 32 {
		query = query[:32]
	}
	set, err := tasmotacmd.Pinned()
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var versions []string
	for _, d := range h.fleetDevices(r.Context()) {
		versions = append(versions, d.Version)
	}
	s := set.Suggest(query, versions, suggestLimit, commandKeep)
	v := suggestView{Query: query, Total: s.Total, Versions: s.Versions, Shown: len(s.List)}
	for _, x := range s.List {
		if x.Starts {
			v.Starts = append(v.Starts, x)
		} else {
			v.Contains = append(v.Contains, x)
		}
	}
	h.component(w, r, http.StatusOK, commandSuggestList(v))
}

// ---- value kinds from the catalogue ----

// catalogueValue fills vc from the catalogue for a command no curated setting covers. It reports
// whether the catalogue knew the command with a usable kind.
func catalogueValue(vc *valueControl, command string, keepInvalid bool) bool {
	cat, err := tasmotacmd.Default()
	if err != nil {
		return false
	}
	f, ok := cat.Find(command)
	if !ok {
		return false
	}
	spec := f.Value()
	switch spec.Kind {
	case tasmotacmd.ValueNumber:
		vc.Kind, vc.Decimal = "number", spec.Decimal
		what := "Whole number"
		if spec.Decimal {
			what = "Decimal number"
		}
		if spec.Min != nil {
			vc.Min, vc.Max, vc.HasRange = *spec.Min, *spec.Max, true
			what += fmt.Sprintf(", %d to %d", *spec.Min, *spec.Max)
		}
		vc.Hint = what + "."
		if !keepInvalid && vc.Want != "" && catalogueWantProblem(command, vc.Want) != "" {
			vc.Want = ""
		}
	case tasmotacmd.ValueState:
		vc.Kind = "state"
		vc.Hint = "On or off. Compared with the state the device reports."
	case tasmotacmd.ValueText:
		vc.Hint = "Free text. Compared with exactly what the device reports. No range is known for this command."
	}
	return true
}

// catalogueWantProblem checks a value against what the catalogue says the command takes: a
// number where it answers one, within the range its handler checks. "" when fine or unknown.
func catalogueWantProblem(command, want string) string {
	if _, ok := settings.Lookup(command); ok || settings.SetOptionParam(command) || setOptionRe.MatchString(command) {
		return "" // the curated checks decide
	}
	var vc valueControl
	if !catalogueValue(&vc, command, true) {
		return ""
	}
	switch vc.Kind {
	case "number":
		n, err := strconv.ParseFloat(want, 64)
		if err != nil {
			return "Enter a number."
		}
		if vc.HasRange && n < float64(vc.Min) {
			return fmt.Sprintf("%s is below the minimum of %d.", want, vc.Min)
		}
		if vc.HasRange && n > float64(vc.Max) {
			return fmt.Sprintf("%s is above the maximum of %d.", want, vc.Max)
		}
	case "state":
		switch strings.ToLower(want) {
		case "1", "0", "on", "off":
		default:
			return "Choose on or off."
		}
	}
	return ""
}

// kindBadge is the small label beside "Expected value".
func kindBadge(kind string) string {
	switch kind {
	case "number":
		return "number"
	case "toggle", "state":
		return "on / off"
	case "select":
		return "choice"
	case "text":
		return "text"
	}
	return ""
}
