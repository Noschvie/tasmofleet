package ui

import (
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func body(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

func render(t *testing.T, d ShellData) string {
	t.Helper()
	var b strings.Builder
	if err := Shell(d, body(`<h1 id="page">Page</h1>`)).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func admin() ShellData {
	return ShellData{
		Title: "Devices", Nav: "devices", CSRF: "tok", UserName: "ada", UserMethod: "local", RoleName: "admin", IsAdmin: true,
		Transactions: true, Firmware: true, Policy: true, Discovery: true, SysConfig: true, Snapshots: true, Backups: true,
	}
}

// Why: the shell is the only thing every page has in common. The page's content has to land
// inside <main>, after the crumbs and notices, or the layout's main region and the live regions
// break.
func TestShellPutsContentInMain(t *testing.T) {
	d := admin()
	d.Flash = "Saved."
	got := render(t, d)
	main := got[strings.Index(got, `<main id="main">`):strings.Index(got, "</main>")]
	if !strings.Contains(main, `<h1 id="page">Page</h1>`) {
		t.Fatalf("content not inside <main>: %s", main)
	}
	if strings.Index(main, "Saved.") > strings.Index(main, `id="page"`) || strings.Index(main, `aria-label="Breadcrumb"`) > strings.Index(main, "Saved.") {
		t.Fatalf("order must be crumbs, flash, content: %s", main)
	}
	if !strings.HasPrefix(got, "<!doctype html>") {
		t.Fatalf("no doctype (quirks mode): %.40s", got)
	}
	// The drawer and the toasts are fixed to the window: inside the sticky header they would
	// leave the page's stacking order with it.
	header := got[strings.Index(got, `<header class="top">`):strings.Index(got, "</header>")]
	if strings.Contains(header, "<dialog") || strings.Contains(header, `class="toasts"`) {
		t.Fatal("a fixed element sits inside the header")
	}
}

// Why: user-controlled text (a display name from the identity provider, a CSRF token, a
// device-derived flash) must never become markup or break out of an attribute.
func TestShellEscapesEveryText(t *testing.T) {
	d := admin()
	hostile := `"><img src=x onerror=alert(1)>`
	d.Title, d.UserName, d.RoleName, d.Flash, d.Banner, d.CSRF, d.Base = hostile, hostile, hostile, hostile, hostile, hostile, hostile
	d.Crumbs = []Crumb{{Label: hostile, Path: "/x"}, {Label: hostile}}
	got := render(t, d)
	if strings.Contains(got, "<img src=x") || strings.Contains(got, "<img") {
		t.Fatalf("hostile text reached the markup unescaped:\n%s", got)
	}
}

// Why: the CSRF token travels in an htmx default header; it must stay valid JSON whatever the
// token contains, or every htmx request of the page loses its protection.
func TestCSRFHeaderIsJSON(t *testing.T) {
	if got := csrfHeaders("abc"); got != `{"X-CSRF-Token": "abc"}` {
		t.Fatalf("got %s", got)
	}
	if got := csrfHeaders(`a"b\`); got != `{"X-CSRF-Token": "a\"b\\"}` {
		t.Fatalf("quotes not escaped: %s", got)
	}
}

func labels(gs []NavGroupData) string {
	var b strings.Builder
	for _, g := range gs {
		b.WriteString(g.Label + "[")
		for _, it := range g.Items {
			b.WriteString(it.Label + ";")
		}
		b.WriteString("]")
	}
	return b.String()
}

// Why: a missing service has no page, so a menu entry to it is a dead end. The role does not
// change the menu (F4, plan S7): every role sees the same navigation, so the style guide (open to
// all, D17) is reachable, and each page enforces its own permission. The groups and pages are
// the prototype's (design v2 SUB table).
func TestMenuFollowsRoleAndFeatures(t *testing.T) {
	if got, want := labels(admin().Groups()), "Dashboard[Overview;]Devices[List;Policy;Backups;Add;]Updates[Transactions;Firmware;]System[Settings;Snapshots;Audit log;Users;About;Styleguide;]"; got != want {
		t.Errorf("admin menu\n got %s\nwant %s", got, want)
	}
	op := admin()
	op.IsAdmin, op.RoleName = false, "operator"
	if got, want := labels(op.Groups()), labels(admin().Groups()); got != want {
		t.Errorf("operator menu differs from the admin's\n got %s\nwant %s", got, want)
	}
	bare := ShellData{Title: "x", Nav: "dashboard", UserName: "o", UserMethod: "header", RoleName: "operator"}
	if got, want := labels(bare.Groups()), "Dashboard[Overview;]Devices[List;Add;]System[Audit log;Users;About;Styleguide;]"; got != want {
		t.Errorf("deployment without services\n got %s\nwant %s", got, want)
	}
	// Transactions run without a firmware cache (backups, commands): their page stays in the menu, so the
	// result of a bulk backup can be found again (#881).
	tx := bare
	tx.Transactions = true
	if got, want := labels(tx.Groups()), "Dashboard[Overview;]Devices[List;Add;]Updates[Transactions;]System[Audit log;Users;About;Styleguide;]"; got != want {
		t.Errorf("transactions without firmware\n got %s\nwant %s", got, want)
	}
	fw := bare
	fw.Firmware = true
	for _, g := range fw.Groups() {
		if g.Key == "updates" && g.Path != "/firmware" {
			t.Errorf("Updates without transactions leads to %s, want its only page /firmware", g.Path)
		}
	}
	// A group button never leads to a 403: an operator's System and Devices buttons go to the first
	// page the role may open.
	for _, c := range []struct {
		d     ShellData
		group string
		want  string
	}{{admin(), "system", "/settings/system"}, {op, "system", "/styleguide"}, {op, "devices", "/devices"}} {
		for _, g := range c.d.Groups() {
			if g.Key == c.group && g.Path != c.want {
				t.Errorf("%s (admin %v) leads to %s, want %s", c.group, c.d.IsAdmin, g.Path, c.want)
			}
		}
	}
	got := render(t, op)
	for _, link := range []string{"/users", "/audit", "/styleguide", "/devices/new"} {
		if !strings.Contains(got, link) {
			t.Errorf("operator page lacks the menu link %q", link)
		}
	}
}

// Why: the current group and page must be marked in the top bar, the sub-navigation and the
// drawer from the one navigation model, so they cannot disagree.
func TestCurrentPageMarkedEverywhere(t *testing.T) {
	for nav, want := range map[string][2]string{
		"policy": {"Devices", "Policy"}, "discovery": {"Devices", "Add"}, "snapshots": {"System", "Snapshots"}, "firmware": {"Updates", "Firmware"},
		"updates": {"Updates", "Transactions"}, "users": {"System", "Users"}, "system": {"System", "Settings"}, "dashboard": {"Dashboard", "Overview"},
	} {
		d := admin()
		d.Nav = nav
		got := render(t, d)
		if !regexp.MustCompile(`class="tb-group" href="[^"]*" aria-current="true">` + want[0] + `<`).MatchString(got) {
			t.Errorf("%s: top bar does not mark %s", nav, want[0])
		}
		if !regexp.MustCompile(`class="sn-item" href="[^"]*" aria-current="page">` + want[1] + `<`).MatchString(got) {
			t.Errorf("%s: sub-navigation does not mark %s", nav, want[1])
		}
		if !regexp.MustCompile(`class="dr-item" href="[^"]*" aria-current="page">` + want[1] + `<`).MatchString(got) {
			t.Errorf("%s: drawer does not mark %s", nav, want[1])
		}
		if n := strings.Count(got, `aria-current="true"`); n != 1 {
			t.Errorf("%s: %d current groups", nav, n)
		}
	}
}

// Why: the crumbs say where a page sits (#1000): the group, then the sub-navigation entry the page
// lights up (the first entry too, so /transactions reads Updates / Transactions and not Updates), then
// the page's own steps. A device lights no entry and hangs directly under its group; System has no page
// of its own, so it is never a link.
func TestTrailFollowsTheNavigation(t *testing.T) {
	cases := []struct {
		nav, path, title string
		steps            []Crumb
		want             string
	}{
		{"dashboard", "/", "Dashboard", nil, "Dashboard"},
		{"devices", "/devices", "Devices", nil, "Devices>/devices|List"},
		{"backups", "/devices/backups", "Backups", nil, "Devices>/devices|Backups"},
		{"policy", "/policy", "Policy", nil, "Devices>/devices|Policy"},
		{"discovery", "/devices/new", "Add devices", nil, "Devices>/devices|Add"},
		{"snapshots", "/settings/system/snapshots", "Snapshots", nil, "System|Snapshots"},
		{"transactions", "/transactions", "Transactions", nil, "Updates>/transactions|Transactions"},
		{"firmware", "/firmware", "Firmware", nil, "Updates>/transactions|Firmware"},
		{"about", "/about", "About", nil, "System|About"},
		{"users", "/users", "Users", nil, "System|Users"},
		{"account", "/account", "Account", nil, "System|Account"},
		{"", "/nowhere", "Not found", nil, "Dashboard>/|Not found"},
		// the answer to "Run now" (POST /policy/live) is titled like its entry: no doubled crumb
		{"policy", "/policy/live", "Policy", nil, "Devices>/devices|Policy"},
		// a missing device keeps List highlighted and says what is missing
		{"devices", "/devices/9999", "Device not found", nil, "Devices>/devices|List>/devices|Device not found"},
		// a page below an entry: the entry is a link, the page's own steps follow
		{"devices", "/devices/bulk", "Bulk action", []Crumb{{Label: "Bulk action"}}, "Devices>/devices|List>/devices|Bulk action"},
		{"transactions", "/transactions/7/retry", "Retry", []Crumb{{Label: "TX-0007", Path: "/transactions/7"}, {Label: "Retry"}},
			"Updates>/transactions|Transactions>/transactions|TX-0007>/transactions/7|Retry"},
		{"system", "/settings/system/reset", "Reset", []Crumb{{Label: "Advanced", Path: "/settings/system?tab=advanced"}, {Label: "Reset"}},
			"System|Settings>/settings/system|Advanced>/settings/system?tab=advanced|Reset"},
		// a device lights no entry: Devices / the device, whatever tab or subpage it is
		{"device", "/devices/7", "kitchen", []Crumb{{Label: "kitchen"}}, "Devices>/devices|kitchen"},
		{"device", "/devices/7/edit", "Edit kitchen", []Crumb{{Label: "kitchen", Path: "/devices/7"}, {Label: "Edit"}}, "Devices>/devices|kitchen>/devices/7|Edit"},
		{"dashboard", "/whats-new", "What’s new", []Crumb{{Label: "What’s new"}}, "Dashboard>/|What’s new"},
	}
	flat := func(trail []Crumb) string {
		var parts []string
		for _, cr := range trail {
			p := cr.Label
			if cr.Path != "" {
				p += ">" + cr.Path
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, "|")
	}
	for _, c := range cases {
		d := admin()
		d.Nav, d.Path, d.Title, d.Crumbs = c.nav, c.path, c.title, c.steps
		if got := flat(d.Trail()); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.nav, c.path, got, c.want)
		}
	}
	// a page outside the menu (not found, setup) keeps the sub-navigation row: the title must
	// not jump up by its height
	for _, nav := range []string{"", "setup"} {
		d := admin()
		d.Nav = nav
		if got := render(t, d); !strings.Contains(got, `<div class="sn"><nav class="shell-in sn-in" aria-label="Dashboard">`) {
			t.Errorf("nav %q: no sub-navigation row", nav)
		}
	}
}

