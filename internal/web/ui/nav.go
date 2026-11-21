package ui

import "strings"

// The navigation model of the shell (design v2, design/v2/README.md):
// four groups in the top bar, the group's pages in the sub-navigation, the same lists in the
// phone drawer and on /nav. One model feeds all of them, so they cannot drift apart.

// NavItem is one page of a group.
type NavItem struct {
	Key, Label, Path string
	Current          bool
	Badge            int  // a count that needs attention (new devices on "Add"); 0 = none
	Admin            bool // the page answers 403 to other roles: a crumb of it is plain text for them
}

// NavGroupData is one top-bar group with its pages. Path is where the group button leads: its
// first page the user may open (an operator's System button leads to the style guide).
type NavGroupData struct {
	Key, Label, Path string
	Current          bool
	Items            []NavItem
}

// Crumb is one step of the breadcrumb trail; a crumb without Path is plain text. KeepQuery marks
// a link to a list whose filter and sort listback.js restores from the browser tab (#127).
type Crumb struct {
	Label, Path string
	KeepQuery   bool
}

// NavGroup names the group a page belongs to ("dashboard", "devices", "updates", "system"). The
// key "device" is a page of one device (its tabs and the pages that act on it): it belongs to the
// Devices group and lights no sub-navigation entry, because a device is reached from several
// tabs and hangs directly under the group in the trail. Pages outside the menu (messages, not found, the setup) belong to Dashboard, as the prototype's error
// screens do, so the top bar keeps its sub-navigation row and the title never jumps.
func NavGroup(nav string) string {
	switch nav {
	case "dashboard", "", "setup":
		return "dashboard"
	case "devices", "device", "discovery", "policy", "backups":
		return "devices"
	case "updates", "transactions", "firmware":
		return "updates"
	case "system", "snapshots", "users", "audit", "about", "styleguide", "account":
		return "system"
	}
	return "dashboard"
}

// groupLabels are the names of the groups, spelled once: the top bar, the drawer and the trail read them
// through Groups, and the phone header reads them through AreaLabel.
var groupLabels = map[string]string{"dashboard": "Dashboard", "devices": "Devices", "updates": "Updates", "system": "System"}

// AreaLabel is the name of the area a page belongs to: the label of its top-bar group. The phone header shows it
// and never the page's own title (#1079): the header is one line that cannot break at a word, so a title such as
// "<device name> backups" was cut mid-word and "Users" stood above the h1 "Users". The h1 below carries the name.
// A page outside the menu belongs to Dashboard, as in NavGroup.
func AreaLabel(nav string) string { return groupLabels[NavGroup(nav)] }

// navItemKey is the sub-navigation entry a page lights up.
func navItemKey(nav string) string {
	switch nav {
	case "discovery":
		return "add"
	case "updates":
		return "transactions"
	case "system":
		return "settings"
	}
	return nav
}

// Groups lists the groups and pages of the navigation. The switched-on features decide (a
// missing service has no page, so it has no entry); the role does not: every role sees the same
// navigation and each page enforces its own permission (design v2 F4, plan S7), so the menu
// reads the same for everybody and the style guide is reachable for all.
func (d ShellData) Groups() []NavGroupData {
	type entry struct {
		key, label, path string
		on               bool
		admin            bool // the page answers 403 to other roles
	}
	cur := navItemKey(d.Nav)
	build := func(key, label string, items []entry) NavGroupData {
		g := NavGroupData{Key: key, Label: label, Current: NavGroup(d.Nav) == key}
		for _, e := range items {
			if e.on {
				it := NavItem{Key: e.key, Label: e.label, Path: e.path, Current: e.key == cur && g.Current, Admin: e.admin}
				if e.key == "add" {
					it.Badge = d.NewDevices
				}
				g.Items = append(g.Items, it)
			}
		}
		// The group button leads to the first page this user may open, so it is never a 403.
		for _, e := range items {
			if e.on && (d.IsAdmin || !e.admin) {
				g.Path = e.path
				break
			}
		}
		if g.Path == "" && len(g.Items) > 0 {
			g.Path = g.Items[0].Path
		}
		return g
	}
	out := []NavGroupData{
		build("dashboard", groupLabels["dashboard"], []entry{{"dashboard", "Overview", "/", true, false}}),
		build("devices", groupLabels["devices"], []entry{
			{"devices", "List", "/devices", true, false},
			{"policy", "Policy", "/policy", d.Policy, false},
			{"backups", "Backups", "/devices/backups", d.Backups, false},
			{"add", "Add", "/devices/new", true, true},
		}),
	}
	if g := build("updates", groupLabels["updates"], []entry{
		{"transactions", "Transactions", "/transactions", d.Transactions, false},
		{"firmware", "Firmware", "/firmware", d.Firmware, false},
	}); len(g.Items) > 0 {
		out = append(out, g)
	}
	out = append(out, build("system", groupLabels["system"], []entry{
		{"settings", "Settings", "/settings/system", d.SysConfig, true},
		{"snapshots", "Snapshots", "/settings/system/snapshots", d.Snapshots, true},
		{"audit", "Audit log", "/audit", true, true},
		{"users", "Users", "/users", true, true},
		{"about", "About", "/about", true, true},
		{"styleguide", "Styleguide", "/styleguide", true, false},
	}))
	return out
}

