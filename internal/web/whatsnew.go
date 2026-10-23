package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/changelog"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// prefNewsSeen is the user preference holding the newest release the user has seen on What's new
// (design v2, N5): a JSON string such as "0.10.1". No row means the user never opened the page.
const prefNewsSeen = "news.seen"

// whatsNewView is the data of GET /whats-new: the changelog of this binary, newest first, with
// the installed release and the releases that are new since the user's last visit marked.
type whatsNewView struct {
	Installed string // this binary's version (may be "dev")
	Seen      string // the release stored at the last visit, "" = never
	NewCount  int    // releases newer than Seen ("N new since your last visit")
	Releases  []whatsNewRelease
}

type whatsNewRelease struct {
	changelog.Release
	Installed bool // the running version
	New       bool // newer than the user's last visit
}

// seenVersion reads the stored news.seen preference ("" when absent or invalid).
func seenVersion(prefs map[string]string) string {
	var v string
	if json.Unmarshal([]byte(prefs[prefNewsSeen]), &v) != nil {
		return ""
	}
	return v
}

// newsCount is the number behind the avatar's news dot and the user menu's "N new".
func newsCount(prefs map[string]string) int {
	return changelog.NewSince(changelog.Embedded(), seenVersion(prefs))
}

// whatsNewData builds the page data from the state before this visit (prefs as read for the
// request), so the page can still mark what was new although opening it records the visit.
func whatsNewData(installed string, prefs map[string]string) whatsNewView {
	rels := changelog.Embedded()
	seen := seenVersion(prefs)
	p := whatsNewView{Installed: installed, Seen: seen, NewCount: changelog.NewSince(rels, seen)}
	latest := changelog.Latest(rels)
	for _, r := range rels {
		if !changelog.Valid(r.Version) {
			continue // Unreleased is not a release a user can be on or catch up with
		}
		isNew := changelog.Compare(r.Version, seen) > 0
		if !changelog.Valid(seen) {
			isNew = r.Version == latest // matches NewSince's single hint for a first visit
		}
		// a local build says "v0.10.1"; the image is stamped "0.10.1"
		inst := r.Version == strings.TrimPrefix(installed, "v")
		p.Releases = append(p.Releases, whatsNewRelease{Release: r, Installed: inst, New: isNew})
	}
	return p
}

// markNewsSeen records that the user has seen every release in this binary's changelog; the
// What's new page calls it after reading the old state. A failure only keeps the dot.
func (h *handler) markNewsSeen(r *http.Request) {
	latest := changelog.Latest(changelog.Embedded())
	p, ok := auth.FromContext(r.Context())
	if h.d.Prefs == nil || !ok || p.Subject == "" || latest == "" {
		return
	}
	val, _ := marshalPref(latest)
	if err := h.d.Prefs.SetUserPref(r.Context(), p.Subject, prefNewsSeen, val); err != nil {
		h.log.WarnContext(r.Context(), "record what's new visit", "err", err)
	}
}

// validateNewsSeen accepts news.seen through POST /prefs/news.seen ("Mark all as read" without
// opening the page): only a released version of this binary's changelog, so a client cannot
// store an arbitrary string or a future version that would hide later news.
func validateNewsSeen(raw []byte) (string, error) {
	var v string
	if err := decodeStrict(raw, &v); err != nil {
		return "", err
	}
	rels := changelog.Embedded()
	if v == changelog.Unreleased || !changelog.Known(rels, v) {
		return "", errPref
	}
	return marshalPref(v)
}

// whatsNew is GET /whats-new for every signed-in user: the page is built from the state before
// this visit, then the visit is recorded, so the page still marks what was new while the avatar
// dot (drawn after the mark) is gone.
func (h *handler) whatsNew(w http.ResponseWriter, r *http.Request) {
	p := whatsNewData(h.d.Version, h.userPrefs(r))
	h.markNewsSeen(r)
	v := h.view(r, "What’s new", "about", nil)
	v.Crumbs = []ui.Crumb{{Label: "System"}, {Label: "About"}, {Label: "What’s new"}}
	if v.IsAdmin { // About is an admin page; the others see the step without a link
		v.Crumbs[1].Path = "/about"
	}
	h.show(w, r, http.StatusOK, "whatsnew", v, whatsNewPage(p))
}

// sectionTone is the colour of a changelog section's label: fixes green, additions accent,
// security and breaking changes warn, the rest neutral.
func sectionTone(title string) ui.Tone {
	switch title {
	case "Fixed":
		return ui.ToneOK
	case "Added":
		return ui.ToneAccent
	case "Security", "Breaking":
		return ui.ToneWarn
	}
	return ui.ToneNeutral
}

// isoDay turns a changelog date (YYYY-MM-DD) into "8 Oct 2026"; anything else stays as written.
func isoDay(s string) string {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return s
	}
	return dayText(t)
}