// Why (#1000, rule 4): the trail and the lit sub-navigation entry must agree, for every entry the
// navigation has. Both come out of Groups(), so a page below an entry names exactly the entry the
// menu lights, in the group the top bar lights; renaming an entry cannot leave a stale trail.
func TestTrailNamesTheLitEntry(t *testing.T) {
	for _, nav := range []string{"transactions", "firmware", "devices", "policy", "backups", "discovery", "snapshots", "audit", "users", "about", "system"} {
		d := admin()
		d.Nav, d.Path, d.Title, d.Crumbs = nav, "/somewhere/deeper", "Deeper", []Crumb{{Label: "Deeper"}}
		g, ok := d.currentGroup()
		if !ok {
			t.Fatalf("%s: no current group", nav)
		}
		var lit NavItem
		for _, it := range g.Items {
			if it.Current {
				lit = it
			}
		}
		if lit.Key == "" {
			t.Fatalf("%s lights no entry", nav)
		}
		trail := d.Trail()
		if len(trail) != 3 || trail[0].Label != g.Label || trail[1].Label != lit.Label {
			t.Errorf("%s: trail %v, want %s / %s / Deeper", nav, trail, g.Label, lit.Label)
		}
	}
}

// Why (#1000): a crumb that is a link the person cannot open ends on a 403. An operator sees the
// entry of an admin-only page as plain text (the menu still lists it), an admin gets the link.
func TestTrailLinksOnlyWhatThePersonMayOpen(t *testing.T) {
	d := admin()
	d.Nav, d.Path, d.Title, d.Crumbs = "about", "/whats-new", "What’s new", []Crumb{{Label: "What’s new"}}
	if got := d.Trail(); got[1].Path != "/about" {
		t.Errorf("admin: About is not linked: %v", got)
	}
	d.IsAdmin = false
	if got := d.Trail(); got[1].Label != "About" || got[1].Path != "" {
		t.Errorf("operator: About must be plain text: %v", got)
	}
	d.Nav, d.Path, d.Title, d.Crumbs = "policy", "/policy/history", "History", []Crumb{{Label: "History"}}
	if got := d.Trail(); got[1].Path != "/policy" {
		t.Errorf("operator: Policy is open to every role and must stay a link: %v", got)
	}
}