// currentGroup is the group of the page, if the user can see it.
func (d ShellData) currentGroup() (NavGroupData, bool) {
	for _, g := range d.Groups() {
		if g.Current {
			return g, true
		}
	}
	return NavGroupData{}, false
}

// WayBack is where a refused or missing page leads back to, by name and path (below the base path): the
// entry the page lights up when this user may open it and it is more than the group's own first page, else
// the group (its button's target: the first page of it the user may open), else the dashboard. Nothing
// here decides who may open what: it reads the same model as the tabs, so the button never leads to the
// page that refused (#1085).
func (d ShellData) WayBack() (label, path string) {
	g, ok := d.currentGroup()
	if !ok || g.Key == "dashboard" {
		return "Dashboard", "/"
	}
	for _, it := range g.Items {
		if it.Current && (d.IsAdmin || !it.Admin) && it.Path != g.Path {
			return it.Label, it.Path
		}
	}
	return g.Label, g.Path
}

// Trail is the breadcrumb trail of the page, and the only place that builds its start (#1000):
// the group (the top-bar button), then the sub-navigation entry the page lights up, then the
// page's own steps (Crumbs, which therefore holds no group and no entry). The trail and the
// lit entry cannot disagree, because both come from Groups(). A page that lights no entry (a
// device, the account page) goes straight from the group to its own steps. Without own steps
// the trail ends at the entry, or at the title when the page is deeper than the entry. Pages
// outside the groups get Dashboard / title.
func (d ShellData) Trail() []Crumb {
	home := Crumb{Label: "Dashboard", Path: "/"}
	g, ok := d.currentGroup()
	if !ok || len(g.Items) == 0 {
		return append([]Crumb{home}, d.steps()...)
	}
	var item NavItem
	for _, it := range g.Items {
		if it.Current {
			item = it
		}
	}
	onItem := item.Key != "" && strings.TrimSuffix(d.Path, "/") == strings.TrimSuffix(item.Path, "/")
	if g.Key == "dashboard" {
		if onItem && len(d.Crumbs) == 0 {
			return []Crumb{{Label: "Dashboard"}}
		}
		return append([]Crumb{home}, d.steps()...)
	}
	group := Crumb{Label: g.Label}
	if g.Key != "system" { // System has no page of its own
		group.Path = g.Path
	}
	// A device page leads back to the list as the person left it (#127, listback.js).
	group.KeepQuery = d.Nav == "device"
	if item.Key == "" { // a page of the group that is no menu entry (a device, the account page)
		return append([]Crumb{group}, d.steps()...)
	}
	entry := Crumb{Label: item.Label}
	// A page that is the entry's own answer (a POST result titled like it) is the entry: no doubled crumb.
	below := len(d.Crumbs) > 0 || (!onItem && d.Title != item.Label)
	if below && (d.IsAdmin || !item.Admin) { // not a link the person cannot open
		entry.Path = item.Path
	}
	if !below {
		return []Crumb{group, entry}
	}
	return append([]Crumb{group, entry}, d.steps()...)
}

// steps is the page's own steps, or its title when it has none.
func (d ShellData) steps() []Crumb {
	if len(d.Crumbs) > 0 {
		return d.Crumbs
	}
	return []Crumb{{Label: d.Title}}
}

// Initials are the avatar's letters: the first letters of the first two words of the name, or
// the first two letters of a one-word name, upper case.
func Initials(name string) string {
	f := strings.Fields(name)
	var r []rune
	switch {
	case len(f) >= 2:
		r = append([]rune(f[0])[:1], []rune(f[1])[:1]...)
	case len(f) == 1:
		r = []rune(f[0])
		if len(r) > 2 {
			r = r[:2]
		}
	default:
		return "?"
	}
	return strings.ToUpper(string(r))
}

// FirstName is the name on the avatar button: the first word of the display name.
func FirstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

// RoleLine says what the user may do and how they signed in, under their name in the user menu.
func (d ShellData) RoleLine() string {
	role := d.RoleName
	if role != "" {
		role = strings.ToUpper(role[:1]) + role[1:]
	}
	switch d.UserMethod {
	case "local":
		return role + " · Local account"
	case "header":
		return role + " · Signed in by the proxy"
	case "", "oidc":
		return role + " · SSO"
	}
	return role + " · SSO (" + d.UserMethod + ")"
}
