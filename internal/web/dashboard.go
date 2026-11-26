package web

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/stability"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The dashboard (design v2 screen "dashboard", WP P1): six stat tiles, the fleet health bar,
// "Needs attention" and the update transactions. The counts are N7's fleetSummary.

const (
	// dashGroupMax is how many devices one reason of "Needs attention" lists (#1109): a glance, the group says how many
	// it leaves out and the device list has them.
	dashGroupMax = 5
	// dashProposalsMax is how many discovery proposals it lists; the "New devices found" tile
	// leads to all of them.
	dashProposalsMax = 5
	// dashTxnMax is how many transactions the dashboard lists: the active ones as cards, then the finished ones as rows.
	dashTxnMax = 4
)

// dashView is what the dashboard page draws.
type dashView struct {
	Base, CSRF string
	Total      int
	Sub        string
	// AddHref is the "Add devices" button (admins); empty hides it. NewUpdate likewise.
	AddHref, NewUpdate string
	Tiles              []ui.StatTileData
	OK, Off, Unk       int // the health bar
	// AttTitle is the heading of "Needs attention" with the number of its entries; Att its content (#1109).
	AttTitle string
	Att      ui.AttentionData
	// TxnCard is shown when updates are on or transactions exist; Cards are the transactions still at work,
	// Rows the finished ones (#1199).
	TxnCard bool
	Cards   []ui.TxnData
	Rows    []ui.TxnRowData
	Plans   ui.PlanSummary // the plans waiting for their start (#707); N 0 shows nothing
	// Empty: no devices. The page then shows only the empty state: for an admin one button, Add devices (AddHref), for
	// everyone else the words that say who adds them.
	Empty bool
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	seq := h.eventSeq() // before anything is read (#160)
	// An admin on a fresh install lands in the setup until it is finished or skipped.
	if p, _ := auth.FromContext(r.Context()); h.d.Setup != nil && h.d.SysConfig != nil && p.Role >= model.RoleAdmin && !h.d.Setup.Done() {
		redirect(w, r, setupPath)
		return
	}
	rows, err := h.rows(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, "Dashboard", "dashboard", nil)
	v.EventSeq = seq
	v.Live = true // the body reloads on state changes (TestDashboardIsLive)
	facts := h.fleetFacts(r.Context(), actor(r))
	facts.annotate(rows)
	h.annotateStability(r.Context(), rows)
	_, hasUsage := h.d.Backups.(BackupStorage)
	if hasUsage {
		// The tile is not worth a failed dashboard: without the numbers it is left out.
		if err := h.annotateBackups(r.Context(), rows); err != nil {
			h.log.Warn("dashboard: backup usage", "err", err)
			hasUsage = false
		}
	}
	s := h.fleetSummaryFor(r.Context(), v, rows, facts)
	if hasUsage {
		s.HasBackups = true
		for _, c := range s.Chips {
			if c.Key == chipNoBackup {
				s.NoBackup = c.N
			}
		}
	}
	if s.HasBackups && h.d.BackupFleet != nil && h.d.SysConfig != nil {
		s.BackupPage, s.Now = true, h.d.Clock.Now()
		if run, ok, err := h.d.BackupFleet.LastRun(r.Context()); err != nil {
			h.log.Warn("dashboard: last backup run", "err", err)
		} else if ok {
			s.BackupRun = &run
		}
	}
	var txs []txView
	var plans ui.PlanSummary
	if h.d.Jobs != nil {
		// Enough jobs to cover the last few transactions, however many devices each has.
		jobs, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{Limit: 500})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		txs = groupTransactions(jobs)
		// A plan is not a transaction of the card: the line above it says how many wait (#707, planSummary).
		txs = slices.DeleteFunc(txs, func(t txView) bool { return planWaits(t.Planned, t.Running+t.Queued) })
		// Own read: the list above is capped and says nothing for sure of the plans. A failed read drops the line.
		if open, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{States: openJobStates}); err != nil {
			h.log.Warn("dashboard: plans", "err", err)
		} else if plans = planSummary(h.d.Clock.Now(), open); plans.N > 0 {
			plans.Href = scheduledHref(v.Base)
		}
	}
	var props []discovery.Proposal
	if s.HasDiscovery {
		// fleetSummaryFor read them already; a second read is the same list (no device I/O).
		if props, err = h.d.Discovery.Proposals(r.Context()); err != nil {
			props = nil
		}
	}
	devs := map[model.DeviceID]model.Device{}
	for _, row := range rows {
		devs[row.Device.ID] = row.Device
	}
	d := dashView{Base: v.Base, CSRF: v.CSRF, Total: len(rows), Empty: len(rows) == 0}
	d.Sub = fmt.Sprintf("%d %s", len(rows), noun(len(rows), "device", "devices"))
	if v.IsAdmin {
		d.AddHref = v.Base + "/devices/new" // the page that carries the ways to add (scan, by hand, import)
		// Offered with devices only, as in the prototype: with updates switched off the page explains what to
		// configure (FIRMWARE_BASE_URL) instead of the button just missing. A fleet without devices has nothing to
		// update, and its one first action is Add devices (#1109).
		d.NewUpdate = v.Base + "/updates/new"
	}
	d.Tiles = dashTiles(v.Base, s)
	d.OK, d.Off, d.Unk = s.Online, s.Offline, s.Unknown
	now := h.d.Clock.Now()
	d.AttTitle, d.Att = dashNeedsAttention(v.Base, v.CSRF, now, rows, facts, h.channelWindow(), h.brokerACLOpen(), props, devs)
	d.TxnCard = v.Features.Transactions || len(txs) > 0 || plans.N > 0
	d.Plans = plans
	d.Cards, d.Rows = dashTxnEntries(v.Base, now, txs)
	h.show(w, r, http.StatusOK, "dashboard", v, dashboardPage(d))
}