// Why (#127): the Devices crumb of a device page leads back to the list as the person left it
// (listback.js reads the marker); no other page carries it.
func TestOnlyDevicePagesKeepTheListQuery(t *testing.T) {
	d := admin()
	d.Nav, d.Path, d.Crumbs = "device", "/devices/7", []Crumb{{Label: "kitchen"}}
	if tr := d.Trail(); !tr[0].KeepQuery || tr[0].Path != "/devices" {
		t.Errorf("device page: %v", tr)
	}
	d.Nav, d.Crumbs = "devices", []Crumb{{Label: "Bulk action"}}
	if tr := d.Trail(); tr[0].KeepQuery {
		t.Errorf("a page that is no device page carries the marker: %v", tr)
	}
}

// Why: the SSE connection is opened by the body attributes; a page without live data (forms)
// must not hold a stream open per tab, and without a stream the status bar cannot claim "Live" or
// show the connection banners.
func TestLiveStreamOnlyWhenAsked(t *testing.T) {
	d := admin()
	if got := render(t, d); strings.Contains(got, "sse-connect") || strings.Contains(got, "sb-live") || strings.Contains(got, "conn-banner") {
		t.Fatal("stream, indicator or banners on a page that did not ask")
	}
	d.Live, d.Base = true, "/fleet"
	got := render(t, d)
	if !strings.Contains(got, `hx-ext="sse" sse-connect="/fleet/events?since=0"`) {
		t.Fatalf("live page without the stream: %s", got)
	}
	for _, want := range []string{"<b>Reconnecting…</b>", "Connection to the server lost.", "Retry now", ">Reconnecting…<", ">Offline<"} {
		if !strings.Contains(got, want) {
			t.Errorf("live page lacks %q", want)
		}
	}
}

