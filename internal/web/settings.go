package web

import (
	"context"
	"errors"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"net/http"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// SettingsService is the part of settings.Service the UI uses.
type SettingsService interface {
	Read(ctx context.Context, id model.DeviceID) (map[string]string, error)
	Apply(ctx context.Context, actor string, id model.DeviceID, changes map[string]string) ([]settings.Result, error)
	Save(ctx context.Context, actor string, id model.DeviceID, saveData string) (settings.Result, error)
}

type settingsRow struct {
	Field        settings.Field
	Label, Help  string // Label is short; a SetOption's description moves into the help
	Value, Orig  string
	Err          string
	NotReporting bool // the device did not report a value
}

type settingsGroup struct {
	Name string
	Rows []settingsRow
}

type settingsPage struct {
	Device    model.Device
	Groups    []settingsGroup
	Error     string
	Notice    string // neutral outcome that is not a failure ("Nothing changed.")
	ReadError string
	Results   []settings.Result
	Done      bool
	Saved     string // what happened to the write to flash after the batch
	Tabs      deviceTabs
	// PolicyWant is the value a policy rule expects, by field key, for the settings this device
	// deviates in: the field says so instead of its help.
	PolicyWant map[string]string
}

// groupOrder is the order of the form sections.
var groupOrder = []string{settings.GroupGeneral, settings.GroupOptions, settings.GroupMQTT, settings.GroupTime}

// buildGroups lays out the form. orig are the values read from the device,
// cur what the form shows (the posted values on a re-render). Secret fields
// never carry a value.
func buildGroups(orig, cur map[string]string, errs map[string]string) []settingsGroup {
	var out []settingsGroup
	for _, g := range groupOrder {
		grp := settingsGroup{Name: g}
		for _, f := range settings.Fields {
			if f.Group != g {
				continue
			}
			row := settingsRow{Field: f, Err: errs[f.Key], Label: f.Label, Help: f.Help}
			if name, desc, ok := strings.Cut(f.Label, ": "); ok { // "SetOption0 (save_state): restore relay state..."
				row.Label, row.Help = name, strings.ToUpper(desc[:1])+desc[1:]+". "+f.Help
			}
			if !f.Secret {
				row.Orig = orig[f.Key]
				row.Value = cur[f.Key]
				_, reported := orig[f.Key]
				row.NotReporting = !reported
			}
			grp.Rows = append(grp.Rows, row)
		}
		out = append(out, grp)
	}
	return out
}

func (h *handler) registerSettings(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Settings == nil {
		return
	}
	route("GET /devices/{id}/settings", adm, h.settingsForm)
	route("POST /devices/{id}/settings", adm, h.settingsApply)
}

func (h *handler) renderSettings(w http.ResponseWriter, r *http.Request, status int, p settingsPage) {
	v := h.view(r, "Settings of "+p.Device.Name, "devices", nil)
	p.Tabs = h.tabsOf(r, v, p.Device, "settings")
	if len(p.Groups) > 0 {
		p.PolicyWant = h.policyWants(r, p.Device.ID)
	}
	h.headPage(w, r, status, "settings", v, p.Device, settingsTab(p, v.Base, v.CSRF))
}

// policyWants reads the device's deviations from the policy report (values already read;
// nothing is sent): setting key -> the value the rule expects.
func (h *handler) policyWants(r *http.Request, id model.DeviceID) map[string]string {
	if h.d.Policy == nil {
		return nil
	}
	rep, err := h.d.Policy.Report(r.Context(), actor(r), false)
	if err != nil {
		h.log.WarnContext(r.Context(), "device settings: policy report", "err", err)
		return nil
	}
	out := map[string]string{}
	for _, f := range rep.Findings {
		if f.DeviceID != id || f.Unknown {
			continue
		}
		for _, fld := range settings.Fields {
			if strings.EqualFold(fld.Key, f.Rule.Command) {
				out[fld.Key] = f.Rule.Want
			}
		}
	}
	return out
}

// readErrorText keeps device output out of the page.
func readErrorText(err error) string {
	switch {
	case errors.Is(err, tasmota.ErrAuth):
		return "The device refused the stored credentials."
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return "The device likely refuses HTTP API commands from this address. Set SetOption128 1 on it."
	case errors.Is(err, tasmota.ErrUnreachable):
		return "The device is unreachable."
	case errors.Is(err, tasmota.ErrTimeout):
		return "The device did not answer in time."
	}
	return "The device could not be read."
}

func (h *handler) settingsForm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	cur, err := h.d.Settings.Read(r.Context(), d.ID)
	p := settingsPage{Device: d}
	if err != nil {
		// Show nothing editable: a form built from a partial read would make
		// every missing field look like a change.
		p.ReadError = readErrorText(err)
		h.renderSettings(w, r, http.StatusBadGateway, p)
		return
	}
	p.Groups = buildGroups(cur, cur, nil)
	h.renderSettings(w, r, http.StatusOK, p)
}

