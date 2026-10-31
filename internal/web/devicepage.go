package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The device overview (#145, mockups in docs/design/device-overview): a row of live tiles, the
// relay box, Identity next to Connection and one Activity list. The live part (tiles, relay box,
// Activity, header, offline banner) is rebuilt by GET /devices/{id}/live whenever the device
// changes (#126); the Identity card is never swapped because it holds answers and notices.

// deviceOverview is the view of GET /devices/{id}.
type deviceOverview struct {
	Tabs     deviceTabs
	Base     string
	CSRF     string
	Updating *jobView // an operation holds the device: it is silent on purpose
	NotYet   string   // the device never answered: what to check
	Live     liveView
	Stab     *stabCard // nil without the stability store
	Ident    identCard
	Conn     connCard
	Paths    *pathCard // nil without the transport service
	Activity devActivity
}

// liveView is the part of the overview that follows the device: tiles and the relay box.
type liveView struct {
	ID   model.DeviceID
	Base string
	CSRF string
	// Off dims the region: the device does not answer, what shows is its last reported state.
	Off   bool
	Tiles []tile
	Box   *relayBox // nil: the device has no relays
	// Sensors replaces the relay box on a device without relays
	Sensors *sensorBox
	// ActSig is devActivity.Sig of the same answer: the next refresh sends it back.
	ActSig string
}

// tile is one stat tile of the top row. A tile that does not apply is not built at all, so the
// row never has holes.
type tile struct {
	Key, Label, Value, Sub string
	Tone                   ui.Tone // colour of the value
	Bar                    int     // Wi-Fi signal quality 0..100; 0 draws no bar
	BarTone                ui.Tone
	SubPrefix              string    // "Last restart ": the sub line is this text and the time At
	At                     time.Time // zero: the sub line is just Sub
	AtText                 string    // the fallback text of At (StampDay), for a page without JavaScript
	Fix                    bool      // "Make compliant…" for this device
	Mask                   bool      // a reading the pixel comparison cannot reproduce
}

// relayBox is the box of relay switches. Right is the metering line of the box header.
type relayBox struct {
	Right   string
	Rows    []relayRow
	Enabled bool   // the switches can be used now
	Err     string // why the last switch failed (htmx switches show it in place)
}

// sensorBox is the Sensors box of a device without relays.
type sensorBox struct {
	Right string
	Cells []sensorCell
	Note  string // why there are no cells
}

// sensorCell is one reading: "Temperature · BME280" and "12.4 °C".
type sensorCell struct{ Label, Value string }

// relayRow is one relay switch. Line is what shows under the name: the channel's watts when the
// device meters each channel, else On/Off.
type relayRow struct {
	N            int
	Name, Cmd    string
	Line         string
	On           bool
	Action, Aria string
}

// devActivity is the Activity box: the newest state changes and transactions of the device,
// merged. All is the complete, paged list.
type devActivity struct {
	Base string
	Rows []devActRow
	All  string
	// Sig identifies the rows. The live region names it in its refresh request, and a refresh whose
	// answer would carry the same rows leaves the box out (Unchanged), so the table is not redrawn
	// and does not flicker while nothing in it changed (#308).
	Sig       string
	Unchanged bool
}

// activitySig is a short fingerprint of what the Activity box shows.
func activitySig(rows []devActRow) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(strconv.FormatInt(r.At.UnixNano(), 10))
		if e := r.Event; e != nil {
			b.WriteString("|e|" + string(e.From) + "|" + string(e.To) + "|" + string(e.Source) + "|" + e.Detail)
		}
		if j := r.Job; j != nil {
			b.WriteString("|j|" + strconv.FormatInt(int64(j.ID), 10) + "|" + strconv.FormatInt(int64(j.BatchID), 10) + "|" + string(j.State) + "|" + j.LastError + "|" + j.Target())
		}
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// devActRow is one state change or one transaction. Exactly one of Event and Job is set.
type devActRow struct {
	At    time.Time
	Event *store.HealthEvent
	Job   *model.Job
}

// connCard is the Connection card: how TasmoFleet reaches the device and how the device is
// connected.
type connCard struct {
	Title string // "Connection · Wi-Fi" or "Connection · Ethernet"
	Rows  []ui.KVRow
}

// overviewRows is how many entries the Activity box shows; the complete list is one link away.
const overviewRows = 10

// historyView is the page of all activity of one device.
type historyView struct {
	Tabs  deviceTabs
	Base  string
	Rows  []devActRow
	Pager pager
}

// identCard is the Identity card. "Refresh identity" swaps the whole card, so the rows show what
// the probe found and a failed probe leaves the button where it was.
type identCard struct {
	Base     string
	ID       model.DeviceID
	Rows     []ui.KVRow
	CanProbe bool // not while an operation holds the device
	Off      bool // dimmed: the device does not answer, so Refresh identity is disabled too
	Notice   string
	Error    string
	// NameOffer is the DeviceName the device reports while the stored name differs (#125);
	// "" shows nothing. Admins get the two answers, KeepOK says Keep can be saved.
	NameOffer string
	CanAnswer bool
	KeepOK    bool
	// FWR are the StatusFWR fields of Status 2 as the device sent them, Reading what TasmoFleet
	// derives from them (#289). FWRRead says the probe stored them at all.
	FWR     []fwrRow
	Reading []ui.KVRow
	FWRRead bool
	// CRWarn is the warning that the settings text area is nearly full; "" shows nothing.
	CRWarn string
}

// policyCard is the device's policy compliance: the rules that apply, the deviations, and
// "Make compliant…" for admins.
type policyCard struct {
	Rules      int
	Deviations []deviation
	Unknown    int // rules whose value could not be read
	CanFix     bool
}

type deviation struct{ Rule, Have, Want string }

// snapFacts are the network settings of the device (Status 5 and 11). The live health has none
// of them; they change only when someone reconfigures the device. A probe stores them on the
// device (model.Device.Net); the newest backup snapshot is the fallback for devices that were
// not probed since (see factsOf).
type snapFacts struct {
	Hostname, IP, Gateway string
	Subnet, DNS           string // the active interface's subnet mask and DNS servers ("" = not reported)
	SSID, BSSID           string
	Channel               int
	Wired                 bool // on Ethernet: Hostname, IP and Gateway are the Ethernet ones
}

// parseSnapFacts reads the facts from a snapshot document (Status1..Status11 replies by key).
// A failed command is recorded as {"_error": ...} and simply has no facts.
func parseSnapFacts(doc []byte) snapFacts {
	var s struct {
		Status5 struct {
			StatusNET struct{ Hostname, IPAddress, Gateway string }
		}
		Status11 struct {
			StatusSTS struct {
				Wifi struct {
					SSId, BSSId string
					Channel     int
				}
			}
		}
	}
	if json.Unmarshal(doc, &s) != nil {
		return snapFacts{}
	}
	w := s.Status11.StatusSTS.Wifi
	return snapFacts{Hostname: s.Status5.StatusNET.Hostname, IP: s.Status5.StatusNET.IPAddress, Gateway: s.Status5.StatusNET.Gateway, SSID: w.SSId, BSSID: w.BSSId, Channel: w.Channel}
}

// snapFactsOf reads the newest snapshot of the device. Without one (or when it cannot be
// read) the facts are empty and the rows say "Unknown"; the page still renders.
func (h *handler) snapFactsOf(ctx context.Context, list []model.Backup) snapFacts {
	for _, b := range list { // newest first
		if b.Kind != model.BackupSnapshot {
			continue
		}
		doc, _, err := h.d.Backups.Open(ctx, b.ID)
		if err != nil {
			h.log.WarnContext(ctx, "device overview: snapshot unreadable", "backup", b.ID, "err", err)
			return snapFacts{}
		}
		return parseSnapFacts(doc)
	}
	return snapFacts{}
}

// factsOf are the device's network facts: what a probe stored on it, field by field; the newest
// backup snapshot only fills in a device that was never probed.
func (h *handler) factsOf(ctx context.Context, d model.Device) snapFacts {
	f := snapFacts{Hostname: d.Net.Hostname, IP: d.Net.IP, Gateway: d.Net.Gateway, Subnet: d.Net.Subnet, DNS: d.Net.DNS, SSID: d.Net.SSID, BSSID: d.Net.BSSID, Channel: d.Net.Channel, Wired: d.Net.Wired}
	if !d.Net.ReadAt.IsZero() {
		return f
	}
	list, err := h.d.Backups.List(ctx, d.ID)
	if err != nil {
		h.log.WarnContext(ctx, "device overview: backups", "err", err)
		return f
	}
	return h.snapFactsOf(ctx, list)
}

// macShown is the MAC of the interface the device is on: the Ethernet one for a wired board,
// else the primary (Wi-Fi) one that identifies the device. A wired board's Ethernet MAC is the
// other entry of its MAC set.
func macShown(d model.Device, wired bool) string {
	if wired {
		for _, m := range d.MACs {
			if m != d.MAC {
				return m
			}
		}
	}
	return d.MAC
}

// orUnknown is the value of a row, "Unknown" when there is none.
func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// identity builds the Identity card for the viewer. Credentials never leave the server: the
// card says only whether a web login is stored.
func (h *handler) identity(v view, d model.Device, updating, off bool, f snapFacts) identCard {
	build := d.Identity.Variant
	if build == "unknown" && d.Identity.Image != "" && !strings.Contains(d.Identity.Image, "-") {
		build = d.Identity.Image + " (self-compiled)" // a bare tag: the name the device reports
	}
	rows := []ui.KVRow{
		{Key: "Device name", Value: d.Name, Mono: true},
		{Key: "Hostname", Value: orUnknown(f.Hostname), Mono: true, Mask: true},
		{Key: "MQTT topic", Value: orUnknown(d.Topic), Mono: true},
	}
	if d.OTAURL != "" { // #127: where the device would update from; the prober stored it without credentials
		rows = append(rows, ui.KVRow{Key: "OTA URL", Value: d.OTAURL, Mono: true, Soft: true, Wide: true})
	}
	rows = append(rows, ui.KVRow{Key: "MAC", Value: orUnknown(d.MAC), Mono: true, Mask: true})
	if d.HTTPUser != "" || d.HTTPPassword != "" {
		login := "password set"
		if d.HTTPPassword == "" {
			login = "no password"
		}
		if d.HTTPUser != "" {
			login = d.HTTPUser + ", " + login
		}
		rows = append(rows, ui.KVRow{Key: "Web login", Value: login, Mono: true, Wide: true})
	}
	if d.OTAExcluded {
		rows = append(rows, ui.KVRow{Key: "Updates", Value: "Excluded from OTA updates"})
	}
	for _, t := range d.Tags {
		rows = append(rows, ui.KVRow{Key: "Tag", Value: t})
	}
	if d.Notes != "" {
		rows = append(rows, ui.KVRow{Key: "Notes", Value: d.Notes, Wide: true})
	}
	c := identCard{Base: v.Base, ID: d.ID, Rows: rows, CanProbe: !updating, Off: off,
		NameOffer: nameOffer(d), CanAnswer: v.IsAdmin, KeepOK: h.d.DeviceNames != nil}
	c.FWR, c.Reading = statusFWRRows(d.Identity), tasmoFleetReading(d.Identity, build)
	c.FWRRead = d.Identity.FWR.Read()
	c.CRWarn = crWarning(d.Identity.FWR, h.crWarnPercent())
	return c
}

// defaultCRWarnPercent is the threshold when no system setting is wired (tests).
const defaultCRWarnPercent = 90

func (h *handler) crWarnPercent() int {
	if h.d.CRWarnPercent == nil {
		return defaultCRWarnPercent
	}
	return h.d.CRWarnPercent()
}

// fwrRow is one StatusFWR field of Status 2 under Tasmota's own name, with the value as the
// device sent it and a short explanation (#289).
type fwrRow struct {
	Name, Value, Info string
	// Wide: a long value, which takes the whole row on a phone.
	Wide bool
}

// statusFWRRows are the StatusFWR fields in the order of Tasmota's reply. A field the probe did
// not read (never, or an old identity) is left out; the card then points at Refresh identity.
func statusFWRRows(i model.Identity) []fwrRow {
	f := i.FWR
	var rows []fwrRow
	add := func(name, value, info string) {
		if value != "" {
			rows = append(rows, fwrRow{Name: name, Value: value, Info: info, Wide: name == "Version" || name == "BuildDateTime"})
		}
	}
	add("Version", f.Version, "The firmware version and its build in brackets, as Tasmota prints it.")
	add("BuildDateTime", f.BuildDateTime, "When this firmware was compiled.")
	add("Core", f.Core, "Version of the Arduino core the firmware is built on.")
	add("SDK", f.SDK, "Version of the Espressif SDK below the core.")
	if f.CPUFrequency > 0 {
		add("CpuFrequency", strconv.Itoa(f.CPUFrequency), "Current CPU clock in MHz.")
	}
	add("Hardware", i.Hardware, "The chip as Tasmota names it: model, revision and, on an ESP32, the package.")
	add("CR", f.CR, "Characters used / available in the settings text area, which names, topics, rules, URLs and passwords share. When it is full, longer values are not saved.")
	return rows
}

// tasmoFleetReading is what TasmoFleet derives from Status 2 and Status 4 itself. It is shown
// apart from the StatusFWR fields because the device does not report it (#289).
func tasmoFleetReading(i model.Identity, build string) []ui.KVRow {
	var rows []ui.KVRow
	if build != "" {
		rows = append(rows, ui.KVRow{Key: "Variant", Value: build, Mono: true})
	}
	if i.Language != "" {
		rows = append(rows, ui.KVRow{Key: "Language", Value: i.Language, Mono: true})
	}
	if mb := i.FlashSizeKB / 1024; mb > 0 {
		rows = append(rows, ui.KVRow{Key: "Flash size", Value: strconv.Itoa(mb) + " MB", Mono: true})
	}
	return rows
}

// crWarning is the Identity card's warning when the settings text area is fuller than percent,
// "" when it is not or CR was not read.
func crWarning(f model.FWRFacts, percent int) string {
	used, total, ok := f.CRUsage()
	if !ok || used*100 <= percent*total {
		return ""
	}
	return "Settings text area almost full (" + strconv.Itoa(used) + "/" + strconv.Itoa(total) + "): longer values such as OtaUrl may not be saved."
}

// policyOf is the device's part of the policy report (values already read; nothing is sent).
func (h *handler) policyOf(r *http.Request, v view, id model.DeviceID) *policyCard {
	if h.d.Policy == nil {
		return nil
	}
	ctx := r.Context()
	rep, err := h.d.Policy.Report(ctx, actor(r), false)
	if err != nil {
		h.log.WarnContext(ctx, "device overview: policy report", "err", err)
		return nil
	}
	c := &policyCard{Rules: len(rep.Rules), CanFix: v.IsAdmin && h.d.PolicyFix != nil}
	for _, f := range rep.Findings {
		switch {
		case f.DeviceID != id:
		case f.Unknown:
			c.Unknown++
		default:
			c.Deviations = append(c.Deviations, deviation{Rule: ruleTitle(f.Rule), Have: f.Rule.Command + " " + f.Have, Want: f.Rule.Want})
		}
	}
	c.CanFix = c.CanFix && len(c.Deviations) > 0
	return c
}

// ruleTitle is a rule's name, or its setting when it has none.
func ruleTitle(r policy.Rule) string {
	if r.Name != "" {
		return r.Name
	}
	return r.Command + " " + r.Want
}

func (h *handler) deviceProbe(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	v := h.view(r, "", "", nil)
	var notice, failure string
	err := h.d.Prober.Refresh(r.Context(), d.ID, actor(r))
	var attention interface{ Notice() string }
	if errors.As(err, &attention) {
		err = nil // refreshed, with one thing to tell the admin (#111)
	}
	if err != nil {
		failure = h.refreshProblem(r, err)
	} else {
		notice = "Identity refreshed."
		if attention != nil {
			notice += " " + attention.Notice()
		}
		if fresh, gerr := h.d.Inventory.Get(r.Context(), d.ID); gerr == nil {
			d = fresh
		} else {
			h.log.WarnContext(r.Context(), "probe: reload device", "err", gerr)
		}
	}
	hl, running := h.deviceState(r, d)
	tabs := h.headOf(r.Context(), v, d, hl, running, "overview")
	c := h.identity(v, d, running != nil, tabs.offline(), h.factsOf(r.Context(), d))
	c.Notice, c.Error = notice, failure
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	parts := []templ.Component{identityCard(c)}
	if failure == "" {
		// The header, breadcrumb and title show the same stored device as the card (#104).
		parts = append(parts, deviceHeadSwap(tabs, deviceCrumbs(d)))
	}
	if err := templ.Join(parts...).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "probe: render", "err", err)
	}
}
