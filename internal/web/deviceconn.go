package web

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The "Paths" card of the device page (#261): how TasmoFleet reaches the device (HTTP, MQTT, the
// device's MQTT password, the firmware server) and which way each action takes, or why none.
// Every sentence comes from package transport (Choose and its explainers); this file only
// capitalises, orders and tones them, so the card, the disabled buttons and the errors of the
// router say the same thing. The one exception is the "Other broker" hint of the MQTT row
// (otherBrokerRow): web only, it never changes a path or a reason.

// reasonOffline is blockReason's sentence for a device that stopped answering.
const reasonOffline = "The device is offline."

// How a device with its HTTP API switched off is named everywhere (#447): the tile, the card, the list tooltip, the
// Back up note, the banner and the refusals of the actions. A refusal only happens when the device is reached by an
// address other than its own IP or hostname (TasmoFleet sends a matching Referer), so that is a fix too.
const (
	apiOffName   = "HTTP API off (SetOption128 0)"
	apiOffFix    = "Switch it on (SetOption128 1) or add the device by its own IP or hostname."
	reasonAPIOff = apiOffName + ". " + apiOffFix
)

// forgetFWReach is the admin route that forgets what is known about the device reaching the
// firmware server (WP-E2, spec §17).
const forgetFWReach = "/fwreach/forget"

// Row keys the summaries look up.
const (
	keyHTTP = "HTTP"
	keyMQTT = "MQTT"
	keyFW   = "Reaches the firmware server"
)

// pathRow is one line of the card: a label, a state with its tone, and a sentence under it.
type pathRow struct {
	Key, Value string
	Tone       ui.Tone
	Sub        string
	Since      time.Time // set: Sub is followed by "since" and this instant
	SinceText  string    // its fallback text
	Experiment bool      // the row carries the "Experimental" badge
	Forget     bool      // the row carries the Forget button
}

// pathCard is the card; Facts are the connection rows, Actions the per-action rows.
type pathCard struct {
	ID      model.DeviceID
	Base    string
	CSRF    string
	Facts   []pathRow
	Actions []pathRow
	Forget  string // action of the Forget form, "" for a viewer who may not use it
}

// FactsSummary is the line of the closed Connection list: the two states that decide the path.
// Both lists are closed so the overview stays within the phone height budget.
func (c pathCard) FactsSummary() string {
	s := "Connection: HTTP " + lowerWord(rowByKey(c.Facts, keyHTTP).Value) + ", MQTT " + lowerWord(rowByKey(c.Facts, keyMQTT).Value)
	if fw := rowByKey(c.Facts, keyFW); fw.Value == "No" {
		s += ", firmware server no"
	}
	return s
}

// lowerWord lowercases a row value for the summary line, but keeps an acronym ("API off") as it is.
func lowerWord(s string) string {
	if len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z' {
		return s
	}
	return lowerFirst(s)
}

// rowByKey finds a row by its label; a missing row is the zero row.
func rowByKey(rows []pathRow, key string) pathRow {
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	return pathRow{}
}

// ActionsSummary is the line of the closed Actions list: how many actions have no way now. The
// list is closed so the overview stays within the phone height budget.
func (c pathCard) ActionsSummary() string {
	n := 0
	for _, r := range c.Actions {
		if r.Value == "Not available" {
			n++
		}
	}
	if n == 0 {
		return "What each action uses (all available)"
	}
	return "What each action uses (" + strconv.Itoa(n) + " not available)"
}

// pathLabel is the short word of a path for the Action rows.
func pathLabel(p transport.Path) string {
	switch p {
	case transport.HTTP:
		return "HTTP"
	case transport.MQTT:
		return "MQTT"
	case transport.DownloadHTTP:
		return "Download over HTTP"
	case transport.DownloadMQTT:
		return "Download over MQTT"
	case transport.PushHTTP:
		return "Upload over HTTP"
	case transport.PushSafeboot:
		return "Upload over HTTP (safeboot)"
	case transport.PushMQTT:
		return "Upload over MQTT"
	case transport.None:
	}
	return "Not available"
}

