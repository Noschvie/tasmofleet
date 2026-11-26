package web

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Why (#1097, DS-016, A5-042): the device overview and the rule page drew their tiles with a family of their own
// (.dev-tile*, a word at the size of a number), the header drew a status pill of its own (.devhd-state) and the
// policy page kept a status line family that nothing used (.pol-line*). ui.InfoTile and ui.StatusBadge draw them now;
// the old names must stay gone from the stylesheet and the templates, or a page grows a copy again and the copies
// drift. .pol-line-acts is the one survivor: the live check's buttons still use it.
var oldInfoTileName = regexp.MustCompile(`(?:^|[^\w-])(dev-tile[\w-]*|devhd-state|devhd-dot|pol-line(?:-[a-z]+)?)(?:$|[^\w-])`)

func TestOldInfoTileNamesStayGone(t *testing.T) {
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
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range oldInfoTileName.FindAllStringSubmatch(line, -1) {
				if m[1] == "pol-line-acts" {
					continue
				}
				t.Errorf("%s:%d still names %s: use ui.InfoTile, ui.StatusBadge or the policy summary", f, i+1, m[1])
			}
		}
	}
}

// Why (#1097): the guard above is a regular expression; one that matches nothing would pass for ever.
func TestOldInfoTileNameGuardSeesTheNames(t *testing.T) {
	for _, line := range []string{".dev-tile-v {", `class="dev-tile"`, ".devhd-state {", ".pol-line {", ".pol-line-dot {", ".pol-line .pol-line-btn {"} {
		if !oldInfoTileName.MatchString(line) {
			t.Errorf("the guard does not see %q", line)
		}
	}
	for _, line := range []string{".pol-line-acts {", ".dev-stale {", ".dev-box {", ".dev-relay {", ".itile-v {", ".dv-st {"} {
		if m := oldInfoTileName.FindStringSubmatch(line); m != nil && m[1] != "pol-line-acts" {
			t.Errorf("the guard flags %q (%s)", line, m[1])
		}
	}
}

// Why (#1097, DS-016): a word as a value ("MQTT · HTTP", "Ethernet") is 600 18px (--fs-word) and a number is
// --fs-stat; at the size of a number a word shouted over its neighbours and broke on a phone. The token must exist
// where the rule reads it.
func TestWordValueUsesTheWordSize(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	if !regexp.MustCompile(`(?m)^\s*--fs-word:\s*18px;`).MatchString(css) {
		t.Error("--fs-word: 18px is not defined in :root")
	}
	i := strings.Index(css, ".itile-word {")
	if i < 0 {
		t.Fatal("no .itile-word rule")
	}
	if rule, _, _ := strings.Cut(css[i:], "}"); !strings.Contains(rule, "font-size: var(--fs-word)") {
		t.Errorf(".itile-word does not use --fs-word:\n%s", rule)
	}
}

// Why (#1097): the live refresh swaps #devlive and finds a tile by data-tile, and the rule page's preview swaps
// #rule-tiles; ui.InfoTile must keep both hooks, and set the word (Reachability) apart from the number.
func TestOverviewTilesAreInfoTiles(t *testing.T) {
	e := truthEnv(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `id="devlive"`) || !strings.Contains(body, `<div class="itiles">`) {
		t.Fatal("the live region or its tile row is missing")
	}
	reach := between(body, `data-tile="reach"`, "</div></div>")
	if !regexp.MustCompile(`<div class="itile-v itile-word( t-[a-z]+)?"`).MatchString(reach) {
		t.Errorf("Reachability is a word and must be set as one: %s", reach)
	}
	for _, m := range regexp.MustCompile(`data-tile="(wifi|uptime|power|energy)"><div class="itile-l">[^<]*</div><div class="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[2], "itile-word") {
			t.Errorf("tile %s holds a number but is set as a word: %s", m[1], m[2])
		}
	}
	if strings.Contains(body, "dev-tile") {
		t.Error("the overview still writes the old tile markup")
	}
}

func TestRulePageTilesKeepTheirSwapTarget(t *testing.T) {
	rp := defaultRP()
	e, mem := ruleEnvWith(t, rp)
	id := ruleSeed(t, e, mem, map[string][]string{"name": {"Everyone"}, "family": {}, "tag": {}, "tags_extra": {""}, "except": {}, "want": {"0"}})
	_, body := e.do(t, "GET", "/policy/rules/"+id, "admin", nil)
	if !strings.Contains(body, `<div id="rule-tiles" class="itiles">`) || !strings.Contains(body, `data-tile="deviate"`) {
		t.Errorf("the rule page lost its tile row: %s", between(body, `id="rule-tiles"`, `id="rule-line"`))
	}
	if strings.Contains(body, "dev-tile") {
		t.Error("the rule page still writes the old tile markup")
	}
}

// Why (#1097): while an update runs the header says so, in the accent tone: Updating is not a health state, so it
// must not be drawn as one (no t-ok on a device that is silent on purpose), and it is the same badge as the others.
func TestHeaderBadgeSaysUpdatingWhileAnOperationHoldsTheDevice(t *testing.T) {
	render := func(tabs deviceTabs) string {
		var b bytes.Buffer
		if err := ui.StatusBadge(tabs.stateBadge()).Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	busy := render(deviceTabs{Health: model.HealthOnline, Updating: true})
	if !strings.Contains(busy, `class="bdg dv-st t-accent" data-state="updating"`) || !strings.Contains(busy, ">Updating</span>") {
		t.Errorf("busy header badge: %s", busy)
	}
	idle := render(deviceTabs{Health: model.HealthOffline})
	if !strings.Contains(idle, `class="bdg dv-st t-danger" data-state="offline"`) || !strings.Contains(idle, ">Offline</span>") {
		t.Errorf("idle header badge: %s", idle)
	}
}