// formKeys are the posted names besides the field values.
const (
	origPrefix = "orig."
)

func (h *handler) settingsApply(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// A crafted POST with a key outside the curated list is refused before
	// anything is sent.
	for k := range r.PostForm {
		if k == "csrf" {
			continue
		}
		if f, ok := settings.Lookup(strings.TrimPrefix(k, origPrefix)); !ok || f.PolicyOnly {
			http.Error(w, "unknown field", http.StatusBadRequest)
			return
		}
	}
	orig, cur, changes := map[string]string{}, map[string]string{}, map[string]string{}
	for _, f := range settings.Fields {
		v, o := r.PostFormValue(f.Key), r.PostFormValue(origPrefix+f.Key)
		if f.Secret {
			if v != "" {
				changes[f.Key] = v
			}
			continue
		}
		orig[f.Key], cur[f.Key] = o, v
		if v != o {
			changes[f.Key] = v
		}
	}
	page := settingsPage{Device: d}
	// rerender shows the form again. The posted original values cannot say which
	// fields the device did not report (every key is posted), so the baseline is
	// read from the device again: unreported fields keep their "not reported"
	// state and the change counter starts at zero. If that read fails the posted
	// values are the best there is.
	rerender := func(status int, msg string, errs map[string]string, keepTyped bool) {
		page.Error = msg
		base, cur2 := orig, cur
		if dev, err := h.d.Settings.Read(r.Context(), d.ID); err == nil {
			base = dev
			if !keepTyped {
				cur2 = dev
			}
		}
		page.Groups = buildGroups(base, cur2, errs)
		h.renderSettings(w, r, status, page)
	}
	switch {
	case len(changes) == 0:
		// Not a failure: the form matches the device, so there is nothing to send.
		page.Notice = "Nothing changed."
		rerender(http.StatusOK, "", nil, false)
		return
	}
	results, err := h.d.Settings.Apply(r.Context(), actor(r), d.ID, changes)
	var ve *settings.ValidationError
	switch {
	case errors.As(err, &ve):
		rerender(http.StatusUnprocessableEntity, "Some values are invalid. Nothing was sent to the device.", ve.Fields, true)
	case errors.Is(err, settings.ErrUnknownField):
		http.Error(w, "unknown field", http.StatusBadRequest)
	case errors.Is(err, devlock.ErrBusy):
		h.message(w, r, http.StatusConflict, h.view(r, "Device busy", "devices",
			"Nothing was written. Another operation on this device is running ("+busyHolder(err)+"). Try again when it has finished."))
	case err != nil:
		h.fail(w, r, err)
	default:
		page.Results, page.Done = results, true
		page.Saved = h.persist(r, d.ID, orig["SaveData"], changes, results)
		h.renderSettings(w, r, http.StatusOK, page)
	}
}

// persist writes the applied settings to flash once, after every batch. With
// SaveData 0 nothing else does. A restart (Hostname, Topic, Mqtt*) saves by
// itself, and changing SaveData itself saves too.
func (h *handler) persist(r *http.Request, id model.DeviceID, saveData string, changes map[string]string, results []settings.Result) string {
	applied := false
	for _, res := range results {
		if !res.OK() {
			continue
		}
		applied = true
		if res.Key == "SaveData" {
			return "Written to flash by the SaveData change."
		}
		if settings.RestartsDevice(res.Key) {
			return "The device restarts after this change, which writes all settings to flash."
		}
	}
	switch {
	case !applied:
		return "Nothing was applied, so nothing was written to flash."
	}
	if saveData == "" { // the form had no SaveData value: ask the device, never guess
		cur, err := h.d.Settings.Read(r.Context(), id)
		if saveData = cur["SaveData"]; err != nil || saveData == "" {
			return "Not written to flash: the device did not report its SaveData value. The changes are held in memory until the device saves or restarts."
		}
	}
	res, err := h.d.Settings.Save(r.Context(), actor(r), id, saveData)
	if err != nil || !res.OK() {
		return "Writing to flash failed. The changes are held in memory until the device saves or restarts."
	}
	return "Written to flash with one SaveData " + saveData + " command."
}
