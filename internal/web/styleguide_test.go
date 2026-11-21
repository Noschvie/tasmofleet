package web

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/policy"
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
	for _, want := range []string{`<h2>Transactions</h2>`, "No transactions yet.", `href="/tf/updates/new">New update`} {
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

// Why (#514): the style guide is the reference new pages are copied from. Its Severity control
// said "Advisory" for warn and invented a value "require"; the app's warn is the stronger level
// ("Required"), info is "Advisory". Someone copying the sample would save the stronger level
// under the weaker name. The sample is built from severityOptions, so it cannot drift again.
func TestStyleguideSeveritySampleIsTheAppsSeverityControl(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/styleguide", "operator", nil)
	re := regexp.MustCompile(`<input type="radio" name="sg-sev" value="([^"]*)"( checked)?/?>\s*<span>([^<]*)</span>`)
	got := re.FindAllStringSubmatch(body, -1)
	want := severityOptions(policy.SeverityWarn)
	if len(got) != len(want) {
		t.Fatalf("sg-sev has %d options, the app's severity control has %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i][1] != w.Value || html.UnescapeString(got[i][3]) != w.Label || (got[i][2] != "") != w.Checked {
			t.Errorf("sg-sev option %d = value %q label %q checked %v; the app has value %q label %q checked %v",
				i, got[i][1], got[i][3], got[i][2] != "", w.Value, w.Label, w.Checked)
		}
	}
}

// Why (#515): the style guide is the accessibility reference of the app, so it has to pass the
// checks the app's pages are held to: unique ids (an htmx out-of-band swap of #crumbs hits the
// first match), one top-level heading and one Breadcrumb landmark. The component samples draw the
// page header, crumbs, error page and sign-in card without that page-level identity; they must
// still be on the page, or the fix would be to delete the samples.
func TestStyleguideHasOneH1OneBreadcrumbAndUniqueIDs(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/styleguide", "operator", nil)
	seen := map[string]int{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id=%q appears %d times", id, n)
		}
	}
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("the page has %d h1 elements, want 1 (the page's own title)", n)
	}
	if n := strings.Count(body, `aria-label="Breadcrumb"`); n != 1 {
		t.Errorf("the page has %d Breadcrumb landmarks, want 1 (the shell's)", n)
	}
	for name, want := range map[string]string{
		"page header sample": "Balcony Windowsill",
		"crumbs sample":      ">Balcony Windowsill</a>",
		"error page sample":  "This page doesn’t exist",
		"sign-in sample":     "Sign in</div>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s is gone: missing %q", name, want)
		}
	}
}

// Why (#515): the samples draw their title as a div, not an h1. It must look exactly like the h1
// it stands in for, so the style guide still shows what the page header, the error page and the
// sign-in card really look like: each h1 rule of those three components also styles .h1-sample.
func TestSampleTitlesAreStyledLikeTheH1TheyReplace(t *testing.T) {
	b, err := Assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{".pghead", ".errpage", ".auth-card"} {
		re := regexp.MustCompile(regexp.QuoteMeta(c+" h1,") + `\s*` + regexp.QuoteMeta(c+" .h1-sample") + `\s*\{`)
		if !re.Match(b) {
			t.Errorf("app.css: the %s h1 rule does not also style .h1-sample", c)
		}
	}
}
