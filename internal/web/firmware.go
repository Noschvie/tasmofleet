package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

type firmwarePage struct {
	Releases  []releaseView
	Artifacts []artifactView
	Error     string
	Notice    string
	// Index is set when the cache reports its refresh status.
	Index *firmware.IndexStatus
	Now   time.Time

	// Source names where releases come from (the subtitle), Feed its short name (the status line).
	Source, Feed string

	Latest      string // newest stable release in the index
	Updatable   int    // devices that can be updated to Latest: a known older version and hardware a build exists for
	Devices     int
	Known       int    // devices whose firmware version is known
	Oldest      string // the oldest version running in the fleet ("Changes since"; "" = none known)
	Unknown     int    // devices that reported no version yet
	ShowOlder   bool   // ?older=1: the older releases are listed
	ShowNotes   bool   // ?notes=1: the full notes of the selected release in the page (no-JS "Release notes")
	ShowUpload  bool   // ?upload=1: the upload form in the page (no-JS "Upload custom firmware")
	CanManage   bool   // the cache can delete files
	CanRefresh  bool   // the cache can refresh its index ("Check for new releases now")
	CacheSize   int64
	UnusedCount int
	UnusedSize  int64
	CanUpload   bool         // the cache accepts custom images
	Customs     []customView // uploaded images, newest first

	// selected is the index in Releases of the release the detail card shows: ?release=,
	// else the newest one.
	selected int
	// Failed says why a file of the selected release could not be downloaded (by release/file):
	// the answer to its fetch, or a failed item of the running prefetch.
	Failed map[string]string
	// Fetching marks the files the running prefetch of the selected release has not finished.
	Fetching map[string]bool
	Run      string // id of that prefetch run (its page shows the progress)
}

// releasesShown is how many of the newest releases the firmware page lists as cards; the
// rest are behind "Show N older releases".
const releasesShown = 3

// Newest returns the releases listed as cards.
func (p firmwarePage) Newest() []releaseView { return p.Releases[:min(releasesShown, len(p.Releases))] }

// Older returns the releases behind "Show N older releases".
func (p firmwarePage) Older() []releaseView {
	if len(p.Releases) <= releasesShown {
		return nil
	}
	return p.Releases[releasesShown:]
}

// Selected is the release of the detail card; nil without releases.
func (p firmwarePage) Selected() *releaseView {
	if len(p.Releases) == 0 {
		return nil
	}
	return &p.Releases[p.selected]
}

// checkState is the state of the release index for the status line next to "Check for new
// releases now": checking (the first fetch since start has not answered), failed (the last
// check failed), ok, or "" when the cache does not report it.
func (p firmwarePage) checkState() string {
	switch {
	case p.Index == nil:
		return ""
	case p.Index.LastError != "":
		return "failed"
	case !p.Index.Loaded:
		return "checking"
	}
	return "ok"
}

// credURL matches the user information of a URL ("https://user:secret@host"): it is cut from
// error texts before the page shows them.
var credURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)

// safeErr is an upstream error as the page shows it: never with credentials of a source URL.
func safeErr(s string) string { return credURL.ReplaceAllString(s, "$1") }

// shortHash is a SHA-256 as the page lists it: the first and last four characters
// ("9f2c…41ab"); the full value is in the title and the delete dialog.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:4] + "…" + h[len(h)-4:]
}

// excerpt is the part of the release notes the detail card lists: the points of the first list,
// at most excerptItems; without a list the first paragraph. The whole notes are behind
// "Release notes".
func excerpt(blocks []firmware.NoteBlock) [][]firmware.Inline {
	for _, b := range blocks {
		if (b.Kind == "ul" || b.Kind == "ol") && len(b.Items) > 0 {
			return b.Items[:min(excerptItems, len(b.Items))]
		}
	}
	for _, b := range blocks {
		if b.Kind == "p" {
			return [][]firmware.Inline{b.Inlines}
		}
	}
	return nil
}

const excerptItems = 6

// deviceRef is a device as the page names it.
type deviceRef struct {
	Base string // base path, for the link
	ID   model.DeviceID
	Name string
}

// fileView is one file a release offers to the fleet.
type fileView struct {
	Name    string
	Chip    string // chip family of the devices that use it ("ESP32-C3")
	Cached  bool
	Size    int64 // of the cached file; 0 = not cached (the upstream size is unknown until then)
	UsedBy  []deviceRef
	Missing bool // the fleet wants it, the release does not offer it
}

