package web

import (
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
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
	CanRules   bool // the rules service exists: Import / Export / New rule; false in env mode
	// Show: tiles and tabs are drawn. Without rules and versions the page is an empty state.
	Show        bool
	HasVersions bool
	Tiles       []fleetTile
	Line        *liveLine
	Fleet       policy.Fleet
	Tabs        []ui.Tab
}

const policySub = "Settings every device should have. Nothing is written automatically."

// policyHead builds the shared header from the specs and the report of the page.
func (h *handler) policyHead(v view, specs []sysconfig.RuleSpec, rep PolicyReport, repErr error) policyHead {
	p := policyHead{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin, CanRules: h.d.PolicyRules != nil}
	disabled := 0
	for _, s := range specs {
		if s.Disabled {
			disabled++
		}
	}
	n := strconv.Itoa
	rules := fleetTile{Key: "rules", Label: "Rules", Value: n(len(specs)), Sub: n(len(specs)-disabled) + " enabled · " + n(disabled) + " disabled"}
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
			{Key: "unjudged", Label: "Not judged", Value: n(f.NotJudged), Sub: "no backup or recent reading"},
		}
	}
	p.Line = h.liveLine(rep, repErr, v.IsAdmin)
	return p
}

// policyTabs lists the tabs whose routes exist. The Deviating devices and History tabs add their
// own line when their pages exist.
func policyTabs(base, current string, rules int) []ui.Tab {
	return []ui.Tab{
		{Label: "Rules", Href: base + policyPath, Current: current == "rules", Badge: rules},
	}
}
