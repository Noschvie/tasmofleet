package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/importer"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// between returns the part of s after the first from and before the next to ("" when from is
// missing), so an assertion looks at one region of the page only.
func between(s, from, to string) string {
	_, after, ok := strings.Cut(s, from)
	if !ok {
		return ""
	}
	before, _, _ := strings.Cut(after, to)
	return before
}

// dashFleet is a fleet with every case the dashboard words differently: online, offline after
// being seen, never answered, refusing the login, and a policy deviation.
func dashFleet(t *testing.T, mutate func(*Deps)) (*env, *fakeDiscovery) {
	t.Helper()
	disc := &fakeDiscovery{props: []discovery.Proposal{
		{ID: 31, Kind: discovery.KindAddressChange, Via: discovery.ViaMQTT, Version: "v31", ExistingID: 1, Device: model.Device{Name: "kitchen", Address: "10.0.0.11"}},
		{ID: 30, Kind: discovery.KindNew, Via: discovery.ViaScan, Version: "v30", Device: model.Device{Name: "Shed Light", Address: "10.0.0.88"}},
		{ID: 32, Kind: discovery.KindConflict, Via: discovery.ViaScan, Version: "v32", Device: model.Device{Name: "clash", Address: "10.0.0.2"}},
	}}
	seen := time.Now().Add(-5 * time.Hour)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = disc
		d.Policy = &fakePolicy{rep: PolicyReport{Findings: []policy.Finding{{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"}}}}
		if mutate != nil {
			mutate(d)
		}
	})
	e.inv.devs[3] = model.Device{ID: 3, Name: "hallway", Address: "10.0.0.3", Identity: model.Identity{Version: "15.0.0", DetectedAt: seen}}
	e.inv.devs[4] = model.Device{ID: 4, Name: "annex", Address: "10.0.0.4"}
	e.inv.devs[5] = model.Device{ID: 5, Name: "porch", Address: "10.0.0.5", Identity: model.Identity{DetectedAt: seen}}
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: seen}
	e.health[4] = model.Health{DeviceID: 4, State: model.HealthOffline} // polled, failed, never answered
	e.health[5] = model.Health{DeviceID: 5, State: model.HealthAuthFailed, LastSeen: seen}
	e.inv.devs[6] = model.Device{ID: 6, Name: "desk", Address: "10.0.0.6", Identity: model.Identity{DetectedAt: seen}}
	e.health[6] = model.Health{DeviceID: 6, State: model.HealthOnline, LastSeen: time.Now()} // online and compliant: nothing to do
	return e, disc
}

// Why (design v2, P1): a device that never answered is "Unknown" (prototype: "never answered",
// "Has not answered since it was added") even after its first polls failed; "Offline" is kept
// for a device that was seen and then lost. Without this the fixture's two never-reached devices
// read as Offline and the Unknown tile is always 0.
func TestFleetSummaryNeverAnsweredIsUnknown(t *testing.T) {
	seen := time.Now().Add(-time.Hour)
	rows := []deviceRow{
		{Device: model.Device{ID: 1}, Health: model.Health{DeviceID: 1, State: model.HealthOffline}},                                                            // never answered
		{Device: model.Device{ID: 2, Identity: model.Identity{DetectedAt: seen}}, Health: model.Health{DeviceID: 2, State: model.HealthOffline}},                // answered before this start
		{Device: model.Device{ID: 3}, Health: model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: seen}},                                            // answered since this start
		{Device: model.Device{ID: 4}, Health: model.Health{DeviceID: 4, State: model.HealthUnknown}},                                                            // not polled yet
		{Device: model.Device{ID: 5, Identity: model.Identity{DetectedAt: seen}}, Health: model.Health{DeviceID: 5, State: model.HealthOnline, LastSeen: seen}}, // online
	}
	s := summarize(rows, fleetFacts{}, nil, false)
	if s.Online != 1 || s.Offline != 2 || s.Unknown != 2 {
		t.Errorf("online/offline/unknown = %d/%d/%d, want 1/2/2", s.Online, s.Offline, s.Unknown)
	}
	if s.States[model.HealthOffline] != 2 || s.States[model.HealthUnknown] != 2 {
		t.Errorf("states disagree with the tiles: %v", s.States)
	}
}

// Why (design v2, P1): the six tiles are the prototype's, each a link to the list that shows
// exactly what it counts (the chip filters of N7), and the health bar draws the same three numbers.
func TestDashboardTilesLinkToTheirFilters(t *testing.T) {
	e, _ := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	// templ separates the tile's lines with a space; only the order of the parts matters here
	tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
	for _, want := range []string{
		`href="/tf/devices?health=online"`, `>Online</span><span class="stat-value">2</span><span class="stat-hint">of 6 devices</span>`,
		`href="/tf/devices?chip=down"`, `>Offline</span><span class="stat-value">1</span><span class="stat-hint">not reachable</span>`,
		`>Unknown</span><span class="stat-value">2</span><span class="stat-hint">never answered</span>`,
		`>Differs from policy</span><span class="stat-value">1</span><span class="stat-hint">1 device differs from 1 rule</span>`,
		`href="/tf/devices?chip=noncompliant"`,
		`href="/tf/devices/new#card-discovered"`, `>New devices found</span><span class="stat-value">1</span><span class="stat-hint">+ 1 address changed</span>`,
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("tiles: missing %q in %s", want, tiles)
		}
	}
	// No release list is cached: "Update available" must not claim 0 outdated devices.
	if !strings.Contains(tiles, `>Update available</span><span class="stat-value">–</span>`) {
		t.Errorf("firmware tile without a feed: %s", tiles)
	}
	if !strings.Contains(body, `aria-label="2 online, 1 offline, 2 unknown"`) {
		t.Error("the health bar does not draw the tiles' numbers")
	}
	// Operators get no discovery numbers (proposals are admin-only) and no admin buttons.
	_, body = e.do(t, "GET", "/", "operator", nil)
	if strings.Contains(body, "New devices found") || strings.Contains(body, "/discovery/proposals/") {
		t.Error("operator sees discovery proposals")
	}
	if strings.Contains(between(body, `<div class="pghead-acts">`, `</div>`), "Add devices") {
		t.Error("operator is offered Add devices, an admin page")
	}
}