// releaseView is one upstream release with the fleet's view of it.
type releaseView struct {
	Version     string
	Recommended bool
	Published   time.Time // zero when the source does not say
	URL         string
	NumFiles    int
	Needed      int // files the fleet needs (offered ones)
	Cached      int // of those, already in the cache
	Missing     []string
	Below, At   []deviceRef
	Above       []deviceRef
	Unknown     []deviceRef
	// Updatable is how many of Below can actually be updated: unknown hardware and devices
	// excluded from OTA are older but need no file. Known counts the devices whose version
	// is known, the base of every figure about this release.
	Updatable   int
	Known       int
	Skipped     []skippedDevice
	Files       []fileView
	Notes       []firmware.NoteBlock
	NotesCut    bool
	CanPrefetch bool
}

type skippedDevice struct {
	deviceRef
	Reason string
}

// artifactView is a cached file with what the page knows about its use.
type artifactView struct {
	model.Artifact
	InUse   bool
	Jobs    []model.JobID
	Batches []model.BatchID // the pending or running transactions that need the file
	Devices []deviceRef     // devices that would use it for its release ("Installed on")
	// Selectable: "Delete unused" takes it. Needed files and uploads (deleted one
	// by one, they cannot be downloaded again) are not.
	Selectable bool
	// Waiting is how many devices still need the file for an update to its release (the
	// delete dialog says it is downloaded again for them).
	Waiting int
	// Upload is the uploaded image this file is, nil for an upstream file.
	Upload *customView
}

// NeededText is "transaction #3, #5" for the "Needed by" column and the
// tooltip of a disabled checkbox.
func (a artifactView) NeededText() string { return neededText(a.Batches, a.Jobs) }

// neededText names the transactions that need a file; when only the jobs are
// known it names them, when nothing is known it says "a running update".
func neededText(batches []model.BatchID, jobs []model.JobID) string {
	var ids []string
	noun := "transaction"
	switch {
	case len(batches) > 0:
		for _, id := range batches {
			ids = append(ids, "#"+strconv.FormatInt(int64(id), 10))
		}
	case len(jobs) > 0:
		noun = "update job"
		for _, id := range jobs {
			ids = append(ids, strconv.FormatInt(int64(id), 10))
		}
	default:
		return "a running update"
	}
	if len(ids) > 1 {
		noun += "s"
	}
	return noun + " " + strings.Join(ids, ", ")
}

// maxReleasesShown caps the releases the page lists (the newest three as cards, the rest behind
// "Show N older releases").
const maxReleasesShown = 12

func (h *handler) firmwareData(r *http.Request) firmwarePage {
	p := firmwarePage{Now: h.d.Clock.Now(), Source: h.d.FirmwareSource, Feed: h.d.FirmwareFeed}
	if p.Feed == "" {
		p.Feed = "the release sources"
	}
	var rels []firmware.Release
	// While the first background refresh has not succeeded, do not fetch on
	// render: it would block the page behind the slow upstream.
	loaded := true
	if sr, ok := h.d.Firmware.(firmware.StatusReporter); ok {
		st := sr.IndexStatus()
		st.LastError = safeErr(st.LastError)
		p.Index, loaded = &st, st.Loaded
	}
	_, p.CanRefresh = h.d.Firmware.(firmware.Refresher)
	if loaded {
		var err error
		if rels, err = h.d.Firmware.Releases(r.Context()); err != nil {
			p.Error = "The upstream index could not be loaded: " + safeErr(err.Error())
		}
	}
	if len(rels) > maxReleasesShown {
		rels = rels[:maxReleasesShown]
	}
	arts, devs, used := h.fleetFiles(r, &p)
	h.fillFirmwarePage(httpBase(r), &p, rels, arts, devs, used)
	p.Oldest = firmware.OldestVersion(devs)
	for i := range p.Artifacts {
		a := &p.Artifacts[i]
		if a.InUse {
			a.Batches = h.batchesOf(r.Context(), a.Jobs)
		}
		a.Selectable = p.CanManage && !a.InUse && !firmware.IsCustomSource(a.Source)
	}
	if cu, ok := h.customService(); ok {
		p.CanUpload = true
		imgs, err := cu.CustomImages(r.Context())
		if err != nil {
			p.Error = "The uploaded images could not be read."
		}
		for _, img := range imgs {
			_, busy := used[img.Artifact.ID]
			p.Customs = append(p.Customs, customView{CustomImage: img, InUse: busy})
		}
		for i := range p.Artifacts {
			for j := range p.Customs {
				if p.Customs[j].Artifact.ID == p.Artifacts[i].ID {
					p.Artifacts[i].Upload = &p.Customs[j]
				}
			}
		}
	}
	// The release is a query parameter on the page and a form field of its POST actions (a
	// fetch answers with the release it was started from).
	q := r.URL.Query()
	release := q.Get("release")
	if release == "" && r.Method == http.MethodPost {
		release = r.PostFormValue("release")
	}
	for i := range p.Releases {
		if p.Releases[i].Version == release {
			p.selected = i
		}
	}
	p.ShowOlder = q.Get("older") == "1" || p.selected >= releasesShown
	p.ShowNotes = q.Get("notes") == "1"
	p.ShowUpload = q.Get("upload") == "1"
	if sel := p.Selected(); sel != nil && p.CanManage {
		h.markPrefetch(&p, sel.Version)
	}
	return p
}

