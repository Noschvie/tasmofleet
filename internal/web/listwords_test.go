package web

import (
	"bytes"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
)

// rowIDs are the devices a list page shows (the table and the phone cards both link to each).
func rowIDs(body string) map[int]bool {
	ids := map[int]bool{}
	for _, m := range regexp.MustCompile(`href="/tf/devices/(\d+)"`).FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[1])
		ids[n] = true
	}
	return ids
}

// Why (#1132, A1-025): the dashboard tiles Offline and Unknown and the list's chips of the same names must count the
// same devices, and a tile must open exactly the devices it counted. They were one chip, "Offline / unknown", fed by
// two tiles that both linked to it, so a person who clicked "Unknown (2)" saw six rows. The fleet has the hard case
// too: a device whose version was read over MQTT (that sets Version, never DetectedAt) which never answered in this
// run. The tile called it Unknown, the badge and ?health=offline called it Offline: three answers for one device.
func TestStateChipsEqualTiles(t *testing.T) {
	seen := time.Now().Add(-time.Hour)
	e := setup(t) // 1 kitchen online, 2 attic: no health yet, so unknown
	e.inv.devs[3] = model.Device{ID: 3, Name: "hallway", Address: "10.0.0.3", Identity: model.Identity{Version: "15.0.0", DetectedAt: seen}}
	e.inv.devs[4] = model.Device{ID: 4, Name: "annex", Address: "10.0.0.4"}
	e.inv.devs[5] = model.Device{ID: 5, Name: "porch", Address: "10.0.0.5", Identity: model.Identity{Version: "14.3.0"}} // version over MQTT
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOffline, LastSeen: seen}
	e.health[4] = model.Health{DeviceID: 4, State: model.HealthOffline}
	e.health[5] = model.Health{DeviceID: 5, State: model.HealthOffline}

	_, dash := e.do(t, "GET", "/", "operator", nil)
	tile := func(label string) (n int, href string) {
		m := regexp.MustCompile(`<a class="stat-tile" href="([^"]*)"><span class="stat-label"><span [^>]*></span>` + label + `</span>\s*<span class="stat-value">(\d+)</span>`).FindStringSubmatch(dash)
		if m == nil {
			t.Fatalf("no %s tile in the dashboard", label)
		}
		n, _ = strconv.Atoi(m[2])
		return n, strings.ReplaceAll(m[1], "&amp;", "&")
	}
	_, list := e.do(t, "GET", "/devices", "operator", nil)
	chips := dvChips(list)
	if strings.Contains(chips, "Offline / unknown") {
		t.Errorf("the merged chip is gone, the states are separate chips: %s", chips)
	}
	for label, want := range map[string]int{"Offline": 1, "Unknown": 3} {
		n, href := tile(label)
		m := regexp.MustCompile(`<span class="chip-l">` + label + `</span><span class="chip-n">(\d+)</span>`).FindStringSubmatch(chips)
		if m == nil {
			t.Fatalf("no %s chip: %s", label, chips)
		}
		if c, _ := strconv.Atoi(m[1]); c != n || n != want {
			t.Errorf("%s: tile %d, chip %s, want %d (hallway answered once; annex, attic and porch never did)", label, n, m[1], want)
		}
		if !strings.HasSuffix(href, "/devices?health="+strings.ToLower(label)) {
			t.Errorf("the %s tile links to %q, want the health filter of the chip", label, href)
		}
		_, shown := e.do(t, "GET", strings.TrimPrefix(href, "/tf"), "operator", nil)
		if got := len(rowIDs(shown)); got != n {
			t.Errorf("the %s tile counts %d but its link lists %d devices", label, n, got)
		}
	}
	// the MQTT-read device is Unknown in the filter and in its badge: one definition (neverAnswered)
	_, off := e.do(t, "GET", "/devices?health=offline", "operator", nil)
	_, unk := e.do(t, "GET", "/devices?health=unknown", "operator", nil)
	if rowIDs(off)[5] || !rowIDs(unk)[5] {
		t.Errorf("porch (version over MQTT, never answered) is in Offline: %v, in Unknown: %v", rowIDs(off)[5], rowIDs(unk)[5])
	}
	if badge := between(unk, `sse-swap="health-5"`, "</td>"); !strings.Contains(badge, ">Unknown<") || strings.Contains(badge, ">Offline<") {
		t.Errorf("porch's badge: %s", badge)
	}
}

