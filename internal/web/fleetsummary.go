package web

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/model"
)

// Fleet counts (design v2, N7): the dashboard's six stat tiles and the device
// list's filter chips. Everything is counted in one pass over the rows the
// pages already build from the health cache; there is no query per device.

// Device list chips, also the values of the ?chip= filter.
const (
	chipOutdated     = "outdated"     // a newer stable release exists
	chipNonCompliant = "noncompliant" // at least one policy deviation
	chipNoBackup     = "nobackup"     // no backup run yet (#381; the Devices, Backups tile)
)

// The words of one fact, the same on the dashboard's tile, the list's chip and the dashboard's group (#1109): one name,
// not "Firmware behind" on one page and "Outdated" on the next.
const (
	wordUpdateAvailable   = "Update available"
	wordDiffersFromPolicy = "Differs from policy"
	wordNotCheckedYet     = "Not checked yet"
)

// notChecked is "6 not checked yet": how many devices have a rule value that is not read yet.
func notChecked(n int) string { return strconv.Itoa(n) + " " + strings.ToLower(wordNotCheckedYet) }

// policyHint is the hint of the policy tile when something was read: "3 devices differ from 2 rules; 6 not checked
// yet". With no deviation it says so, and the part about the rules that were not read stays, because a 0 beside
// unread values is only a lower bound.
func policyHint(s fleetSummary) string {
	hint := "No device differs from a rule"
	if s.Deviating > 0 {
		hint = fmt.Sprintf("%d %s %s from %d %s", s.Deviating, noun(s.Deviating, "device", "devices"), noun(s.Deviating, "differs", "differ"), s.DeviatingRules, noun(s.DeviatingRules, "rule", "rules"))
	}
	if s.PolicyUnchecked > 0 { // partly read: the known count is a lower bound
		hint += "; " + notChecked(s.PolicyUnchecked)
	}
	return hint
}

// chipCount is one filter chip with the number of devices it would show.
type chipCount struct {
	Key   string // "" = All, else one of the chip constants
	Label string
	N     int
	// Unchecked: devices whose rule values are not read yet. Only the policy chip sets it; the
	// count N is then a lower bound (shown as the number, "n not checked yet" in the tooltip),
	// or unknown ("–") when nothing was read at all.
	Unchecked int
	Unread    bool
}

// Text is the count as shown: the number, "–" when no device is checked yet (the same rule as
// the dashboard's policy tile: never claim 0 for what is unknown). While only part of the fleet
// is checked the number is the known count and Title says how many are not checked yet, as the
// tile's hint does ("0+" read oddly, v0.11.1 feedback).
func (c chipCount) Text() string {
	if c.Unread {
		return "–"
	}
	return strconv.Itoa(c.N)
}

// Title is the hover text explaining an uncertain count, "" when the count is exact.
func (c chipCount) Title() string {
	if c.Unread || c.Unchecked > 0 {
		return strconv.Itoa(c.Unchecked) + " not checked yet"
	}
	return ""
}

// fleetFacts is what the chips and tiles need beyond the health cache: the
// newest release of the cached feed and the policy deviations per device.
type fleetFacts struct {
	Latest     string                 // "" when the feed is not loaded (outdated is then unknown, not 0)
	Deviations map[model.DeviceID]int // values that differ from a rule; unreadable values do not count
	Rules      map[model.DeviceID]map[string]bool
	Details    map[model.DeviceID][]string // "TelePeriod 60 → 300" per deviation, in report order
	HasPolicy  bool                        // a policy report was available
	// PolicyUnread: values are wanted but none is known, live or from a backup (live values are
	// in memory only, so this is the state after a restart until the first background read).
	PolicyUnread bool
	// PolicyUnchecked counts the devices with at least one rule whose value is not read yet.
	PolicyUnchecked int
}

// fleetSummary is the counts of one view of the fleet.
type fleetSummary struct {
	Total int
	// Online, Offline and Unknown are the tiles; States has every state, so
	// Stale and AuthFailed (shown in "Needs attention") are counted too. Unknown
	// is "never answered since start", separate from Offline.
	Online, Offline, Unknown int
	States                   map[model.HealthState]int
	Latest                   string // the release "Firmware behind" compares with; "" hides the tile's number
	Outdated                 int
	HasPolicy                bool
	Deviating                int // devices with at least one deviation
	PolicyUnread             bool
	PolicyUnchecked          int // devices with at least one rule not read yet
	DeviatingRules           int // distinct rules those deviations break
	// HasDiscovery is true for admins with discovery switched on: proposals
	// are admin-only. NewFound counts devices found but not adopted (kind
	// "new"), AddressChanged the known devices that announced a new address.
	HasDiscovery    bool
	NewFound        int
	NewFoundOffline int // of NewFound, those whose LWT says Offline
	AddressChanged  int
	Chips           []chipCount
	// HasBackups: the backup service reports usage, so NoBackup (the "No backup" chip's count) is
	// known. Only the dashboard sets them (#438).
	HasBackups bool
	NoBackup   int
	// BackupPage: Devices -> Backups exists, so the tile links there; BackupRun is the last scheduled
	// run (nil without one) and Now the time its hint is worded against (#409).
	BackupPage bool
	BackupRun  *model.BackupRun
	Now        time.Time
}