// Why (design v2, P1): "Needs attention" lists what an admin has to act on: proposals first,
// each with its two actions posting to the existing proposal routes (with the shown version,
// or Accept refuses), then the devices with the reason in words. Conflicts are resolved on the
// discovery page and online, compliant devices need no attention.
func TestDashboardNeedsAttention(t *testing.T) {
	e, disc := dashFleet(t, nil)
	_, body := e.do(t, "GET", "/", "admin", nil)
	att := between(body, `<h2>Needs attention`, `</section>`)
	order := []string{
		"Shed Light", "New device · 10.0.0.88 · Network scan", "/tf/discovery/proposals/30/dismiss", ">Ignore<", "/tf/discovery/proposals/30/accept", ">Add<",
		"kitchen", "Address changed · 10.0.0.1 → 10.0.0.11", ">Ignore<", "/tf/discovery/proposals/31/accept", ">Apply<",
		"hallway", "Last seen 5h ago", ">Offline</span>",
		"annex", "Has not answered since it was added",
		"porch", "Refuses the stored web login",
		"kitchen", "TelePeriod 60 → 300",
	}
	rest := att
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("needs attention: %q missing or out of order in %s", want, att)
		}
		rest = rest[i+len(want):]
	}
	// Within one reason the devices keep the order they were added in (prototype), not the name.
	if strings.Index(att, ">attic<") > strings.Index(att, ">annex<") {
		t.Error("never-answered devices are not in the order they were added (attic is 2, annex 4)")
	}
	if !strings.Contains(att, `name="version" value="v30"`) || !strings.Contains(att, `name="version" value="v31"`) {
		t.Error("the proposal forms do not carry the shown version")
	}
	if strings.Contains(att, "clash") || strings.Contains(att, `href="/tf/devices/6"`) {
		t.Error("a conflict or an online, compliant device is listed")
	}
	// The form really accepts: the version reaches the discovery service.
	resp, _ := e.do(t, "POST", "/discovery/proposals/30/accept", "admin", map[string][]string{"csrf": {"tok"}, "version": {"v30"}})
	if resp.StatusCode != http.StatusSeeOther || len(disc.versions) != 1 || disc.versions[0] != "v30" {
		t.Errorf("accept from the dashboard form: %d %v", resp.StatusCode, disc.versions)
	}
}

// Why (#1247): the list is a glance, not the device list, and it must say what it leaves out: with 20 offline
// devices the card lists five of them, says "Show all 20" and leads to the list that holds exactly those 20, and the
// heading counts the 20, so nobody takes five rows for the whole fleet.
func TestDashboardNeedsAttentionIsCapped(t *testing.T) {
	e := setup(t)
	for i := model.DeviceID(10); i < 30; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "dead", Address: "10.0.1.1", Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOffline, LastSeen: time.Now()}
	}
	_, body := e.do(t, "GET", "/", "admin", nil)
	// the 20 dead ones and attic, which has not answered yet
	if !strings.Contains(body, "<h2>Needs attention (21)</h2>") {
		t.Errorf("the heading does not count the 21 devices: %s", between(body, `<div class="ulist-head">`, `</div>`))
	}
	off := between(body, `>Offline</h3>`, `<h3`)
	if n := strings.Count(off, `class="att-row att-item"`); n != attGroupRows {
		t.Errorf("%d offline devices listed, want %d", n, attGroupRows)
	}
	if !strings.Contains(off, `<a class="att-more" href="/tf/devices?health=offline" aria-label="Show all 20: Offline">Show all 20</a>`) {
		t.Errorf("no link to the 20 offline devices: %s", off)
	}
}

// Why (#1109, #1247): "Needs attention" is grouped by reason in today's severity order and each group is capped on its
// own. Before, one cap of eight over a flat list let seven offline devices push the deviations off the card without a
// word. Now a group says how many devices it holds, the card heading counts every entry once, and the broker ACL notice
// follows the Offline group and is not capped, so a long offline list cannot hide it.
func TestNeedsAttentionGroupsByReason(t *testing.T) {
	e := groupsFleet(t, 6, 2)
	_, body := e.do(t, "GET", "/", "admin", nil)
	// 6 offline + 2 deviating devices + the broker item; offline device 10 also deviates and is listed once
	if !strings.Contains(body, "<h2>Needs attention (9)</h2>") {
		t.Fatalf("heading: %s", between(body, `<div class="ulist-head">`, `</div>`))
	}
	att := attRegion(body)
	if !strings.Contains(att, "Each device once, for its worst reason. Firmware and backups are on the tiles.") {
		t.Errorf("no note on the criteria: %s", att)
	}
	order := []string{`>Offline</h3>`, `>Show all 6<`, `>Broker</h3>`, `>MQTT broker<`, `>Differs from policy</h3>`, `>Show all 3<`}
	rest := att
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("%q missing or out of order in %s", want, att)
		}
		rest = rest[i+len(want):]
	}
	off := between(att, `>Offline</h3>`, `>Broker</h3>`)
	if n := strings.Count(off, `class="att-row att-item"`); n != attGroupRows {
		t.Errorf("the Offline group shows %d rows, want the cap of %d", n, attGroupRows)
	}
	if !strings.Contains(off, `href="/tf/devices?health=offline"`) {
		t.Errorf("Show all 6 does not lead to the offline list: %s", off)
	}
	// the policy group holds the two online devices; the offline one that deviates too is in Offline only, so the
	// group shows 2 rows while its list holds 3: the link says 3, as the list will
	pol := between(att, `>Differs from policy</h3>`, `</section>`)
	if n := strings.Count(pol, `class="att-row att-item"`); n != 2 {
		t.Errorf("the policy group shows %d rows, want the 2 online devices: %s", n, pol)
	}
	if strings.Contains(pol, "off-1") || !strings.Contains(pol, `href="/tf/devices?chip=noncompliant"`) {
		t.Errorf("the policy group lists the offline device again, or does not lead to the filtered list: %s", pol)
	}
	// each device once
	for _, name := range []string{">off-1<", ">dev-20<"} {
		if n := strings.Count(att, name); n > 1 {
			t.Errorf("%s is listed %d times", name, n)
		}
	}
	// the list the links lead to holds what they say
	_, list := e.do(t, "GET", "/devices?chip=noncompliant", "admin", nil)
	if n := strings.Count(list, `<tr data-id="`); n != 3 {
		t.Errorf("the noncompliant list holds %d devices, the link says 3", n)
	}
}

