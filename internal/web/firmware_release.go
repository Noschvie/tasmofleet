package web

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The release page (design v2 screens "release" and "release-v-since", N6): the full notes of one
// release with the files this fleet needs and the versions still below it, and "Changes since
// <oldest>": every release between the oldest version in the fleet and this one. It reads only
// the cached release feed: a page view never sends a request upstream.

// releasePage is the data of GET /firmware/releases/{version}.
type releasePage struct {
	firmwarePage // Failed, Fetching and Run of the selected release; CanManage, Error, Notice

	Version string
	Loaded  bool         // the feed is in memory (false: the empty state, nothing was fetched)
	Rel     *releaseView // the release; nil when Loaded is false
	Groups  []noteGroup  // its notes, the breaking section(s) pulled into a box
	Older   []versionCount
	Since   bool // ?view=since
	Between []sinceRelease
}

// noteGroup is a run of note blocks; Breaking marks the run of a breaking section.
type noteGroup struct {
	Breaking bool
	Blocks   []firmware.NoteBlock
}

// versionCount is one version still running below the release: "14.2.0 · 17 devices".
type versionCount struct {
	Version string
	Count   int
}

// sinceRelease is one release of "Changes since": its items, each with its section's label.
type sinceRelease struct {
	Version   string
	Published time.Time
	Rows      []sinceRow
	Breaking  int // breaking items
}

type sinceRow struct {
	Label    string // the section heading ("Added", "Breaking"); "" without one
	Breaking bool
	Text     []firmware.Inline
}

// HasOlder reports whether the fleet runs a version older than the release ("Changes since").
func (p releasePage) HasOlder() bool {
	return p.Oldest != "" && compareVersions(p.Oldest, p.Version) < 0
}

// releaseData builds the page; ok is false when the feed is loaded and has no such release.
func (h *handler) releaseData(r *http.Request, version string) (releasePage, bool) {
	p := releasePage{Version: version, Since: r.URL.Query().Get("view") == "since"}
	p.Now, p.Source, p.Feed = h.d.Clock.Now(), h.d.FirmwareSource, h.d.FirmwareFeed
	var rels []firmware.Release
	if cl, ok := h.d.Firmware.(firmware.CachedLister); ok {
		rels = cl.CachedReleases()
	}
	if rels == nil {
		return p, true // not loaded yet: the empty state, never a fetch from a page view
	}
	p.Loaded = true
	rel, ok := firmware.FindRelease(rels, version)
	if !ok {
		return p, false
	}
	arts, devs, used := h.fleetFiles(r, &p.firmwarePage)
	// The aside and Update need this release only, plus the newest for the Latest badge.
	h.fillFirmwarePage(httpBase(r), &p.firmwarePage, []firmware.Release{rel}, arts, devs, used)
	p.Latest = firmware.Latest(rels)
	rv := &p.Releases[0]
	rv.Recommended = rv.Version == p.Latest
	p.Rel = rv
	if p.CanManage {
		h.markPrefetch(&p.firmwarePage, version)
	}
	p.Groups = groupNotes(rv.Notes)
	p.Older = olderVersions(rv.Below, devs)
	p.Oldest = firmware.OldestVersion(devs)
	if p.Since {
		for _, b := range firmware.ReleasesBetween(rels, p.Oldest, version) {
			p.Between = append(p.Between, sinceOf(b, h.d.NoteLinkHosts))
		}
	}
	return p, true
}

// groupNotes splits the notes into runs, the blocks of a breaking section (heading included) in
// runs of their own, so the page draws them in the warning box.
func groupNotes(blocks []firmware.NoteBlock) []noteGroup {
	marks, _ := firmware.BreakingBlocks(blocks)
	var out []noteGroup
	for i, b := range blocks {
		if len(out) == 0 || out[len(out)-1].Breaking != marks[i] {
			out = append(out, noteGroup{Breaking: marks[i]})
		}
		g := &out[len(out)-1]
		if marks[i] && b.Kind == firmware.NoteHeading {
			continue // the box has its own title
		}
		g.Blocks = append(g.Blocks, b)
	}
	return out
}

// olderVersions counts the devices below the release per version, newest first.
func olderVersions(below []deviceRef, devs []model.Device) []versionCount {
	ver := map[model.DeviceID]string{}
	for _, d := range devs {
		ver[d.ID] = d.Identity.Version
	}
	n := map[string]int{}
	for _, d := range below {
		n[ver[d.ID]]++
	}
	out := make([]versionCount, 0, len(n))
	for v, c := range n {
		out = append(out, versionCount{v, c})
	}
	sort.Slice(out, func(i, j int) bool { return compareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

// sinceOf lists a release's notes as labelled rows: every list item and paragraph under its
// section heading, breaking ones flagged.
func sinceOf(rel firmware.Release, hosts []string) sinceRelease {
	blocks := firmware.ParseNotesWith(rel.Notes, hosts)
	marks, _ := firmware.BreakingBlocks(blocks)
	s := sinceRelease{Version: rel.Version, Published: rel.Published}
	label := ""
	for i, b := range blocks {
		switch b.Kind {
		case firmware.NoteHeading:
			label = headingText(b.Inlines)
			if marks[i] {
				label = "Breaking"
			}
		case firmware.NoteBullets, firmware.NoteOrdered:
			for _, it := range b.Items {
				s.Rows = append(s.Rows, sinceRow{Label: label, Breaking: marks[i], Text: it})
			}
		case firmware.NoteParagraph:
			s.Rows = append(s.Rows, sinceRow{Label: label, Breaking: marks[i], Text: b.Inlines})
		}
	}
	for _, r := range s.Rows {
		if r.Breaking {
			s.Breaking++
		}
	}
	return s
}

func headingText(in []firmware.Inline) string {
	var sb strings.Builder
	for _, x := range in {
		sb.WriteString(x.Text)
	}
	return strings.TrimSpace(sb.String())
}

// firmwareRelease is GET /firmware/releases/{version}.
func (h *handler) firmwareRelease(w http.ResponseWriter, r *http.Request) {
	p, ok := h.releaseData(r, r.PathValue("version"))
	if !ok {
		h.renderNotFound(w, r, "Release not found", "This release isn't in the release list",
			"The list holds the releases of the last check. The link may be mistyped, or the release was withdrawn upstream.",
			ui.Btn{Label: "Firmware", Kind: "primary", Href: "/firmware"}, ui.Btn{Label: "Go to dashboard", Href: "/"})
		return
	}
	h.showRelease(w, r, http.StatusOK, p)
}

// showRelease renders the release page; its notice becomes the toast.
func (h *handler) showRelease(w http.ResponseWriter, r *http.Request, status int, p releasePage) {
	v := h.view(r, "Release notes", "firmware", nil)
	v.Flash = p.Notice
	v.Crumbs = []ui.Crumb{{Label: "Updates", Path: "/transactions"}, {Label: "Firmware", Path: "/firmware"}, {Label: "Tasmota " + p.Version}}
	h.show(w, r, status, "release", v, releaseContent(v, p))
}

// releaseFetchAnswer answers a Prefetch or Retry sent from the release page with that page.
func (h *handler) releaseFetchAnswer(w http.ResponseWriter, r *http.Request, status int, rel, file, notice string, err error) {
	p, ok := h.releaseData(r, rel)
	if !ok {
		h.notFound(w, r)
		return
	}
	if err != nil {
		p.failed(rel+"/"+file, err.Error())
		if !p.listsFile(rel, file) {
			p.Error = "Download of " + rel + "/" + file + " failed: " + safeErr(err.Error())
		}
	}
	p.Notice = notice
	h.showRelease(w, r, status, p)
}
