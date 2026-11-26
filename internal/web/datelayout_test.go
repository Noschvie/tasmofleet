package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#1104): a date the server writes with its own Go layout is shown in the server's zone and in the server's
// order ("2 Jan 2006", month names in English) next to the dates time.js writes in the browser's locale and zone, which
// is how one instant came to be written in five ways. The rule of the design system is one place for it:
// ui/stamp.go holds every human layout, and every visible instant is the fallback text of a <time data-ts> element,
// which time.js repaints. These three source tests keep a page from going back to its own Format call:
//
//	(a) a Format call with a human layout appears only in ui/stamp.go (a machine layout, one that carries the date
//	    in the digits of RFC 3339 or of a file name, is fine anywhere). A layout is judged by its value, whether it is
//	    a literal, a constant of the file, or a name from another package (ui.StampDateTime.Layout, time.Kitchen): a name
//	    the test cannot resolve counts as human. The one exception is the list in humanLayoutDebt below;
//	(b) no Go code glues the fallback text of an instant into a sentence (ui.Stamped.Plain is the one way to get
//	    the text of a sentence with an instant for an attribute);
//	(c) in a .templ file the fallback text of an instant is the child of a <time> element, or sits in an attribute
//	    (a title, an aria-label) or inside an <option>, which cannot hold an element.

// datelayoutSources are the non-generated, non-test Go and templ files of internal/web (ui included).
func datelayoutSources(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "static" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		n := d.Name()
		switch {
		case strings.HasSuffix(n, "_templ.go"), strings.HasSuffix(n, "_test.go"):
		case strings.HasSuffix(n, ".go"), strings.HasSuffix(n, ".templ"):
			files = append(files, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

var (
	// formatCall is x.Format("layout") or x.Format(name): a literal, a constant, or a qualified name (pkg.Name,
	// value.Layout, ui.StampDateTime.Layout).
	formatCall = regexp.MustCompile(`\.Format\(\s*("(?:[^"\\]|\\.)*"|[A-Za-z_][\w.]*)\s*\)`)
	// machineLayout is a layout that writes the date in digits for a machine: RFC 3339 and its forms, the day
	// "2006-01-02" of a comparison or a date input, a file name's "20060102T150405Z".
	machineLayout = regexp.MustCompile(`^(2006-01-02|20060102)([ T]?15:?04(:?05)?)?(Z07:00|Z)?$`)
	// machineConstant are the layouts of the standard library that are digits for a machine.
	machineConstant = regexp.MustCompile(`^time\.(RFC3339|RFC3339Nano|DateOnly)$`)
)

// humanLayouts returns the line numbers and layouts of the Format calls in src that write a human date. A name that
// is neither a constant of src nor a machine constant of package time counts as human: a layout that cannot be read
// cannot be shown to be safe.
func humanLayouts(src string) (lines []int, layouts []string) {
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		for _, m := range formatCall.FindAllStringSubmatch(line, -1) {
			layout := m[1]
			if !strings.HasPrefix(layout, `"`) {
				if machineConstant.MatchString(layout) {
					continue
				}
				if cm := regexp.MustCompile(`\b` + regexp.QuoteMeta(layout) + `\s*=\s*("(?:[^"\\]|\\.)*")`).FindStringSubmatch(src); cm != nil {
					layout = cm[1] // a constant of this file: judge its value
				}
			}
			if machineLayout.MatchString(strings.Trim(layout, `"`)) {
				continue
			}
			lines, layouts = append(lines, i+1), append(layouts, layout)
		}
	}
	return lines, layouts
}

// humanLayoutDebt is the list of files that may still write a human layout themselves: the scheduled-update page, whose
// start picker, rule text and "It is now ..." error are painted by its own script (updatestart.js) and not by time.js,
// and which shows a server date inside sentences (updatestart.go, schedview.go plannedAt/startedText, auditsummary.go).
// It is a debt, not a rule: converting that page to <time> elements and the keys of time.js is the follow-up issue
// that the coordinator files for #1104 (number to be added here), and the list only shrinks. An entry that no longer
// has a human layout fails TestHumanLayoutDebtIsStillOwed, so the list cannot outlive the debt.
var humanLayoutDebt = map[string]string{
	"updatestart.go": "scheduler start picker and rule text (follow-up of #1104)",
}

func TestNoHumanDateLayoutOutsideStampGo(t *testing.T) {
	for _, f := range datelayoutSources(t) {
		if _, owed := humanLayoutDebt[f]; f == "ui/stamp.go" || owed {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines, layouts := humanLayouts(string(b))
		for i := range lines {
			t.Errorf("%s:%d: Format(%s) is a human date layout; ui/stamp.go is the one place for those: render the instant with ui.StampAttrs and ui.StampText in a <time> element (#1104)", f, lines[i], layouts[i])
		}
	}
}

// The checks above must see what they are meant to see: a rule that matches nothing passes on every page.
func TestDateLayoutRulesSeeTheirTargets(t *testing.T) {
	_, human := humanLayouts("x := t.Format(\"2 Jan 2006\")\ny := t.Local().Format(\"15:04\")\nz := t.Format(\"Mon, Jan 2\")\nconst l = \"2006-01-02 15:04:05 MST\"\nw := t.Format(l)")
	if len(human) != 4 {
		t.Errorf("human layouts found: %v, want the four of the sample", human)
	}
	// A layout handed over by name is read like a literal: a layout of ui or of package time is a human date, a
	// constant of the file is judged by its value, and only RFC 3339, its nano form and DateOnly are digits.
	q := "a := t.Format(ui.StampDateTime.Layout)\nb := now.Format(ui.StampLayoutPastNow)\nc := t.Format(time.Kitchen)\nd := t.Format(f.Layout)\n" +
		"e := t.Format(time.RFC3339Nano)\nf := t.Format(time.DateOnly)\ng := t.Format(time.RFC3339)"
	if lines, qualified := humanLayouts(q); len(qualified) != 4 || lines[0] != 1 || lines[3] != 4 {
		t.Errorf("qualified layouts found: %v (lines %v), want the four human ones of the sample", qualified, lines)
	}
	_, machine := humanLayouts("a := t.Format(\"2006-01-02T15:04:05Z\")\nb := t.Format(\"2006-01-02\")\nc := t.Format(\"20060102T150405Z\")\nd := t.Format(time.RFC3339)\n// e := t.Format(\"2 Jan\")")
	if len(machine) != 0 {
		t.Errorf("machine layouts, a constant of the standard library and a comment are not human dates, got %v", machine)
	}
	bad := "<span>{ ui.StampText(t, ui.StampDate) }</span>\n<time x>{ ui.StampText(t, ui.StampDate) }</time>\n<a title={ \"x\" + ui.StampText(t, ui.StampTitle) }>x</a>\n<option>{ StampText(t, ui.StampDate) }</option>"
	if got := stampTextOutsideTime(bad); len(got) != 1 || got[0] != 0 {
		t.Errorf("outside a time element, an attribute and an option: lines %v, want [0]", got)
	}
}

// Why (#1104): the list of files with a human layout is a debt that only shrinks; a file that has paid it must leave
// the list, or the list hides the next human layout written there.
func TestHumanLayoutDebtIsStillOwed(t *testing.T) {
	for f, why := range humanLayoutDebt {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s (%s) is on the list but cannot be read: %v", f, why, err)
			continue
		}
		if lines, _ := humanLayouts(string(b)); len(lines) == 0 {
			t.Errorf("%s has no human date layout left: remove it from humanLayoutDebt (%s)", f, why)
		}
	}
}

// stampTextCall is whether e is a call of ui.StampText, ui.StampRelativeText (or their bare names inside package ui).
func stampTextCall(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	name := ""
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		name = f.Sel.Name
	case *ast.Ident:
		name = f.Name
	}
	return name == "StampText" || name == "StampRelativeText" || name == "StampTextIn"
}

func TestNoFallbackTextGluedIntoASentence(t *testing.T) {
	for _, f := range datelayoutSources(t) {
		if !strings.HasSuffix(f, ".go") || f == "ui/stamp.go" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.ADD && (stampTextCall(x.X) || stampTextCall(x.Y)) {
					t.Errorf("%s: the text of an instant is part of a concatenation; put the instant in a ui.Stamped (a <time> element, or Plain for an attribute) (#1104)", fset.Position(x.Pos()))
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") {
					for _, a := range x.Args {
						if stampTextCall(a) {
							t.Errorf("%s: the text of an instant is an argument of %s; put the instant in a ui.Stamped (#1104)", fset.Position(x.Pos()), sel.Sel.Name)
						}
					}
				}
			}
			return true
		})
	}
}