// Why (#1109): a group holds at most five rows, and a group below the cap says nothing it does not have to: with 5
// offline devices there is no "Show all", the card is the whole list.
func TestNeedsAttentionGroupBelowTheCapHasNoShowAll(t *testing.T) {
	e := groupsFleet(t, 5, 0)
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, "<h2>Needs attention (6)</h2>") || !strings.Contains(body, ">Offline</h3>") {
		t.Errorf("the 5 offline devices and the broker item are one group and one entry: %s", between(body, `<div class="ulist-head">`, `</div>`))
	}
	att := attRegion(body)
	if strings.Contains(att, "Show all") || strings.Contains(att, " more<") {
		t.Errorf("a group that shows every device offers more: %s", att)
	}
	if n := strings.Count(att, `class="att-row att-item"`); n != 5+1 {
		t.Errorf("%d rows, want 5 devices and the broker item", n)
	}
}

// Why (#1109): a boot loop has no filter on the device list, so a link would lead to a list that does not show what
// the card says. The group says "N more" as plain text instead.
func TestNeedsAttentionGroupWithoutAFilterSaysNMore(t *testing.T) {
	boots := map[model.DeviceID][]model.DeviceBoot{}
	e := setupWith(t, func(d *Deps) { d.Stability = fakeStab{boots: boots} })
	for i := model.DeviceID(20); i < 26; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "loop-" + itoa(int(i)), Address: "10.0.4." + itoa(int(i)), Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOnline, LastSeen: time.Now(), SignalDBm: -50, RSSI: 90, LinkCount: 1, UptimeSec: 300}
		boots[i] = loopBoots(i, 5)
	}
	_, body := e.do(t, "GET", "/", "admin", nil)
	loop := between(attRegion(body), `>Boot loop</h3>`, `</section>`)
	if n := strings.Count(loop, `class="att-row att-item"`); n != attGroupRows {
		t.Errorf("%d boot loop rows, want %d", n, attGroupRows)
	}
	if !strings.Contains(loop, `<p class="att-more">1 more</p>`) || strings.Contains(loop, "Show all") {
		t.Errorf("the sixth boot loop is not counted as plain text: %s", loop)
	}
}

// Why (#1109): the proposals stay above the devices (an admin acts on them first), count in the heading like every
// other entry, and show at most five; the rest is one click away on the page that lists them.
func TestNeedsAttentionProposalsAreCountedAndCapped(t *testing.T) {
	disc := &fakeDiscovery{}
	for i := 1; i <= 7; i++ {
		disc.props = append(disc.props, discovery.Proposal{ID: int64(40 + i), Kind: discovery.KindNew, Via: discovery.ViaScan, Version: "v", Device: model.Device{Name: "new-" + itoa(i), Address: "10.0.5." + itoa(i)}})
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = disc })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, "<h2>Needs attention (7)</h2>") {
		t.Errorf("the heading does not count the 7 proposals: %s", between(body, `<div class="ulist-head">`, `</div>`))
	}
	att := attRegion(body)
	if n := strings.Count(att, `class="att-row att-prop"`); n != dashProposalsMax {
		t.Errorf("%d proposals listed, want %d", n, dashProposalsMax)
	}
	if !strings.Contains(att, `<a class="att-more" href="/tf/devices/new#card-discovered" aria-label="Show all 7: New devices">Show all 7</a>`) {
		t.Errorf("the 7 proposals have no link to the page that lists them: %s", att)
	}
}

// Why (#1109, #1247): the same fact has one name on the tile and the chip, and the tile hint is a sentence, not a clause
// fragment. 3 devices off 2 rules with 6 values not read says exactly that.
func TestDashboardTileAndChipWordsAreOneVocabulary(t *testing.T) {
	findings := []policy.Finding{
		{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"},
		{DeviceID: 2, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"},
		{DeviceID: 3, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Have: "0"},
		{DeviceID: 4, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
		{DeviceID: 5, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
		{DeviceID: 6, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
		{DeviceID: 7, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
		{DeviceID: 8, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
		{DeviceID: 9, Rule: policy.Rule{Command: "SetOption1", Want: "1"}, Unknown: true},
	}
	e := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: findings}} })
	for i := model.DeviceID(3); i <= 9; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "d" + itoa(int(i)), Address: "10.0.6." + itoa(int(i)), Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOnline, LastSeen: time.Now()}
	}
	_, body := e.do(t, "GET", "/", "admin", nil)
	tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
	want := `>Differs from policy</span><span class="stat-value">3</span><span class="stat-hint">3 devices differ from 2 rules; 6 not checked yet</span>`
	if !strings.Contains(tiles, want) {
		t.Errorf("policy tile: missing %q in %s", want, tiles)
	}
	for _, old := range []string{"Firmware behind", "Policy deviations", "Outdated", "Not compliant"} {
		if strings.Contains(body, old) {
			t.Errorf("the dashboard still says %q", old)
		}
	}
	_, list := e.do(t, "GET", "/devices", "admin", nil)
	for _, w := range []string{`<span class="chip-l">Update available</span>`, `<span class="chip-l">Differs from policy</span>`} {
		if !strings.Contains(list, w) {
			t.Errorf("the device list's chips lack %s", w)
		}
	}
	// one device, one rule: the singular forms
	one := setupWith(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: findings[:1]}} })
	_, body = one.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(squash(body), `<span class="stat-hint">1 device differs from 1 rule</span>`) {
		t.Errorf("singular hint: %s", between(body, `<div class="stat-grid">`, `<svg class="health-bar"`))
	}
}

