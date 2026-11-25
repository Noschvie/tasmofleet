package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Why (#1095): eight families of CSS drew a filter chip or a tag (label.chip, .ucheck.chip, .dv-chip, .add-chip,
// .pol-pill and its kin, .tag): different heights, radii, weights and an inverted selected look, one copy per page.
// ui.Chip and ui.Tag draw one each now (docs/spec/design-system.md, "Chips and tags"). The old names must stay
// gone from the stylesheet, the templates and the scripts that build pills, or a page grows a copy again and the
// two drift apart. The strip wrappers (.dv-chips, .add-chips, .pol-pills, .chips), the release chip .fw-chip and
// the status bar's .sbar-chip are other things and stay.
var oldChipToken = regexp.MustCompile(`(?:^|[^\w-])(label\.chip|ucheck[ .]chip|dv-chip|dv-n|add-chip|add-chip-n|pol-pill|pol-pill-n|pol-tag|pol-exc|pol-chip|pol-chip-x|tag-except|chip-main)(?:$|[^\w-])`)

// handChip finds a chip or tag written by hand in a template: the class belongs to ui.Chip and ui.Tag.
var handChip = regexp.MustCompile(`class=(?:"|\{ ")(chip|tag)(?:["\s])`)

func oldChipNames(src string) []string {
	var out []string
	for i, line := range strings.Split(src, "\n") {
		for _, m := range oldChipToken.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1]+" (line "+itoa(i+1)+")")
		}
	}
	return out
}

func TestOldChipNamesStayGone(t *testing.T) {
	files := []string{"static/app.css"}
	for _, pat := range []string{"static/*.js", "*.templ", "ui/*.templ"} {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 20 {
		t.Fatalf("only %d files found: the glob does not see the sources", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bad := oldChipNames(string(b)); len(bad) > 0 {
			t.Errorf("%s still names the old chip classes %v: use ui.Chip or ui.Tag (and the classes chip, chip-l, chip-n, chip-x, tag)", f, bad)
		}
		if strings.HasSuffix(f, ".templ") && !strings.HasPrefix(f, "ui/") && handChip.MatchString(string(b)) {
			t.Errorf("%s writes class=\"chip\" or class=\"tag\" by hand: use ui.Chip, ui.Tag or ui.TagOf", f)
		}
	}
}

// Why (#1095): the guard above is a regular expression; one that matches nothing would pass for ever. It must report
// each old name on a line that has it, and leave the neighbours alone (the strip wrappers, the other chips, form
// fields that are called tag or chip).
func TestOldChipNamesGuardSeesThem(t *testing.T) {
	for _, old := range []string{
		`label.chip {`, `.ucheck.chip:has(input)`, `class="ucheck chip"`, `<a class="dv-chip" href="/x">`, `<span class="dv-n">`,
		`.add-chip.is-on {`, `class="add-chip-n"`, `.pol-pill input`, `"pol-pill pol-tag"`, `.pol-pill-n::before`, `label.pol-exc`,
		`.pol-chip-x {`, `className = "pol-chip"`, `.tag-except {`, `".chip-main"`,
	} {
		if len(oldChipNames(old)) == 0 {
			t.Errorf("the guard misses %q", old)
		}
	}
	for _, fine := range []string{
		`.dv-chips {`, `<div class="add-chips">`, `.pol-pills {`, `.pol-chips {`, `.fw-chip {`, `.sbar-chip {`, `.chips {`,
		`<select name="tag" aria-label="Tag">`, `<input type="hidden" name="chip"`, `.chip {`, `.chip-l`, `.chip-n`, `.chip-x`, `.tag {`,
	} {
		if got := oldChipNames(fine); len(got) > 0 {
			t.Errorf("the guard reports %q: %v", fine, got)
		}
	}
	for _, hand := range []string{`<span class="tag">x</span>`, `<a class="tag mono" href="/x">`, `<label class="chip">`, `class={ "chip", x }`} {
		if !handChip.MatchString(hand) {
			t.Errorf("the hand-written check misses %q", hand)
		}
	}
	for _, fine := range []string{`<div class="chips">`, `class="chips scroll"`, `<span class="tags-list">`, `@ui.Tag("x")`} {
		if handChip.MatchString(fine) {
			t.Errorf("the hand-written check reports %q", fine)
		}
	}
}

// betweenElements is the whitespace templ puts between elements it finds on separate lines (a chip is a flex box,
// which draws none of it).
var betweenElements = regexp.MustCompile(`>\s+<`)

// squash is a page without the whitespace between its elements: the chips of ui.Chip, whose parts stand on separate
// lines in the component, read as one run (`>All</span><span class="chip-n">3</span>`).
func squash(s string) string { return betweenElements.ReplaceAllString(s, "><") }

// dvChips is the device list's chip strip, from its opening tag to the separator before the pickers.
func dvChips(body string) string { return squash(between(body, `class="dv-chips"`, `class="dv-vsep"`)) }