var stampTextInTempl = regexp.MustCompile(`\b(?:ui\.)?(?:StampText|StampRelativeText)\(`)

// stampTextOutsideTime returns the lines of src (a .templ file) where the fallback text of an instant is neither inside
// a <time> element, nor in an attribute, nor inside an <option>.
func stampTextOutsideTime(src string) []int {
	var lines []int
	for _, loc := range stampTextInTempl.FindAllStringIndex(src, -1) {
		before := src[:loc[0]]
		if strings.LastIndex(before, "<time") > strings.LastIndex(before, "</time>") {
			continue // inside the element, its tag or its child
		}
		if strings.LastIndex(before, "<option") > strings.LastIndex(before, "</option>") {
			continue // an option holds text only
		}
		lineStart := strings.LastIndex(before, "\n") + 1
		if attrValue.MatchString(before[lineStart:]) {
			continue // an attribute value: a title, an aria-label
		}
		lines = append(lines, strings.Count(before, "\n"))
	}
	return lines
}

var attrValue = regexp.MustCompile(`=\{[^{}]*$`)

// Why (#1104): see above; (c) reads the .templ source, where a call outside a <time> element is a date nobody repaints.
func TestFallbackTextIsTheChildOfATimeElement(t *testing.T) {
	for _, f := range datelayoutSources(t) {
		if !strings.HasSuffix(f, ".templ") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range stampTextOutsideTime(string(b)) {
			t.Errorf("%s:%d: the fallback text of an instant is outside a <time> element and an attribute; time.js cannot repaint it (#1104)", f, line+1)
		}
	}
}

// Why (#1104): the tooltip of a channel in the Status column is an attribute, which cannot hold a <time> element.
// It said "last contact 2026-11-26 03:12:05 CET (5m)": a zoned instant in the server's zone and layout. It names the
// age in words, like the rest of the column, and nothing else.
func TestChannelTipNamesTheAgeNotAServerDate(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	chs := channelsOf(now, time.Hour, model.Health{SeenMQTT: now.Add(-5 * time.Minute)})
	if got, want := chs[0].Tip, "MQTT: last contact 5m ago"; got != want {
		t.Errorf("tip = %q, want %q", got, want)
	}
}
