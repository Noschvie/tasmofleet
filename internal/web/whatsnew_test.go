package web

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/changelog"
	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: news.seen decides the avatar dot. A client may only store a release this binary knows:
// a future or made-up version would hide news after the next upgrade, Unreleased is no release.
func TestNewsSeenPreference(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Prefs = newFakePrefs() })
	latest := changelog.Latest(changelog.Embedded())
	for _, c := range []struct {
		body string
		want int
	}{{`"` + latest + `"`, 204}, {`"Unreleased"`, 400}, {`"99.0.0"`, 400}, {`"x"`, 400}, {`1`, 400}} {
		if resp, _ := e.doJSON(t, "/prefs/news.seen", "operator", c.body); resp.StatusCode != c.want {
			t.Errorf("news.seen %s: status %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
}

// Why: opening What's new clears the dot, but the page itself must still show what was new
// before this visit: the data is built from the old state, then the visit is recorded.
func TestWhatsNewMarksVisit(t *testing.T) {
	rels := changelog.Embedded()
	latest := changelog.Latest(rels)
	var older string
	for _, r := range rels {
		if changelog.Valid(r.Version) && changelog.Compare(r.Version, latest) < 0 {
			older = r.Version
			break
		}
	}
	prefs := newFakePrefs()
	h := &handler{d: Deps{Prefs: prefs}, log: slog.New(slog.DiscardHandler)}
	ctx := auth.WithPrincipal(context.Background(), model.Principal{Subject: "local:op"})
	val, _ := marshalPref(older)
	if err := prefs.SetUserPref(ctx, "local:op", prefNewsSeen, val); err != nil {
		t.Fatal(err)
	}
	before, _ := prefs.UserPrefs(ctx, "local:op")
	if newsCount(before) != 1 {
		t.Fatalf("one release after %s: newsCount = %d, want 1", older, newsCount(before))
	}
	p := whatsNewData(latest, before)
	var marked []string
	for _, r := range p.Releases {
		if r.New {
			marked = append(marked, r.Version)
		}
		if r.Installed != (r.Version == latest) {
			t.Errorf("release %s: Installed = %v", r.Version, r.Installed)
		}
	}
	if p.NewCount != 1 || len(marked) != 1 || marked[0] != latest {
		t.Errorf("new before the visit: count %d, marked %v; want 1, [%s]", p.NewCount, marked, latest)
	}
	h.markNewsSeen(httptest.NewRequest("GET", "/whats-new", nil).WithContext(ctx))
	after, _ := prefs.UserPrefs(ctx, "local:op")
	if seenVersion(after) != latest || newsCount(after) != 0 {
		t.Errorf("after the visit: seen %q, newsCount %d; want %q, 0", seenVersion(after), newsCount(after), latest)
	}
}

// Why: the dot is drawn on every page from h.page; a user who never opened What's new gets
// exactly one hint, not one per release in the history.
func TestNewsCountFirstVisit(t *testing.T) {
	if n := newsCount(nil); n != 1 {
		t.Errorf("no news.seen: newsCount = %d, want 1", n)
	}
	p := whatsNewData("dev", nil)
	n := 0
	for _, r := range p.Releases {
		if r.New {
			n++
		}
	}
	if n != 1 || p.NewCount != 1 {
		t.Errorf("first visit marks %d releases (count %d), want 1", n, p.NewCount)
	}
}

// Why: What's new is for every signed-in user, and opening it is what clears the dot. The page
// must still show the "New" chip of this visit, while the next page view has neither the chip
// nor the dot; Unreleased is never listed (no one can be on it).
func TestWhatsNewPage(t *testing.T) {
	latest := changelog.Latest(changelog.Embedded())
	for _, role := range []string{"operator", "admin"} {
		e := setupWith(t, func(d *Deps) { d.Prefs = newFakePrefs(); d.Version = latest })
		_, before := e.do(t, "GET", "/devices", role, nil)
		if !strings.Contains(before, `class="avatar-dot"`) {
			t.Fatalf("%s: no dot before the first visit", role)
		}
		res, first := e.do(t, "GET", "/whats-new", role, nil)
		if res.StatusCode != 200 {
			t.Fatalf("%s: /whats-new status %d", role, res.StatusCode)
		}
		main := first[strings.Index(first, `<main id="main">`):]
		for _, want := range []string{"What’s new in TasmoFleet", "You’re on " + latest, `id="v` + latest + `"`, "Installed", "New since your last visit"} {
			if !strings.Contains(main, want) {
				t.Errorf("%s: first visit lacks %q", role, want)
			}
		}
		if strings.Contains(main, "Unreleased") {
			t.Errorf("%s: Unreleased is listed", role)
		}
		if strings.Contains(first, `class="avatar-dot"`) {
			t.Errorf("%s: the page that records the visit still draws the dot", role)
		}
		_, second := e.do(t, "GET", "/whats-new", role, nil)
		if strings.Contains(second, "New since your last visit") || strings.Contains(second, `class="avatar-dot"`) {
			t.Errorf("%s: second visit still marks news", role)
		}
		if !strings.Contains(second, "last visit you saw "+latest) {
			t.Errorf("%s: second visit does not name the seen release", role)
		}
	}
}

// Why: changelog text is rendered like third-party release notes: markup in it stays text.
func TestWhatsNewEscapes(t *testing.T) {
	var b strings.Builder
	p := whatsNewView{Installed: "1.0.0", Releases: []whatsNewRelease{{Release: changelog.Parse("## 1.0.0 (2026-01-02)\n\n### Fixed\n\n- <script>alert(1)</script> `<b>`\n")[0]}}}
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "<script>") || strings.Contains(b.String(), "<b>") {
		t.Errorf("markup from the changelog reached the page: %s", b.String())
	}
	if !strings.Contains(b.String(), "2 Jan 2026") {
		t.Error("the date is not written as on the release page")
	}
}

// Why (#510): a path in code has no space to break at; without a break point after each "/" the browser
// cuts the word that happens to meet the line end ("tasmofl / eet") on a phone. The text stays the same.
func TestWhatsNewCodeBreaksAtPathSegments(t *testing.T) {
	var b strings.Builder
	p := whatsNewView{Installed: "1.0.0", Releases: []whatsNewRelease{{Release: changelog.Parse("## 1.0.0 (2026-01-02)\n\n### Fixed\n\n- see `deploy/homeassistant/tasmofleet`\n")[0]}}}
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if want := "<code>deploy/<wbr>homeassistant/<wbr>tasmofleet</code>"; !strings.Contains(b.String(), want) {
		t.Errorf("code lacks break points after the slashes, want %s in %s", want, b.String())
	}
}

// Why: "Installed" must find the running release whether the binary was stamped from the tag
// ("v0.10.1", local builds) or by the image build ("0.10.1").
func TestWhatsNewInstalledWithTagPrefix(t *testing.T) {
	latest := changelog.Latest(changelog.Embedded())
	for _, v := range []string{latest, "v" + latest} {
		n := 0
		for _, r := range whatsNewData(v, nil).Releases {
			if r.Installed {
				n++
				if r.Version != latest {
					t.Errorf("%s: %s marked installed", v, r.Version)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d releases marked installed, want 1", v, n)
		}
	}
}

// Why (#124): the header must show the same string as the About page, from the one Deps.Version.
func TestHeaderVersionIsTheAboutVersion(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Version = "9.9.9+edge (abcdef012345)" })
	for _, path := range []string{"/", "/about"} {
		_, body := e.do(t, "GET", path, "admin", nil)
		if !strings.Contains(body, `class="ver" href="/tf/whats-new"`) || !strings.Contains(body, ">9.9.9+edge (abcdef012345)</a>") {
			t.Errorf("%s: header lacks the version link", path)
		}
	}
}

// outsideFolds is the page's main content without the content of every Details fold: what a person reads
// without opening anything.
func outsideFolds(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<main id="main">`)
	if i < 0 {
		t.Fatal("no <main> in the page")
	}
	main := page[i:]
	if j := strings.Index(main, "</main>"); j >= 0 {
		main = main[:j]
	}
	main = regexp.MustCompile(`(?s)<details class="fold">.*?</details>`).ReplaceAllString(main, "")
	return html.UnescapeString(main) // &#39; is no issue number
}

// developerDetail is what a person who runs TasmoFleet cannot use: a migration, an issue number.
var developerDetail = regexp.MustCompile(`Migration 00|#\d`)

const developerRelease = "## 2.0.0 \"Pharos\" (2026-03-01)\n\n" +
	"### Upgrade notes\n\n- Migration 0054 adds the columns `more_group_topics` and `more_group_topics_gen` to `devices` (#1005)\n\n" +
	"### Changed\n\n- Group topics: the list shows the topic of every device (#998, thanks @Noschvie)\n" +
	"  - sub bullet with `device_column` and #12\n" +
	"\n" +
	"### Fixed\n\n- **Backups no longer stall** (`backup_gen`) when a device is offline (#14)\n"

// Why (#1108): What's new was the changelog file on a page: "Migration 0054 adds the columns ...", issue
// numbers and credits on every line, 74 releases. A person who runs TasmoFleet reads headlines; the developer
// text stays one click away. Nothing a user cannot use (a migration, an issue number) may stand outside the
// closed Details folds, for the changelog this binary ships and for a release written the old way.
func TestWhatsNewShowsNoDeveloperDetailOutsideTheFolds(t *testing.T) {
	latest := changelog.Latest(changelog.Embedded())
	e := setupWith(t, func(d *Deps) { d.Prefs = newFakePrefs(); d.Version = latest })
	_, body := e.do(t, "GET", "/whats-new", "operator", nil)
	if m := developerDetail.FindString(outsideFolds(t, body)); m != "" {
		t.Errorf("the page shows %q outside the Details folds", m)
	}

	var b strings.Builder
	p := whatsNewFrom(changelog.Parse(developerRelease), "2.0.0", nil)
	if err := whatsNewPage(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	out := outsideFolds(t, `<main id="main">`+b.String()+"</main>")
	if m := developerDetail.FindString(out); m != "" {
		t.Errorf("a release written the old way shows %q outside the folds: %s", m, out)
	}
	for _, want := range []string{"Group topics", "Backups no longer stall"} {
		if !strings.Contains(out, want) {
			t.Errorf("the headline %q is missing: %s", want, out)
		}
	}
	for _, want := range []string{"Migration 0054 adds the columns", "<code>more_<wbr>group_<wbr>topics</code>", "thanks @Noschvie", "sub bullet"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("Details lost %q: the full text must stay on the page", want)
		}
	}
}

// Why (#1108): an admin who updates must see first what asks them to act; every other note is detail. A
// Breaking section is a warn banner at the top of its release, with its headline once (the banner replaces the
// list), and plain Upgrade notes are only in Details.
func TestWhatsNewBreakingIsAWarnBannerFirst(t *testing.T) {
	md := "## 3.0.0 (2026-04-01)\n\n### Upgrade notes\n\n- Nothing to set up; the schema stays at 0054\n\n" +
		"### Changed\n\n- **Faster lists** The list renders in one pass\n\n" +
		"### Breaking\n\n- **The old API key setting is gone** set a new key under Settings\n\n" +
		"## 2.0.0 (2026-03-01)\n\n### Upgrade notes\n\n- Nothing to set up\n\n### Fixed\n\n- **Backups no longer stall**\n"
	var b strings.Builder
	if err := whatsNewPage(whatsNewFrom(changelog.Parse(md), "3.0.0", nil)).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	rels := strings.Split(b.String(), `<section class="wn-rel`)
	if len(rels) != 3 {
		t.Fatalf("%d releases rendered", len(rels)-1)
	}
	three, two := rels[1], rels[2]
	banner := strings.Index(three, `class="banner t-warn`)
	if banner < 0 || !strings.Contains(three[banner:], "Action may be required") || !strings.Contains(three[banner:], "The old API key setting is gone") {
		t.Fatalf("the Breaking change is no warn banner: %s", three)
	}
	if first := strings.Index(three, `<div class="wn-sec">`); first < banner {
		t.Errorf("the banner (%d) does not come before the headline lists (%d)", banner, first)
	}
	if n := strings.Count(outsideFolds(t, `<main id="main">`+three+"</main>"), "The old API key setting is gone"); n != 1 {
		t.Errorf("the Breaking headline stands %d times outside Details, want once (in the banner)", n)
	}
	if strings.Contains(outsideFolds(t, `<main id="main">`+three+"</main>"), "Nothing to set up") {
		t.Error("plain Upgrade notes stand outside Details")
	}
	if !strings.Contains(three, "Nothing to set up") {
		t.Error("Details lost the Upgrade notes")
	}
	if strings.Contains(two, `class="banner`) {
		t.Errorf("a release without action notes shows a banner: %s", two)
	}
}

// Why (#1108): the page shows five releases, so a person who skipped more than five has to be told that
// the older ones exist and where they are, instead of finding the news dot gone and the middle missing.
func TestWhatsNewMoreThanFiveNewSaysSo(t *testing.T) {
	var md strings.Builder
	for i := 8; i >= 0; i-- {
		fmt.Fprintf(&md, "## 1.%d.0 (2026-01-%02d)\n\n### Added\n\n- x\n\n", i, i+1)
	}
	rels := changelog.Parse(md.String())
	p := whatsNewFrom(rels, "1.8.0", map[string]string{prefNewsSeen: `"1.1.0"`})
	if len(p.Releases) != 5 {
		t.Fatalf("%d releases on the page, want 5", len(p.Releases))
	}
	if p.NewCount != 7 {
		t.Fatalf("NewCount = %d, want 7 (the count of the dot does not change)", p.NewCount)
	}
	if got, want := whatsNewSub(p), "7 releases are new; the 2 older ones are in the full changelog"; !strings.Contains(got, want) {
		t.Errorf("sub line %q lacks %q", got, want)
	}
	if got := whatsNewSub(whatsNewFrom(rels, "1.8.0", map[string]string{prefNewsSeen: `"1.3.0"`})); strings.Contains(got, "older ones") {
		t.Errorf("five new releases are all on the page, but the sub line says %q", got)
	}
}