// Why (#1078): that the broker has no ACL is a property of the whole fleet, not of a device, and the dashboard is where
// a person looks first. Needs attention lists it once, right after the offline devices (a dead device is the more
// urgent look), leading to the guide. It is not a device, so it does not count against the device rows: a fleet with
// many offline devices must not push it off the list. Without a broker, or after the admin hid the warning for this
// broker, it is not listed; when it is the only entry, the card shows it instead of "Nothing needs attention".
func TestDashboardListsTheBrokerACLOnce(t *testing.T) {
	const item = `<a class="att-row att-item" href="https://github.com/hellwand/tasmofleet/blob/main/docs/mqtt-broker-acl.md"><span class="sdot-dot t-warn" aria-hidden="true"></span> <span class="att-text"><span class="att-name">MQTT broker</span><span class="att-why">No ACL · any client on it can read device passwords and backups</span>`
	att := func(e *env) string {
		_, body := e.do(t, "GET", "/", "operator", nil)
		return between(body, `<h2>Needs attention`, `</section>`)
	}
	svc := aclBrokerService(t)
	e := setupWith(t, func(d *Deps) { d.SysConfig = svc })
	e.inv.devs[3] = model.Device{ID: 3, Name: "cellar", Address: "10.0.0.3", Identity: model.Identity{DetectedAt: time.Now()}}
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: time.Now()}
	got := att(e)
	if n := strings.Count(got, item); n != 1 {
		t.Fatalf("Needs attention lists the broker ACL %d times, want once:\n%s", n, got)
	}
	// after the offline device, before the device that has not answered yet (attic, never seen)
	off, acl, unk := strings.Index(got, ">cellar<"), strings.Index(got, item), strings.Index(got, ">attic<")
	if off < 0 || unk < 0 || off >= acl || acl >= unk {
		t.Errorf("the broker ACL item is not between the offline device (%d) and the next reason (%d): %d", off, unk, acl)
	}

	// a full list of offline devices keeps it: a full group of device rows and the item
	for i := model.DeviceID(10); i < 30; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "dead", Address: "10.0.1.1", Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOffline, LastSeen: time.Now()}
	}
	got = att(e)
	if n, rows := strings.Count(got, item), strings.Count(got, `class="att-row att-item"`); n != 1 || rows != 5+1+1 { // the group, attic (not answered yet) and the item
		t.Errorf("with many offline devices: the item %d times in %d rows, want once in %d", n, rows, 5+1+1)
	}

	// the only entry: every device answers
	calm := setupWith(t, func(d *Deps) { d.SysConfig = aclBrokerService(t) })
	calm.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	got = att(calm)
	if strings.Count(got, item) != 1 || strings.Contains(got, "Nothing needs attention") {
		t.Errorf("as the only entry the item is missing or the card says nothing needs attention:\n%s", got)
	}

	// hidden for this broker, and no broker at all: not listed
	aclHideNow(t, svc)
	if strings.Contains(att(e), "MQTT broker") {
		t.Error("the item is listed after the admin hid the warning for this broker")
	}
	cleared := aclBrokerService(t)
	if _, err := cleared.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL"}}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Deps){"no settings service": nil, "empty broker URL": func(d *Deps) { d.SysConfig = cleared }} {
		if got := att(setupWith(t, mutate)); strings.Contains(got, "MQTT broker") {
			t.Errorf("%s: the item is listed without a broker", name)
		}
	}
}

// Why (design v2, P1): the transactions card shows the newest transaction with its progress
// and the next ones as rows, each a link to the transaction, worded as the prototype words them.
func TestDashboardTransactionsCard(t *testing.T) {
	job := func(id model.JobID, batch model.BatchID, st model.JobState) model.Job {
		return model.Job{ID: id, BatchID: batch, DeviceID: 1, State: st, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: time.Now()}
	}
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{ // newest first
			job(9, 5, model.JobAwaitReboot), job(8, 5, model.JobFailed), job(7, 5, model.JobSucceeded), job(6, 5, model.JobQueued),
			job(4, 4, model.JobSucceeded), job(3, 3, model.JobFailed), job(2, 3, model.JobSucceeded), job(1, 2, model.JobCancelled),
			{ID: 0, BatchID: 1, State: model.JobSucceeded, CreatedAt: time.Now()},
		}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := between(body, `<h2>Activity</h2>`, `</section>`)
	for _, want := range []string{
		`<a class="txn-card" href="/tf/transactions/5">`, `TX-0005`, "Update to Tasmota 16.1.0", ">Running<", "1 of 4 done · 1 failed · 1 in progress · 1 waiting",
		`href="/tf/transactions/4"`, ">Succeeded<",
		`href="/tf/transactions/3"`, ">1 succeeded · 1 failed<",
		`href="/tf/transactions/2"`, ">Cancelled<",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("transactions card: missing %q in %s", want, card)
		}
	}
	// Why: "Today"/"Yesterday" is the browser's call (#137), so the when of a row is a <time> time.js repaints
	if !strings.Contains(card, `data-ts="relative"`) {
		t.Errorf("history rows are not day stamps for time.js: %s", card)
	}
	if strings.Contains(card, `/tf/transactions/1"`) {
		t.Error("more than three history rows")
	}
	if !strings.Contains(between(body, `<h2>Activity</h2>`, `</div>`), `href="/tf/transactions"`) {
		t.Error("History does not lead to the transactions list")
	}
}

// dashTxnJob is a job of batch b for the dashboard's transaction tests; created is when the batch was created.
func dashTxnJob(id model.JobID, b model.BatchID, st model.JobState, created time.Time) model.Job {
	return model.Job{ID: id, BatchID: b, DeviceID: 1, State: st, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: created}
}

// txnPart is the dashboard's transactions card.
func txnPart(body string) string { return between(body, `<h2>Activity</h2>`, `</section>`) }