// Why (#1089): a banner that floats over the page covers the crumbs and the title exactly when the
// person needs to read them, so the connection banners sit in the page flow of <main>, under the crumbs,
// like the shell's other banners. In the sticky header they would also keep their space for the whole
// scroll.
func TestConnBannersSitInTheFlowOfMain(t *testing.T) {
	d := admin()
	d.Live = true
	got := render(t, d)
	header := got[strings.Index(got, `<header class="top">`):strings.Index(got, "</header>")]
	if strings.Contains(header, "conn-banners") {
		t.Fatal("the connection banners sit in the sticky header, where they float over the page")
	}
	main := got[strings.Index(got, `<main id="main">`):strings.Index(got, "</main>")]
	at := strings.Index(main, `class="conn-banners"`)
	if at < 0 {
		t.Fatalf("the connection banners are not in <main>: %s", main)
	}
	if strings.Index(main, `aria-label="Breadcrumb"`) > at || at > strings.Index(main, `id="page"`) {
		t.Fatalf("order must be crumbs, connection banners, content: %s", main)
	}
}

// Why (#1089, WCAG 4.1.3 and 2.2.2): the Reconnecting banner is a status region and the Offline banner an
// alert; a number that changes every second inside either one makes a screen reader read the page to
// pieces. The regions hold words that change with the state only: no attempt counter at all, and the
// Offline countdown is aria-hidden (the sighted countdown stays, a static sentence replaces it for the
// region).
func TestConnBannersAnnounceStatesNotCountdowns(t *testing.T) {
	d := admin()
	d.Live = true
	got := render(t, d)
	from := strings.Index(got, `conn-re" role="status"`)
	mid := strings.Index(got, `conn-off" role="alert"`)
	if from < 0 || mid < from {
		t.Fatalf("the two banners are not there: %s", got)
	}
	status := got[from:mid]
	alert := got[mid : mid+strings.Index(got[mid:], "Retry now")]
	for _, ticking := range []string{"data-live-attempt", "data-live-next"} {
		if strings.Contains(status, ticking) {
			t.Errorf("the status banner holds %s, which changes while it is announced", ticking)
		}
	}
	if strings.Count(alert, "data-live-next") != 1 {
		t.Fatalf("the alert banner must hold exactly one countdown: %s", alert)
	}
	hidden := strings.Index(alert, `aria-hidden="true"`)
	if hidden < 0 || hidden > strings.Index(alert, "data-live-next") {
		t.Errorf("the alert banner's countdown is announced: %s", alert)
	}
	if !strings.Contains(alert, "Retrying automatically.") {
		t.Errorf("the alert banner has no static sentence for the countdown: %s", alert)
	}
}