// fleetFacts reads the cached feed and the policy report. Neither talks to a
// device or to upstream: CachedReleases never fetches and the report is built
// from values read in the background and from backups (live = false).
func (h *handler) fleetFacts(ctx context.Context, actor string) fleetFacts {
	var f fleetFacts
	if cl, ok := h.d.Firmware.(firmware.CachedLister); ok {
		f.Latest = firmware.Latest(cl.CachedReleases())
	}
	if h.d.Policy != nil {
		rep, err := h.d.Policy.Report(ctx, actor, false)
		if err != nil {
			h.log.WarnContext(ctx, "fleet counts: policy report", "err", err)
			return f
		}
		f.HasPolicy = true
		f.Deviations = map[model.DeviceID]int{}
		f.Rules = map[model.DeviceID]map[string]bool{}
		f.Details = map[model.DeviceID][]string{}
		unknown, known := 0, 0
		unchecked := map[model.DeviceID]bool{}
		for _, vs := range rep.Provenance {
			known += len(vs)
		}
		for _, x := range rep.Findings {
			if x.Unknown {
				unknown++
				unchecked[x.DeviceID] = true
				continue // not readable is not a deviation (and not compliance either)
			}
			known++
			f.Deviations[x.DeviceID]++
			if f.Rules[x.DeviceID] == nil {
				f.Rules[x.DeviceID] = map[string]bool{}
			}
			f.Rules[x.DeviceID][x.Rule.Command+"\x00"+x.Rule.Want] = true
			f.Details[x.DeviceID] = append(f.Details[x.DeviceID], x.Rule.Command+" "+x.Have+" → "+x.Rule.Want)
		}
		f.PolicyUnread = unknown > 0 && known == 0
		f.PolicyUnchecked = len(unchecked)
	}
	return f
}

// annotate sets the per-row facts the chips filter on (and the list shows as
// the "update" and "N deviations" badges).
func (f fleetFacts) annotate(rows []deviceRow) {
	for i := range rows {
		rows[i].Outdated = firmware.UpdateAvailable(f.Latest, rows[i].Device.Identity)
		if rows[i].Outdated {
			rows[i].UpdateTo = f.Latest
		}
		rows[i].Deviations = f.Deviations[rows[i].Device.ID]
	}
}

// chipMatch reports whether an annotated row belongs to a chip.
func chipMatch(chip string, r deviceRow) bool {
	switch chip {
	case chipOutdated:
		return r.Outdated
	case chipNonCompliant:
		return r.Deviations > 0
	case chipNoBackup:
		return r.NoBackup
	}
	return true
}

var deviceChips = []chipCount{
	{Key: "", Label: "All"},
	{Key: chipOutdated, Label: wordUpdateAvailable},
	{Key: chipNonCompliant, Label: wordDiffersFromPolicy},
	{Key: chipNoBackup, Label: "No backup"},
}

// summarize counts annotated rows in one pass. props are the visible
// discovery proposals (nil when the viewer may not see them).
func summarize(rows []deviceRow, f fleetFacts, props []discovery.Proposal, withDiscovery bool) fleetSummary {
	s := fleetSummary{Total: len(rows), States: map[model.HealthState]int{}, Latest: f.Latest, HasPolicy: f.HasPolicy, PolicyUnread: f.PolicyUnread, PolicyUnchecked: f.PolicyUnchecked, HasDiscovery: withDiscovery}
	s.Chips = append([]chipCount(nil), deviceChips...)
	rules := map[string]bool{}
	for _, r := range rows {
		s.States[r.Health.State]++ // rows carry shownHealth: the one never-answered rule (devices.go)
		if r.Outdated {
			s.Outdated++
		}
		if r.Deviations > 0 {
			s.Deviating++
			for k := range f.Rules[r.Device.ID] {
				rules[k] = true
			}
		}
		for i := range s.Chips {
			if chipMatch(s.Chips[i].Key, r) {
				s.Chips[i].N++
			}
		}
	}
	s.Online, s.Offline, s.Unknown = s.States[model.HealthOnline], s.States[model.HealthOffline], s.States[model.HealthUnknown]
	s.DeviatingRules = len(rules)
	for i := range s.Chips {
		if s.Chips[i].Key == chipNonCompliant {
			s.Chips[i].Unread, s.Chips[i].Unchecked = s.PolicyUnread, s.PolicyUnchecked
		}
	}
	for _, p := range props {
		switch p.Kind {
		case discovery.KindNew:
			s.NewFound++
			if p.LWT == "Offline" {
				s.NewFoundOffline++
			}
		case discovery.KindAddressChange:
			s.AddressChanged++
		}
	}
	return s
}

// fleetSummaryFor builds the summary for the requesting user from rows the
// caller has (the dashboard and the list both have them already).
func (h *handler) fleetSummaryFor(ctx context.Context, v view, rows []deviceRow, f fleetFacts) fleetSummary {
	var props []discovery.Proposal
	withDiscovery := v.IsAdmin && h.d.Discovery != nil
	if withDiscovery {
		var err error
		if props, err = h.d.Discovery.Proposals(ctx); err != nil {
			h.log.WarnContext(ctx, "fleet counts: discovery proposals", "err", err)
			withDiscovery = false
		}
	}
	return summarize(rows, f, props, withDiscovery)
}
