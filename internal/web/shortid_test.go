package web

import (
	"strings"
	"testing"
)

// A full hash in a table cell cannot wrap and once made /policy/rules 668 px
// wide on a phone. It must be shortened, with the full value one hover away.
func TestShortIDsShortensLongTokensAndKeepsTheFullValueInTitle(t *testing.T) {
	const sha = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b"
	got := string(shortIDs("set by " + sha + " today"))
	if strings.Contains(got, sha+" ") || !strings.Contains(got, `title="`+sha+`"`) || !strings.Contains(got, ">9f86d081<") {
		t.Fatalf("hash not shortened with title: %s", got)
	}
}

// Ordinary text, including markup characters, stays text.
func TestShortIDsEscapesAndLeavesShortWordsAlone(t *testing.T) {
	got := string(shortIDs(`restored <b>v2</b> by adam`))
	if got != "restored &lt;b&gt;v2&lt;/b&gt; by adam" {
		t.Fatalf("got %q", got)
	}
}

// A token that merely looks long but sits inside markup characters cannot break out of the title attribute.
func TestShortIDsCannotInjectThroughATitle(t *testing.T) {
	got := string(shortIDs(strings.Repeat("a", 40) + `"><script>`))
	if strings.Contains(got, "<script>") {
		t.Fatalf("unescaped output: %s", got)
	}
}