// Why: below a path prefix every link, form and asset must carry it, or the app is half dead
// behind a reverse proxy.
func TestBasePathPrefixesEveryURL(t *testing.T) {
	d := admin()
	d.Base = "/fleet"
	d.Asset = func(n string) string { return n + "?v=1" }
	got := render(t, d)
	for _, u := range regexp.MustCompile(`(?:href|src|action)="/[^"]*"`).FindAllString(got, -1) {
		if !strings.Contains(u, `="/fleet`) {
			t.Errorf("URL without the base path: %s", u)
		}
	}
	if !strings.Contains(got, `href="/fleet/static/app.css?v=1"`) {
		t.Errorf("hashed asset URL missing: %s", got)
	}
}

// Why: the setup banner points at the setup page; on that page it would link to itself.
func TestBannerHiddenOnSetupPage(t *testing.T) {
	d := admin()
	d.Banner = "No device networks are configured."
	if !strings.Contains(render(t, d), "Open the setup") {
		t.Fatal("banner missing")
	}
	d.Nav = "setup"
	if strings.Contains(render(t, d), "Open the setup") {
		t.Fatal("banner on the setup page itself")
	}
}

// Why: how the user signed in decides what the account menu promises: only a local account has a
// password this app can change, everybody has an account page (who they are, how they sign
// in); everybody can sign out and pick a theme.
func TestAccountMenuByMethod(t *testing.T) {
	for _, method := range []string{"local", "oidc", "header"} {
		d := admin()
		d.UserMethod = method
		got := render(t, d)
		if (method == "local") != strings.Contains(got, "Account &amp; password") {
			t.Errorf("%s: password page offered = %v", method, method != "local")
		}
		if strings.Count(got, `href="/account"`) != 2 { // user menu and drawer
			t.Errorf("%s: the account page is not in both menus", method)
		}
		if strings.Count(got, ">Sign out</button>") != 2 { // user menu and drawer
			t.Errorf("%s: Sign out not in both menus", method)
		}
		if strings.Count(got, `aria-label="Theme"`) != 2 {
			t.Errorf("%s: theme switch not in both menus", method)
		}
	}
}

