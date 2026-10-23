package firmware

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func devOn(v string) model.Device { return model.Device{Identity: model.Identity{Version: v}} }

// Why: "Changes since" starts at the oldest version the fleet still runs. It is
// compared as a version, not as text ("9.5.0" is older than "13.4.0"), and a
// device that never reported a version does not pull it to "".
func TestOldestVersion(t *testing.T) {
	if got := OldestVersion([]model.Device{devOn("14.3.0"), devOn(""), devOn("9.5.0"), devOn("13.4.0")}); got != "9.5.0" {
		t.Errorf("oldest = %q, want 9.5.0", got)
	}
	if got := OldestVersion([]model.Device{devOn("")}); got != "" {
		t.Errorf("no versions = %q", got)
	}
}

func versionList(rels []Release) string {
	var v []string
	for _, r := range rels {
		v = append(v, r.Version)
	}
	return strings.Join(v, ",")
}

// Why: the list must show exactly what a jump from the oldest running version
// to the selected one brings: not the version the fleet already runs, the
// selected one included, nothing newer, newest first whatever the feed order.
func TestReleasesBetween(t *testing.T) {
	rels := []Release{{Version: "13.4.0"}, {Version: "15.0.0"}, {Version: "14.2.0"}, {Version: "14.3.0"}, {Version: "13.10.0"}, {Version: "12.0.0"}}
	cases := []struct{ from, to, want string }{
		{"13.4.0", "14.3.0", "14.3.0,14.2.0,13.10.0"},
		{"13.9.0", "14.2.0", "14.2.0,13.10.0"}, // from need not be in the feed
		{"14.3.0", "14.3.0", ""},               // the fleet is on it: nothing changes
		{"15.0.0", "14.3.0", ""},               // a downgrade is not "changes since"
		{"", "14.3.0", ""},                     // no version known
	}
	for _, c := range cases {
		if got := versionList(ReleasesBetween(rels, c.from, c.to)); got != c.want {
			t.Errorf("between %q and %q = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// Why: breaking changes are what an admin must not miss in a long list; every
// block of the section under a "Breaking" heading is flagged, and the next
// heading ends the section.
func TestBreakingBlocks(t *testing.T) {
	blocks := ParseNotes("## v14.3.0\n### Added\n- a\n### Breaking Changed\n- old API removed\n\nmore text\n### Fixed\n- b\n")
	marks, found := BreakingBlocks(blocks)
	if !found {
		t.Fatal("breaking section not found")
	}
	var flagged []string
	for i, b := range blocks {
		if marks[i] {
			if b.Kind == NoteHeading {
				flagged = append(flagged, "h:"+inlineText(b.Inlines))
			} else if len(b.Items) > 0 {
				flagged = append(flagged, "li:"+inlineText(b.Items[0]))
			} else {
				flagged = append(flagged, b.Kind+":"+inlineText(b.Inlines))
			}
		}
	}
	if got := strings.Join(flagged, "|"); got != "h:Breaking Changed|li:old API removed|p:more text" {
		t.Errorf("flagged = %q", got)
	}
	if _, found := BreakingBlocks(ParseNotes("### Added\n- breaking news in a bullet is no heading\n")); found {
		t.Error("only a heading opens a breaking section")
	}
}

// Why: the release page is addressed by the version in its URL; an unknown
// version is a 404, never the nearest release.
func TestFindRelease(t *testing.T) {
	rels := []Release{{Version: "14.3.0"}, {Version: "14.2.0"}}
	if r, ok := FindRelease(rels, "14.2.0"); !ok || r.Version != "14.2.0" {
		t.Errorf("found %v %v", r, ok)
	}
	if _, ok := FindRelease(rels, "14.2"); ok {
		t.Error("a prefix must not match")
	}
}