// transportOf reads the facts of a device and decides its paths. ok is false when the page has no
// transport service, or the facts could not be read: the page then behaves as before (a failed
// read costs the card, not the page).
func (h *handler) transportOf(ctx context.Context, id model.DeviceID) (transport.Facts, transport.Paths, bool) {
	if h.d.Transport == nil {
		return transport.Facts{}, transport.Paths{}, false
	}
	f, err := h.d.Transport.Facts(ctx, id)
	if err != nil {
		h.log.Warn("transport facts", "device", id, "err", err)
		return transport.Facts{}, transport.Paths{}, false
	}
	// The router's own facts: they carry the tracker's finding that the HTTP API is off (#447), so what the page
	// decides on (buttons, the card, the refusals below) is what the router will do.
	return f, transport.Choose(f), true
}

// pathFor is the path an action takes in paths.
func pathFor(p transport.Paths, a transport.Action) transport.Path {
	switch a {
	case transport.ActCommands:
		return p.Commands
	case transport.ActWrites:
		return p.Writes
	case transport.ActBackup:
		return p.Backup
	case transport.ActRestore:
		return p.Restore
	case transport.ActUpdate:
		return p.Update
	}
	return transport.None
}

// apiOffBlock is why an action is refused up front: the device's HTTP API is off and nothing else (MQTT) can
// carry the action. "" when the action may go ahead. A device whose API is off is Online, so the offline rules do
// not catch it, and trying would only fail with the client's refusal (#447).
func (h *handler) apiOffBlock(ctx context.Context, id model.DeviceID, a transport.Action) string {
	if h.d.Health == nil || !h.d.Health.Get(id).APIOff() {
		return ""
	}
	_, p, ok := h.transportOf(ctx, id)
	if !ok || pathFor(p, a) != transport.None {
		return ""
	}
	return reasonAPIOff
}

// overMQTT says whether a path travels over MQTT. An offline device can only be reached that way:
// every other path (HTTP, a download or upload over HTTP) needs the device to answer HTTP.
func overMQTT(p transport.Path) bool {
	return p == transport.MQTT || p == transport.DownloadMQTT || p == transport.PushMQTT
}

// FailingHTTPSource gives the facts of the devices whose HTTP calls fail, for the ids asked: only
// those can be managed over MQTT, so the device list reads nothing for the others. A source that
// knows of no failure answers without a single read. Optional on Deps.Transport.
type FailingHTTPSource interface {
	FailingHTTP(ctx context.Context, ids []model.DeviceID) (map[model.DeviceID]transport.Facts, error)
}

