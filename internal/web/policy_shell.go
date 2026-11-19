package web

import (
	"strconv"
	"time"

	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The header the three policy tabs share: page header with Import / Export / New rule, the five
// tiles (rules, compliant, deviating, not judged, live check) and the tab row. The tile numbers
// are policy.FleetTally of the one evaluation; a failed live check leaves the fleet tiles blank
// and says so in the live-check tile.

// fleetTile is one of the four fleet tiles.
type fleetTile struct {
	Key, Label, Value, Sub string
	Tone                   ui.Tone
}

// policyHead is what policyShell draws.
type policyHead struct {
	Base, CSRF string
	Admin      bool
	// Show: tiles and tabs are drawn. Without rules and versions the page is an empty state.
	Show        bool
	HasVersions bool
	Tiles       []fleetTile
	Line        *liveLine
	Fleet       policy.Fleet
	Tabs        []ui.Tab
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

const policySub = "Settings every device should have. Nothing is written automatically."

// policyHead builds the shared header from the valid specs (invalid: how many stored rules were left
// out) and the report of the page.
func (h *handler) policyHead(v view, specs []sysconfig.RuleSpec, invalid int, rep PolicyReport, repErr error) policyHead {
	p := policyHead{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin}
	disabled := 0
	for _, s := range specs {
		if s.Disabled {
			disabled++
		}
	}
	n := strconv.Itoa
	rules := fleetTile{Key: "rules", Label: "Rules", Value: n(len(specs)), Sub: n(len(specs)-disabled) + " enabled · " + n(disabled) + " disabled"}
	if invalid > 0 {
		rules.Sub += " · " + n(invalid) + " invalid" // not counted above: they are not evaluated
	}
	if repErr != nil {
		dash := func(key, label string) fleetTile {
			return fleetTile{Key: key, Label: label, Value: "–", Sub: "needs the live check"}
		}
		p.Tiles = []fleetTile{rules, dash("compliant", "Compliant devices"), dash("deviating", "Deviating"), dash("unjudged", "Not judged")}
	} else {
		f := policy.FleetTally(rep.Rules, rep.Devices, rep.Outcomes)
		p.Fleet = f
		deviating := fleetTile{Key: "deviating", Label: "Deviating", Value: n(f.Deviating), Sub: n(f.DeviatingRequired) + " required · " + n(f.DeviatingAdvisory) + " advisory"}
		if f.Deviating > 0 {
			deviating.Tone = ui.ToneWarn
		}
		p.Tiles = []fleetTile{
			rules,
			{Key: "compliant", Label: "Compliant devices", Value: n(f.Compliant) + " / " + n(f.Devices), Sub: "all required rules met", Tone: ui.ToneOK},
			deviating,
			{Key: "unjudged", Label: "Not judged", Value: n(f.NotJudged), Sub: "no reading, or firmware cannot answer"},
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

// policyTabs lists the tabs of the policy pages. History is the version history of the rules.
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
	return append(tabs, ui.Tab{Label: "History", Href: base + policyHistoryPath, Current: current == "history"})
}
