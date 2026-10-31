package tasmotacmd

import (
	"os"
	"strings"
	"testing"
)

// pinnedAnchors reads docs-anchors.txt on its own, so the test does not trust DocsURL's loader.
func pinnedAnchors(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("docs-anchors.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out[l] = true
		}
	}
	if len(out) < 300 {
		t.Fatalf("the pinned anchor list looks truncated: %d entries", len(out))
	}
	return out
}

func TestEveryHelpLinkResolvesToAPinnedDocsAnchor(t *testing.T) {
	// A help link that leads nowhere on the docs page is worse than none: DocsURL may only
	// return an anchor that the page at the pinned commit has (make docs-anchors).
	anchors := pinnedAnchors(t)
	s, err := Pinned()
	if err != nil {
		t.Fatal(err)
	}
	linked := 0
	for _, c := range s.cats {
		for _, e := range c.Commands {
			for _, typed := range []string{e.Command, e.Command + "1", e.Command + "19"} {
				u, ok := DocsURL(typed)
				if !ok {
					continue
				}
				linked++
				frag, found := strings.CutPrefix(u, DocsBase+"#")
				if !found || !anchors[frag] {
					t.Fatalf("%s: %s is not an anchor of the pinned docs page", typed, u)
				}
			}
		}
	}
	if linked < 1000 {
		t.Fatalf("only %d links built: the lookup is too strict to be useful", linked)
	}
}

func TestDocsURLKeepsTheAnchorsExactCase(t *testing.T) {
	// HTML ids are case sensitive: the docs have id="ClimateModeSet", a lower-cased link would not jump.
	u, ok := DocsURL("climatemodeset")
	if !ok || !strings.HasSuffix(u, "#ClimateModeSet") {
		t.Fatalf("%q %v", u, ok)
	}
	if u, ok := DocsURL("SetOption19"); !ok || !strings.HasSuffix(u, "#setoption19") {
		t.Fatalf("%q %v", u, ok)
	}
	// the bare SetOption has no id of its own; the page's SetOptions section is the closest entry
	if u, ok := DocsURL("SetOption"); !ok || !strings.HasSuffix(u, "#setoptions") {
		t.Fatalf("%q %v", u, ok)
	}
	if _, ok := DocsURL("NoSuchCommandAnywhere"); ok {
		t.Fatal("no anchor, no link")
	}
}

func TestDescribedCommandsExistAndHaveADocsLink(t *testing.T) {
	// A description for a command that is not in the catalogue would be a second hand-written
	// table drifting from the first; every one must be a real command (or an indexed form of
	// one) and carry the link that makes the description checkable.
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(descriptions); n < 35 || n > 60 {
		t.Fatalf("%d descriptions; the approved design is about 40", n)
	}
	for key, desc := range descriptions {
		f, ok := c.Find(key)
		if !ok {
			t.Errorf("%s is not a command in Tasmota %s", key, c.Tasmota)
			continue
		}
		if _, ok := DocsURL(key); !ok {
			t.Errorf("%s has no docs anchor", f.Name)
		}
		if desc == "" || len(desc) > 110 || !strings.HasSuffix(desc, ".") {
			t.Errorf("%s: %q is not one short sentence", key, desc)
		}
		if Describe(key) != desc {
			t.Errorf("Describe(%s)", key)
		}
	}
}

func TestDescribeFallsBackToTheCommandForOtherIndexes(t *testing.T) {
	if Describe("SetOption19") == Describe("SetOption") || Describe("SetOption19") == "" {
		t.Fatal("SetOption19 has a description of its own")
	}
	if Describe("Power2") != Describe("Power") || Describe("Power2") == "" {
		t.Fatal("Power2 shares Power's description")
	}
	if Describe("SetOption77") != Describe("SetOption") {
		t.Fatal("an index without its own text uses the command's")
	}
	if Describe("Bh1750Resolution") != "" {
		t.Fatal("no invented description")
	}
}