// Why: D11: the stored choice is rendered on <html> so the first paint has the right theme (no
// flash); without a choice the page follows the system, and the control says which is active.
func TestThemeChoiceRendered(t *testing.T) {
	d := admin()
	got := render(t, d)
	if !strings.Contains(got, `<html lang="en" data-theme-auto>`) || !strings.Contains(got, `data-theme-choice="auto" aria-pressed="true"`) {
		t.Fatalf("no choice must follow the system: %.300s", got)
	}
	d.Theme = "dark"
	got = render(t, d)
	if !strings.Contains(got, `<html lang="en" data-theme="dark">`) || !strings.Contains(got, `data-theme-choice="dark" aria-pressed="true"`) || strings.Contains(got, `data-theme-choice="auto" aria-pressed="true"`) {
		t.Fatalf("dark choice not rendered: %.300s", got)
	}
}

// Why: D15/D16: the product is "TasmoFleet" with its mark wherever the shell names it (tab
// title, top bar, drawer); the lower-case spelling is for identifiers only.
func TestShellNamesTheProduct(t *testing.T) {
	got := render(t, admin())
	if !strings.Contains(got, "<title>Devices - TasmoFleet</title>") {
		t.Error("page title does not end in TasmoFleet")
	}
	if strings.Count(got, `class="brand-mark"`) != 2 || strings.Count(got, "TasmoFleet") < 3 {
		t.Error("the mark and wordmark are not in the top bar and the drawer")
	}
}

// Why: the avatar shows two letters of the display name whatever it looks like (a one-word
// header user, a full name from the identity provider, nothing at all).
func TestInitials(t *testing.T) {
	for in, want := range map[string]string{"Maren Koch": "MK", "maren": "MA", "a": "A", "": "?", "Ägir van der Berg": "ÄV"} {
		if got := Initials(in); got != want {
			t.Errorf("Initials(%q) = %q, want %q", in, got, want)
		}
	}
}

// Why: discovered devices wait until an admin adds them; the count on "Add devices" (sub-nav
// and drawer) is how they notice without opening the page, and no count means no pill.
func TestAddDevicesBadge(t *testing.T) {
	d := admin()
	if strings.Contains(render(t, d), `class="nav-badge"`) {
		t.Fatal("a badge without waiting devices")
	}
	d.NewDevices = 5
	if got := render(t, d); strings.Count(got, `<span class="nav-badge"><span class="sr-only">, new: </span>5</span>`) != 2 {
		t.Fatalf("the count is not on Add devices in both menus: %s", got)
	}
}

// Why: the avatar's dot is the only hint that What's new has unseen releases (N5). It must be
// drawn when there is news and never otherwise, or the hint either never shows or never goes
// away; the user menu and the phone drawer always lead to the page.
func TestNewsDotOnlyWithNews(t *testing.T) {
	d := admin()
	quiet := render(t, d)
	if strings.Contains(quiet, "avatar-dot") || strings.Contains(quiet, "new since last visit") {
		t.Error("News 0 draws the dot or the count")
	}
	if strings.Count(quiet, `href="/whats-new"`) < 2 {
		t.Error("user menu and drawer should link What's new also without news")
	}
	d.News = 2
	loud := render(t, d)
	if strings.Count(loud, `class="avatar-dot"`) != 2 { // top bar and phone header
		t.Errorf("News 2: want the dot on both avatars, got %d", strings.Count(loud, `class="avatar-dot"`))
	}
	if !strings.Contains(loud, "2 new since last visit") || !strings.Contains(loud, ">2 new</span>") {
		t.Error("News 2: user menu or drawer does not say how many releases are new")
	}
}