// failingHTTP reads, once for the whole list, the facts of the devices on our broker whose HTTP
// fails (the "MQTT" marker needs nothing else). A failed read only costs the markers.
func (h *handler) failingHTTP(ctx context.Context, devs []model.Device) map[model.DeviceID]transport.Facts {
	src, ok := h.d.Transport.(FailingHTTPSource)
	if !ok {
		return nil
	}
	var ids []model.DeviceID
	for _, d := range devs {
		if d.MQTT == model.MQTTOurs {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m, err := src.FailingHTTP(ctx, ids)
	if err != nil {
		h.log.Warn("transport facts for the device list", "err", err)
		return nil
	}
	return m
}

// actionWhy is the reason an action's button is disabled, "" when it may be used. A path of None
// carries Choose's sentence; a device that is offline keeps the offline sentence for every path
// that is not over MQTT (nothing said HTTP fails, but the device does not answer).
func actionWhy(general string, p transport.Path, reason string) string {
	if p == transport.None {
		return sentence(reason)
	}
	if general == reasonOffline && !overMQTT(p) {
		return general
	}
	return ""
}

// applyPaths sets the per-action reasons of the tabs, the Back up button and the card. Without
// paths every action shows the general reason, as before.
func (t *deviceTabs) applyPaths(p transport.Paths, ok bool) {
	t.SettingsWhy, t.ConsoleWhy, t.BackupWhy = t.Reason, t.Reason, t.Reason
	if t.Health == model.HealthOffline {
		t.UpdateWhy = reasonOffline
	}
	if !ok {
		return
	}
	// A running operation or a device that never answered blocks everything, whatever the path.
	general := t.Reason
	if general != "" && general != reasonOffline {
		return
	}
	t.SettingsWhy = actionWhy(general, p.Writes, p.Reason(transport.ActWrites))
	t.ConsoleWhy = actionWhy(general, p.Commands, p.Reason(transport.ActCommands))
	t.BackupWhy = actionWhy(general, p.Backup, p.Reason(transport.ActBackup))
	t.RestoreWhy = actionWhy(general, p.Restore, p.Reason(transport.ActRestore))
	t.UpdateWhy = actionWhy(general, p.Update, p.Reason(transport.ActUpdate))
	if t.APIOff {
		// The transport words the cause its own way; the page names it one way everywhere (#447).
		for _, a := range []struct {
			why  *string
			path transport.Path
		}{{&t.SettingsWhy, p.Writes}, {&t.ConsoleWhy, p.Commands}, {&t.BackupWhy, p.Backup}, {&t.RestoreWhy, p.Restore}, {&t.UpdateWhy, p.Update}} {
			if a.path == transport.None {
				*a.why = reasonAPIOff
			}
		}
	}
}

// updateBtnWhy is why the header's "Update to" is disabled. Only the device's state gates it: the
// delivery (and so the path) is chosen on the update page, so the card's pull-only Update path
// must not decide here.
func (t deviceTabs) updateBtnWhy() string {
	if t.Updating || t.Health == model.HealthOffline {
		return t.Reason
	}
	return ""
}

// backupNote is the visible line next to the actions for a Back up that has no path; the offline
// case has its banner already (described by it).
func (t deviceTabs) backupNote() string {
	if t.BackupWhy == "" || t.BackupWhy == reasonOffline {
		return ""
	}
	return t.BackupWhy
}

// updateDescribedBy names the element that says why "Update to" is disabled.
func (t deviceTabs) updateDescribedBy() string {
	if t.Updating {
		return "devhd-why"
	}
	return "devhd-off"
}

// backupDescribedBy names the element that says why Back up is disabled.
func (t deviceTabs) backupDescribedBy() string {
	if t.backupNote() != "" {
		return "devhd-why"
	}
	return "devhd-off"
}

// pathCardOf builds the card for the viewer.
//
// The Actions rows show the reason the buttons carry (t), so a device that never answered or is
// held by an operation reads the same in the card as on its buttons; otherwise the chooser's.
func pathCardOf(v view, d model.Device, f transport.Facts, p transport.Paths, t deviceTabs) *pathCard {
	c := &pathCard{ID: d.ID, Base: v.Base, CSRF: v.CSRF}
	c.Facts = append(c.Facts, httpRow(f), mqttRow(f), passwordRow(f), fwRow(f), uploadRow(d))
	if v.IsAdmin && transport.EffectiveFWReach(f) == model.FWReachNo {
		c.Forget = v.Base + devPath(d.ID, forgetFWReach)
		for i := range c.Facts {
			if c.Facts[i].Key == keyFW {
				c.Facts[i].Forget = true
			}
		}
	}
	for _, a := range []struct {
		key  string
		act  transport.Action
		path transport.Path
		why  string
	}{
		{"Console and Settings: reading", transport.ActCommands, p.Commands, t.ConsoleWhy},
		{"Console and Settings: changing", transport.ActWrites, p.Writes, t.SettingsWhy},
		{"Back up", transport.ActBackup, p.Backup, t.BackupWhy},
		{"Restore", transport.ActRestore, p.Restore, t.RestoreWhy},
		{"Update (download)", transport.ActUpdate, p.Update, t.UpdateWhy},
	} {
		row := pathRow{Key: a.key, Value: pathLabel(a.path), Tone: ui.ToneOK, Sub: sentence(p.Reason(a.act))}
		if a.path == transport.None || a.why != "" {
			row.Value, row.Tone = "Not available", ui.ToneWarn
			if a.why != "" {
				row.Sub = a.why
			}
		}
		c.Actions = append(c.Actions, row)
	}
	return c
}

// httpWord is the one wording of the HTTP state (ok, failing, API off, not checked lately); the connection
// card and the Reachability tile both use it, so they cannot disagree (#416).
func httpWord(st transport.HTTPState) string {
	switch st.State {
	case transport.HTTPFailing:
		if st.Kind == transport.HTTPFailAPIRefused {
			return "API off"
		}
		return "Not reachable"
	case transport.HTTPOK:
		return "Reachable"
	case transport.HTTPUnknown:
	}
	return "Not checked lately"
}

func httpRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyHTTP}
	switch f.HTTP.State {
	case transport.HTTPFailing:
		r.Value, r.Tone, r.Sub = httpWord(f.HTTP), ui.ToneDanger, sentence(f.HTTP.Describe())
		if f.HTTP.Kind == transport.HTTPFailAPIRefused {
			r.Sub = reasonAPIOff
		}
		r.setSince(f.HTTP.Since, f.Now)
	case transport.HTTPOK:
		r.Value, r.Tone, r.Sub = httpWord(f.HTTP), ui.ToneOK, "TasmoFleet's last HTTP call to the device succeeded."
	case transport.HTTPUnknown:
		r.Value, r.Tone, r.Sub = httpWord(f.HTTP), ui.ToneNeutral, "Reads go over MQTT first for a device live on TasmoFleet's broker, over HTTP otherwise; changes try HTTP first. TasmoFleet checks HTTP when this page opens or a task needs it."
	}
	return r
}

