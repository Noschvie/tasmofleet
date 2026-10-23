package web

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// cachedFW is a firmware cache whose feed is only readable from memory: a
// call to Releases (which may fetch upstream) fails the test.
type cachedFW struct {
	firmware.Cache // nil: any other call panics
	t              *testing.T
	rels           []firmware.Release
}

func (f cachedFW) CachedReleases() []firmware.Release { return f.rels }
func (f cachedFW) Releases(context.Context) ([]firmware.Release, error) {
	f.t.Error("fleet counts called Releases: a page view must not fetch the feed")
	return f.rels, nil
}

// summaryRow is a device in a state; one with a known version has answered once (its identity
// was detected), so Offline stays Offline and is not "never answered" (shownState).
func summaryRow(id model.DeviceID, st model.HealthState, version string) deviceRow {
	idn := model.Identity{Version: version}
	if version != "" {
		idn.DetectedAt = time.Unix(1, 0)
	}
	return deviceRow{Device: model.Device{ID: id, Identity: idn}, Health: model.Health{DeviceID: id, State: st}}
}

// Why (design v2, N7): the tiles and chips must count what their filters show.
// Unknown (never answered) is its own tile, not part of Offline, but the
// "Offline / unknown" chip covers both; "Firmware behind" uses the cached feed
// and never fetches it; an unreadable policy value is no deviation; proposals
// split into found-but-not-adopted and address changes, conflicts in neither.
func TestFleetSummaryCountsWhatTheFiltersShow(t *testing.T) {
	rule := policy.Rule{Command: "TelePeriod", Want: "300"}
	rule2 := policy.Rule{Command: "SetOption0", Want: "0"}
	h := &handler{log: slog.New(slog.NewTextHandler(io.Discard, nil)), d: Deps{
		Firmware: cachedFW{t: t, rels: []firmware.Release{{Version: "14.2.0"}, {Version: "14.3.0"}}},
		Policy: &fakePolicy{rep: PolicyReport{Findings: []policy.Finding{
			{DeviceID: 1, Rule: rule, Have: "60"},
			{DeviceID: 1, Rule: rule2, Have: "1"},
			{DeviceID: 2, Rule: rule, Have: "120"},
			{DeviceID: 3, Rule: rule, Unknown: true},
		}}},
		Discovery: &fakeDiscovery{props: []discovery.Proposal{
			{Kind: discovery.KindNew}, {Kind: discovery.KindNew}, {Kind: discovery.KindNew},
			{Kind: discovery.KindAddressChange}, {Kind: discovery.KindAddressChange}, {Kind: discovery.KindConflict},
		}},
	}}
	rows := []deviceRow{
		summaryRow(1, model.HealthOnline, "14.3.0"),
		summaryRow(2, model.HealthOnline, "14.2.0"),
		summaryRow(3, model.HealthOffline, "13.4.0"),
		summaryRow(4, model.HealthUnknown, ""), // no version: not "behind", nothing known
		summaryRow(5, model.HealthStale, "14.3.0"),
		summaryRow(6, model.HealthAuthFailed, "14.2.0"),
	}
	facts := h.fleetFacts(context.Background(), "test")
	facts.annotate(rows)
	s := h.fleetSummaryFor(context.Background(), view{IsAdmin: true}, rows, facts)

	if s.Total != 6 || s.Online != 2 || s.Offline != 1 || s.Unknown != 1 || s.States[model.HealthStale] != 1 || s.States[model.HealthAuthFailed] != 1 {
		t.Errorf("states: %+v", s)
	}
	if s.Latest != "14.3.0" || s.Outdated != 3 {
		t.Errorf("outdated = %d against %q, want 3 against 14.3.0 (2, 3 and 6)", s.Outdated, s.Latest)
	}
	if !s.HasPolicy || s.Deviating != 2 || s.DeviatingRules != 2 {
		t.Errorf("policy: %d devices, %d rules, want 2 and 2 (device 3's unknown value is no deviation)", s.Deviating, s.DeviatingRules)
	}
	if !s.HasDiscovery || s.NewFound != 3 || s.AddressChanged != 2 {
		t.Errorf("discovery: %d new, %d moved", s.NewFound, s.AddressChanged)
	}
	want := map[string]int{"": 6, chipDown: 2, chipOutdated: 3, chipNonCompliant: 2}
	for _, c := range s.Chips {
		if c.N != want[c.Key] {
			t.Errorf("chip %q (%s) = %d, want %d", c.Key, c.Label, c.N, want[c.Key])
		}
		n := 0
		for _, r := range rows {
			if (deviceFilter{Chip: c.Key}).match(r) {
				n++
			}
		}
		if n != c.N {
			t.Errorf("chip %q counts %d but its filter shows %d", c.Key, c.N, n)
		}
	}
	if rows[0].Deviations != 2 || rows[1].Outdated != true || rows[0].Outdated {
		t.Errorf("rows are not annotated for the badges: %+v %+v", rows[0], rows[1])
	}

	// Proposals are admin-only: an operator gets no discovery numbers.
	if s := h.fleetSummaryFor(context.Background(), view{}, rows, facts); s.HasDiscovery || s.NewFound != 0 {
		t.Errorf("operator sees discovery counts: %+v", s)
	}
}

// Why: without a loaded feed, "Firmware behind" is unknown, not zero: no
// device may be marked outdated against an empty version.
func TestFleetSummaryWithoutFeedOrPolicy(t *testing.T) {
	h := &handler{log: slog.New(slog.NewTextHandler(io.Discard, nil)), d: Deps{Firmware: cachedFW{t: t}}}
	rows := []deviceRow{summaryRow(1, model.HealthOnline, "1.0.0")}
	facts := h.fleetFacts(context.Background(), "test")
	facts.annotate(rows)
	s := summarize(rows, facts, nil, false)
	if s.Latest != "" || s.Outdated != 0 || rows[0].Outdated || s.HasPolicy {
		t.Errorf("%+v", s)
	}
}

// Why: the chip is a URL filter (?chip=down) so the dashboard tiles can link
// to the filtered list, and it survives in the sort links.
func TestDevicesChipFilter(t *testing.T) {
	e := setup(t) // kitchen is online, attic has no health: unknown
	_, body := e.do(t, "GET", "/devices?chip=down", "operator", nil)
	if !strings.Contains(body, `/devices/2"`) || strings.Contains(body, `/devices/1"`) {
		t.Fatalf("chip=down must show the unknown attic only")
	}
	if !strings.Contains(body, "chip=down") {
		t.Fatal("the chip is lost from the sort links")
	}
	_, body = e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `/devices/2"`) || !strings.Contains(body, `/devices/1"`) {
		t.Fatal("without a chip both devices show")
	}
}