// Why (#1199): the expanded card with its progress bar is for a transaction that is still at work. A
// finished one (here the newest, all Succeeded) is one compact row with its result and the time it
// ran, like the older ones; before, the newest transaction was the card whatever its state and had no time.
func TestDashboardFinishedNewestTransactionIsARow(t *testing.T) {
	at := time.Now().Add(-90 * time.Minute)
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{dashTxnJob(2, 2, model.JobSucceeded, at), dashTxnJob(1, 1, model.JobSucceeded, at.Add(-time.Hour))}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := txnPart(body)
	if strings.Contains(card, "txn-card") || strings.Contains(card, "seg-bar") {
		t.Errorf("a finished transaction is drawn as the progress card: %s", card)
	}
	row := between(card, `<a class="att-row txn-row" href="/tf/transactions/2">`, `</a>`)
	for _, want := range []string{"TX-0002", ">Succeeded<", `<time data-ts="relative" datetime="` + at.UTC().Format(time.RFC3339) + `">`} {
		if !strings.Contains(row, want) {
			t.Errorf("the newest transaction's row lacks %q: %s", want, card)
		}
	}
}

// Why (#1199): a transaction that is running keeps its card with the progress, and the card says when it
// started ("started" outside the <time>, which time.js repaints as a day stamp in the browser's zone).
// A plan that began is dated by its planned start (the one rule of the Transactions history), not by
// the day somebody planned it.
func TestDashboardActiveTransactionCardShowsStart(t *testing.T) {
	created := time.Now().Add(-48 * time.Hour)
	planned := time.Now().Add(-20 * time.Minute)
	e := setupWith(t, func(d *Deps) {
		run := dashTxnJob(2, 2, model.JobAwaitReboot, created)
		run.StartAt = planned
		d.Jobs = &fakeJobs{jobs: []model.Job{run, dashTxnJob(1, 1, model.JobSucceeded, created)}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := between(txnPart(body), `<a class="txn-card" href="/tf/transactions/2">`, `</a>`)
	if card == "" || !strings.Contains(card, "seg-bar") {
		t.Fatalf("a running transaction is not the progress card: %s", txnPart(body))
	}
	want := `<span class="txn-when">started <time data-ts="relative" datetime="` + planned.UTC().Format(time.RFC3339) + `">`
	if !strings.Contains(card, want) {
		t.Errorf("the card does not say when the plan started (want %q): %s", want, card)
	}
}

// Why (#1199): the dashboard has four entries, and every active transaction is a card, even one older than a
// finished one, so a running transaction is never hidden behind a row; the finished ones fill what is left.
func TestDashboardActiveCardsThenFinishedRows(t *testing.T) {
	now := time.Now()
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{ // newest first
			dashTxnJob(5, 5, model.JobSucceeded, now), dashTxnJob(4, 4, model.JobQueued, now), dashTxnJob(3, 3, model.JobFailed, now),
			dashTxnJob(2, 2, model.JobSucceeded, now), dashTxnJob(1, 1, model.JobAwaitReboot, now),
		}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := txnPart(body)
	if n, r := strings.Count(card, `class="txn-card"`), strings.Count(card, `class="att-row txn-row"`); n != 2 || r != 2 {
		t.Fatalf("want 2 cards and 2 rows, got %d and %d: %s", n, r, card)
	}
	i4, i1, i5, i3 := strings.Index(card, "TX-0004"), strings.Index(card, "TX-0001"), strings.Index(card, "TX-0005"), strings.Index(card, "TX-0003")
	if i4 < 0 || i1 < i4 || i5 < i1 || i3 < i5 || strings.Contains(card, "TX-0002") {
		t.Errorf("want the active ones (4, 1) first, then the newest finished (5, 3), and not 2: %s", card)
	}
}

// Why (#1199): with nothing at work, which is the usual fleet, the transactions are four plain rows and no card.
func TestDashboardFinishedOnlyIsFourRows(t *testing.T) {
	now := time.Now()
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{
			dashTxnJob(5, 5, model.JobSucceeded, now), dashTxnJob(4, 4, model.JobFailed, now), dashTxnJob(3, 3, model.JobCancelled, now),
			dashTxnJob(2, 2, model.JobSucceeded, now), dashTxnJob(1, 1, model.JobSucceeded, now),
		}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	card := txnPart(body)
	if n, r := strings.Count(card, `class="txn-card"`), strings.Count(card, `class="att-row txn-row"`); n != 0 || r != 4 {
		t.Errorf("want no card and 4 rows, got %d and %d: %s", n, r, card)
	}
}

// Why (#1199): a transaction that ends turns from the card into a row on its own. The dashboard re-fetches
// itself on every job event (TestDashboardIsLive), so the GET after the last job finished is the switch.
func TestDashboardTransactionTurnsIntoRowWhenItEnds(t *testing.T) {
	fj := &fakeJobs{jobs: []model.Job{dashTxnJob(1, 1, model.JobQueued, time.Now())}}
	e := setupWith(t, func(d *Deps) { d.Jobs = fj })
	_, body := e.do(t, "GET", "/", "operator", nil)
	if card := txnPart(body); !strings.Contains(card, `class="txn-card"`) || strings.Contains(card, "txn-row") {
		t.Fatalf("a waiting transaction is not the card: %s", card)
	}
	fj.jobs[0].State = model.JobSucceeded
	_, body = e.do(t, "GET", "/", "operator", nil)
	if card := txnPart(body); strings.Contains(card, "txn-card") || !strings.Contains(card, `class="att-row txn-row"`) {
		t.Errorf("a finished transaction is still the card: %s", card)
	}
}

// Why (#1109): a person with no devices needs exactly one next step. Before, the header offered "New update" (primary)
// next to the empty card's "Scan network" (primary) and two more buttons, and none of them could do anything for an
// empty fleet but the first. The empty card has one primary, Add devices, which leads to the page that carries the
// ways to add (scan, by hand, import); the header has no button and no update. An operator may not add devices: the card
// says who does, and offers no button that would only be refused.
func TestEmptyDashboardOffersOneFirstAction(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscovery{}
		d.Import = importer.New(importer.Options{Inventory: &recInv{}})
	})
	delete(e.inv.devs, 1)
	delete(e.inv.devs, 2)
	_, body := e.do(t, "GET", "/", "admin", nil)
	dash := between(body, `<div id="dash"`, `</main>`)
	if !strings.Contains(body, `<p class="pghead-sub">0 devices`) || !strings.Contains(dash, "<h2>No devices yet</h2>") {
		t.Fatalf("empty dashboard: %s", body)
	}
	if n := strings.Count(dash, "btn-primary"); n != 1 {
		t.Errorf("the empty dashboard has %d primary buttons, want 1: %s", n, dash)
	}
	if !strings.Contains(dash, `href="/tf/devices/new">Add devices</a>`) || !strings.Contains(dash, "btn-primary") {
		t.Errorf("the one primary is not Add devices: %s", dash)
	}
	if n := strings.Count(dash, `<a class="btn`) + strings.Count(dash, "<button"); n != 1 {
		t.Errorf("the empty dashboard has %d buttons, want the one: %s", n, dash)
	}
	for _, gone := range []string{"New update", "Scan network", "Add manually", "Import from TasmoAdmin", "stat-grid", "Needs attention"} {
		if strings.Contains(dash, gone) {
			t.Errorf("the empty dashboard still draws %q", gone)
		}
	}
	// the three ways are on the page the button leads to
	if _, page := e.do(t, "GET", "/devices/new", "admin", nil); !strings.Contains(page, `id="card-scan"`) || !strings.Contains(page, "Import from TasmoAdmin") {
		t.Error("the Add devices page does not carry the ways to add the first devices")
	}
	// the page that New update leads to is still there for a fleet that has devices
	if _, page := e.do(t, "GET", "/updates/new", "admin", nil); !strings.Contains(page, "Updates are switched off") {
		t.Error("New update with updates off does not explain why")
	}

	_, body = e.do(t, "GET", "/", "operator", nil)
	dash = between(body, `<div id="dash"`, `</main>`)
	for _, want := range []string{"<h2>No devices yet</h2>", "An admin adds them."} {
		if !strings.Contains(dash, want) {
			t.Errorf("operator: missing %q in %s", want, dash)
		}
	}
	if strings.Contains(dash, `<a class="btn`) || strings.Contains(dash, "<button") || strings.Contains(dash, "Add your Tasmota devices") {
		t.Errorf("an operator is offered a button or an instruction he cannot follow: %s", dash)
	}
}

// Why (design v2, P1, structure deviation S6 resolved): the dashboard holds the event stream and
// reloads its body when a device changes state, a device is added or removed, or a transaction moves.
func TestDashboardIsLive(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/", "operator", nil)
	if !strings.Contains(body, `sse-connect="/tf/events?since=`) {
		t.Error("the dashboard opens no event stream")
	}
	if !strings.Contains(body, `<div id="dash" hx-get="/tf/" hx-trigger="sse:state throttle:2s, sse:device throttle:2s, sse:job throttle:2s, sse:backup throttle:2s, sse:resync" hx-select="#dash" hx-swap="outerHTML">`) {
		t.Errorf("the dashboard body does not refresh on stream events: %s", body)
	}
}

// Why (#1089): the subtitle used to say "state streams in live" as fixed text, so the dashboard kept
// claiming it while the stream was down. The subtitle now only counts the devices; a second part, shown
// by the stylesheet only while html[data-live] says the stream is down, names the time of the last
// values from the same element live.js fills for the status bar (one clock).
func TestDashboardSubtitleDoesNotClaimLive(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/", "operator", nil)
	if strings.Contains(body, "streams in live") {
		t.Error("the dashboard subtitle still says the state streams in live, whatever the connection does")
	}
	sub := body[strings.Index(body, `<p class="pghead-sub">`):]
	sub = sub[:strings.Index(sub, "</p>")]
	if !strings.HasPrefix(sub, `<p class="pghead-sub">2 devices`) {
		t.Errorf("subtitle: %s", sub)
	}
	if !strings.Contains(sub, `Values from <time data-live-since>`) {
		t.Errorf("the subtitle has no place for the time of the last values: %s", sub)
	}
}

// Why: telemetry arrives every few seconds per device; the dashboard must reload only when a
// device's state really changes ("state"), not on every health message.
func TestSSEStateEventOnlyOnChange(t *testing.T) {
	e := setup(t) // kitchen (1) is online
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		for _, st := range []model.HealthState{model.HealthOnline, model.HealthOnline, model.HealthOffline, model.HealthOffline} {
			time.Sleep(50 * time.Millisecond)
			e.hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, State: st}})
		}
		time.Sleep(50 * time.Millisecond)
		e.hub.Publish(events.Event{Topic: events.TopicDevice, Payload: model.Device{ID: 1}})
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "event: device") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, got)
		}
	}
	if n := strings.Count(got, "event: state\n"); n != 1 || !strings.Contains(got, "event: state\ndata: 1\n") {
		t.Errorf("%d state events, want exactly one (online -> offline): %q", n, got)
	}
}

