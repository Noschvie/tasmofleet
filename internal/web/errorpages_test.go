package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/auth"
)

// Why: Go's plain "404 page not found" has no app shell and no way back; a mistyped or
// stale link must land on the app's own page (visual QA finding 43).
func TestUnknownPathShowsAppNotFoundPage(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/no/such/page", "/transactions/7/nope"} {
		resp, body := e.do(t, "GET", p, "operator", nil)
		if resp.StatusCode != 404 {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
		if !strings.Contains(body, "404 · Not found") || !strings.Contains(body, `<nav class="tb-groups" aria-label="Main">`) || !strings.Contains(body, ">Go to dashboard</a>") {
			t.Fatalf("%s: not the app's not-found page: %.300s", p, body)
		}
	}
}

// Why: a bookmark to /firmware, /transactions or /updates/new must explain that OTA is off
// (no firmware base URL) instead of looking like a broken link (finding 43).
func TestSwitchedOffUpdatePagesExplain(t *testing.T) {
	e := setup(t) // no OTA, no firmware service
	for _, p := range []string{"/transactions", "/updates/new", "/firmware"} {
		resp, body := e.do(t, "GET", p, "operator", nil)
		if resp.StatusCode != 404 || !strings.Contains(body, "no firmware base URL") || !strings.Contains(body, "Go to the dashboard") {
			t.Fatalf("%s: status %d, body %.300s", p, resp.StatusCode, body)
		}
	}
}

// Why: the Add devices tabs must not link to /import/tasmoadmin when the web import is off
// (header mode): that tab was a dead link (finding 43).
func TestAddTabsHideImportWhenUnavailable(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "/import/tasmoadmin") {
		t.Fatalf("import tab without the import feature: %s", body)
	}
	if !strings.Contains(body, "Add manually") {
		t.Fatalf("add tabs missing: %.300s", body)
	}
}

// Why: "No devices match" for a fresh install sends the admin in circles; an empty inventory
// says so and offers the ways to add devices, a filter that matches nothing offers to clear the
// filter, and neither shows a table whose header stops short (finding 14, 46; design v2 wording).
func TestDevicesEmptyStates(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Inventory = newInv() })
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, "No devices yet") || !strings.Contains(body, "Add manually") || strings.Contains(body, "<table") {
		t.Fatalf("empty inventory: %.600s", body)
	}
	e = setup(t)
	_, body = e.do(t, "GET", "/devices?q=zzz-no-such-device", "admin", nil)
	if !strings.Contains(body, "No devices match these filters") || !strings.Contains(body, "Clear filters") || strings.Contains(body, "<table") {
		t.Fatalf("no match: %.600s", body)
	}
}

// Why: Go's list and duration forms ("[127.0.0.1/32]", "720h0m0s") are not for people, and a
// blank value must not look like a masked secret (finding 77).
func TestCfgText(t *testing.T) {
	for in, want := range map[string]string{"[127.0.0.1/32]": "127.0.0.1/32", "[a b]": "a, b", "[]": "", "720h0m0s": "30d 0h", "debug": "debug", "http://h/x": "http://h/x"} {
		if got := cfgText(in); got != want {
			t.Errorf("cfgText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why: a service's lowercase clause ("replaced; restored") is not a sentence on a page (finding 55, 57).
func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"replaced; restored": "Replaced; restored.", " already done. ": "Already done.", "": ""} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why (design v2, e403): a signed-in user who opens a page above their role keeps the
// navigation and reads which role the page needs, what theirs allows and which address was
// refused, with a way back; a bare "access denied" leaves them guessing.
func TestForbiddenPageKeepsTheShellAndExplains(t *testing.T) {
	pages := NewPages()
	setupWith(t, func(d *Deps) { d.Pages = pages })
	body := renderAccountPage(t, pages, func(w http.ResponseWriter, r *http.Request) {
		pages.Message(w, r, http.StatusForbidden, auth.MessageData{BasePath: "/tf", Title: "Only admins can open this page", Text: "You are signed in as ops with the operator role."})
	})
	for _, want := range []string{`<nav class="tb-groups" aria-label="Main">`, "403 · Not allowed", "<h1>Only admins can open this page</h1>", "with the operator role", `<div class="errpage-path">/tf/x</div>`, `href="/tf/">Back to dashboard</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("403 page lacks %q", want)
		}
	}
}