// Why (#1132, A1-029): "Back up now" twice within the cooldown is not a failure, so the answer is a warn banner in
// the list's notice slot with one plain sentence, not a red toast that counts seconds.
func TestListBackupCooldownWarnBanner(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.BackupCooldown = time.Hour })
	var reswap string
	post := func() (int, string, string) {
		resp, body := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{"from": {"list"}}, "HX-Request", "true")
		reswap = resp.Header.Get("HX-Reswap")
		return resp.StatusCode, body, resp.Header.Get("HX-Trigger")
	}
	if code, body, _ := post(); code != 200 || strings.TrimSpace(body) != "" {
		t.Fatalf("first: %d %q", code, body)
	}
	if reswap != "" {
		t.Errorf("an empty answer must not jump the page: HX-Reswap %q", reswap)
	}
	code, body, trig := post()
	// Why: the slot is above the toolbar and does not stick; a click on a row below the fold would look dead
	// if the refusal were swapped in out of sight (the red toast it replaced was always on screen).
	if reswap != "innerHTML show:#list-notice:top" {
		t.Errorf("the refusal must scroll its slot into view: HX-Reswap %q", reswap)
	}
	if code != 429 || e.bk.taken != 1 {
		t.Fatalf("second: status %d, backups taken %d (a refused click must not back up)", code, e.bk.taken)
	}
	if !strings.Contains(body, `class="banner t-warn`) || !strings.Contains(body, "Too many requests. Try again in a few seconds.") {
		t.Errorf("want a warn banner with the sentence: %s", body)
	}
	if strings.Contains(body, "t-danger") || strings.Contains(body, "role=\"alert\"") {
		t.Errorf("a refusal is no failure: %s", body)
	}
	if strings.Contains(trig, "toast") {
		t.Errorf("no toast beside the banner: %s", trig)
	}
	if strings.Contains(body, " s ago") || strings.Contains(body, "possible in") {
		t.Errorf("no timing words: %s", body)
	}
	// the device page says the same sentence (header refusal, Backups tab)
	_, tab := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{"from": {"tab"}}, "HX-Request", "true")
	if !strings.Contains(tab, "Too many requests. Try again in a few seconds.") {
		t.Errorf("the Backups tab: %s", tab)
	}
	_, page := e.do(t, "POST", "/devices/1/backup", "operator", url.Values{})
	if !strings.Contains(page, "Too many requests. Try again in a few seconds.") {
		t.Errorf("the header's Back up: %s", page)
	}
}

