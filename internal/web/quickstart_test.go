package web

import (
	"strings"
	"testing"
)

// stepsOf returns the list items of the quick start's ordered list, and whether there is one.
func stepsOf(body string) ([]string, bool) {
	i := strings.Index(body, `<ol class="uempty-steps"`)
	if i < 0 {
		return nil, false
	}
	list := body[i:]
	list = list[:strings.Index(list, "</ol>")]
	var items []string
	for _, part := range strings.Split(list, "<li>")[1:] {
		items = append(items, part[:strings.Index(part, "</li>")])
	}
	return items, true
}

// Why (#1050): a fresh install is a blank list and a person who has no idea what comes next. The empty Devices
// page names the three things that make a first fleet (the broker, the devices, the first backup) in the order they
// depend on each other, with one primary button to the next click, never more than three. The live fragment that
// replaces the list after the last device was deleted renders the same card, so both are checked. An operator may
// do none of it: they keep the one line that says who can, and no link to a page they would be refused.
func TestEmptyDeviceListShowsAdminsTheQuickStart(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Inventory = newInv()
		d.SysConfig = aclBrokerService(t)
	})
	for _, path := range []string{"/devices", "/devices/table"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		steps, ok := stepsOf(body)
		if !ok || len(steps) != 3 {
			t.Fatalf("%s: want a list of three steps, got %v (list: %v): %.600s", path, steps, ok, body)
		}
		for i, word := range []string{"MQTT broker", "Add devices", "Back"} {
			if !strings.Contains(steps[i], word) {
				t.Errorf("%s: step %d = %q, want it to name %q", path, i+1, steps[i], word)
			}
		}
		card := body[strings.Index(body, `<div class="btn-row">`):]
		card = card[:strings.Index(card, "</div>")]
		if n := strings.Count(card, `class="btn`); n < 1 || n > 3 {
			t.Errorf("%s: %d buttons in the empty card, want one to three: %s", path, n, card)
		}
		for _, href := range []string{"/devices/new", "?tab=mqtt"} {
			if !strings.Contains(card, href+`"`) {
				t.Errorf("%s: no link to %s in the empty card", path, href)
			}
		}
	}

	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, "<h2>No devices yet</h2>") || !strings.Contains(body, "An administrator can add the first ones.") {
		t.Errorf("the operator lost the line that says who adds devices: %.600s", body)
	}
	if _, ok := stepsOf(body); ok || strings.Contains(body, "tab=mqtt") {
		t.Errorf("an operator sees the admin quick start or a link to the broker settings: %.600s", body)
	}
}

// Why (#1050): without the settings service (the configuration comes from the environment) there is no MQTT tab, so
// the first step must not send an admin to a page that does not exist.
func TestQuickStartWithoutSettingsPageDoesNotLinkIt(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Inventory = newInv() })
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	steps, ok := stepsOf(body)
	if !ok || len(steps) != 3 {
		t.Fatalf("want three steps, got %v: %.600s", steps, body)
	}
	if strings.Contains(body, "tab=mqtt") || strings.Contains(steps[0], "Settings") {
		t.Errorf("the first step points to a settings page this install does not have: %q", steps[0])
	}
}

// Why (#1050): a first-run person reads the broker as the way TasmoFleet talks to devices, so the setup asks for it
// before the device networks (the networks only matter to the HTTP fallback and to the optional scan), and says in
// one sentence that HTTP is the fallback. A broker error replaces that sentence, as every other help line.
func TestSetupAsksForTheBrokerBeforeTheNetworks(t *testing.T) {
	e, _, _ := setupHandlerEnv(t)
	_, body := e.do(t, "GET", "/setup", "admin", nil)
	host, nets := strings.Index(body, `name="mqtt.host"`), strings.Index(body, `name="v.DEVICE_NETWORKS"`)
	if host < 0 || nets < 0 || host > nets {
		t.Fatalf("the broker (%d) must come before the device networks (%d)", host, nets)
	}
	const fallback = "HTTP is only the fallback"
	if n := strings.Count(body, fallback); n != 1 {
		t.Errorf("the fallback sentence appears %d times, want once", n)
	}
	if !strings.Contains(body[host:nets], fallback) {
		t.Errorf("the fallback sentence is not under the broker fields")
	}
}

// Why (#1050): the page is called Dashboard in the menu, the crumb and the tab title; a heading that reads
// "Fleet overview" made first-run people look for a second page.
func TestDashboardHeadingIsItsName(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, "<h1>Dashboard</h1>") || strings.Contains(body, "Fleet overview") {
		t.Errorf("the dashboard heading is not Dashboard: %.800s", body)
	}
}
