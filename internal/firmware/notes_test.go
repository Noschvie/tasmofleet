package firmware

import (
	"strings"
	"testing"
)

// Why: SafeLink is the single gate between third-party text and an href.
// Anything that is not an absolute http(s) URL with a host must be refused,
// whatever tricks the scheme or whitespace play.
func TestSafeLinkAcceptsOnlyAbsoluteHTTP(t *testing.T) {
	good := []string{"https://github.com/arendst/Tasmota/pull/1", "http://ota.tasmota.com/x", "HTTPS://Example.org/a?b=c#d", "https://tasmota.github.io:443/docs/"}
	for _, u := range good {
		if got, ok := SafeLink(u); !ok || got == "" {
			t.Errorf("SafeLink(%q) refused", u)
		}
	}
	bad := []string{
		"http://192.168.1.50/cm?cmnd=Reset%201", "https://[::1]/", "http://127.0.0.1/", "http://2130706433/", "http://0x7f.1/", "http://localhost/", "http://router/",
		"http://printer.local/", "http://nas.internal/x", "http://box.lan/", "http://x.home/", "https://github.com:8443/x", "http://github.com:81/", "https://github.com:80/",
		"", "javascript:alert(1)", "JaVaScRiPt:alert(1)", " javascript:alert(1)", "java\tscript:alert(1)",
		"data:text/html,<script>alert(1)</script>", "file:///etc/passwd", "vbscript:x", "//evil.example/x", "/relative",
		"relative/path", "https:///nohost", "https://user:pw@host/", "http://host/a b", "https://host/\x00", "https://evil\u200b.example.org/",
		"mailto:a@b.c", "ftp://host/x", "https:evil.example", "https://host\\@evil.example", "https://" + strings.Repeat("a", 3000),
	}
	for _, u := range bad {
		if got, ok := SafeLink(u); ok {
			t.Errorf("SafeLink(%q) = %q, want refusal", u, got)
		}
	}
}

func flat(blocks []NoteBlock) (hrefs []string, text string) {
	var sb strings.Builder
	add := func(in []Inline) {
		for _, i := range in {
			sb.WriteString(i.Text)
			if i.Href != "" {
				hrefs = append(hrefs, i.Href)
			}
		}
		sb.WriteString("|")
	}
	for _, b := range blocks {
		add(b.Inlines)
		for _, it := range b.Items {
			add(it)
		}
		sb.WriteString(b.Code)
	}
	return hrefs, sb.String()
}

// Why: a hostile or merely sloppy release body must not yield a link with a
// dangerous target, in any of the syntaxes we parse. The words stay readable.
func TestParseNotesDropsUnsafeLinks(t *testing.T) {
	src := "[click](javascript:alert(1)) [lan](http://192.168.1.50/cm?cmnd=Reset%201) [other](https://example.org/x) and ![x](data:text/html;base64,AAAA) and [ok](https://github.com/a/b) " +
		"and <a href=\"javascript:alert(1)\">raw</a> and https://github.com/path."
	blocks := ParseNotes(src)
	hrefs, text := flat(blocks)
	want := []string{"https://github.com/a/b", "https://github.com/path"}
	if len(hrefs) != len(want) || hrefs[0] != want[0] || hrefs[1] != want[1] {
		t.Fatalf("hrefs = %v, want %v", hrefs, want)
	}
	for _, s := range []string{"click", "lan", "other", "raw", "<a href"} {
		if !strings.Contains(text, s) {
			t.Errorf("text lost %q: %s", s, text)
		}
	}
}

// Why: raw HTML is not interpreted. Inline HTML is handed on as text so the
// template escapes it; no inline may carry markup semantics. A whole-line raw
// HTML block is dropped (see TestParseNotesDropsHTMLBlocks).
func TestParseNotesKeepsHTMLAsText(t *testing.T) {
	blocks := ParseNotes("# <img src=x onerror=alert(1)>\n\n- <b>x</b>\n\n```\n<script>y</script>\n```")
	_, text := flat(blocks)
	for _, s := range []string{"<img src=x onerror=alert(1)>", "<b>x</b>", "<script>y</script>"} {
		if !strings.Contains(text, s) {
			t.Errorf("expected literal %q in %q", s, text)
		}
	}
	if len(blocks) != 3 || blocks[0].Kind != NoteHeading || blocks[1].Kind != NoteBullets || blocks[2].Kind != NoteCode {
		t.Fatalf("unexpected structure: %+v", blocks)
	}
}