func (r *pathRow) setSince(at, now time.Time) {
	if !at.IsZero() {
		r.Since, r.SinceText = at, ui.StampDayText(now, at)
	}
}

func mqttRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyMQTT}
	topic := ""
	if f.MQTT.Topic != "" {
		topic = "Topic " + f.MQTT.Topic
	}
	join := func(parts ...string) string {
		var out []string
		for _, s := range parts {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, " · ")
	}
	switch strict, reads := transport.MQTTUnusable(f, true), transport.MQTTUnusable(f, false); {
	case strict == "":
		r.Value, r.Tone, r.Sub = "On our broker", ui.ToneOK, join(topic, "Last will online")
	case reads == "":
		r.Value, r.Tone = "Reading only", ui.ToneWarn
		r.Sub = join(topic, "Changing the device over MQTT needs more: "+strict+".")
	default:
		r.Value, r.Tone = "Not usable", ui.ToneNeutral
		r.Sub = sentence(reads)
	}
	return r
}

// silentOnOurBroker is how long a device may send nothing on our broker before it counts as
// gone from it (#580). A device on our broker sends telemetry at least every TelePeriod (default
// 5 minutes, at most 1 hour), so two hours cover the slowest setting with a margin.
const silentOnOurBroker = 2 * time.Hour

// heardOnOurBroker says whether the device is on our broker. The stored "ours" state never turns
// back and a retained LWT outlives a broker move, so neither counts on its own (#580): a message
// within silentOnOurBroker does. A device that answers HTTP but sent nothing for longer is not
// on our broker (any more); one that does not answer HTTP may simply be switched off, so it keeps
// whatever it had.
//
// Known limits (accepted, #580): the health tracker counts a retained "Online" LWT as a message
// whenever the broker replays it, at startup and after every reconnect (clean session), so then
// a device that moved away still reads as heard and the hint takes up to silentOnOurBroker to
// appear; telling the replay apart would need a change in package health. A device on our
// broker that sends nothing for longer (TelePeriod 0) and names it by an address the matcher
// cannot resolve to ours (a Docker service name, a second interface) gets the hint too, unless
// another device configured the same way is heard on our broker (broker aliases, #597).
func heardOnOurBroker(d model.Device, hl model.Health, httpOK bool, now time.Time) bool {
	if !hl.SeenMQTT.IsZero() && now.Sub(hl.SeenMQTT) < silentOnOurBroker {
		return true
	}
	if httpOK {
		return false
	}
	return d.MQTT == model.MQTTOurs || hl.LWT != "" || !hl.SeenMQTT.IsZero()
}