// fleetFiles reads what the firmware pages join with the releases: the cached files, the fleet
// and the jobs that use cached files. A read error is set on p; CanManage says whether files may
// be deleted (never without the jobs' answer: fail closed).
func (h *handler) fleetFiles(r *http.Request, p *firmwarePage) ([]model.Artifact, []model.Device, map[model.ArtifactID][]model.JobID) {
	var arts []model.Artifact
	if h.d.Artifacts != nil {
		var err error
		if arts, err = h.d.Artifacts.ListArtifacts(r.Context()); err != nil {
			p.Error = "The cache index could not be read."
		}
	}
	var devs []model.Device
	if h.d.Inventory != nil {
		var err error
		if devs, err = h.d.Inventory.List(r.Context()); err != nil {
			p.Error = "The device list could not be read."
		}
	}
	var used map[model.ArtifactID][]model.JobID
	mgr, canManage := h.d.Firmware.(firmware.Manager)
	p.CanManage = canManage
	if canManage {
		var err error
		if used, err = mgr.ArtifactsInUse(r.Context()); err != nil {
			p.CanManage, p.Error = false, "The jobs that use cached files could not be determined."
		}
	}
	return arts, devs, used
}

// markPrefetch marks the files of release the running prefetch has not cached yet, and the
// ones it failed to download, so their rows say so instead of offering Prefetch again.
func (h *handler) markPrefetch(p *firmwarePage, release string) {
	run, ok := h.prefetcher().Active()
	if !ok || run.Release != release {
		return
	}
	p.Run = run.ID
	for _, it := range run.Items {
		key := release + "/" + it.File
		switch it.State {
		case firmware.PrefetchCached:
		case firmware.PrefetchFailed:
			p.failed(key, it.Err)
		default:
			if p.Fetching == nil {
				p.Fetching = map[string]bool{}
			}
			p.Fetching[key] = true
		}
	}
}

// listsFile reports whether the selected release lists file among the files this fleet needs.
func (p firmwarePage) listsFile(release, file string) bool {
	sel := p.Selected()
	if sel == nil || sel.Version != release {
		return false
	}
	for _, f := range sel.Files {
		if f.Name == file {
			return true
		}
	}
	return false
}

// failed records why release/file could not be downloaded.
func (p *firmwarePage) failed(key, why string) {
	if p.Failed == nil {
		p.Failed = map[string]string{}
	}
	p.Failed[key] = safeErr(why)
}