// channelWindow is how recent a channel's last contact must be to count as working: the
// configured stale threshold (the same window as the status cell's MQTT/HTTP marks).
func (h *handler) channelWindow() time.Duration {
	if h.d.StaleAfter != nil {
		return h.d.StaleAfter()
	}
	return 11 * time.Minute // the health default (2 x TelePeriod + 60 s)
}

func itoa(n int) string { return strconv.Itoa(n) }

// noun is the word for a count without the number: "device" or "devices".
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// dashTiles are the prototype's six tiles and "No backup" (#438); the policy tile needs a policy report and the
// discovery tile is for admins with discovery on. Each links to the list that shows what it counts.
func dashTiles(base string, s fleetSummary) []ui.StatTileData {
	chip := func(c string) string { return base + "/devices?chip=" + c }
	tiles := []ui.StatTileData{
		{Tone: ui.ToneOK, Label: "Online", Value: itoa(s.Online), Hint: "of " + itoa(s.Total) + " " + noun(s.Total, "device", "devices"), Href: base + "/devices?health=" + string(model.HealthOnline)},
		{Tone: ui.ToneDanger, Label: "Offline", Value: itoa(s.Offline), Hint: "not reachable", Href: chip(chipDown)},
		{Tone: ui.ToneNeutral, Label: "Unknown", Value: itoa(s.Unknown), Hint: "never answered", Href: chip(chipDown)},
	}
	fw := ui.StatTileData{Tone: ui.ToneAccent, Label: wordUpdateAvailable, Value: itoa(s.Outdated), Hint: "to " + s.Latest, Href: chip(chipOutdated)}
	if s.Latest == "" { // no release list yet: unknown, never "0 behind"
		fw.Value, fw.Hint = "–", "no release list yet"
	}
	tiles = append(tiles, fw)
	if s.HasPolicy {
		pol := ui.StatTileData{Tone: ui.ToneWarn, Label: wordDiffersFromPolicy, Value: itoa(s.Deviating), Hint: policyHint(s), Href: chip(chipNonCompliant)}
		if s.PolicyUnread { // no value read yet (e.g. just restarted): unknown, never "0 deviations"
			pol.Value, pol.Hint = "–", wordNotCheckedYet
		}
		tiles = append(tiles, pol)
	}
	if s.HasBackups {
		// Accent, warn only when the last scheduled run really failed for a device: a device that never
		// answered also has no backup, and a skipped one is not a failure (#438, #409).
		tile := ui.StatTileData{Tone: ui.ToneAccent, Label: "No backup", Value: itoa(s.NoBackup), Hint: "never backed up", Href: chip(chipNoBackup)}
		if s.BackupPage {
			tile.Href = base + fleetPath + "?filter=" + fleetNone
		}
		if r := s.BackupRun; r != nil {
			tile.Hint, tile.HintAt, tile.HintAtText = "Last run ", r.StartedAt, ui.StampRelativeText(s.Now, r.StartedAt)
			if r.Failed > 0 {
				tile.HintAfter = " · " + itoa(r.Failed) + " failed"
				tile.Tone = ui.ToneWarn
			}
		}
		tiles = append(tiles, tile)
	}
	if s.HasDiscovery {
		hint := "None waiting"
		switch {
		case s.AddressChanged > 0:
			hint = "+ " + itoa(s.AddressChanged) + " address changed"
		case s.NewFound > 0:
			hint = "waiting to be added"
		}
		if s.NewFoundOffline > 0 { // the list below hides them, the Discovered page shows them
			hint += " · " + itoa(s.NewFoundOffline) + " offline"
		}
		tiles = append(tiles, ui.StatTileData{Tone: ui.ToneAccent, Label: "New devices found", Value: itoa(s.NewFound), Hint: hint, Href: base + "/devices/new#card-discovered"})
	}
	return tiles
}