// Why: live policy values are kept in memory only. After a restart, until the first background
// read (spread over up to 10 minutes, never while the live check is paused) and without a decoded
// backup, every value is unknown; the tile must then say so instead of claiming 0 deviations,
// like "Update available" without a release list. One value known (here a compliant one) makes 0 real.
func TestDashboardPolicyTileUnknownBeforeFirstRead(t *testing.T) {
	unread := []policy.Finding{
		{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
		{DeviceID: 3, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
	}
	for _, c := range []struct {
		name string
		prov map[model.DeviceID]map[string]ValueSource
		want string
	}{
		{"nothing read", nil, `>Differs from policy</span><span class="stat-value">–</span><span class="stat-hint">Not checked yet</span>`},
		{"one value read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`>Differs from policy</span><span class="stat-value">0</span><span class="stat-hint">No device differs from a rule; 2 not checked yet</span>`},
	} {
		e, _ := dashFleet(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: unread, Provenance: c.prov}} })
		_, body := e.do(t, "GET", "/", "admin", nil)
		tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
		if !strings.Contains(tiles, c.want) {
			t.Errorf("%s: missing %q in %s", c.name, c.want, tiles)
		}
	}
}

// Why: the device list's "Differs from policy" chip must follow the tile's rule: "–" while nothing was
// read, and the known count with "n not checked yet" beside it (the tooltip, the tile's hint
// wording) while only part of the fleet was, so a 0 never stands alone as a claim of compliance
// nobody checked. "0+" read oddly (v0.11.1 feedback 4): the count stays a plain
// number, the uncertainty is said in words.
func TestDeviceListNotCompliantChipUnknownOrPartial(t *testing.T) {
	unread := []policy.Finding{
		{DeviceID: 1, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
		{DeviceID: 3, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Unknown: true},
	}
	for _, c := range []struct {
		name string
		prov map[model.DeviceID]map[string]ValueSource
		want string
	}{
		{"nothing read", nil, `title="2 not checked yet"><span class="chip-l">Differs from policy</span><span class="chip-n">–</span>`},
		{"partly read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`title="2 not checked yet"><span class="chip-l">Differs from policy</span><span class="chip-n">0</span>`},
	} {
		e, _ := dashFleet(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: unread, Provenance: c.prov}} })
		_, body := e.do(t, "GET", "/devices", "admin", nil)
		body = squash(body)
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: missing %q in %s", c.name, c.want, between(body, `class="dv-chips"`, `class="dv-vsep"`))
		}
	}
}

