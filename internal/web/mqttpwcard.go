package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The card of the devices' MQTT passwords on System, MQTT (#609) and its one bulk action, "Use the broker
// password for these…" (admin). The bulk action follows the #622 pattern: a dialog for scripts and the GET twin as a
// page for browsers without them, both posting the ids and confirm=1 to a route outside the settings form.
const pwBrokerPath = "/mqtt-passwords/broker"

// AllFactsSource gives the transport facts of every device at once (the card needs each device's word); without it the
// card asks per device. app's webFacts implements it with one read of the inventory.
type AllFactsSource interface {
	AllFacts(ctx context.Context) (map[model.DeviceID]transport.Facts, error)
}

// pwCardRow is one device on the card or in the dialog: the device, the reason it has no password, where to enter it.
type pwCardRow struct {
	ID      model.DeviceID
	Name    string
	Word    string
	Tone    ui.Tone
	Reason  string
	Meta    string // online or offline
	Href    string // the device
	SetHref string // its edit form
}

// pwCard is the data of the card: the counts, and the devices without a password (every one of them is on
// TasmoFleet's broker; the others need none).
type pwCard struct {
	OnBroker    int // devices on TasmoFleet's broker
	FromBackups int // of them, with a password taken from a backup
	Entered     int // of them, with one a person entered (or set in bulk)
	Refused     int
	NotTested   int
	LearnOn     bool
	Rows        []pwCardRow
	Testing     int  // devices whose test is queued or running
	CanBulk     bool // a broker password is set and there are devices to give it to
	Base, CSRF  string
}

// NotKnown is the number of devices without a password.
func (c pwCard) NotKnown() int { return len(c.Rows) }

// passwordCardOf collects the card's data; nil without the fleet service, the inventory or the facts, and when the
// facts of the devices could not be read (the card then shows the switch only, as before).
func (h *handler) passwordCardOf(ctx context.Context, v view) *pwCard {
	if h.d.PasswordFleet == nil || h.d.Inventory == nil || h.d.Transport == nil {
		return nil
	}
	devs, err := h.d.Inventory.List(ctx)
	if err != nil {
		h.log.Warn("password card: could not list the devices", "err", err)
		return nil
	}
	facts, err := h.passwordFacts(ctx, devs)
	if err != nil {
		h.log.Warn("password card: could not read the transport facts", "err", err)
		return nil
	}
	c := &pwCard{LearnOn: h.learnPasswordsOn(), Base: v.Base, CSRF: v.CSRF, Testing: h.d.PasswordFleet.Testing()}
	var unset []model.Device
	for _, d := range devs {
		f, ok := facts[d.ID]
		if !ok {
			continue
		}
		st := mqttPasswordState(pwInputOf(d, f), c.LearnOn)
		if st.Kind != pwNotNeeded {
			c.OnBroker++
		}
		switch st.Kind {
		case pwNotKnown, pwNotSet:
			unset = append(unset, d)
		case pwRefused:
			c.Refused++
		case pwNotTested:
			c.NotTested++
		case pwNotNeeded, pwUnusable, pwWorks:
		}
		if d.MQTTPasswordMode != model.MQTTPasswordNone && st.Kind != pwNotNeeded {
			if d.MQTTPasswordSource == model.MQTTPasswordFromBackup {
				c.FromBackups++
			} else {
				c.Entered++
			}
		}
	}
	ids := make([]model.DeviceID, len(unset))
	for i, d := range unset {
		ids[i] = d.ID
	}
	var gaps map[model.DeviceID]PasswordGap
	if len(ids) > 0 {
		gaps = h.d.PasswordFleet.Gaps(ctx, ids)
	}
	for _, d := range unset {
		f := facts[d.ID]
		st := mqttPasswordState(pwInputOf(d, f), c.LearnOn)
		row := pwCardRow{ID: d.ID, Name: d.Name, Word: st.Word, Tone: st.Tone, Reason: gapText(gaps[d.ID], f), Meta: "offline",
			Href: v.Base + devPath(d.ID, ""), SetHref: v.Base + devPath(d.ID, "/edit")}
		if h.d.Health != nil && h.d.Health.Get(d.ID).State == model.HealthOnline {
			row.Meta = "online"
		}
		c.Rows = append(c.Rows, row)
	}
	c.CanBulk = v.IsAdmin && len(c.Rows) > 0 && h.brokerHasPassword()
	return c
}

