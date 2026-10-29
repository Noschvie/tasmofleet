package web

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// listFixture is three new devices whose order differs for every sort key, one of them offline.
func listFixture(t *testing.T) (*env, *fakeDiscovery) {
	t.Helper()
	e, fd := setupDiscovery(t)
	now := time.Now()
	mk := func(id int64, name, topic, addr, mac, via, lwt string, first, seen time.Duration) discovery.Proposal {
		return discovery.Proposal{ID: id, Version: fmt.Sprintf("v%d", id), Kind: discovery.KindNew, Via: via, LWT: lwt,
			FirstSeen: now.Add(-first), SeenAt: now.Add(-seen),
			Device: model.Device{Name: name, Topic: "topic-" + topic[len(topic)-3:], Address: addr, MAC: mac, Net: model.NetFacts{Hostname: topic}}}
	}
	fd.props = []discovery.Proposal{
		mk(1, "Charlie", "tasmota_ccc", "10.0.0.9", "AA:00:00:00:00:02", "mqtt", "Online", 3*time.Hour, 5*time.Minute),
		mk(2, "alpha", "tasmota_bbb", "10.0.0.10", "AA:00:00:00:00:03", "scan", "Offline", 1*time.Hour, 1*time.Minute),
		mk(3, "", "tasmota_aaa", "10.0.0.2", "AA:00:00:00:00:01", "mqtt", "Offline", 2*time.Hour, 9*time.Minute),
	}
	return e, fd
}

// tableOrder lists the topics (hostname column) of the desktop table in the order shown.
func tableOrder(t *testing.T, body string) []string {
	t.Helper()
	i, j := strings.Index(body, "<tbody>"), strings.Index(body, "</tbody>")
	if i < 0 || j < i {
		t.Fatal("no table body")
	}
	var out []string
	for _, m := range regexp.MustCompile(`tasmota_[a-z]+`).FindAllString(body[i:j], -1) {
		if len(out) == 0 || out[len(out)-1] != m {
			out = append(out, m)
		}
	}
	return out
}

func joined(s []string) string { return strings.Join(s, ",") }

