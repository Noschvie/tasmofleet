package ui

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// operator is the admin's shell data for an operator: the same switched-on features, another role.
func operator() ShellData {
	d := admin()
	d.IsAdmin, d.RoleName = false, "operator"
	return d
}

// drawer renders the phone drawer's list (NavList), which /nav shows as a page too.
func drawer(t *testing.T, d ShellData) string {
	t.Helper()
	var b strings.Builder
	if err := NavList(d).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Why (#1106, A1-001): a group with one page has nothing to choose between. A sub-navigation row with that one
// page, or a drawer heading above it, names the same screen a second and third time and costs the dashboard 40 px
// of chrome above its title. So a one-item group is a plain top-bar link (the row is sn-empty) and one drawer link,
// while a group with two or more pages keeps its row.
func TestOneItemGroupHasNoSubNavRow(t *testing.T) {
	d := admin()
	d.Nav, d.Path, d.Title = "dashboard", "/", "Fleet overview"
	got := render(t, d)
	if !strings.Contains(got, `<div class="sn sn-empty"></div>`) || strings.Contains(got, `class="sn-item"`) {
		t.Errorf("the dashboard draws a sub-navigation row for its one page: %s", between(got, `<header class="top">`, "</header>"))
	}
	dr := drawer(t, d)
	if strings.Contains(dr, `<p class="dr-label">Dashboard</p>`) {
		t.Errorf("the drawer heads the dashboard's one link with its own name again: %s", dr)
	}
	if n := strings.Count(dr, `>Dashboard</a>`); n != 1 {
		t.Errorf("the drawer has %d Dashboard links, want 1: %s", n, dr)
	}

	// Updates with the firmware cache but without transactions is a one-item group too.
	fw := admin()
	fw.Transactions, fw.Nav, fw.Path, fw.Title = false, "firmware", "/firmware", "Firmware"
	got = render(t, fw)
	if !strings.Contains(got, `<div class="sn sn-empty"></div>`) {
		t.Errorf("Updates with only Firmware draws a sub-navigation row")
	}
	if strings.Contains(drawer(t, fw), `<p class="dr-label">Updates</p>`) {
		t.Errorf("Updates with only Firmware has a drawer heading above its one link")
	}

	// A group with several pages keeps its row: that is where one picks between them.
	dv := admin()
	dv.Nav, dv.Path = "devices", "/devices"
	if got := render(t, dv); !strings.Contains(got, `<nav class="shell-in sn-in" aria-label="Devices">`) || strings.Contains(got, `sn-empty`) {
		t.Errorf("the Devices group lost its sub-navigation row")
	}
}

// Why (#1106, A1-003, A1-005, A1-006): each page has one entry and one name. "Add" was a menu entry next to the
// Add devices button that opens the same page; the style guide is a developer's page that About links, not a task;
// and "Backups" next to "Snapshots" made people look in two groups for one word, so the pages are named by what
// they hold.
func TestMenuHasOneEntryPerPageNamedByItsObject(t *testing.T) {
	want := "Dashboard[Dashboard;]Devices[List;Policy;Device backups;]Updates[Activity;Firmware;]System[Settings;TasmoFleet backup;Audit log;Users;About;]"
	if got := labels(admin().Groups()); got != want {
		t.Errorf("admin menu\n got %s\nwant %s", got, want)
	}
	for _, g := range admin().Groups() {
		for _, it := range g.Items {
			if it.Path == "/devices/new" || it.Path == "/styleguide" {
				t.Errorf("%s lists %s (%s): the page has its button or its link elsewhere", g.Label, it.Label, it.Path)
			}
		}
	}
}

// Why (#1106, decision 1): an operator's menu holds only pages they may open. Entries that answer 403 sent them
// to a refusal, and the System group, all of whose pages are admin-only, led them to the style guide. The pages
// still refuse on their own (403); the menu only stops offering them, and a group left empty is not shown.
func TestOperatorSeesOnlyPagesTheyMayOpen(t *testing.T) {
	if got, want := labels(operator().Groups()), "Dashboard[Dashboard;]Devices[List;Policy;Device backups;]Updates[Activity;Firmware;]"; got != want {
		t.Errorf("operator menu\n got %s\nwant %s", got, want)
	}
	bare := ShellData{Title: "x", Nav: "dashboard", UserName: "o", UserMethod: "header", RoleName: "operator"}
	if got, want := labels(bare.Groups()), "Dashboard[Dashboard;]Devices[List;]"; got != want {
		t.Errorf("operator without services\n got %s\nwant %s", got, want)
	}
	op := operator()
	op.Nav, op.Path = "devices", "/devices"
	page := render(t, op)
	for _, link := range []string{"/settings/system", "/users", "/audit", "/about", "/devices/new", "/styleguide"} {
		if strings.Contains(page, `href="`+link+`"`) {
			t.Errorf("an operator's page offers %s, which answers them 403 or is no menu entry", link)
		}
	}
	if strings.Contains(page, `>System</a>`) || strings.Contains(page, `<p class="dr-label">System</p>`) {
		t.Errorf("an operator sees the System group, which holds nothing they may open")
	}
}

// Why (#1085, #1106): the way back from a refused page must lead to a page the person may open. An operator who
// types the address of an admin page now has no System group at all, so the way back is the dashboard, never the
// group's first page (that was the style guide, and before #1085 the page that refused).
func TestWayBackNeverLeadsToARefusal(t *testing.T) {
	for _, c := range []struct {
		d               ShellData
		nav             string
		label, wantPath string
	}{
		{operator(), "system", "Dashboard", "/"},
		{operator(), "users", "Dashboard", "/"},
		{operator(), "discovery", "Devices", "/devices"},
		{operator(), "policy", "Policy", "/policy"},
		{admin(), "users", "Users", "/users"},
		{admin(), "system", "System", "/settings/system"},
	} {
		c.d.Nav = c.nav
		label, path := c.d.WayBack()
		if label != c.label || path != c.wantPath {
			t.Errorf("%s (admin %v): way back %q %s, want %q %s", c.nav, c.d.IsAdmin, label, path, c.label, c.wantPath)
		}
	}
}

// Why (#1106, A1-004): the top bar, the sub-navigation and the h1 already say where a page is. A trail of one or
// two steps only repeats them (the phone showed "Devices" three times in its first 120 px), so crumbs start at
// three steps. The one exception is a device's page: its Devices crumb is the only way back to the list as the
// person left it, with the filter and sort (#127; the top-bar Devices opens the plain list).
func TestTrailStartsAtThreeSteps(t *testing.T) {
	for _, c := range []struct {
		nav, path, title string
		steps            []Crumb
		want             string
	}{
		{"dashboard", "/", "Fleet overview", nil, ""},
		{"devices", "/devices", "Devices", nil, ""},
		{"policy", "/policy", "Policy", nil, ""},
		{"backups", "/devices/backups", "Device backups", nil, ""},
		{"transactions", "/transactions", "Activity", nil, ""},
		{"about", "/about", "About", nil, ""},
		{"account", "/account", "Account", nil, ""},
		{"", "/nowhere", "Not found", nil, ""},
		{"dashboard", "/whats-new", "What’s new", []Crumb{{Label: "What’s new"}}, ""},
		{"devices", "/devices/bulk", "Bulk action", []Crumb{{Label: "Bulk action"}}, "Devices>/devices|List>/devices|Bulk action"},
		{"discovery", "/devices/new", "Add devices", nil, "Devices>/devices|List>/devices|Add devices"},
		{"styleguide", "/styleguide", "Styleguide", nil, "System|About>/about|Styleguide"},
		{"device", "/devices/7", "kitchen", []Crumb{{Label: "kitchen"}}, "Devices>/devices+|kitchen"},
		{"device", "/devices/7/edit", "Edit kitchen", []Crumb{{Label: "kitchen", Path: "/devices/7"}, {Label: "Edit"}}, "Devices>/devices+|kitchen>/devices/7|Edit"},
	} {
		d := admin()
		d.Nav, d.Path, d.Title, d.Crumbs = c.nav, c.path, c.title, c.steps
		if got := flatTrail(d.Trail()); got != c.want {
			t.Errorf("%s %s: trail %q, want %q", c.nav, c.path, got, c.want)
		}
		if drawn := strings.Contains(render(t, d), `id="crumbs"`); drawn != (c.want != "") {
			t.Errorf("%s %s: the page draws a trail: %v, want %v", c.nav, c.path, drawn, c.want != "")
		}
	}
}

// Why (#1106): Add devices and the style guide left the menu but are still pages in a group, so they light the
// entry they are reached from: Add devices is the Devices list's button, the style guide is linked from About.
// The lit tab and the trail then agree (#1000) without an entry of their own.
func TestPagesWithoutAnEntryLightWhereTheyAreReachedFrom(t *testing.T) {
	for nav, want := range map[string]string{"discovery": "List", "styleguide": "About"} {
		d := admin()
		d.Nav = nav
		got := render(t, d)
		if !regexp.MustCompile(`class="sn-item" href="[^"]*" aria-current="page">\s*` + want + `\s*<`).MatchString(got) {
			t.Errorf("%s does not light %s: %s", nav, want, between(got, `<div class="sn">`, "</div>"))
		}
	}
}

// flatTrail writes a trail as "Label>path|Label"; a crumb that keeps the list query ends its path with "+".
func flatTrail(trail []Crumb) string {
	var parts []string
	for _, cr := range trail {
		p := cr.Label
		if cr.Path != "" {
			p += ">" + cr.Path
		}
		if cr.KeepQuery {
			p += "+"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "|")
}

// Why (#1077, A1-013): the line under the name in the account menu said how the person signed in
// ("Admin · Signed in by the proxy", "Operator · SSO"). That is not what the line is for: it tells
// what the person may do here, and the sign-in words ask a visitor to know what a proxy is. The
// account page is where the sign-in method is explained, so the line names the role and nothing else.
func TestRoleLineNamesTheRoleOnly(t *testing.T) {
	for _, c := range []struct{ method, role, want string }{
		{"local", "admin", "Admin"},
		{"header", "admin", "Admin"},
		{"oidc", "operator", "Operator"},
		{"", "operator", "Operator"},
		{"ldap", "viewer", "Viewer"},
		{"header", "", ""},
	} {
		d := ShellData{UserMethod: c.method, RoleName: c.role}
		if got := d.RoleLine(); got != c.want {
			t.Errorf("method %q role %q: RoleLine = %q, want %q", c.method, c.role, got, c.want)
		}
	}
}
