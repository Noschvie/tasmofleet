package web

import (
	"strconv"
	"time"

	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The header the policy tabs share: page header with New rule, the summary
// (Rules and Deviating devices only: one line of fleet counts and the live check) and the tab row.
// The counts are policy.FleetTally of the one evaluation; a failed live check leaves them out and
// says so in the live-check part.

// fleetSum is the summary line "52 of 60 compliant · 3 deviating · 6 not judged".
type fleetSum struct {
	Compliant, Deviating, NotJudged string
	DeviatingTone                   ui.Tone
}

// policyHead is what policyShell draws.
type policyHead struct {
	Base, CSRF string
	Admin      bool
	// Show: summary and tabs are drawn. Without rules and versions the page is an empty state.
	Show        bool
	HasVersions bool
	// Counts: the summary line with the fleet counts and the live check is drawn. Only the pages that are
	// about the rules' results (Rules, Deviating devices) set it; History and the GPIO templates have
	// nothing to do with a rule count (#1086). Sum is empty when the live check failed.
	Counts bool
	Sum    *fleetSum
	Line   *liveLine
	Fleet  policy.Fleet
	Tabs   []ui.Tab
	// Back: the tab Run now returns to ("" = the overview).
	Back string
	// Cooldown names the devices the live check just left alone because they were read recently (#989): at
	// most cooldownNamed of them, CooldownMore counts the rest. Now is the page's clock for the day-relative times.
	Cooldown     []cooldownHold
	CooldownMore int
	Now          time.Time
}

// cooldownHold is one device the per-device cooldown held back and the time its next read is possible.
type cooldownHold struct {
	Name  string
	Until time.Time
	Sep   string // "," when another entry or the "and N more" follows
}

// cooldownNamed is how many held devices the note names; a fleet-wide hold would otherwise be a wall of text.
const cooldownNamed = 5

// liveCooldownText is the length of the cooldown in words ("5 minutes").
func liveCooldownText() string { return longDuration(LiveDeviceCooldown) }

// cooldownHolds lists the held devices by the order of the report's devices.
func cooldownHolds(rep PolicyReport) []cooldownHold {
	var out []cooldownHold
	for _, d := range rep.Devices {
		if at, ok := rep.Cooldown[d.ID]; ok {
			out = append(out, cooldownHold{Name: d.Name, Until: at})
		}
	}
	return out
}

const policySub = "Settings every device should have. You decide when they are applied."

// policyHead builds the shared header from the report of the page.
func (h *handler) policyHead(v view, rep PolicyReport, repErr error) policyHead {
	p := policyHead{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin}
	if repErr == nil {
		f := policy.FleetTally(rep.Rules, rep.Devices, rep.Outcomes)
		p.Fleet = f
		n := strconv.Itoa
		p.Sum = &fleetSum{Compliant: n(f.Compliant) + " of " + n(f.Devices), Deviating: n(f.Deviating), NotJudged: n(f.NotJudged)}
		if f.Deviating > 0 {
			p.Sum.DeviatingTone = ui.ToneWarn
		}
	}
	p.Line = h.liveLine(rep, repErr, v.IsAdmin)
	if repErr == nil {
		p.Cooldown, p.Now = cooldownHolds(rep), h.d.Clock.Now()
		if len(p.Cooldown) > cooldownNamed {
			p.CooldownMore = len(p.Cooldown) - cooldownNamed
			p.Cooldown = p.Cooldown[:cooldownNamed]
		}
		for i := range p.Cooldown {
			if i < len(p.Cooldown)-1 || p.CooldownMore > 0 {
				p.Cooldown[i].Sep = ","
			}
		}
	}
	return p
}

// policyTabs lists the tabs of the policy pages. History is the version history of the rules; Import / export
// (#1088) is the last one and needs no live evaluation, so it never has a badge.
// The GPIO templates tab (#327) is there when the service is; its badge (the devices that differ)
// is only known on its own page, the other pages show the tab without one.
func (h *handler) policyTabs(base, current string, rules, deviating, templates int) []ui.Tab {
	tabs := []ui.Tab{
		{Label: "Rules", Href: base + policyPath, Current: current == "rules", Badge: rules},
		{Label: "Deviating devices", Href: base + policyDevPath, Current: current == "deviations", Badge: deviating},
	}
	if h.d.Templates != nil {
		tabs = append(tabs, ui.Tab{Label: "GPIO templates", Href: base + policyTemplatesPath, Current: current == "templates", Badge: templates})
	}
	return append(tabs,
		ui.Tab{Label: "History", Href: base + policyHistoryPath, Current: current == "history"},
		ui.Tab{Label: "Import / export", Href: base + policyIOPath, Current: current == "io"})
}
