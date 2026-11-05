package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Why (#538): a section that loads from the device by itself (hx-trigger="load") looks empty or
// stale until the answer comes. Each one must say aria-busy="true" so the shared busy pattern
// (screen readers, the "still working" note) covers it; a new loading section without it fails here.
func TestLoadPlaceholdersAreBusy(t *testing.T) {
	files, err := filepath.Glob("*.templ")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templ files: %v", err)
	}
	tag := regexp.MustCompile(`<[a-z]+[^>]*hx-trigger="load"[^>]*>`)
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tag.FindAllString(string(b), -1) {
			n++
			if !strings.Contains(m, `aria-busy="true"`) {
				t.Errorf("%s: loading placeholder without aria-busy: %.120s", f, m)
			}
		}
	}
	if n == 0 {
		t.Fatal("no hx-trigger=load placeholder found; the guard checks nothing")
	}
}

// Why (#538): the shared script marks every plain POST form busy until the next page arrives. A
// form whose answer is a file download never gets a next page, so its button would stay busy for
// ever: the snapshot download form opts out.
func TestDownloadFormOptsOutOfBusy(t *testing.T) {
	b, err := os.ReadFile("syssettings.templ")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`<form id="db-dl"[^>]*data-no-busy`).Match(b) {
		t.Fatal(`form#db-dl (POST answered with a file) needs data-no-busy`)
	}
}
