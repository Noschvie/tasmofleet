package web

import (
	"strings"
	"testing"
)

// Why: "if a component is not on the style guide, it is not part of the design system". The
// page is the prototype's UI-states screen (every banner tone, the three toasts, the three
// connection states) and then every component, with each overlay drawn open in the flow so a
// reviewer, the layout guards and make styleguide-docs see it without clicking.
func TestStyleguideShowsStatesAndOpenOverlays(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/styleguide", "operator", nil)
	for name, want := range map[string]string{
		"ok banner":         `class="banner tone-ok" role="status"`,
		"info banner":       `class="banner tone-accent" role="status"`,
		"warn banner":       `class="banner tone-warn" role="status"`,
		"failure banner":    `class="banner tone-danger" role="alert"`,
		"failure toast":     `<span class="toast-text">Restart failed on Laundry Ceiling Light: timed out</span>`,
		"live states":       `class="live-pill lp-off"`,
		"toast trigger":     `data-toast="Settings saved" data-toast-tone="ok"`,
		"user menu open":    `<details class="navgroup usermenu" open data-static>`,
		"drawer list":       `<div class="dr"><div class="dr-brand">`,
		"row menu open":     `<details class="rowmenu umenu" open data-static>`,
		"disabled reason":   `<span class="menu-why">The last admin can&#39;t be deleted.</span>`,
		"dialog open":       `<dialog id="sg-dlg-delete" class="udlg" aria-labelledby="sg-dlg-delete-h" open data-static>`,
		"step-up open":      `<dialog id="sg-stepup" class="udlg stepup" aria-labelledby="sg-stepup-h" open data-static>`,
		"disabled tab":      `aria-disabled="true" title="The device has not answered yet."`,
		"skeleton":          `<div class="skel" aria-busy="true">`,
		"safe console hint": `placeholder="Type a Tasmota command, e.g. Status 5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s: missing %q", name, want)
		}
	}
	// Every section is on the page, in the order of sgSections, as an anchor with its title.
	at := 0
	for _, s := range sgSections {
		i := strings.Index(body[at:], `id="sg-`+s.ID+`"`)
		if i < 0 {
			t.Errorf("section %s is missing or out of order", s.ID)
			continue
		}
		at += i
		if !strings.Contains(body[at:], ">"+s.Title+"</h2>") {
			t.Errorf("section %s has no heading %q", s.ID, s.Title)
		}
	}
	// D9: the console sample never offers Status 0.
	if strings.Contains(body, "Status 0") {
		t.Error("the style guide shows Status 0, which our code never sends (safety rule 2)")
	}
}

// Why: the style guide's static examples must not be mistaken for a live open
// menu by the scripts (they would close it, suppress tooltips or stop the
// table refresh); data-static is their opt-out.
func TestScriptsIgnoreStaticOpenMenus(t *testing.T) {
	for file, want := range map[string]string{
		"static/menu.js":    `!d.hasAttribute("data-static")`,
		"static/tooltip.js": `details.rowmenu[open]:not([data-static])`,
		"static/tables.js":  `details.colmenu[open]:not([data-static])`,
	} {
		b, err := Assets.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not skip data-static menus (%s)", file, want)
		}
	}
}

// Why (QA v0.9.0): the dashboard dropped the whole transactions card while there were none,
// so a fresh install had nothing that says where updates show up and no way to start one.
// Admins get the first step as a button ("New update" in the page header, design v2).
func TestDashboardEmptyTransactionsCard(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = nil
	_, body := e.do(t, "GET", "/", "admin", nil)
	for _, want := range []string{`<h2>Update transactions</h2>`, "No transactions yet.", `href="/tf/updates/new">New update`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin: missing %q", want)
		}
	}
	_, body = e.do(t, "GET", "/", "operator", nil)
	if !strings.Contains(body, "No transactions yet.") {
		t.Error("operator: the empty state is missing")
	}
	if strings.Contains(body, `/tf/updates/new`) {
		t.Error("operator: the dashboard offers New update, which only admins may start")
	}
}

// Why: the dashboard's transactions are a glance, not a data table: the prototype draws a card
// and rows there, with no Columns chooser, and the chooser's key was never registered for it.
func TestDashboardTransactionsTableHasNoColumnChooser(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/", "admin", nil)
	if !strings.Contains(body, `<a class="txn-card" href="/tf/transactions/7">`) {
		t.Fatalf("expected the transaction card: %s", body)
	}
	if dash := between(body, `<div id="dash"`, `</main>`); strings.Contains(dash, "<table") || strings.Contains(dash, "data-table=") {
		t.Error("the dashboard draws a data table (with a Columns chooser)")
	}
}