// otherBrokerRow is the MQTT row for a device that reports to a broker other than TasmoFleet's
// (#441): it will not show up live. It answers only when TasmoFleet has a broker (the live MQTT
// switch is not "disabled": the matcher in Deps.MQTTBroker is never nil and matches nothing once
// MQTT is off), the device names a broker host that is not ours, and it is not on our broker
// (heardOnOurBroker; silence there counts only while TasmoFleet is connected to its broker, since
// an outage of our own silences every device). Web only: the transport verdict (SAFETY) is unchanged.
func (h *handler) otherBrokerRow(d model.Device, hl model.Health, httpOK bool, now time.Time) (pathRow, bool) {
	if h.d.MQTT == nil || h.d.MQTT() == "disabled" || h.d.MQTTBroker == nil || d.MQTT == model.MQTTNone || d.MQTTHost == "" || heardOnOurBroker(d, hl, httpOK && h.d.MQTT() == "connected", now) {
		return pathRow{}, false
	}
	port := d.MQTTPort
	if port == 0 {
		port = 1883
	}
	if h.d.MQTTBroker.Same(d.MQTTHost, port) {
		return pathRow{}, false
	}
	sub := "Reports to " + net.JoinHostPort(d.MQTTHost, strconv.Itoa(port)) + ", not to TasmoFleet's broker"
	if h.d.MQTTStatus != nil {
		if b := h.d.MQTTStatus(); b.Host != "" {
			sub += " " + b.Host
		}
	}
	return pathRow{Key: keyMQTT, Value: "Other broker", Tone: ui.ToneWarn,
		Sub: sub + ", so its state does not arrive live over MQTT."}, true
}

func passwordRow(f transport.Facts) pathRow {
	r := pathRow{Key: "Device MQTT password", Tone: ui.ToneNeutral}
	p := f.Password
	switch {
	case p.Unusable != "":
		r.Value, r.Tone, r.Sub = "Unusable", ui.ToneWarn, sentence(p.Unusable)
	case !p.BadAt.IsZero():
		r.Value, r.Tone, r.Sub = "Refused", ui.ToneDanger, "The device refused it."
		r.setSince(p.BadAt, f.Now)
	case p.Mode == model.MQTTPasswordBroker:
		r.Value, r.Tone, r.Sub = "Same as the broker's", ui.ToneOK, "Back up, restore and upload over MQTT send TasmoFleet's broker password."
	case p.Mode == model.MQTTPasswordOwn:
		r.Value, r.Tone, r.Sub = "Set", ui.ToneOK, "Back up, restore and upload over MQTT send the stored password."
	default:
		r.Value, r.Sub = "Not set", "Needed only to back up, restore and upload over MQTT; commands never use it."
	}
	return r
}

func fwRow(f transport.Facts) pathRow {
	r := pathRow{Key: keyFW}
	if !f.FirmwareBaseURL {
		r.Value, r.Tone, r.Sub = "Not available", ui.ToneWarn, sentence(transport.FirmwareServerBlocker(f))
		return r
	}
	switch transport.EffectiveFWReach(f) {
	case model.FWReachYes:
		r.Value, r.Tone, r.Sub = "Yes", ui.ToneOK, "The device fetched from TasmoFleet's firmware server."
	case model.FWReachNo:
		r.Value, r.Tone, r.Sub = "No", ui.ToneDanger, "The device cannot reach TasmoFleet's firmware server."
		r.setSince(f.FWReach.At, f.Now)
	case model.FWReachUnknown:
		r.Value, r.Tone, r.Sub = "Unknown", ui.ToneNeutral, "TasmoFleet learns it when the device fetches a file or fails to."
	}
	return r
}

// uploadRow is the experimental upload over MQTT: whether the chip and its free flash allow it at
// all. The fit of a real image is decided when an update is planned.
func uploadRow(d model.Device) pathRow {
	r := pathRow{Key: "Upload over MQTT", Experiment: true, Tone: ui.ToneNeutral}
	// An image size of 1 byte asks the question "could any image go this way".
	ok, _, room, why := fwselect.MQTTUploadFits(d.Identity, fwselect.UpLayoutUnknown, 1)
	if ok {
		r.Value, r.Tone, r.Sub = "Fits", ui.ToneOK, strconv.Itoa(room)+" KB room for a firmware image."
		return r
	}
	r.Value, r.Sub = "Not available", sentence(why)
	return r
}
