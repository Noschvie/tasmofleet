package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The guard of #1056. The Settings page has one list of tabs, sysLayout, and one place that says how a tab is
// addressed (sysTabQuery, sysTabPath in syscards.go). A link that spells sysPath + "?tab=" + slug by hand keeps
// working until a tab is renamed or a slug is mistyped; then it opens another tab, or none, and no handler test
// notices because each asserts only its own string.

// TestSettingsTabLinksAreBuiltFromTheLayout reads the source of the package: no file but syscards.go may put the
// tab query behind the Settings path. The failure names every line, so a new link is written with sysTabPath.
func TestSettingsTabLinksAreBuiltFromTheLayout(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	templs, _ := filepath.Glob("*.templ")
	files = append(files, templs...)
	seen := 0
	for _, f := range files {
		if f == "syscards.go" || strings.HasSuffix(f, "_templ.go") || strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "?tab=") && (strings.Contains(line, "sysPath") || strings.Contains(line, "/settings/system")) {
				t.Errorf("%s:%d spells a Settings tab link by hand; use sysTabPath or sysTabQuery (syscards.go): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if seen < 50 {
		t.Fatalf("read only %d source files: the guard looks at the wrong directory", seen)
	}
}

// TestNotifyLinksNameASettingsTab is the drift guard for the two links package notify keeps as literals (notify must
// not import web): the tab they name must be one of sysLayout. It passes on a revert of #1056 by design, as notify
// is not changed; it fails when a literal names a slug the page does not have.
func TestNotifyLinksNameASettingsTab(t *testing.T) {
	files, err := filepath.Glob("../notify/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no notify sources found (err %v)", err)
	}
	re := regexp.MustCompile(`/settings/system\?tab=([A-Za-z0-9_-]*)`)
	links := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			links++
			if sysTabSlug(m[1]) == "" {
				t.Errorf("%s links to the Settings tab %q, which sysLayout does not list", f, m[1])
			}
		}
	}
	if links == 0 {
		t.Fatal("found no Settings tab link in package notify: the guard looks at nothing")
	}
}

// TestSysTabPathIsTheTabBarLink: the link of every tab is the one the tab bar shows, and a slug that is no tab never
// becomes a dead link (the Settings page opens its first tab for the plain path).
func TestSysTabPathIsTheTabBarLink(t *testing.T) {
	for _, def := range sysLayout {
		want := sysPath + "?tab=" + def.Slug
		if got := sysTabPath(def.Slug); got != want {
			t.Errorf("sysTabPath(%q) = %q, want %q", def.Slug, got, want)
		}
		if got := sysTabQuery(def.Slug); got != "?tab="+def.Slug {
			t.Errorf("sysTabQuery(%q) = %q", def.Slug, got)
		}
		if got := sysTabCrumb(def.Slug).Path; got != want {
			t.Errorf("sysTabCrumb(%q).Path = %q, want %q", def.Slug, got, want)
		}
	}
	for _, bad := range []string{"", "nope", "MQTT", "mqtt&x=1"} {
		if got := sysTabPath(bad); got != sysPath {
			t.Errorf("sysTabPath(%q) = %q, want the plain %q", bad, got, sysPath)
		}
		if got := sysTabQuery(bad); got != "" {
			t.Errorf("sysTabQuery(%q) = %q, want none", bad, got)
		}
	}
}