// passwordFacts reads the facts of devs: one call when the source can, else one per device.
func (h *handler) passwordFacts(ctx context.Context, devs []model.Device) (map[model.DeviceID]transport.Facts, error) {
	if src, ok := h.d.Transport.(AllFactsSource); ok {
		return src.AllFacts(ctx)
	}
	out := make(map[model.DeviceID]transport.Facts, len(devs))
	for _, d := range devs {
		f, err := h.d.Transport.Facts(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		out[d.ID] = f
	}
	return out, nil
}

func (h *handler) registerPasswordBulk(route routeFunc, adm func(http.Handler) http.Handler) {
	if h.d.PasswordFleet == nil {
		return
	}
	route("GET "+sysPath+pwBrokerPath, adm, h.pwBrokerConfirm)
	route("POST "+sysPath+pwBrokerPath, adm, h.pwBrokerSet)
}

// pwBrokerData is the confirmation, as the dialog and as the page: the devices that would get the broker password,
// all ticked, and the box for the tests.
type pwBrokerData struct {
	Base, CSRF string
	Rows       []pwCardRow
	ACL        bool // the broker ACL notice (T18): the tests put a chunk of each device's settings on the broker
}

func (h *handler) pwBrokerRefuse(w http.ResponseWriter, r *http.Request, status int, text string) {
	h.message(w, r, status, h.view(r, "Nothing changed", "system", messageData{Text: text, LinkText: "Back to the MQTT settings", Link: sysTabPath("mqtt"), LinkPrimary: true}))
}

// pwBrokerConfirm is the page that asks before the broker password is given to the devices without one (the twin
// of the dialog for browsers without scripts). ?id= limits it to those devices (the device page's link names one).
func (h *handler) pwBrokerConfirm(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Use the broker password", "system", nil)
	c := h.passwordCardOf(r.Context(), v)
	if c == nil || !h.brokerHasPassword() {
		h.pwBrokerRefuse(w, r, http.StatusUnprocessableEntity, "TasmoFleet has no broker password to use. Set it under Broker first.")
		return
	}
	rows := c.Rows
	if ids := r.URL.Query()["id"]; len(ids) > 0 {
		rows = slices.DeleteFunc(slices.Clone(rows), func(row pwCardRow) bool { return !slices.Contains(ids, strconv.FormatInt(int64(row.ID), 10)) })
	}
	if len(rows) == 0 {
		h.pwBrokerRefuse(w, r, http.StatusOK, "Nothing to do: every device on TasmoFleet's broker has a password.")
		return
	}
	v.Crumbs = []ui.Crumb{sysTabCrumb("mqtt"), {Label: "Use the broker password"}}
	d := pwBrokerData{Base: v.Base, CSRF: v.CSRF, Rows: rows}
	d.ACL = h.brokerACLOpen()
	h.show(w, r, http.StatusOK, "mqtt_password_broker", v, pwBrokerPage(d))
}

// pwBrokerSet gives the broker password to the devices the confirmation named, as far as they are still without
// one and on TasmoFleet's broker. The posted ids are cut to what the card would list now (a device that a backup
// taught, or that was typed, since the dialog opened is left alone), and the store checks it once more per device
// in one transaction. One audit entry names the devices written and those left out, never a password.
func (h *handler) pwBrokerSet(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		h.pwBrokerRefuse(w, r, http.StatusUnprocessableEntity, "Nothing was changed: it was not confirmed.")
		return
	}
	var posted []model.DeviceID
	for _, s := range r.PostForm["id"] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			h.notFound(w, r)
			return
		}
		if id := model.DeviceID(n); !slices.Contains(posted, id) {
			posted = append(posted, id)
		}
	}
	if len(posted) == 0 {
		h.pwBrokerRefuse(w, r, http.StatusUnprocessableEntity, "Nothing was changed: no device was selected.")
		return
	}
	if !h.brokerHasPassword() {
		h.pwBrokerRefuse(w, r, http.StatusUnprocessableEntity, "Nothing was changed: TasmoFleet has no broker password to use.")
		return
	}
	c := h.passwordCardOf(r.Context(), h.view(r, "", "system", nil))
	if c == nil {
		h.fail(w, r, errors.New("the devices' passwords could not be read"))
		return
	}
	var keep, skipped []model.DeviceID
	for _, id := range posted {
		if slices.ContainsFunc(c.Rows, func(row pwCardRow) bool { return row.ID == id }) {
			keep = append(keep, id)
		} else {
			skipped = append(skipped, id)
		}
	}
	if len(keep) == 0 {
		h.pwBrokerRefuse(w, r, http.StatusOK, "Nothing was changed: none of the selected devices is still without a password.")
		return
	}
	test := r.PostFormValue("test") == "1"
	applied, err := h.d.PasswordFleet.UseBroker(r.Context(), actor(r), keep, test)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	for _, id := range keep {
		if !slices.Contains(applied, id) {
			skipped = append(skipped, id) // taught or typed between the check and the write
		}
	}
	if h.d.Recorder != nil {
		detail, jerr := json.Marshal(map[string]any{"mode": string(model.MQTTPasswordBroker), "applied": ids64(applied), "skipped": ids64(skipped), "test": test})
		if jerr != nil {
			detail = []byte("{}")
		}
		h.d.Recorder.Record(r.Context(), model.AuditEvent{At: h.d.Clock.Now(), Actor: actor(r), Action: "device.mqtt_password_bulk", Target: "devices",
			Detail: string(detail), Result: "ok", RemoteAddr: httpx.ClientIP(r.Context()).String()})
	}
	q := url.Values{"tab": {"mqtt"}, "ok": {"pw-bulk"}, "n": {strconv.Itoa(len(applied))}, "s": {strconv.Itoa(len(skipped))}}
	if test && len(applied) > 0 {
		q.Set("t", "1")
	}
	redirect(w, r, sysPath+"?"+q.Encode()+"#card-devicepasswords")
}

func ids64(ids []model.DeviceID) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out
}

// pwBulkText words the result the bulk action redirected with.
func pwBulkText(q url.Values) string {
	n, _ := strconv.Atoi(q.Get("n"))
	s, _ := strconv.Atoi(q.Get("s"))
	t := "TasmoFleet's broker password is set for " + plural(max(n, 0), "device") + "."
	if q.Get("t") == "1" {
		t += " Each device is tested in the background; the card shows the results."
	}
	if s > 0 {
		t += " " + plural(s, "device") + " left out: it has a password now."
	}
	return t
}

// pwCardNote is the line under the counts: how many devices refused their password or were never tested.
func pwCardNote(c *pwCard) string {
	var parts []string
	if c.Refused > 0 {
		parts = append(parts, strconv.Itoa(c.Refused)+" refused")
	}
	if c.NotTested > 0 {
		parts = append(parts, strconv.Itoa(c.NotTested)+" not tested yet")
	}
	return strings.Join(parts, " · ") + ". Open the device to test it or change its password."
}
