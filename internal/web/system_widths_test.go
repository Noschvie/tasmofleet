package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/changelog"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// #419-S: the System pages fill the content width with column stacks (the card-width rule,
// design-system.md "Card widths follow their content"). These tests pin the structure that makes
// them wide: which cards sit in the wide stack and which in the narrow one. e2e/tests/card-widths
// measures the result in a browser.

// stackOf returns the markup of the first stack of the given class ("cstack cstack-w2" or "cstack")
// inside body, up to the start of the next stack or the end of the container.
func stackOf(t *testing.T, body, class string) string {
	t.Helper()
	i := strings.Index(body, `<div class="`+class+`">`)
	if i < 0 {
		t.Fatalf("no stack %q in the page", class)
	}
	s := body[i+1:]
	if j := strings.Index(s, `<div class="cstack`); j >= 0 {
		s = s[:j]
	}
	return s
}

// Why: a user list of five short columns in a card as wide as the page left most of every row
// empty. The list takes the wide stack; the two notes that were a loose paragraph below it (the
// roles and the safeguards) become cards in the narrow stack, and the roles show how many people
// hold each, which the paragraph never said.
func TestUsersPageFillsTheWidthWithTwoStacks(t *testing.T) {
	d := usersFixture() // admin, ops (operator), Sam (operator)
	d.Users = append(d.Users, auth.UserRow{Subject: "local:old", Name: "old", Method: auth.MethodLocal, Role: "admin", Disabled: true, Guards: allowed})
	body := renderUsersPage(t, d)
	if !strings.Contains(body, `class="cstacks cstacks-3"`) {
		t.Fatal("the users page is not in a three-column stack grid")
	}
	wide := stackOf(t, body, "cstack cstack-w2")
	if !strings.Contains(wide, `class="ulist users-list"`) || strings.Contains(wide, "Safeguards") {
		t.Error("the user list belongs alone in the wide stack")
	}
	narrow := stackOf(t, body, "cstack")
	for _, want := range []string{">Roles</h2>", ">Safeguards</h2>", "Nobody can deactivate, delete or demote themselves"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("the narrow stack lacks %q", want)
		}
	}
	// a deactivated admin holds no power: the counts are the people who can act
	if !strings.Contains(narrow, `<span class="roles-n">1 user</span>`) || !strings.Contains(narrow, `<span class="roles-n">2 users</span>`) {
		t.Errorf("roles lack their counts (1 admin, 2 operators): %s", narrow)
	}
	if strings.Contains(body, `class="users-note"`) {
		t.Error("the loose note below the list is back")
	}
}

// Why (#419 decision, "check every timestamp"): "3h ago" is not a time anyone can look up later,
// and a last sign-in can be older than today. Anyone but a session that is active right now shows
// the day it happened, worded like the status bar ("Yesterday 21:10", "19 Aug, 20:02").
func TestUsersLastSeenIsADateNotAnAge(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.Local)
	d := usersFixture()
	d.Users = []auth.UserRow{
		{Subject: "local:admin", Name: "admin", Method: auth.MethodLocal, Role: "admin", LastLogin: now.Add(-3 * time.Hour), Sessions: 0, Guards: allowed},
		{Subject: "local:ops", Name: "ops", Method: auth.MethodLocal, Role: "operator", LastLogin: now.Add(-26 * time.Hour), Guards: allowed},
		{Subject: "local:live", Name: "live", Method: auth.MethodLocal, Role: "operator", LastLogin: now.Add(-time.Minute), Sessions: 1, Guards: allowed},
	}
	rows := usersViewOf(d, now, func(s string) string { return s }).Rows
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.Last
		if strings.HasSuffix(r.Last, "ago") {
			t.Errorf("%s: %q is an age", r.Name, r.Last)
		}
	}
	want := map[string]string{"admin": "Today 05:00", "ops": "Yesterday 06:00", "live": "Active now"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s last seen = %q, want %q", k, got[k], v)
		}
	}
	// the browser repaints the day in its own zone; "Active now" has no day to repaint
	var b strings.Builder
	if err := usersPage(usersViewOf(d, now, func(s string) string { return s })).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), `data-ts="day"`); n != 2 {
		t.Errorf("%d times carry data-ts=day, want 2 (not the active session)", n)
	}
}

// Why: the Version card and the components are the page; the configuration table is the long part.
// Components used to sit under the Version card at full width with the licence line as a loose
// paragraph at the bottom. Version and the licence line now stand in the narrow stack next to the
// components (wide stack), and the configuration keeps the full row below.
func TestAboutPutsVersionAndLicenceNextToComponents(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Database = fakeDBInfo{} })
	_, body := e.do(t, "GET", "/about", "admin", nil)
	narrow := stackOf(t, body, "cstack")
	for _, want := range []string{`about-version`, ">Licence and links</h2>", "AGPL-3.0-or-later", ">Source</a>"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("the narrow stack lacks %q", want)
		}
	}
	if !strings.Contains(stackOf(t, body, "cstack cstack-w2"), `about-comps`) {
		t.Error("Components are not in the wide stack")
	}
	if !strings.Contains(stackOf(t, body, "cstack cstack-all"), `id="card-config"`) {
		t.Error("the configuration lost its full row")
	}
}