// fillFirmwarePage joins index, cache, fleet and job data. It does no I/O.
func (h *handler) fillFirmwarePage(base string, p *firmwarePage, rels []firmware.Release, arts []model.Artifact, devs []model.Device, used map[model.ArtifactID][]model.JobID) {
	cached := map[string]bool{}
	byRelease := map[string][]string{} // cached file names per release
	for _, a := range arts {
		if a.Custom != "" {
			continue // an upload of the same version is not the upstream file
		}
		cached[a.Release+"/"+a.Filename] = true
		byRelease[a.Release] = append(byRelease[a.Release], a.Filename)
	}
	p.Latest = firmware.Latest(rels)
	p.Devices = len(devs)
	for _, d := range devs {
		if d.Identity.Version == "" {
			p.Unknown++
		}
	}
	p.Known = p.Devices - p.Unknown
	chipOf := map[model.DeviceID]string{}
	for _, d := range devs {
		chipOf[d.ID] = chipLabel(d.Identity.Family)
	}
	sizeOf := map[string]int64{}
	for _, a := range arts {
		if a.Custom == "" {
			sizeOf[a.Release+"/"+a.Filename] = a.Size
		}
	}
	needs := map[string]firmware.ReleaseNeeds{}
	needsFor := func(release string, offered []string) firmware.ReleaseNeeds {
		if n, ok := needs[release]; ok {
			return n
		}
		n := firmware.FleetNeeds(firmware.Release{Version: release, Files: offered}, devs)
		needs[release] = n
		return n
	}
	canWrite := p.CanManage
	for _, rel := range rels {
		n := needsFor(rel.Version, rel.Files)
		v := releaseView{
			Version: rel.Version, Recommended: rel.Version == p.Latest, Published: rel.Published, URL: rel.URL,
			NumFiles: len(rel.Files), Missing: n.Missing, Notes: firmware.ParseNotesWith(rel.Notes, h.d.NoteLinkHosts), NotesCut: rel.NotesTruncated,
		}
		v.Known = p.Known
		for _, d := range n.Devices {
			ref := deviceRef{Base: base, ID: d.DeviceID, Name: d.Name}
			switch d.Position {
			case firmware.PosBelow:
				v.Below = append(v.Below, ref)
				if d.Skip == "" {
					v.Updatable++
				}
			case firmware.PosAt:
				v.At = append(v.At, ref)
			case firmware.PosAbove:
				v.Above = append(v.Above, ref)
			case firmware.PosUnknown:
				v.Unknown = append(v.Unknown, ref)
			}
			if d.Skip != "" {
				v.Skipped = append(v.Skipped, skippedDevice{ref, d.Skip})
			}
		}
		for _, f := range n.Files {
			fv := fileView{Name: f, Cached: cached[rel.Version+"/"+f], Size: sizeOf[rel.Version+"/"+f]}
			for _, d := range n.UsedBy(f) {
				fv.UsedBy = append(fv.UsedBy, deviceRef{Base: base, ID: d.DeviceID, Name: d.Name})
				if fv.Chip == "" {
					fv.Chip = chipOf[d.DeviceID]
				}
			}
			v.Needed++
			if fv.Cached {
				v.Cached++
			}
			v.Files = append(v.Files, fv)
		}
		for _, f := range n.Missing {
			fv := fileView{Name: f, Missing: true}
			for _, d := range n.UsedBy(f) {
				fv.UsedBy = append(fv.UsedBy, deviceRef{Base: base, ID: d.DeviceID, Name: d.Name})
				if fv.Chip == "" {
					fv.Chip = chipOf[d.DeviceID]
				}
			}
			v.Files = append(v.Files, fv)
		}
		v.CanPrefetch = v.Needed > 0
		if v.Version == p.Latest {
			p.Updatable = v.Updatable
		}
		p.Releases = append(p.Releases, v)
	}
	sortCached(arts)
	for _, a := range arts {
		av := artifactView{Artifact: a}
		av.Jobs, av.InUse = used[a.ID]
		offered := byRelease[a.Release]
		for _, rel := range rels {
			if rel.Version == a.Release {
				offered = rel.Files
			}
		}
		for _, d := range needsFor(a.Release, offered).UsedBy(a.Filename) {
			if a.Custom != "" {
				break // the fleet needs of an upstream release say nothing about an upload
			}
			av.Devices = append(av.Devices, deviceRef{Base: base, ID: d.DeviceID, Name: d.Name})
			if d.Position == firmware.PosBelow && d.Skip == "" {
				av.Waiting++
			}
		}
		p.CacheSize += a.Size
		if canWrite && !av.InUse && !firmware.IsCustomSource(a.Source) { // "delete unused" never touches uploads
			p.UnusedCount++
			p.UnusedSize += a.Size
		}
		p.Artifacts = append(p.Artifacts, av)
	}
}

