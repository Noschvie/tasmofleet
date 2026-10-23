package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The style guide is readable in the repository (plan D18): docs/spec/design-system.md has a
// section for every /styleguide section and embeds its screenshots from docs/design/styleguide/
// (make styleguide-docs). The doc is the contract a later package or a reader checks against, so a
// section added to the page without its doc entry and screenshots fails here instead of rotting.

var sgViews = []string{"desktop-light", "desktop-dark", "phone-light", "phone-dark"}

var sgShotRe = regexp.MustCompile(`design/styleguide/([a-z0-9-]+?)-(?:desktop|phone)-(?:light|dark)\.png`)

// styleguideDocProblems lists what the doc and the screenshot directory lack for the sections, and
// what they still carry for a section that no longer exists.
func styleguideDocProblems(doc string, shots map[string]bool, sections []sgSection) []string {
	var out []string
	known := map[string]bool{}
	for _, s := range sections {
		known[s.ID] = true
		if !regexp.MustCompile(`(?m)^### ` + regexp.QuoteMeta(s.Title) + `$`).MatchString(doc) {
			out = append(out, "doc has no heading ### "+s.Title+" (section sg-"+s.ID+")")
		}
		for _, v := range sgViews {
			name := s.ID + "-" + v + ".png"
			if !strings.Contains(doc, "](../design/styleguide/"+name+")") {
				out = append(out, "doc does not show "+name)
			}
			if !shots[name] {
				out = append(out, "no screenshot docs/design/styleguide/"+name+" (run make styleguide-docs)")
			}
		}
	}
	for _, m := range sgShotRe.FindAllStringSubmatch(doc, -1) {
		if !known[m[1]] {
			out = append(out, "doc shows a screenshot of section "+m[1]+", which /styleguide no longer has")
		}
	}
	for name := range shots {
		if m := sgShotRe.FindStringSubmatch("design/styleguide/" + name); m == nil || !known[m[1]] {
			out = append(out, "stale screenshot docs/design/styleguide/"+name)
		}
	}
	return out
}

func styleguideShots(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../docs/design/styleguide/*.png")
	if err != nil {
		t.Fatal(err)
	}
	shots := map[string]bool{}
	for _, f := range files {
		shots[filepath.Base(f)] = true
	}
	return shots
}

// Why (D18): see above. Every section of the page is described and pictured in the repository.
func TestStyleguideDocCoversEverySection(t *testing.T) {
	doc := readRepoFile(t, "../../docs/spec/design-system.md")
	for _, p := range styleguideDocProblems(doc, styleguideShots(t), sgSections) {
		t.Error(p)
	}
}

// Why: a check that never fires protects nothing. Removing a section from the doc, deleting one
// of its screenshots, or keeping a screenshot of a removed section must each be reported.
func TestStyleguideDocCheckCatchesGaps(t *testing.T) {
	doc := readRepoFile(t, "../../docs/spec/design-system.md")
	shots := styleguideShots(t)
	if len(shots) == 0 {
		t.Fatal("no screenshots found: the check looks in the wrong place")
	}
	gone := sgSections[1] // Toasts
	i := strings.Index(doc, "### "+gone.Title+"\n")
	j := strings.Index(doc, "### "+sgSections[2].Title+"\n")
	if i < 0 || j < i {
		t.Fatalf("doc has no section %q before %q", gone.Title, sgSections[2].Title)
	}
	if p := strings.Join(styleguideDocProblems(doc[:i]+doc[j:], shots, sgSections), "\n"); !strings.Contains(p, "### "+gone.Title) || !strings.Contains(p, gone.ID+"-phone-dark.png") {
		t.Errorf("a removed doc section is not reported:\n%s", p)
	}
	missing := map[string]bool{}
	for k := range shots {
		missing[k] = k != "banners-phone-dark.png"
	}
	if p := strings.Join(styleguideDocProblems(doc, missing, sgSections), "\n"); !strings.Contains(p, "no screenshot docs/design/styleguide/banners-phone-dark.png") {
		t.Errorf("a missing screenshot is not reported:\n%s", p)
	}
	if p := strings.Join(styleguideDocProblems(doc, shots, sgSections[1:]), "\n"); !strings.Contains(p, "section banners, which /styleguide no longer has") || !strings.Contains(p, "stale screenshot docs/design/styleguide/banners-desktop-light.png") {
		t.Errorf("a removed page section is not reported:\n%s", p)
	}
}

// Why: the doc's screenshots must exist as files the link check and readers can open; an empty or
// truncated PNG (an interrupted run) would show nothing.
func TestStyleguideShotsArePNGs(t *testing.T) {
	for name := range styleguideShots(t) {
		b, err := os.ReadFile(filepath.Join("../../docs/design/styleguide", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 1000 || string(b[1:4]) != "PNG" {
			t.Errorf("%s is not a complete PNG (%d bytes)", name, len(b))
		}
	}
}
