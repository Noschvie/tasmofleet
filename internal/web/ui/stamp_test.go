package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Why: "Today"/"Yesterday" is a calendar-day decision. The server text is only the fallback
// without JavaScript, but it must agree with time.js on the boundaries: just after midnight
// yesterday evening is still "Yesterday", the minute before midnight two days ago is a date, and
// a time later today or tomorrow is never called "Today" by accident.
func TestStampRelativeText(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 5, 0, 0, time.UTC) // just after midnight
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	for _, c := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"a minute ago, same day", at(2026, 10, 22, 0, 4), "Today 00:04"},
		{"first minute of today", at(2026, 10, 22, 0, 0), "Today 00:00"},
		{"last minute of yesterday", at(2026, 10, 21, 23, 59), "Yesterday 23:59"},
		{"first minute of yesterday", at(2026, 10, 21, 0, 0), "Yesterday 00:00"},
		{"last minute of the day before", at(2026, 10, 20, 23, 59), "20 Oct, 23:59"},
		{"this year", at(2026, 8, 19, 20, 2), "19 Aug, 20:02"},
		{"an earlier year", at(2025, 8, 19, 20, 2), "19 Aug 2025, 20:02"},
		{"later today", at(2026, 10, 22, 23, 0), "Today 23:00"},
		{"tomorrow is a date, not Today", at(2026, 10, 23, 0, 30), "23 Oct, 00:30"},
	} {
		if got := StampRelativeText(now, c.t); got != c.want {
			t.Errorf("%s: StampRelativeText(%s) = %q, want %q", c.name, c.t.Format(time.RFC3339), got, c.want)
		}
	}
}

// Why: the day is the server's calendar day in the zone of "now"; an instant stored in another
// zone must not shift the answer.
func TestStampRelativeTextUsesNowsZone(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*3600)
	now := time.Date(2026, 10, 22, 0, 30, 0, 0, berlin)    // 21 Oct 22:30 UTC
	utc := time.Date(2026, 10, 21, 22, 10, 0, 0, time.UTC) // 22 Oct 00:10 in Berlin
	if got := StampRelativeText(now, utc); got != "Today 00:10" {
		t.Errorf("got %q, want the time in now's zone", got)
	}
}

// Why: the attributes are what time.js reads; "relative" must be the key, with the instant in UTC.
func TestStampRelativeAttrs(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 41, 0, 0, time.FixedZone("x", 7200))
	a := StampAttrs(at, StampRelative)
	if a["data-ts"] != "relative" || a["datetime"] != "2026-10-22T07:41:00Z" {
		t.Errorf("attrs = %v", a)
	}
	if !strings.Contains(StampLog.Layout, "15:04:05") || StampLog.Key != "clock-24" {
		t.Errorf("StampLog = %+v: the log column is 24-hour with seconds", StampLog)
	}
}

// The keys a <time data-ts> may carry. One list for the whole app: a key that only the server or
// only time.js knows shows the server's fallback text in the browser, which is how one instant came
// to be written five ways (#1104). Adding a key means adding it here, in stamp.go and in time.js.
var stampKeys = []string{"clock-24", "date", "date-time", "month-day", "relative", "start", "time", "title", "weekday-time"}

// stampKeysOfGo are the keys of every StampFmt literal in stamp.go (parsed, so a var nobody added
// to a list is still found).
func stampKeysOfGo(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "stamp.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "StampFmt" || len(lit.Elts) == 0 {
			return true
		}
		if b, ok := lit.Elts[0].(*ast.BasicLit); ok && b.Kind == token.STRING {
			k, err := strconv.Unquote(b.Value)
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		return true
	})
	return keys
}

// stampKeysOfJS are the top-level keys of the SPECS table in static/time.js. The extraction is
// narrow on purpose (the one object literal after "var SPECS = {", nested braces, parentheses and
// brackets skipped) and fails loudly when the block is not found: a reformat of time.js must not
// turn into an empty key set that passes.
func stampKeysOfJS(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../static/time.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "var SPECS = {")
	if i < 0 {
		t.Fatal(`time.js has no "var SPECS = {" block: the key table moved, update this test`)
	}
	s = s[i+len("var SPECS = {"):]
	var top strings.Builder // the text at depth 1 only, comments and strings' insides dropped
	depth := 1