// Why (#1132, #1204): the row menu opens the device's own web page, through the one link rule (deviceWebLink:
// plain host, never user information), in a new tab that cannot reach back. A device without a usable address has
// no such item: a dead entry is worse than none.
func TestRowMenuOpenWebUI(t *testing.T) {
	render := func(d model.Device) string {
		var b bytes.Buffer
		if err := deviceMenu(rowMenu{Base: "/tf", Console: true, Admin: true, Device: d}).Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	menu := render(model.Device{ID: 3, Name: "kitchen", Address: "192.0.2.10"})
	a := regexp.MustCompile(`<a [^>]*>Open web UI[^<]*(?:<span[^>]*>[^<]*</span>)?</a>`).FindString(menu)
	if a == "" {
		t.Fatalf("no Open web UI item: %s", menu)
	}
	for _, want := range []string{`href="http://192.0.2.10"`, `target="_blank"`, `rel="noopener noreferrer"`, "(opens in a new tab)"} {
		if !strings.Contains(a, want) {
			t.Errorf("Open web UI misses %q: %s", want, a)
		}
	}
	if strings.Contains(a, "data-needs-device") {
		t.Errorf("an offline device's last known address is still a link: %s", a)
	}
	if o, w := strings.Index(menu, ">Open</a>"), strings.Index(menu, "Open web UI"); o < 0 || o > w {
		t.Errorf("Open web UI follows Open: %s", menu)
	}
	if got := render(model.Device{ID: 3, Name: "x", Address: "192.0.2.10", Port: 8080}); !strings.Contains(got, `href="http://192.0.2.10:8080"`) {
		t.Errorf("a port stays in the link: %s", got)
	}
	for _, addr := range []string{"", "admin:pw@192.0.2.10", "192.0.2.10/evil"} {
		if got := render(model.Device{ID: 3, Name: "x", Address: addr}); strings.Contains(got, "Open web UI") {
			t.Errorf("address %q is no link, the item is left out: %s", addr, got)
		}
	}
}

// Why (#1132, A1-022): the column says what it is about ("Reachable via") and names only the channels that carry
// the device now, in words: "MQTT", "Web" or "MQTT, Web". Green check marks on 54 of 60 rows reported the normal
// case; a problem is the "MQTT down" badge. The live update (SSE) renders the same cell, so it reads the same.
func TestReachableViaWords(t *testing.T) {
	now := time.Now()
	e := setup(t)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: now, SeenMQTT: now, SeenHTTP: now}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: now, SeenMQTT: now}
	e.inv.devs[3] = model.Device{ID: 3, Name: "shed", Address: "10.0.0.3"}
	e.health[3] = model.Health{DeviceID: 3, State: model.HealthOnline, LastSeen: now, SeenHTTP: now}
	e.inv.devs[4] = model.Device{ID: 4, Name: "annex", Address: "10.0.0.4"}
	e.health[4] = model.Health{DeviceID: 4, State: model.HealthOffline}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `data-label="Reachable via"`) || strings.Contains(body, "MQTT · HTTP") {
		t.Errorf("the column is named Reachable via: %s", between(body, "<thead", "</thead>"))
	}
	cell := func(id string) string {
		c := between(body, `sse-swap="reach-`+id+`"`, "</td>")
		c = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(c, "")
		return strings.Join(strings.Fields(strings.TrimPrefix(c, ">")), " ")
	}
	for id, want := range map[string]string{"1": "MQTT, Web", "2": "MQTT", "3": "Web", "4": "-"} {
		got := cell(id)
		if got != want {
			t.Errorf("device %s reads %q, want %q", id, strings.TrimSpace(got), want)
		}
		if strings.ContainsAny(got, "✓✕–") {
			t.Errorf("device %s: no marks: %q", id, got)
		}
	}
	ev := streamUntil(t, e, events.Event{Topic: events.TopicHealth, Payload: e.health[1]}, "event: wifi-1")
	if live := between(ev, "event: reach-1\ndata: ", "\n\n"); !strings.Contains(live, ">MQTT</span>") || !strings.Contains(live, ">Web</span>") || strings.Contains(live, "✓") {
		t.Errorf("the live cell reads like the page's: %q", live)
	}
	_, p := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{`placeholder="Name, address or MAC"`} {
		if !strings.Contains(p, want) {
			t.Errorf("search field: missing %q", want)
		}
	}
	if strings.Contains(p, `placeholder="Filter devices"`) || strings.Contains(p, "chip or firmware") {
		t.Errorf("the placeholder shows an example and the help says what is searched (name, address, MAC, topic): %s", between(p, `role="search"`, "</form>")[:300])
	}
}

// Why (#1132): chip=down is gone without an alias. An old link (a bookmark, a shared URL) must read as All, with All
// marked, and the dead key must not ride along with every later search through the hidden chip input.
func TestUnknownChipReadsAsAll(t *testing.T) {
	if f := filterOf(url.Values{"chip": {"down"}, "q": {"a"}}); f.Chip != "" || f.Q != "a" {
		t.Errorf("chip=down: %+v", f)
	}
	if f := filterOf(url.Values{"chip": {chipOutdated}}); f.Chip != chipOutdated {
		t.Errorf("a real chip is kept: %+v", f)
	}
}
