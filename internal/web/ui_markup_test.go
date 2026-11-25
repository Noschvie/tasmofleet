package web

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Why (#1092, #1091, #1097): the design system says a page never writes a component's markup, it calls ui.Button,
// ui.Badge, ui.Banner and ui.Card (docs/spec/design-system.md, "Components only through ui"). A page that copies the
// markup drifts: the next change to the component misses it, and the hostile-string check of ui/components_test.go
// does not cover the copy. This test counts the component classes that the .templ files of internal/web write by
// hand and compares them with the exemption blocks below.
//
// The blocks are the debt, one block per issue or page group, each file with its exact count per class. They only
// shrink: a file that writes more than its entry is a new copy (use the component), a file that writes less, or
// nothing, is a stale entry (lower or delete it). That is what makes the list a ratchet and not a wish.
// internal/web/ui itself is the one place that writes the markup, so it is not scanned.

// uiMarkupClasses are the component classes the test watches, by the component that owns them.
var uiMarkupClasses = map[string]string{
	"btn":    "ui.Button",
	"bdg":    "ui.Badge or ui.BadgeOf",
	"banner": "ui.Banner",
	"ucard":  "ui.Card",
}

// uiMarkupToken matches a whole class token that starts a string: a quote, the name, then a quote or a space. That
// finds class="btn btn-sm", class={ "bdg", ... } and { "x", "ucard" }, and not the layout wrappers btn-row and
// btn-why or banner-dot, which can never become the component.
var uiMarkupToken = regexp.MustCompile(`"(btn|bdg|banner|ucard)["\s]`)

type markupBlock struct {
	Name  string
	Files map[string]map[string]int // file -> class -> exact count
}

// markupExemptions: the hand-written component markup that still stands. Delete an entry the moment its file
// converts; the owning issue deletes its whole block.
var markupExemptions = []markupBlock{
	{Name: "#1092 buttons: updates", Files: map[string]map[string]int{
		"transactions.templ": {"btn": 16},
	}},
	{Name: "#1092 buttons: firmware", Files: map[string]map[string]int{
		"firmware.templ":        {"btn": 8},
		"firmware_manage.templ": {"btn": 6},
		"release.templ":         {"btn": 2},
	}},
	{Name: "#1092 buttons: accounts", Files: map[string]map[string]int{
		"accountpages.templ": {"btn": 8},
		"messagepage.templ":  {"btn": 2},
		"setup.templ":        {"btn": 1},
		"userdetail.templ":   {"btn": 6},
	}},
	{Name: "#1092 buttons: system", Files: map[string]map[string]int{
		"audit.templ":       {"btn": 3},
		"syssettings.templ": {"btn": 27},
	}},
	{Name: "#1092 buttons: add devices", Files: map[string]map[string]int{
		"adddevices.templ": {"btn": 42},
	}},
	{Name: "#1092 buttons: style guide", Files: map[string]map[string]int{
		"styleguide.templ": {"btn": 3},
	}},
	{Name: "#1097 status badge", Files: map[string]map[string]int{
		"cells.templ": {"bdg": 2},
	}},
}

// markupCounts is the number of hand-written component classes per class in one template.
func markupCounts(src string) map[string]int {
	n := map[string]int{}
	for _, m := range uiMarkupToken.FindAllStringSubmatch(src, -1) {
		n[m[1]]++
	}
	return n
}

// markupFindings compares the templates (file name -> source) with the blocks and returns what is wrong, sorted.
func markupFindings(files map[string]string, blocks []markupBlock) []string {
	exempt := map[string]map[string]int{}
	owner := map[string]map[string]string{}
	var out []string
	for _, b := range blocks {
		for f, classes := range b.Files {
			for c, n := range classes {
				if _, ok := uiMarkupClasses[c]; !ok {
					out = append(out, fmt.Sprintf("block %q lists %s with the unknown class %q", b.Name, f, c))
					continue
				}
				if exempt[f] == nil {
					exempt[f], owner[f] = map[string]int{}, map[string]string{}
				}
				if prev, dup := owner[f][c]; dup {
					out = append(out, fmt.Sprintf("%s lists %q in two blocks (%q and %q): one block owns it", f, c, prev, b.Name))
				}
				exempt[f][c] += n
				owner[f][c] = b.Name
			}
		}
	}
	seen := map[string]bool{}
	for f, src := range files {
		got := markupCounts(src)
		for c := range uiMarkupClasses {
			seen[f+"/"+c] = true
			switch want := exempt[f][c]; {
			case got[c] > want:
				out = append(out, fmt.Sprintf("%s writes class=%q %d times by hand, %d are exempt: use %s (a new copy is not an exemption)", f, c, got[c], want, uiMarkupClasses[c]))
			case got[c] < want:
				out = append(out, fmt.Sprintf("stale exemption: block %q lists %s with %d x %q, the file has %d: lower the entry or delete it", owner[f][c], f, want, c, got[c]))
			}
		}
	}
	for f, classes := range exempt {
		for c := range classes {
			if !seen[f+"/"+c] {
				out = append(out, fmt.Sprintf("stale exemption: block %q lists %s, which is no template of internal/web", owner[f][c], f))
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestPagesRenderComponentsThroughUI(t *testing.T) {
	paths, err := filepath.Glob("*.templ")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	files := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[p] = string(raw)
	}
	for _, f := range markupFindings(files, markupExemptions) {
		t.Error(f)
	}
}

// Why: a test of the ratchet that cannot fail is worse than none. A planted component class in an unlisted file, a
// stale entry and a lowered count each have to be found, and the layout wrappers whose names start like a component
// must not be.
func TestMarkupTestFindsPlantedCopiesAndStaleEntries(t *testing.T) {
	blocks := []markupBlock{{Name: "#1092 buttons: demo", Files: map[string]map[string]int{"a.templ": {"btn": 2}, "gone.templ": {"bdg": 1}}}}
	clean := map[string]string{
		"a.templ": `<a class="btn btn-sm">x</a><button class={ "x", "btn" }>y</button><div class="btn-row"><span class="btn-why banner-dot"></span></div>`,
		"b.templ": `<p>the banner and the btn are words here</p><div class="btn-row"></div>`,
	}
	got := markupFindings(clean, blocks)
	if len(got) != 1 || !strings.Contains(got[0], "gone.templ") || !strings.Contains(got[0], "no template") {
		t.Fatalf("only the entry of a vanished file is wrong here, got %q", got)
	}
	delete(blocks[0].Files, "gone.templ")
	if got := markupFindings(clean, blocks); len(got) != 0 {
		t.Fatalf("a clean tree must pass, got %q", got)
	}

	planted := map[string]string{"a.templ": clean["a.templ"], "b.templ": `<button class="btn btn-primary">x</button>`}
	if got := markupFindings(planted, blocks); len(got) != 1 || !strings.Contains(got[0], "b.templ writes class=\"btn\"") {
		t.Errorf("a planted btn in an unlisted file must be found, got %q", got)
	}
	for class, src := range map[string]string{
		"bdg":    `<span class="bdg t-ok">x</span>`,
		"banner": `<div class="banner tone-warn">x</div>`,
		"ucard":  `<section class={ "r2", "ucard" }>x</section>`,
	} {
		if got := markupFindings(map[string]string{"c.templ": src}, nil); len(got) != 1 || !strings.Contains(got[0], `class="`+class+`"`) {
			t.Errorf("a planted %s must be found, got %q", class, got)
		}
	}
	more := map[string]string{"a.templ": clean["a.templ"] + `<a class="btn">third</a>`}
	if got := markupFindings(more, blocks); len(got) != 1 || !strings.Contains(got[0], "3 times") {
		t.Errorf("a third btn in a file that lists two must be found, got %q", got)
	}
	fewer := map[string]string{"a.templ": `<a class="btn">only one</a>`}
	if got := markupFindings(fewer, blocks); len(got) != 1 || !strings.Contains(got[0], "stale exemption") {
		t.Errorf("a file that has converted one of its two must fail as stale until the entry is lowered, got %q", got)
	}
	twice := []markupBlock{blocks[0], {Name: "#1092 cards", Files: map[string]map[string]int{"a.templ": {"btn": 1}}}}
	if got := markupFindings(clean, twice); len(got) == 0 {
		t.Error("a class listed in two blocks must be found: no block would own the entry")
	}
}