// Why: sixty releases in a column 820 px wide left the rest of the page empty. The releases take the
// wide stack and a narrow stack stays beside them: which release you are on and a jump list of every
// release (the page is long; the list is how to get to 0.12 without scrolling).
func TestWhatsNewHasAWideStackAndANarrowStackWithEveryRelease(t *testing.T) {
	rels := changelog.Parse("## 1.2.0 \"Pharos\" (2026-03-01)\n\n### Added\n\n- b\n\n## 1.1.0 (2026-02-01)\n\n### Fixed\n\n- a\n\n## 1.0.0 (2026-01-02)\n\n### Added\n\n- first\n")
	p := whatsNewView{Installed: "1.1.0", Seen: "1.0.0"}
	for _, r := range rels {
		p.Releases = append(p.Releases, whatsNewRelease{Release: r, Installed: r.Version == "1.1.0"})
	}
	var b strings.Builder
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	wide := stackOf(t, body, "cstack cstack-w2")
	for _, v := range []string{"1.2.0", "1.1.0", "1.0.0"} {
		if !strings.Contains(wide, `id="v`+v+`"`) {
			t.Errorf("release %s is not in the wide stack", v)
		}
	}
	narrow := stackOf(t, body, "cstack")
	if !strings.Contains(narrow, ">You are on 1.1.0</h2>") {
		t.Errorf("the narrow stack does not say which release you are on: %s", narrow)
	}
	for _, v := range []string{"1.2.0", "1.1.0", "1.0.0"} {
		if !strings.Contains(narrow, `<a href="#v`+v+`">`+v+`</a>`) {
			t.Errorf("the jump list lacks %s", v)
		}
	}
}

// Why: a binary stamped "dev" is no release of the changelog: the card must not invent a name or a date.
func TestWhatsNewYouAreOnCardOfADevBuild(t *testing.T) {
	var b strings.Builder
	p := whatsNewView{Installed: "dev", Releases: []whatsNewRelease{{Release: changelog.Parse("## 1.0.0 (2026-01-02)\n\n### Added\n\n- x\n")[0]}}}
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ">You are on dev</h2>") || strings.Contains(b.String(), `class="wn-you-sub"`) {
		t.Error("a build that is no release should say so without a release line")
	}
}

// Why: Settings tabs fill the three-column grid (#419). General and Discovery held two one-column
// cards and left the third column empty at 1280 px. Every tab but Advanced (one card that is its own
// full row after the form) must add up to the three columns, or end in a full-width stack.
func TestSettingsTabsFillTheThreeColumns(t *testing.T) {
	for _, tab := range sysLayout {
		if tab.Slug == "advanced" {
			continue
		}
		sum := 0
		for _, s := range tab.Spans {
			if s == ui.SpanAll {
				sum = 3
				break
			}
			sum += s
		}
		if sum != 3 {
			t.Errorf("tab %s: spans %v add up to %d of 3 columns", tab.Slug, tab.Spans, sum)
		}
	}
}

// Why: from 1200 px the narrow stack stays in view while the page scrolls, so it must stay short as the
// changelog grows: the newest 12 releases are links, every older one sits behind "Older releases (n)".
func TestWhatsNewJumpListShowsTheNewestTwelve(t *testing.T) {
	var md strings.Builder
	for i := 13; i >= 1; i-- {
		fmt.Fprintf(&md, "## 1.%d.0 (2026-01-%02d)\n\n### Added\n\n- x\n\n", i, i)
	}
	p := whatsNewView{Installed: "1.13.0"}
	for _, r := range changelog.Parse(md.String()) {
		p.Releases = append(p.Releases, whatsNewRelease{Release: r})
	}
	var b strings.Builder
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	jump := stackOf(t, b.String(), "cstack")
	i := strings.Index(jump, `<details class="wn-older">`)
	if i < 0 {
		t.Fatal("13 releases and no Older releases")
	}
	if n := strings.Count(jump[:i], `<a href="#v`); n != 12 {
		t.Errorf("%d releases before Older releases, want the newest 12", n)
	}
	if !strings.Contains(jump[i:], "Older releases (1)") || !strings.Contains(jump[i:], `<a href="#v1.1.0">`) {
		t.Errorf("the oldest release is not behind Older releases (1): %s", jump[i:])
	}
}
