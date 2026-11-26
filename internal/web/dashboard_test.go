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
		`>Policy deviations</span><span class="stat-value">1</span><span class="stat-hint">device, 1 rule</span>`,
		`href="/tf/devices?chip=noncompliant"`,
		`href="/tf/devices/new#card-discovered"`, `>New devices found</span><span class="stat-value">1</span><span class="stat-hint">+ 1 address changed</span>`,
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("tiles: missing %q in %s", want, tiles)
		}
	}
	// No release list is cached: "Firmware behind" must not claim 0 outdated devices.
	if !strings.Contains(tiles, `>Firmware behind</span><span class="stat-value">–</span>`) {
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
	att := between(body, `<h2>Needs attention</h2>`, `</section>`)
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

// Why (prototype slice(0, 8)): the list is a glance, not the device list; a fleet with many
// offline devices must not push the transactions off the page.
func TestDashboardNeedsAttentionIsCapped(t *testing.T) {
	e := setup(t)
	for i := model.DeviceID(10); i < 30; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "dead", Address: "10.0.1.1", Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOffline, LastSeen: time.Now()}
	}
	_, body := e.do(t, "GET", "/", "admin", nil)
	if n := strings.Count(body, `class="att-row att-item"`); n != dashAttentionMax {
		t.Errorf("%d devices listed, want %d", n, dashAttentionMax)
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
		return between(body, `<h2>Needs attention</h2>`, `</section>`)
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

	// a full list of offline devices keeps it: eight device rows and the item
	for i := model.DeviceID(10); i < 30; i++ {
		e.inv.devs[i] = model.Device{ID: i, Name: "dead", Address: "10.0.1.1", Identity: model.Identity{DetectedAt: time.Now()}}
		e.health[i] = model.Health{DeviceID: i, State: model.HealthOffline, LastSeen: time.Now()}
	}
	got = att(e)
	if n, rows := strings.Count(got, item), strings.Count(got, `class="att-row att-item"`); n != 1 || rows != dashAttentionMax+1 {
		t.Errorf("with many offline devices: the item %d times in %d rows, want once in %d", n, rows, dashAttentionMax+1)
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
	card := between(body, `<h2>Transactions</h2>`, `</section>`)
	for _, want := range []string{
		`<a class="txn-card" href="/tf/transactions/5">`, `TX-0005`, "Update to Tasmota 16.1.0", ">Running<", "1 of 4 done · 1 failed · 1 in progress · 1 waiting",
		`href="/tf/transactions/4"`, ">Completed<",
		`href="/tf/transactions/3"`, ">1 done · 1 failed<",
		`href="/tf/transactions/2"`, ">Cancelled<",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("transactions card: missing %q in %s", want, card)
		}
	}
	// Why: "Today"/"Yesterday" is the browser's call (#137), so the when of a row is a <time> time.js repaints
	if !strings.Contains(card, `data-ts="day"`) {
		t.Errorf("history rows are not day stamps for time.js: %s", card)
	}
	if strings.Contains(card, `/tf/transactions/1"`) {
		t.Error("more than three history rows")
	}
	if !strings.Contains(between(body, `<h2>Transactions</h2>`, `</div>`), `href="/tf/transactions"`) {
		t.Error("History does not lead to the transactions list")
	}
}

// dashTxnJob is a job of batch b for the dashboard's transaction tests; created is when the batch was created.
func dashTxnJob(id model.JobID, b model.BatchID, st model.JobState, created time.Time) model.Job {
	return model.Job{ID: id, BatchID: b, DeviceID: 1, State: st, TargetRelease: "16.1.0", CreatedBy: "adam", CreatedAt: created}
}

// txnPart is the dashboard's transactions card.
func txnPart(body string) string { return between(body, `<h2>Transactions</h2>`, `</section>`) }

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
	for _, want := range []string{"TX-0002", ">Completed<", `<time data-ts="day" datetime="` + at.UTC().Format(time.RFC3339) + `">`} {
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
	want := `<span class="txn-when">started <time data-ts="day" datetime="` + planned.UTC().Format(time.RFC3339) + `">`
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

// Why (design v2, P1, dashboard-empty): a fleet without devices shows only the empty state with
// the three ways to add devices (admins), not six zero tiles and empty lists.
func TestDashboardEmptyFleet(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscovery{}
		d.Import = importer.New(importer.Options{Inventory: &recInv{}})
	})
	delete(e.inv.devs, 1)
	delete(e.inv.devs, 2)
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, `<p class="pghead-sub">0 devices`) || !strings.Contains(body, "<h2>No devices yet</h2>") {
		t.Fatalf("empty dashboard: %s", body)
	}
	for _, want := range []string{`href="/tf/devices/new#card-scan">Scan network`, `href="/tf/devices/new">Add manually`, `href="/tf/devices/new#card-import">Import from TasmoAdmin`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty state: missing %q", want)
		}
	}
	// Updates are off in this setup: the button stays (prototype) and its page says why.
	if !strings.Contains(body, `href="/tf/updates/new">New update`) {
		t.Error("admin: New update is missing")
	}
	if _, page := e.do(t, "GET", "/updates/new", "admin", nil); !strings.Contains(page, "Updates are switched off") {
		t.Error("New update with updates off does not explain why")
	}
	if strings.Contains(body, "stat-grid") || strings.Contains(body, "Needs attention") {
		t.Error("the empty state still draws tiles or lists")
	}
	_, body = e.do(t, "GET", "/", "operator", nil)
	if strings.Contains(body, "Scan network") || strings.Contains(body, "Add manually") {
		t.Error("operator is offered admin actions in the empty state")
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
// like "Firmware behind" without a release list. One value known (here a compliant one) makes 0 real.
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
		{"nothing read", nil, `>Policy deviations</span><span class="stat-value">–</span><span class="stat-hint">not checked yet</span>`},
		{"one value read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`>Policy deviations</span><span class="stat-value">0</span><span class="stat-hint">devices, 0 rules, 2 not checked yet</span>`},
	} {
		e, _ := dashFleet(t, func(d *Deps) { d.Policy = &fakePolicy{rep: PolicyReport{Findings: unread, Provenance: c.prov}} })
		_, body := e.do(t, "GET", "/", "admin", nil)
		tiles := strings.ReplaceAll(between(body, `<div class="stat-grid">`, `<svg class="health-bar"`), "</span> <span", "</span><span")
		if !strings.Contains(tiles, c.want) {
			t.Errorf("%s: missing %q in %s", c.name, c.want, tiles)
		}
	}
}

// Why: the device list's "Not compliant" chip must follow the tile's rule: "–" while nothing was
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
		{"nothing read", nil, `title="2 not checked yet"><span class="chip-l">Not compliant</span><span class="chip-n">–</span>`},
		{"partly read", map[model.DeviceID]map[string]ValueSource{6: {"teleperiod": {Kind: "live", At: time.Now()}}},
			`title="2 not checked yet"><span class="chip-l">Not compliant</span><span class="chip-n">0</span>`},
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
	got := dashProposals("", props, nil)
	if len(got) != 1 || got[0].Name != "Live Lamp" {
		t.Fatalf("dashboard proposals = %+v, want only the online one", got)
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
	if href != "/tf/devices/backups?filter=none" || !strings.HasPrefix(hint, "Last run <time") || !strings.Contains(hint, `data-ts="day"`) || !strings.HasSuffix(hint, " · 2 failed") || !strings.Contains(tone, "warn") {
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
