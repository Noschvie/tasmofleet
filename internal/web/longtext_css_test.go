package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Why: a device name with no break opportunity once made pages 802 px wide at 375 and pushed a
// dialog's Close button out of its frame. The cure is one shared rule at the end of the
// stylesheet (the e2e test long-name-overflow measures the pages); this keeps that rule and the
// two things that undo it: a device name cut with an ellipsis, and a title that may not shrink.
func TestStylesheetWrapsLongUnbreakableText(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	_, shared, ok := strings.Cut(css, "==== long unbreakable text")
	if !ok {
		t.Fatal("the shared long-text block is gone from app.css")
	}
	for _, sel := range []string{".page-head h1", ".page-head .crumb", ".dlg-head h2", ".dlg-body", ".dev > a"} {
		re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(sel) + `[^{}]*\{[^}]*overflow-wrap:\s*anywhere`)
		if !re.MatchString(shared) {
			t.Errorf("%s has no overflow-wrap: anywhere in the long-text block", sel)
		}
	}
	if regexp.MustCompile(`(?s)\.dev(?: > a)? \{[^}]*text-overflow`).MatchString(css) {
		t.Error("device names in lists (.dev) must wrap, not be cut with an ellipsis")
	}
}
