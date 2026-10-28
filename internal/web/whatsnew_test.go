package web

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/changelog"
	"github.com/nofuturekid/tasmofleet/internal/model"
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