scan:
	for j := 0; j < len(s); j++ {
		switch c := s[j]; {
		case c == '/' && j+1 < len(s) && s[j+1] == '/':
			for j < len(s) && s[j] != '\n' {
				j++
			}
		case c == '"':
			k := strings.IndexByte(s[j+1:], '"')
			if k < 0 {
				t.Fatal("time.js: unterminated string in SPECS")
			}
			if depth == 1 {
				top.WriteString(s[j : j+k+2])
			}
			j += k + 1
		case c == '{' || c == '(' || c == '[':
			depth++
		case c == '}' || c == ')' || c == ']':
			if depth--; depth == 0 {
				break scan
			}
		default:
			if depth == 1 {
				top.WriteByte(c)
			}
		}
	}
	if depth != 0 {
		t.Fatal("time.js: the SPECS block is not closed")
	}
	var keys []string
	for _, m := range regexp.MustCompile(`(?:^|[,\s])"?([A-Za-z][A-Za-z0-9-]*)"?\s*:`).FindAllStringSubmatch(top.String(), -1) {
		keys = append(keys, m[1])
	}
	if len(keys) == 0 {
		t.Fatal("time.js: no key found in SPECS")
	}
	return keys
}

// Why (#1104): a key that one side lacks shows the server's fallback text, so the same instant reads
// differently by how the page was drawn. The Go formats and the browser's table must be the same set,
// and that set is the decided one (date, date-time, month-day, relative, time, log, start, title).
func TestStampKeysMatchTimeJS(t *testing.T) {
	norm := func(k []string) []string { k = slices.Clone(k); slices.Sort(k); return slices.Compact(k) }
	goKeys, jsKeys := norm(stampKeysOfGo(t)), norm(stampKeysOfJS(t))
	if !slices.Equal(goKeys, stampKeys) {
		t.Errorf("stamp.go keys = %v, want %v", goKeys, stampKeys)
	}
	if !slices.Equal(jsKeys, stampKeys) {
		t.Errorf("time.js SPECS keys = %v, want %v", jsKeys, stampKeys)
	}
}

// Why (#1104): the fallback shows when JavaScript does not run and must say the same as time.js: no
// seconds in a date-time, a 24-hour clock, the day first with a month name (never 2026-11-20 beside
// "20 Nov"), and seconds only in the log column.
func TestStampFallbackLayouts(t *testing.T) {
	at := time.Date(2026, 11, 20, 14, 17, 25, 0, time.Local)
	for _, c := range []struct {
		f    StampFmt
		want string
	}{
		{StampDate, "20 Nov 2026"},
		{StampDateTime, "20 Nov 2026, 14:17"},
		{StampTime, "14:17"},
		{StampLog, "14:17:25"},
		{StampMonthDay, "Nov 20"},
	} {
		if got := StampText(at, c.f); got != c.want {
			t.Errorf("%s: StampText = %q, want %q", c.f.Key, got, c.want)
		}
	}
}

// Why (#1104): a day names its year only when it is not this year, and the choice is the server's "now" at render time
// (the key is written into the element, so time.js shows the same fields later).
func TestStampDayOfNamesTheYearOnlyWhenItIsNotThisOne(t *testing.T) {
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	if got := StampDayOf(now, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)); got.Key != "month-day" {
		t.Errorf("this year: %q, want month-day", got.Key)
	}
	if got := StampDayOf(now, time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)); got.Key != "date" {
		t.Errorf("another year: %q, want date", got.Key)
	}
}

// Why (#1104): a changelog day has no time of day. Written through the server's local zone a server west of UTC would
// print the day before; the calendar day is written as it is, in the date key's layout.
func TestStampCalendarDayIgnoresTheServersZone(t *testing.T) {
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	old := time.Local
	time.Local = time.FixedZone("west", -8*3600)
	t.Cleanup(func() { time.Local = old })
	if got := StampCalendarDay(day); got != "8 Oct 2026" {
		t.Errorf("StampCalendarDay = %q, want 8 Oct 2026", got)
	}
}