// viaText words where a proposal came from.
func viaText(via string) string {
	if via == discovery.ViaMQTT {
		return "MQTT discovery"
	}
	return "Network scan"
}

// dashProposals are the proposals an admin can act on from the dashboard: new devices (Ignore,
// Add) first, then address changes (Ignore, Apply). Conflicts need the discovery page. At most dashProposalsMax are
// returned; n is how many there are in all.
func dashProposals(base string, props []discovery.Proposal, devs map[model.DeviceID]model.Device) (shown []ui.AttProposal, n int) {
	var news, moves []ui.AttProposal
	act := func(p discovery.Proposal, verb string) string {
		return fmt.Sprintf("%s/discovery/proposals/%d/%s", base, p.ID, verb)
	}
	for _, p := range props {
		switch p.Kind {
		case discovery.KindNew:
			if p.LWT == "Offline" {
				continue // an offline proposal is listed on the Discovered page only (#294)
			}
			name := p.Device.Name
			if name == "" {
				name = p.Device.Address
			}
			news = append(news, ui.AttProposal{Name: name, Text: "New device · " + p.Device.Address + " · " + viaText(p.Via),
				Primary: "Add", Secondary: "Ignore", PrimaryAction: act(p, "accept"), SecondaryAction: act(p, "dismiss"), Version: p.Version})
		case discovery.KindAddressChange:
			cur, ok := devs[p.ExistingID]
			if !ok {
				continue
			}
			moves = append(moves, ui.AttProposal{Name: cur.Name, Text: "Address changed · " + cur.Address + " → " + p.Device.Address,
				Primary: "Apply", Secondary: "Ignore", PrimaryAction: act(p, "accept"), SecondaryAction: act(p, "dismiss"), Version: p.Version})
		}
	}
	out := append(news, moves...)
	return out[:min(len(out), dashProposalsMax)], len(out)
}

// dashNeedsAttention is the card "Needs attention" (#1109, #1247): its heading with the number of entries, and its
// content. The number counts every entry the card stands for, each once: the proposals (also those it does not show),
// every device of every group (also those it does not show) and the broker item.
func dashNeedsAttention(base, csrf string, now time.Time, rows []deviceRow, f fleetFacts, window time.Duration, acl bool, props []discovery.Proposal, devs map[model.DeviceID]model.Device) (string, ui.AttentionData) {
	a := ui.AttentionData{CSRF: csrf}
	var nProps int
	a.Props, nProps = dashProposals(base, props, devs)
	if nProps > len(a.Props) {
		a.PropsMore = ui.AttMore{Text: "Show all " + itoa(nProps), Href: base + "/devices/new#card-discovered", Label: "Show all " + itoa(nProps) + ": New devices"}
	}
	var nDevices int
	a.Groups, nDevices = dashAttention(base, now, rows, f, window, acl)
	n := nProps + nDevices
	if acl {
		n++
	}
	title := "Needs attention"
	if n > 0 {
		title += " (" + itoa(n) + ")"
		a.Note = attentionNote
	}
	return title, a
}

// attentionNote is the one line under the heading: what the list holds, and what it leaves to the tiles.
const attentionNote = "Each device once, for its worst reason. Firmware and backups are on the tiles."

// attReasons are the reasons a device is listed for, worst first (the order of the card): its group's title and,
// where the device list can filter by it, the filter. A group that has no filter says "N more" instead of leading to
// a list that does not show what it counts.
var attReasons = [...]struct {
	title  func() string
	filter deviceFilter
}{
	{func() string { return label(model.HealthOffline) }, deviceFilter{Health: string(model.HealthOffline)}},
	{func() string { return label(model.HealthUnknown) }, deviceFilter{Health: string(model.HealthUnknown)}},
	{func() string { return label(model.HealthAuthFailed) }, deviceFilter{Health: string(model.HealthAuthFailed)}},
	{func() string { return label(model.HealthStale) }, deviceFilter{Health: string(model.HealthStale)}},
	{stability.BootLoop.Label, deviceFilter{}},
	{stability.WeakWiFi.Label, deviceFilter{}},
	{func() string { return wordDiffersFromPolicy }, deviceFilter{Chip: chipNonCompliant}},
}