// Why: #130, with 40 devices on the broker an admin must tell the live ones from the stale ones;
// Offline is the MQTT last-will, so Online is everything that is not offline.
func TestNewDevicesFilterByState(t *testing.T) {
	e, _ := listFixture(t)
	for state, want := range map[string]string{
		"":        "tasmota_aaa,tasmota_bbb,tasmota_ccc",
		"all":     "tasmota_aaa,tasmota_bbb,tasmota_ccc",
		"online":  "tasmota_ccc",
		"offline": "tasmota_aaa,tasmota_bbb",
		"bogus":   "tasmota_aaa,tasmota_bbb,tasmota_ccc", // an unknown state shows everything, no 500
	} {
		_, body := e.do(t, "GET", "/discovery?state="+state+"&sort=hostname&dir=asc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("state %q: rows %s, want %s", state, got, want)
		}
	}
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	for _, want := range []string{`>All<span class="add-chip-n">3</span>`, `>Online<span class="add-chip-n">1</span>`, `>Offline<span class="add-chip-n">2</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("chips lack %q", want)
		}
	}
	// the chips and the ignored switch keep the other parameters
	_, body = e.do(t, "GET", "/discovery?state=offline&sort=ip&dir=desc", "admin", nil)
	if !strings.Contains(body, "ignored=1") || !strings.Contains(body, "state=online") || !strings.Contains(body, "sort=ip") {
		t.Error("links lose the filter or sort")
	}
}

// Why: a filter that hides every device must say so and keep the chips, or the page looks broken.
func TestNewDevicesFilterWithoutMatchKeepsChips(t *testing.T) {
	e, fd := listFixture(t)
	fd.props = fd.props[1:] // only offline devices left
	_, body := e.do(t, "GET", "/discovery?state=online", "admin", nil)
	if !strings.Contains(body, "No online devices") || !strings.Contains(body, `class="add-chips"`) {
		t.Error("an empty filter result shows no message or no chips")
	}
}

// Why: #130, headers sort like the device list; addresses sort numerically (10.0.0.9 < 10.0.0.10),
// not as text, and both directions work for every key.
func TestNewDevicesSortEveryKey(t *testing.T) {
	e, _ := listFixture(t)
	asc := map[string]string{
		"name":     "tasmota_aaa,tasmota_bbb,tasmota_ccc", // no name first, then alpha, Charlie (case-blind)
		"hostname": "tasmota_aaa,tasmota_bbb,tasmota_ccc",
		"ip":       "tasmota_aaa,tasmota_ccc,tasmota_bbb",
		"mac":      "tasmota_aaa,tasmota_ccc,tasmota_bbb",
		"source":   "tasmota_ccc,tasmota_aaa,tasmota_bbb", // MQTT (id 1, 3) before Scan; ties by ID
		"first":    "tasmota_ccc,tasmota_aaa,tasmota_bbb", // oldest first
		"last":     "tasmota_aaa,tasmota_ccc,tasmota_bbb", // 9 min, 5 min, 1 min ago
	}
	for key, want := range asc {
		_, body := e.do(t, "GET", "/discovery?sort="+key+"&dir=asc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%s asc: %s, want %s", key, got, want)
		}
	}
	// descending reverses, except that equal values keep the ID order (stable, no flicker)
	desc := map[string]string{
		"name":     "tasmota_ccc,tasmota_bbb,tasmota_aaa",
		"hostname": "tasmota_ccc,tasmota_bbb,tasmota_aaa",
		"ip":       "tasmota_bbb,tasmota_ccc,tasmota_aaa",
		"mac":      "tasmota_bbb,tasmota_ccc,tasmota_aaa",
		"source":   "tasmota_bbb,tasmota_ccc,tasmota_aaa",
		"first":    "tasmota_bbb,tasmota_aaa,tasmota_ccc",
		"last":     "tasmota_bbb,tasmota_ccc,tasmota_aaa",
	}
	for key, want := range desc {
		_, body := e.do(t, "GET", "/discovery?sort="+key+"&dir=desc", "admin", nil)
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%s desc: %s, want %s", key, got, want)
		}
	}
}

// Why: the default is what an admin looks for first, the newest announcement; a mistyped or
// hostile sort parameter must fall back to it instead of failing.
func TestNewDevicesDefaultSortAndUnknownKey(t *testing.T) {
	e, _ := listFixture(t)
	want := "tasmota_bbb,tasmota_ccc,tasmota_aaa"
	for _, q := range []string{"", "?sort=bogus", "?sort=bogus&dir=asc", "?sort=;drop&dir=x", "?dir=asc"} {
		r, body := e.do(t, "GET", "/discovery"+q, "admin", nil)
		if r.StatusCode != 200 {
			t.Fatalf("%q: status %d", q, r.StatusCode)
		}
		if got := joined(tableOrder(t, body)); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
}

// Why: the current column's header flips its direction, the others start ascending, and the
// header says which way it is sorted for screen readers.
func TestNewDevicesHeadersAreSortLinks(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/discovery?sort=ip&dir=asc", "admin", nil)
	for _, want := range []string{
		`aria-sort="ascending"><a class="sortlink is-asc" href="/tf/discovery?dir=desc&amp;sort=ip">IP`,
		`<a class="sortlink" href="/tf/discovery?dir=asc&amp;sort=hostname">Hostname`,
		`<a class="sortlink" href="/tf/discovery?dir=asc&amp;sort=name">Device name`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("header lacks %q", want)
		}
	}
}

// Why: #130, Model and Firmware were never filled (a column of dashes) and name and hostname
// were squeezed into one cell.
func TestNewDevicesHasNoModelOrFirmwareAndSplitsNameFromHostname(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/discovery", "admin", nil)
	i := strings.Index(body, "add-disc-table")
	head := body[i : i+strings.Index(body[i:], "</thead>")]
	for _, gone := range []string{">Model<", ">Firmware<", "Name / hostname"} {
		if strings.Contains(head, gone) {
			t.Errorf("table still has %q", gone)
		}
	}
	for _, want := range []string{">Device name<span", ">Hostname<span"} {
		if !strings.Contains(head, want) {
			t.Errorf("table lacks %q", want)
		}
	}
	// a device without a friendly name shows a dash in its name column, its host in the hostname column
	// (the MQTT topic is no column: it is the name cell's tooltip, and never shown as the hostname)
	if !strings.Contains(body, `<td class="name" title="— · topic topic-aaa">`) {
		t.Error("an unnamed device shows no dash, or its topic is lost")
	}
}

// Why: ticking rows and "Add selected" is the point of the page; sorting and filtering must not
// change what a row's checkbox submits.
func TestNewDevicesSelectionSurvivesSortAndFilter(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/discovery?state=offline&sort=name&dir=desc", "admin", nil)
	for _, want := range []string{`value="2:v2"`, `value="3:v3"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, `value="1:v1"`) {
		t.Error("the online device is still selectable under the offline filter")
	}
	if !strings.Contains(body, `action="/tf/discovery/proposals/accept"`) {
		t.Error("the bulk form lost its action")
	}
}

// Why: after Add, Ignore or Restore the admin is back on the list as they left it (filter,
// sort, ignored switch), not on the default view; the page carries them in hidden fields.
func TestNewDevicesActionsKeepStateAndSort(t *testing.T) {
	e, _ := listFixture(t)
	_, body := e.do(t, "GET", "/discovery?state=offline&sort=ip&dir=desc&ignored=1", "admin", nil)
	for _, want := range []string{`name="state" value="offline"`, `name="sort" value="ip"`, `name="dir" value="desc"`, `name="ignored" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("bulk form lacks hidden %s", want)
		}
	}
	form := url.Values{"state": {"offline"}, "sort": {"ip"}, "dir": {"desc"}, "ignored": {"1"}}
	for _, path := range []string{"/discovery/proposals/1/dismiss", "/discovery/proposals/1/undismiss"} {
		r, _ := e.do(t, "POST", path, "admin", form)
		loc := r.Header.Get("Location")
		for _, want := range []string{"state=offline", "sort=ip", "dir=desc", "ignored=1", "ok="} {
			if !strings.Contains(loc, want) {
				t.Errorf("%s redirects to %q, lacking %s", path, loc, want)
			}
		}
	}
	// the default view needs no parameters
	if r, _ := e.do(t, "POST", "/discovery/proposals/1/dismiss", "admin", nil); r.Header.Get("Location") != "/tf/discovery?ok=dismissed" {
		t.Errorf("default redirect %q", r.Header.Get("Location"))
	}
}
