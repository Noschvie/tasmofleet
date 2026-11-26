package web

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// webUIEnv is the test fleet with the first device set to the given address and port and the given health.
func webUIEnv(t *testing.T, address string, port int, hl model.Health, mutate ...func(*Deps)) *env {
	t.Helper()
	e := setupWith(t, func(d *Deps) {
		for _, m := range mutate {
			m(d)
		}
	})
	d := e.inv.devs[1]
	d.Address, d.Port = address, port
	e.inv.devs[1] = d
	hl.DeviceID = 1
	e.health[1] = hl
	return e
}

func answered(state model.HealthState) model.Health {
	return model.Health{State: state, LastSeen: time.Now()}
}

// addrBlockOf is the address block of the Connection card in s: its rows and, when it has one, the caveat line.
func addrBlockOf(s string) string {
	_, rest, ok := strings.Cut(s, `<div id="devpaths-addr"`)
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, ">") // the rest of the div's own tag (the live answer's hx-swap-oob)
	rows, tail, _ := strings.Cut(rest, "</dl>")
	if strings.HasPrefix(tail, `<p class="devcard-note">`) {
		note, _, _ := strings.Cut(tail, "</p>")
		return rows + note
	}
	return rows
}

var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

const webUINoteText = "Opens only where this address is reachable, such as your home network or a VPN."

// Why (#1204): the person who looks at a device wants its own page one click away, the same one rule as the list's
// Address link (deviceWebLink): it opens in a new tab without handing the page an opener, and never carries the
// credentials (safety rule 4). Where the address cannot become a link (empty, user information, a path) there is no
// button and no caveat line about it.
func TestOverviewOpenWebUI(t *testing.T) {
	cases := []struct {
		name, address string
		port          int
		href          string // empty: no link
	}{
		{"address and port", "192.0.2.10", 8080, "http://192.0.2.10:8080"},
		{"the default port is left out", "192.0.2.10", 80, "http://192.0.2.10"},
		{"an IPv6 literal gets brackets", "2001:db8::10", 8080, "http://[2001:db8::10]:8080"},
		{"no address", "", 0, ""},
		{"user information is refused", "admin:pw@192.0.2.10", 0, ""},
		{"a path is refused", "192.0.2.10/evil", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := webUIEnv(t, tc.address, tc.port, answered(model.HealthOnline))
			r, body := e.do(t, "GET", "/devices/1", "admin", nil)
			if r.StatusCode != 200 {
				t.Fatalf("status %d", r.StatusCode)
			}
			// The address field itself is shown as stored (it is not a secret); what must never happen is a link
			// that carries it as user information, or the device's own web login anywhere (safety rule 4).
			for _, m := range hrefRe.FindAllStringSubmatch(body, -1) {
				if strings.Contains(m[1], "@") || strings.Contains(m[1], "s3cret-pw") {
					t.Fatalf("a link carries credentials: %s", m[1])
				}
			}
			if strings.Contains(body, "s3cret-pw") {
				t.Fatal("the device's web password reached the page")
			}
			card := sectionOf(body, `<section id="devpaths"`)
			text := html.UnescapeString(visibleText(card))
			if tc.href == "" {
				if strings.Contains(text, "Open web UI") || strings.Contains(text, webUINoteText) {
					t.Fatalf("an address that is no link must offer no Open web UI and no caveat: %s", text)
				}
				return
			}
			for _, want := range []string{
				`id="devpaths-webui"`, `href="` + tc.href + `"`, `target="_blank"`, `rel="noopener noreferrer"`,
				`aria-label="Open web UI (opens in a new tab)"`, ">Open web UI<",
			} {
				if !strings.Contains(card, want) {
					t.Fatalf("the Connection card lacks %s:\n%s", want, card)
				}
			}
			if !strings.Contains(text, webUINoteText) {
				t.Fatalf("the caveat line is missing: %s", text)
			}
			if strings.Contains(text, "(last known)") {
				t.Fatalf("an online device's address is current: %s", text)
			}
		})
	}
}

// Why (#1204): an offline device may still serve its own page (only MQTT may be down), so the link stays; but the
// address is what TasmoFleet stored, not something the device just said, so it must not read as current. A device
// that is being updated is silent on purpose, not offline; one that never answered has no earlier answer to call
// "last known".
func TestOverviewAddressLastKnown(t *testing.T) {
	cases := []struct {
		name  string
		addr  string
		hl    model.Health
		jobs  bool
		known bool
	}{
		{"offline", "192.0.2.10", answered(model.HealthOffline), false, true},
		{"online", "192.0.2.10", answered(model.HealthOnline), false, false},
		{"never answered", "192.0.2.10", model.Health{State: model.HealthOffline}, false, false},
		{"an update holds the device", "192.0.2.10", answered(model.HealthOffline), true, false},
		// A device added from the broker has no stored address: nothing is "last known" about nothing.
		{"offline without an address", "", answered(model.HealthOffline), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := webUIEnv(t, tc.addr, 0, tc.hl, func(d *Deps) {
				if tc.jobs {
					d.Jobs = &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobApplying, CreatedAt: time.Now()}}}
				}
			})
			_, body := e.do(t, "GET", "/devices/1", "admin", nil)
			card := sectionOf(body, `<section id="devpaths"`)
			text := html.UnescapeString(visibleText(card))
			if got := strings.Contains(text, "Address 192.0.2.10 (last known)"); got != tc.known {
				t.Fatalf("last known %v, want %v: %s", got, tc.known, text)
			}
			if tc.addr == "" {
				if strings.Contains(text, "(last known)") {
					t.Fatalf("no address, so no last known mark: %s", text)
				}
				return
			}
			if !strings.Contains(card, `href="http://192.0.2.10"`) {
				t.Fatalf("the link stays in every state: %s", card)
			}
		})
	}
}

// Why (#1204): the Connection card is not reloaded with the page, so a device that goes offline (or comes back) while
// the page is open must change the address line through the live refresh, and the refresh must say the same as the
// page, also when the card's paths could not be read (this environment has no transport service, so Paths is nil: the
// address rows are drawn without them, and the block must still be swapped).
func TestDeviceLiveSwapsAddressBlock(t *testing.T) {
	off := webUIEnv(t, "192.0.2.10", 8080, answered(model.HealthOffline))
	_, live := off.do(t, "GET", "/devices/1/live", "admin", nil, "HX-Request", "true")
	block := addrBlockOf(live)
	if !strings.Contains(live, `<div id="devpaths-addr" hx-swap-oob="true"`) || !strings.Contains(visibleText(block), "192.0.2.10:8080 (last known)") {
		t.Fatalf("the live answer does not carry the address block marked last known:\n%s", live)
	}
	_, page := off.do(t, "GET", "/devices/1", "admin", nil)
	if a, b := html.UnescapeString(visibleText(addrBlockOf(page))), html.UnescapeString(visibleText(block)); a != b {
		t.Fatalf("page and live answer differ:\n%s\n%s", a, b)
	}

	on := webUIEnv(t, "192.0.2.10", 8080, answered(model.HealthOnline))
	_, live = on.do(t, "GET", "/devices/1/live", "admin", nil, "HX-Request", "true")
	if !strings.Contains(live, `id="devpaths-addr"`) || strings.Contains(visibleText(live), "(last known)") {
		t.Fatal("a device that is back must lose its last known mark through the live answer")
	}
}