// Why (#124): a bug report needs the running version at a glance, on every page and on a phone
// too (where the top bar is hidden and only the drawer shows the brand). It opens What's new, the
// changelog of this binary, in the same tab; a shell without a version draws no empty link. The
// status bar carries it on every page (#307, it left the top bar); the drawer keeps its own.
func TestShellShowsTheVersionAsChangelogLink(t *testing.T) {
	d := admin()
	d.Base, d.Version = "/tf", "0.15.2+edge (0123456789ab)"
	got := render(t, d)
	if n := strings.Count(got, `<a class="sb-i sb-ver" href="/tf/whats-new"`); n != 1 {
		t.Fatalf("version links in the status bar = %d, want 1", n)
	}
	if n := strings.Count(got, `<a class="ver" href="/tf/whats-new"`); n != 1 || strings.Contains(between(got, `<header class="top">`, `</header>`), `class="ver"`) {
		t.Fatalf("version links in the drawer = %d, want 1 (and none in the top bar)", n)
	}
	if !strings.Contains(got, ">0.15.2+edge (0123456789ab)</a>") || strings.Contains(got, `href="/tf/whats-new" target`) {
		t.Error("the link must carry the version and stay in the same tab")
	}
	d.Version = ""
	if strings.Contains(render(t, d), `class="ver"`) || strings.Contains(render(t, d), `sb-ver`) {
		t.Error("no version, no link")
	}
}

func between(s, from, to string) string {
	_, after, _ := strings.Cut(s, from)
	before, _, _ := strings.Cut(after, to)
	return before
}

// Why (#1091, #1089): live.js and the shell-v2 spec find the connection banners by their data-live-* spans, their
// classes and roles, and a state change must not reword them. They are ui.Banner now, and the one markup that
// matters for the page is unchanged: every span, the aria-hidden countdown, the sr-only sentence, the Reconnecting
// dot that pulses, the one action Retry now with the attribute live.js listens on. The only difference to the markup
// before the conversion is aria-hidden on the decorative dot (a dot is no content a screen reader should meet).
func TestConnBannersKeepTheirMarkupThroughBanner(t *testing.T) {
	want := `<div class="conn-banners">` +
		`<div class="banner tone-warn conn-banner conn-re" role="status"><span class="banner-dot pulse" aria-hidden="true"></span>` +
		`<div class="banner-text"><b>Reconnecting…</b> Live updates are paused; values may be out of date.</div></div>` +
		`<div class="banner tone-danger conn-banner conn-off" role="alert"><span class="banner-dot" aria-hidden="true"></span>` +
		`<div class="banner-text"><b>Connection to the server lost.</b> Live updates are paused; values shown are from <time data-live-since>the last page load</time>. ` +
		`<span aria-hidden="true">Retrying in <span data-live-next data-visual-mask>8</span> s.</span><span class="sr-only">Retrying automatically.</span></div>` +
		`<button type="button" class="banner-btn" data-live-retry>Retry now</button></div></div>`
	var b strings.Builder
	if err := connBanners().Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if b.String() != want {
		t.Errorf("the connection banners changed:\n got %s\nwant %s", b.String(), want)
	}
}

// Why (#1091): the flash and the setup notice are banners like every other, so they carry the same dot, the same text
// block and the same roles; the flash keeps data-flash, which toast.js turns into a toast and the shell-css spec looks
// for, and the setup notice keeps its one link in the text.
func TestShellFlashAndSetupNoticeAreBanners(t *testing.T) {
	d := admin()
	d.Flash, d.Banner, d.Nav = "Saved.", "No device networks.", "devices"
	got := render(t, d)
	for _, want := range []string{
		`<div class="banner tone-ok" role="status" data-flash><span class="banner-dot" aria-hidden="true"></span><div class="banner-text">Saved.</div></div>`,
		`<div class="banner tone-warn" role="status"><span class="banner-dot" aria-hidden="true"></span><div class="banner-text">No device networks. <a href="/setup">Open the setup</a></div></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the shell lacks %q", want)
		}
	}
}