// Ranks of attReasons.
const (
	rankOffline = iota
	rankUnknown
	rankAuthFailed
	rankStale
	rankBootLoop
	rankWeakWiFi
	rankPolicy
)

// dashAttention groups the devices that need a look by their worst reason: offline, never answered, refusing the
// login, silent, a boot loop, weak Wi-Fi, then policy deviations. A group shows at most dashGroupMax devices, in the
// order they were added, and says how many more its list holds: "Show all N" where the device list can filter by the
// reason (N counted by the list's own matcher, so the link says what the list will show), else "N more". The second
// result is how many devices the groups hold in all.
// With acl (brokerACLOpen) the broker ACL item is a group of its own right after the offline devices, once (#1078): it
// is not a device, so it does not count against any cap, or a fleet with many offline devices would push it off the
// list.
func dashAttention(base string, now time.Time, rows []deviceRow, f fleetFacts, window time.Duration, acl bool) ([]ui.AttGroup, int) {
	type cand struct {
		rank int
		id   model.DeviceID
		it   ui.AttItem
	}
	var cs []cand
	for _, r := range rows {
		it := ui.AttItem{Name: r.Device.Name, Href: fmt.Sprintf("%s/devices/%d", base, r.Device.ID)}
		rank := -1
		switch shownState(r) {
		case model.HealthOffline:
			// the badge says Offline, so the text only says since when
			rank = rankOffline
			if seen := lastContact(r.Device, r.Health); !seen.IsZero() {
				it.Text = "Last seen " + agoWords(now, seen)
			}
		case model.HealthUnknown:
			rank, it.Text = rankUnknown, "Has not answered since it was added"
		case model.HealthAuthFailed:
			rank, it.Text = rankAuthFailed, "Refuses the stored web login"
		case model.HealthStale:
			rank = rankStale
			it.Text = "No message since " + agoWords(now, r.Health.LastSeen)
			if ch := channelsOf(now, window, r.Health); len(ch) == 2 && ch[0].Tone == "bad" && ch[1].Tone == "ok" {
				it.Text = "MQTT unreachable · HTTP works"
			}
		case model.HealthOnline:
			// answers: only a policy deviation can put it on the list (below)
		}
		if rank >= 0 {
			// the colour of a connection problem is the badge's, from the one state table (#1097)
			b := statusBadgeOf(shownState(r), "")
			it.Status = &b
		}
		if sp := stabProblem(r.Stab); rank < 0 && sp != nil {
			rank, it.Tone, it.Text = rankBootLoop, ui.ToneDanger, stabAttention(*sp)
			if sp.Level == stability.WeakWiFi {
				rank, it.Tone = rankWeakWiFi, ui.ToneWarn
			}
		}
		if rank < 0 && r.Deviations > 0 {
			rank, it.Tone, it.Text = rankPolicy, ui.ToneWarn, deviationText(f.Details[r.Device.ID], r.Deviations)
		}
		if rank >= 0 {
			cs = append(cs, cand{rank, r.Device.ID, it})
		}
	}
	// Within a reason, the order the devices were added in (the prototype's list order); the
	// rows come sorted by name.
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].id < cs[j].id })
	var out []ui.AttGroup
	aclAt := 0 // the broker item follows the Offline group, or leads when there is none
	for rank, reason := range attReasons {
		var items []ui.AttItem
		for _, c := range cs {
			if c.rank == rank {
				items = append(items, c.it)
			}
		}
		if len(items) == 0 {
			continue
		}
		g := ui.AttGroup{Title: reason.title(), Items: items[:min(len(items), dashGroupMax)]}
		if reason.filter != (deviceFilter{}) {
			// the list the link leads to, counted by its own matcher
			held := 0
			for _, r := range rows {
				if reason.filter.match(r) {
					held++
				}
			}
			if held > len(g.Items) {
				g.More = ui.AttMore{Text: "Show all " + itoa(held), Href: base + "/devices?" + reason.filter.values().Encode(), Label: "Show all " + itoa(held) + ": " + g.Title}
			}
		} else if len(items) > len(g.Items) {
			g.More = ui.AttMore{Text: itoa(len(items)-len(g.Items)) + " more"}
		}
		out = append(out, g)
		if rank == rankOffline {
			aclAt = len(out)
		}
	}
	if acl {
		out = slices.Insert(out, aclAt, ui.AttGroup{Title: "Broker", Items: []ui.AttItem{brokerACLItem}})
	}
	return out, len(cs)
}

// brokerACLItem is the broker ACL notice as an entry of Needs attention (#1078): the same warning, in the list's words,
// leading to the guide.
var brokerACLItem = ui.AttItem{Tone: ui.ToneWarn, Name: "MQTT broker", Text: "No ACL · any client on it can read device passwords and backups", Href: ui.BrokerACLDocURL}

