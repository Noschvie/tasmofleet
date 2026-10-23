package firmware

import (
	"sort"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// "Changes since" (design v2, N6): the release page can list every release
// between the oldest version running in the fleet and the selected one, so an
// admin reads everything a jump will bring before updating the stragglers.
// Everything here works on the cached feed (CachedReleases); nothing fetches.

// OldestVersion returns the oldest known firmware version in devs, or "" when
// no device has reported one.
func OldestVersion(devs []model.Device) string {
	oldest := ""
	for _, d := range devs {
		v := d.Identity.Version
		if v != "" && (oldest == "" || compareVersions(v, oldest) < 0) {
			oldest = v
		}
	}
	return oldest
}

// ReleasesBetween returns the releases of rels newer than from and not newer
// than to, newest first. The release from itself is not included: the fleet
// already runs it. Empty when from is "" or not older than to.
func ReleasesBetween(rels []Release, from, to string) []Release {
	if from == "" || to == "" || compareVersions(from, to) >= 0 {
		return nil
	}
	var out []Release
	for _, r := range rels {
		if compareVersions(r.Version, from) > 0 && compareVersions(r.Version, to) <= 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return compareVersions(out[i].Version, out[j].Version) > 0 })
	return out
}

// BreakingBlocks marks the blocks of a parsed note that belong to a section
// whose heading mentions "breaking" (Tasmota writes "### Breaking Changed"):
// out[i] is true for such a heading and every block after it up to the next
// heading. found reports whether at least one block is marked.
func BreakingBlocks(blocks []NoteBlock) (out []bool, found bool) {
	out = make([]bool, len(blocks))
	in := false
	for i, b := range blocks {
		if b.Kind == NoteHeading {
			in = strings.Contains(strings.ToLower(inlineText(b.Inlines)), "breaking")
		}
		out[i] = in
		found = found || in
	}
	return out, found
}

func inlineText(in []Inline) string {
	var sb strings.Builder
	for _, x := range in {
		sb.WriteString(x.Text)
	}
	return sb.String()
}

// FindRelease returns the release with exactly this version.
func FindRelease(rels []Release, version string) (Release, bool) {
	for _, r := range rels {
		if r.Version == version {
			return r, true
		}
	}
	return Release{}, false
}