// Why: #294, a discovered device that is offline is a proposal, not an inventory device: it must
// not sit in "Needs attention" (it is not counted as an offline device either). It shows on the
// Discovered page, with its hostname and "offline since".
func TestOfflineProposalLeavesNeedsAttention(t *testing.T) {
	props := []discovery.Proposal{
		{ID: 40, Kind: discovery.KindNew, Via: discovery.ViaMQTT, Version: "v40", LWT: "Offline", Device: model.Device{Name: "Gone Lamp", Address: "10.0.0.40"}},
		{ID: 41, Kind: discovery.KindNew, Via: discovery.ViaMQTT, Version: "v41", LWT: "Online", Device: model.Device{Name: "Live Lamp", Address: "10.0.0.41"}},
	}
	got, n := dashProposals("", props, nil)
	if len(got) != 1 || n != 1 || got[0].Name != "Live Lamp" {
		t.Fatalf("dashboard proposals = %+v (of %d), want only the online one", got, n)
	}
}

func TestDiscoveredPageShowsOfflineSince(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.props = []discovery.Proposal{{ID: 1, Version: "v1", Kind: discovery.KindNew, Via: discovery.ViaMQTT, LWT: "Offline",
		FirstSeen: time.Now().Add(-3 * time.Hour), SeenAt: time.Now().Add(-time.Hour), OfflineSince: time.Now().Add(-2 * time.Hour),
		Device: model.Device{Name: "Gone Lamp", Topic: "gone", Address: "10.0.0.40", MAC: "AA:00:00:00:00:09", Net: model.NetFacts{Hostname: "tasmota-gone"}}}}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	for _, want := range []string{"tasmota-gone", ">since <time"} {
		if !strings.Contains(body, want) {
			t.Errorf("Discovered page lacks %q", want)
		}
	}
}

// Why: #294 review. The tile counts every new device, offline ones included, and says how many
// are offline so tile and Discovered page agree even though the dashboard list hides them.
func TestNewDevicesTileCountsOfflineProposals(t *testing.T) {
	props := []discovery.Proposal{
		{Kind: discovery.KindNew, LWT: "Offline"}, {Kind: discovery.KindNew, LWT: "Offline"}, {Kind: discovery.KindNew, LWT: "Online"},
	}
	s := summarize(nil, fleetFacts{}, props, true)
	var tile struct{ Value, Hint string }
	for _, ti := range dashTiles("", s) {
		if ti.Label == "New devices found" {
			tile.Value, tile.Hint = ti.Value, ti.Hint
		}
	}
	if tile.Value != "3" || !strings.Contains(tile.Hint, "2 offline") {
		t.Fatalf("tile = %+v, want 3 with \"2 offline\"", tile)
	}
}

// usageBackups is the backup service that also reports each device's backup usage (BackupStorage).
type usageBackups struct {
	backup.Service
	usage []store.BackupUsage
}

func (u usageBackups) Usage(context.Context) ([]store.BackupUsage, error) { return u.usage, nil }
func (u usageBackups) Orphans(context.Context) ([]model.Backup, error)    { return nil, nil }

// usageFailing is a backup service whose usage read fails.
type usageFailing struct{ usageBackups }

func (usageFailing) Usage(context.Context) ([]store.BackupUsage, error) {
	return nil, errors.New("usage: disk gone")
}

// Why (#438): a fleet owner wants to see on the dashboard how many devices have no backup, and
// the click must open exactly those devices: the tile counts with the device list's "No backup"
// chip (same withoutBackup rule), links to that chip, and uses the accent tone, never warn:
// until skipped devices are told apart from failed ones (#409) the number says "no backup yet",
// not "something failed". It appears only when the backup service can report usage (else there
// is no chip to link to), and the dashboard must reload on backup events or the number would
// stay old after a backup.
func TestDashboardNoBackupTileMatchesTheChip(t *testing.T) {
	e, _ := dashFleet(t, func(d *Deps) {
		d.Backups = usageBackups{Service: d.Backups, usage: []store.BackupUsage{
			{Device: 1, Runs: 2}, {Device: 3}, {Device: 4}, {Device: 0, Runs: 5}, // 0 = deleted devices' row
		}}
	})
	_, body := e.do(t, "GET", "/", "admin", nil)
	tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
	m := regexp.MustCompile(`<a class="stat-tile" href="([^"]*)"><span class="stat-label"><span class="sdot-dot (\S+)" aria-hidden="true"></span>No backup</span><span class="stat-value">(\d+)</span><span class="stat-hint">([^<]*)</span></a>`).FindStringSubmatch(tiles)
	if m == nil {
		t.Fatalf("no No backup tile in %s", tiles)
	}
	if m[1] != "/tf/devices?chip=nobackup" || m[3] != "2" || m[4] != "never backed up" {
		t.Errorf("tile: href %q value %q hint %q", m[1], m[3], m[4])
	}
	if !strings.Contains(m[2], "accent") || strings.Contains(m[2], "warn") {
		t.Errorf("tone class %q: want accent, never warn", m[2])
	}
	// the list's chip counts the same devices
	_, list := e.do(t, "GET", "/devices?chip=nobackup", "admin", nil)
	if !strings.Contains(list, "No backup") || strings.Count(between(list, "<tbody", "</tbody>"), "<tr") != 2 {
		t.Errorf("the chip does not show the 2 devices the tile counted")
	}
	if !strings.Contains(body, "sse:backup") {
		t.Error("the dashboard does not reload on backup events")
	}

	// a failed usage read leaves the tile out, never a false "0", and the dashboard still renders
	broken, _ := dashFleet(t, func(d *Deps) { d.Backups = usageFailing{usageBackups{Service: d.Backups}} })
	res, body := broken.do(t, "GET", "/", "admin", nil)
	if res.StatusCode != http.StatusOK || strings.Contains(body, "No backup") {
		t.Errorf("usage read failed: status %d, tile shown %v", res.StatusCode, strings.Contains(body, "No backup"))
	}

	// no BackupStorage: no tile
	plain, _ := dashFleet(t, nil)
	_, body = plain.do(t, "GET", "/", "admin", nil)
	if strings.Contains(body, "No backup") {
		t.Error("tile shown without a backup service that reports usage")
	}
}