// sortCached orders the cached files as the page lists them: the newest release first, uploads
// after the upstream files, then by file name.
func sortCached(arts []model.Artifact) {
	sort.SliceStable(arts, func(i, j int) bool {
		a, b := arts[i], arts[j]
		if (a.Custom != "") != (b.Custom != "") {
			return a.Custom == ""
		}
		if c := compareVersions(a.Release, b.Release); c != 0 {
			return c > 0
		}
		return a.Filename < b.Filename
	})
}

// batchesOf names the transactions the jobs belong to, sorted and without
// duplicates. A job that cannot be read is left out; the page then says "a
// running update" instead of a number it does not know.
func (h *handler) batchesOf(ctx context.Context, jobs []model.JobID) []model.BatchID {
	if h.d.Jobs == nil {
		return nil
	}
	seen := map[model.BatchID]bool{}
	var out []model.BatchID
	for _, id := range jobs {
		if j, err := h.d.Jobs.GetJob(ctx, id); err == nil && j.BatchID != 0 && !seen[j.BatchID] {
			seen[j.BatchID] = true
			out = append(out, j.BatchID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (h *handler) firmwarePage(w http.ResponseWriter, r *http.Request) {
	h.showFirmware(w, r, http.StatusOK, h.firmwareData(r))
}

// showFirmware renders the firmware page; its notice becomes the toast of the answer.
func (h *handler) showFirmware(w http.ResponseWriter, r *http.Request, status int, p firmwarePage) {
	v := h.view(r, "Firmware", "firmware", nil)
	v.Flash = p.Notice
	// the answer to a POST (a check, a fetch, a delete) is the firmware page, not a page below it
	v.Crumbs = []ui.Crumb{{Label: "Updates", Path: "/transactions"}, {Label: "Firmware"}}
	h.show(w, r, status, "firmware", v, firmwareContent(v, p))
}

// firmwareRefresh forces an upstream index refresh (never done on page render): "Check for new
// releases now". The toast says whether it found a newer release.
func (h *handler) firmwareRefresh(w http.ResponseWriter, r *http.Request) {
	rf, ok := h.d.Firmware.(firmware.Refresher)
	if !ok {
		http.Error(w, "refresh is not supported", http.StatusNotImplemented)
		return
	}
	var before string
	if cl, ok := h.d.Firmware.(firmware.CachedLister); ok {
		before = firmware.Latest(cl.CachedReleases())
	}
	err := rf.Refresh(r.Context())
	p := h.firmwareData(r)
	status := http.StatusOK
	if err != nil {
		p.Error, status = "Check failed: "+safeErr(err.Error()), http.StatusBadGateway
	} else {
		p.Notice = checkNotice(before, p.Latest)
	}
	h.showFirmware(w, r, status, p)
}

// checkNotice is the toast after a successful check: a newer release, or that there is none.
func checkNotice(before, after string) string {
	switch {
	case after == "":
		return "Checked: no release found"
	case before != after:
		return "New release " + after + " found"
	}
	return "No release newer than " + after
}

// firmwareFetch downloads one file into the cache (checked against its image header): the
// Prefetch button of a file of the selected release. A failure is shown on the file's row.
func (h *handler) firmwareFetch(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	rel, file := strings.TrimSpace(r.PostFormValue("release")), strings.TrimSpace(r.PostFormValue("filename"))
	status := http.StatusOK
	if rel == "" || file == "" {
		p := h.firmwareData(r)
		p.Error = "Give a release and a file name."
		h.showFirmware(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	a, err := h.d.Firmware.Ensure(r.Context(), rel, file)
	if err != nil {
		status = http.StatusBadGateway
		if errors.Is(err, firmware.ErrSourceNotAllowed) {
			status = http.StatusForbidden
		}
	}
	if r.PostFormValue("from") == "release" { // Prefetch or Retry on the release page answers with it
		notice := ""
		if err == nil {
			notice = a.Filename + " " + a.Release + " cached"
		}
		h.releaseFetchAnswer(w, r, status, rel, file, notice, err)
		return
	}
	p := h.firmwareData(r)
	if err != nil {
		p.failed(rel+"/"+file, err.Error())
		if !p.listsFile(rel, file) { // otherwise the file's row says it, with the reason as its title
			p.Error = "Download of " + rel + "/" + file + " failed: " + safeErr(err.Error())
		}
	} else {
		p.Notice = a.Filename + " " + a.Release + " cached"
	}
	h.showFirmware(w, r, status, p)
}