// lastContact is when the device last answered: in this run, else when its identity was last
// read (stored, so it survives a restart). Zero when it never answered.
func lastContact(d model.Device, hl model.Health) time.Time {
	if !hl.LastSeen.IsZero() {
		return hl.LastSeen
	}
	return d.Identity.DetectedAt
}

// deviationText words a device's deviations like the prototype ("TelePeriod 60 → 300"), or
// counts them when the values are not known.
func deviationText(details []string, n int) string {
	if len(details) > 0 {
		return strings.Join(details, ", ")
	}
	return itoa(n) + " policy " + noun(n, "deviation", "deviations")
}

// dashTxnTitle names a transaction the way the prototype does: "Update to Tasmota 14.3.0", or
// what it does when it is not an update.
func dashTxnTitle(t txView) string {
	if t.Update {
		return "Update to Tasmota " + t.Release
	}
	return t.Release
}

// dashTxnActive is the one rule for a card: the transaction still has jobs at work or waiting (the rule dashResult
// words as "Running").
func dashTxnActive(t txView) bool { return t.Running+t.Queued > 0 }

// dashTxnEntries picks what the transactions card lists, newest first, dashTxnMax entries in all: every transaction
// that is still at work is a card with its progress, then the finished ones fill the rest as compact rows (#1199).
// The state decides, never the position: the newest transaction may already be over, and an older one still run.
func dashTxnEntries(base string, now time.Time, txs []txView) (cards []ui.TxnData, rows []ui.TxnRowData) {
	for _, t := range txs {
		if dashTxnActive(t) && len(cards) < dashTxnMax {
			cards = append(cards, dashTxnCard(base, now, t))
		}
	}
	for _, t := range txs {
		if !dashTxnActive(t) && len(cards)+len(rows) < dashTxnMax {
			rows = append(rows, dashTxnRow(base, now, t))
		}
	}
	return cards, rows
}

// dashTxnCard is a transaction that is still at work: its progress and when it began.
func dashTxnCard(base string, now time.Time, t txView) ui.TxnData {
	parts := []string{fmt.Sprintf("%d of %d done", t.Done, t.Total)}
	for _, x := range []struct {
		n    int
		word string
	}{{t.Failed, "failed"}, {t.Canceled, "cancelled"}, {t.Running, "in progress"}, {t.Queued, "waiting"}, {t.Planned, "planned"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.word))
		}
	}
	at := txBeganAt(t, time.Time{})
	return ui.TxnData{ID: t.ID.TxID(), Title: dashTxnTitle(t), State: t.Outcome, StateTone: t.Tone, Summary: strings.Join(parts, " · "),
		OK: t.Done, Failed: t.Failed, Active: t.Running, Total: t.Total, Href: fmt.Sprintf("%s/transactions/%d", base, t.ID),
		Started: at, StartedText: ui.StampRelativeText(now, at)}
}

// dashResult is a transaction's result in the design system's words: Succeeded, Cancelled, Failed,
// or the counts when it ended mixed ("30 succeeded · 1 failed").
func dashResult(t txView) (string, ui.Tone) {
	switch {
	case t.Running+t.Queued > 0:
		return label("running"), ui.ToneAccent
	case t.Done == t.Total:
		return label(model.JobSucceeded), ui.ToneOK
	case t.NotStarted > 0 && t.NotStarted+t.Early == t.Total:
		return "Not started", ui.ToneWarn
	case t.Canceled == t.Total:
		return label(model.JobCancelled), ui.ToneNeutral
	case t.Done == 0 && t.Canceled == 0:
		return label(model.JobFailed), ui.ToneDanger
	}
	tone := ui.ToneWarn
	if t.Failed > 0 {
		tone = ui.ToneDanger
	}
	return resultCounts(t.Done, t.Failed, t.Canceled), tone
}

func dashTxnRow(base string, now time.Time, t txView) ui.TxnRowData {
	res, tone := dashResult(t)
	at := txBeganAt(t, time.Time{})
	return ui.TxnRowData{ID: t.ID.TxID(), Title: dashTxnTitle(t), Result: res, Tone: tone, When: ui.StampRelativeText(now, at), WhenAt: at,
		Href: fmt.Sprintf("%s/transactions/%d", base, t.ID)}
}

// dashStacks is the number of card columns of the dashboard: the transactions card sits beside
// "Needs attention" when it is shown, otherwise that card takes the width alone.
func dashStacks(d dashView) int {
	if d.TxnCard {
		return 2
	}
	return 1
}