// Why (#409, MOCK-409 decision 2): once the Backups page exists the tile opens it narrowed to the
// devices without a backup, says when the last scheduled run was, and turns warn only when that run
// really failed for some device. Skipped devices (offline, never answered) stay calm, as promised to
// the reporter of #438; without a stored run it keeps the old hint.
func TestDashboardNoBackupTileLinksToTheBackupsPage(t *testing.T) {
	tile := func(run *model.BackupRun) (href, tone, hint string) {
		t.Helper()
		e, _, _, _ := sysEnvFull(t, true, func(d *Deps) {
			d.Backups = usageBackups{Service: d.Backups, usage: []store.BackupUsage{{Device: 1, Runs: 2}, {Device: 2}}}
			d.BackupFleet = &fakeFleet{run: run}
		})
		_, body := e.do(t, "GET", "/", "admin", nil)
		tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
		m := regexp.MustCompile(`<a class="stat-tile" href="([^"]*)"><span class="stat-label"><span class="sdot-dot (\S+)" aria-hidden="true"></span>No backup</span><span class="stat-value">(\d+)</span><span class="stat-hint">(.*?)</span></a>`).FindStringSubmatch(tiles)
		if m == nil {
			t.Fatalf("no No backup tile in %s", tiles)
		}
		return m[1], m[2], m[4]
	}
	now := time.Now()
	href, tone, hint := tile(&model.BackupRun{StartedAt: now, FinishedAt: now, Completed: true, OK: 3, Failed: 2, Skipped: 1, Tried: 5})
	if href != "/tf/devices/backups?filter=none" || !strings.HasPrefix(hint, "Last run <time") || !strings.Contains(hint, `data-ts="relative"`) || !strings.HasSuffix(hint, " · 2 failed") || !strings.Contains(tone, "warn") {
		t.Errorf("failed run: href %q tone %q hint %q", href, tone, hint)
	}
	_, tone, hint = tile(&model.BackupRun{StartedAt: now, FinishedAt: now, Completed: true, OK: 3, Skipped: 4, Tried: 3})
	if strings.Contains(hint, "failed") || !strings.HasPrefix(hint, "Last run <time") || strings.Contains(tone, "warn") || !strings.Contains(tone, "accent") {
		t.Errorf("skipped only must stay accent: tone %q hint %q", tone, hint)
	}
	_, tone, hint = tile(nil)
	if hint != "never backed up" || !strings.Contains(tone, "accent") {
		t.Errorf("no run yet: tone %q hint %q", tone, hint)
	}
}

// attGroupRows is how many rows one group of Needs attention shows (the issue says five, so a change of the cap is a decision, not a refactor).
const attGroupRows = 5

// attRegion is the "Needs attention" card of the dashboard.
func attRegion(body string) string { return between(body, `<h2>Needs attention`, `</section>`) }

// groupsFleet is a fleet of offline devices (off-1 ...; the first of them also differs from the policy) and of online
// devices that differ from it (dev-20 ...), with the broker's ACL warning on. The two seeded devices are gone.
func groupsFleet(t *testing.T, offline, deviating int) *env {
	t.Helper()
	var findings []policy.Finding
	if offline > 0 {
		findings = append(findings, policy.Finding{DeviceID: 10, Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"})
	}
	for i := 0; i < deviating; i++ {
		findings = append(findings, policy.Finding{DeviceID: model.DeviceID(20 + i), Rule: policy.Rule{Command: "TelePeriod", Want: "300"}, Have: "60"})
	}
	e := setupWith(t, func(d *Deps) {
		d.SysConfig = aclBrokerService(t)
		d.Policy = &fakePolicy{rep: PolicyReport{Findings: findings}}
	})
	delete(e.inv.devs, 1)
	delete(e.inv.devs, 2)
	seen := time.Now().Add(-3 * time.Hour)
	for i := 0; i < offline; i++ {
		id := model.DeviceID(10 + i)
		e.inv.devs[id] = model.Device{ID: id, Name: "off-" + itoa(i+1), Address: "10.0.7." + itoa(int(id)), Identity: model.Identity{DetectedAt: seen}}
		e.health[id] = model.Health{DeviceID: id, State: model.HealthOffline, LastSeen: seen}
	}
	for i := 0; i < deviating; i++ {
		id := model.DeviceID(20 + i)
		e.inv.devs[id] = model.Device{ID: id, Name: "dev-" + itoa(int(id)), Address: "10.0.8." + itoa(int(id)), Identity: model.Identity{DetectedAt: seen}}
		e.health[id] = model.Health{DeviceID: id, State: model.HealthOnline, LastSeen: time.Now()}
	}
	return e
}