func TestParseNotesStructure(t *testing.T) {
	src := "## Added\r\n- one **bold** `code`\n- two\n  continued\n\n1. first\n2. second\n\nplain\nwrapped\n\n> quoted\n\n---\n"
	b := ParseNotes(src)
	kinds := []string{}
	for _, x := range b {
		kinds = append(kinds, x.Kind)
	}
	if got := strings.Join(kinds, ","); got != "h,ul,ol,p,quote" {
		t.Fatalf("kinds = %s", got)
	}
	if len(b[1].Items) != 2 || !b[1].Items[0][1].Strong || !b[1].Items[0][3].Code {
		t.Errorf("bullet items: %+v", b[1].Items)
	}
	if got := b[1].Items[1]; len(got) != 2 || !strings.Contains(got[1].Text, "continued") {
		t.Errorf("continuation lost: %+v", got)
	}
	if b[3].Inlines[0].Text != "plain wrapped" {
		t.Errorf("paragraph = %q", b[3].Inlines[0].Text)
	}
}

// Why: release bodies are attacker-influenced input of unbounded size and
// nesting; parsing must stay linear and bounded.
func TestParseNotesBoundsInput(t *testing.T) {
	big := strings.Repeat("[a](https://x.example/", 20000) + strings.Repeat("*", 100000) + strings.Repeat("`", 5000)
	if blocks := ParseNotes(big); len(blocks) > maxNoteLines {
		t.Fatalf("too many blocks: %d", len(blocks))
	}
	s, cut := TruncateNotes(strings.Repeat("ä", MaxNotesBytes))
	if !cut || len(s) > MaxNotesBytes || !strings.HasSuffix(s, "ä") {
		t.Fatalf("truncate: cut=%v len=%d", cut, len(s))
	}
	if _, cut := TruncateNotes("short"); cut {
		t.Fatal("short text was cut")
	}
}

func FuzzParseNotes(f *testing.F) {
	for _, s := range []string{"[a](b)", "**", "`", "![", "- x\n  y", "```\n", "> > >", "http://", "[[[]]](((", "#"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hrefs, _ := flat(ParseNotes(s))
		for _, h := range hrefs {
			if got, ok := SafeLink(h); !ok || got != h {
				t.Fatalf("emitted link %q is not safe", h)
			}
		}
	})
}

// Why: a link in third-party text must not lead into the reader's network
// (Tasmota without web password runs GET commands), so only known project
// hosts become links; anything else is shown as text, and the cosmetic
// leftover of a dropped link target must not stay behind.
func TestNoteLinkHostAllowList(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://github.com/arendst/Tasmota": true, "https://docs.github.com/x": true, "https://tasmota.github.io/docs/": true,
		"http://ota.tasmota.com/": true, "https://github.com.evil.example/": false, "https://evilgithub.com/": false,
		"https://example.org/": false, "https://user.github.io/": false,
	} {
		if _, ok := NoteLink(raw, nil); ok != want {
			t.Errorf("NoteLink(%q) = %v, want %v", raw, ok, want)
		}
	}
	if _, ok := NoteLink("https://wiki.example.org/x", []string{"*.example.org"}); !ok {
		t.Error("configured wildcard host refused")
	}
	if _, ok := NoteLink("https://example.org/x", []string{"*.example.org"}); ok {
		t.Error("wildcard matched the bare domain")
	}
	_, text := flat(ParseNotes("see [x](javascript:alert(1)) and (http://192.168.0.1/ ) ok"))
	if text != "see x and (http://192.168.0.1/ ) ok|" {
		t.Errorf("text = %q", text)
	}
	_, text = flat(ParseNotes("[wiki](https://en.wikipedia.org/wiki/Foo_(bar)) end"))
	if text != "wiki end|" {
		t.Errorf("balanced parentheses: %q", text)
	}
}

// Why: GitHub release bodies start with <picture>/<source>/<img> banners. The
// changelog showed their source as text. They are never rendered, so they are
// dropped completely, including multi-line comments, while real text stays.
func TestParseNotesDropsHTMLBlocks(t *testing.T) {
	src := "<picture>\n  <source media=\"(prefers-color-scheme: dark)\" srcset=\"https://example.org/a.png\">\n  <img alt=\"Tasmota\" src=\"https://example.org/b.png\">\n</picture>\n\n" +
		"<!-- hidden\nstill hidden -->\n\n<script>alert(1)</script>\n\n## Added\n- one <b>inline</b>\n\nA sentence with 3 < 4 and a > b."
	blocks := ParseNotes(src)
	_, text := flat(blocks)
	for _, gone := range []string{"picture", "<source", "<img", "hidden", "alert(1)", "srcset"} {
		if strings.Contains(text, gone) {
			t.Errorf("raw HTML block leaked %q: %s", gone, text)
		}
	}
	for _, kept := range []string{"Added", "one", "<b>inline</b>", "3 < 4 and a > b."} {
		if !strings.Contains(text, kept) {
			t.Errorf("lost %q: %s", kept, text)
		}
	}
}
