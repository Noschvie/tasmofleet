package ui

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// Why (#137): a transaction row's "when" is Today/Yesterday in the browser's zone, so with an
// instant it is a <time data-ts="day">; without one (the styleguide's sample words) plain text.
func TestTxnRowWhenIsADayStampWhenItHasAnInstant(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 41, 0, 0, time.UTC)
	got := comp(t, TxnRow(TxnRowData{ID: "TX-1", When: "Today 09:41", WhenAt: at, Href: "/x"}))
	if !strings.Contains(got, `<time data-ts="day" datetime="2026-10-22T09:41:00Z">Today 09:41</time>`) {
		t.Errorf("no day stamp: %s", got)
	}
	if got := comp(t, TxnRow(TxnRowData{ID: "TX-1", When: "2 days ago", Href: "/x"})); strings.Contains(got, "<time") {
		t.Errorf("sample text became a <time>: %s", got)
	}
}

func comp(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

const hostile = `<script>alert(1)</script>" onmouseover="x`

// Why: components carry text from devices, users and release feeds (names, notes, command
// replies). templ escapes text, but a component that ever switched to raw output would turn a
// device name into script. Every text-carrying component gets the same hostile string and must
// show it as text: no tag, no attribute breaking out of its quotes, and no style attribute (CSP).
func TestComponentsEscapeEveryText(t *testing.T) {
	h := hostile
	all := map[string]templ.Component{
		"PageHeader":       PageHeader(h, h),
		"PageHeaderSample": PageHeaderSample(h, h),
		"CrumbsSample":     CrumbsSample([]Crumb{{Label: h, Path: "/x"}, {Label: h}}, h),
		"ErrorPageSample":  ErrorPageSample(h, h, h, h, Btn{Label: h, Href: "/x"}, Btn{Label: h}),
		"Button":           Button(Btn{Label: h, Aria: h}),
		"ButtonLink":       Button(Btn{Label: h, Href: "/x", Dialog: h}),
		"Badge":            Badge(Tone(h), h),
		"StatusDot":        StatusDot(ToneOK, h),
		"Avatar":           Avatar(h, h),
		"Card":             Card(h, h),
		"ListCard":         ListCard(h, h, "/x"),
		"KV":               KV([]KVRow{{Key: h, Value: h, Mono: true}}),
		"Tabs":             Tabs(h, []Tab{{Label: h, Href: "/x"}, {Label: h, Reason: h}}),
		"RowMenu":          RowMenu(h, []MenuItem{{Label: h, Href: "/x"}, {Label: h, Disabled: true, Why: h}}, false),
		"Banner":           Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Action: &Btn{Label: h}}),
		"ErrorSum":         ErrorSummary(h),
		"BannerLink":       Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Action: &Btn{Label: h, Href: "/x", Target: "_blank", Rel: "noopener"}}),
		"Toast":            ToastStatic(ToastData{Tone: ToneDanger, Text: h, Action: h}),
		"Field":            Field(FieldData{Label: h, Name: "n", Value: h, Placeholder: h, Error: h}),
		"Select":           SelectField(h, "n", []Option{{Value: h, Label: h}}),
		"Check":            Check("n", h, h, false, false),
		"RadioCards":       RadioCards(h, "n", []Option{{Value: h, Label: h, Text: h}}),
		"Segmented":        Segmented(h, "n", []Option{{Value: h, Label: h}}),
		"Toggle":           Toggle("n", h, h, true, false),
		"Power":            PowerSwitch(h, true, false),
		"StatTile":         StatTile(StatTileData{Label: h, Value: h, Hint: h, Href: "/x"}),
		"Attention":        AttentionList([]AttProposal{{Name: h, Text: h, Primary: h, Secondary: h, PrimaryAction: "/a", SecondaryAction: "/b"}}, []AttItem{{Name: h, Text: h, Href: "/x"}}, h),
		"TxnCard":          TxnCard(TxnData{ID: h, Title: h, State: h, Summary: h, StartedText: h, Href: "/x", OK: 1, Total: 1}),
		"TxnRow":           TxnRow(TxnRowData{ID: h, Title: h, Result: h, When: h, Href: "/x"}),
		"Dialog":           Dialog(DialogData{ID: "d", Title: h, Body: h, Command: h, Changes: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Verb: h}),
		"StepUp":           StepUp(StepUpData{ID: "s", Name: h, Sub: h, What: h, IdP: h, IdPHref: "/x", Local: true, Action: "/x", CSRF: h, Error: h}),
		"ConfirmPage":      ConfirmPage(ConfirmData{Title: h, Heading: h, Text: h, Warning: h, Facts: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Fields: []KVRow{{Key: h, Value: h}}, Verb: h, CancelHref: "/x", CancelLabel: h}),
		"Empty":            Empty(h, h),
		"NoMatch":          NoMatch(h, "/x"),
		"Skeleton":         Skeleton(h, 2),
		"ErrorPage":        ErrorPage(h, h, h, h, Btn{Label: h, Href: "/x"}, Btn{Label: h}),
		"AuthCard":         AuthCard("", h, h),
		"Console":          Console(ConsoleData{Title: h, Action: "/x", CSRF: h, Lines: []ConsoleLine{{Time: h, Text: h, Kind: "err"}}, Quick: []string{h}}),
		"Diff":             Diff(DiffData{OldLabel: h, NewLabel: h, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"DiffUnified":      Diff(DiffData{OldLabel: h, NewLabel: h, Unified: true, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"Notes":            Notes([]NoteBlock{{Kind: "h", Text: h}, {Kind: "p", Text: h}, {Kind: "ul", Items: []string{h}}, {Kind: "code", Text: h}, {Kind: "breaking", Text: h, Items: []string{h}}}),
		"LivePill":         LivePill("ok", h),
	}
	for name, c := range all {
		out := comp(t, c)
		if strings.Contains(out, "<script>") {
			t.Errorf("%s: the text became a <script> tag", name)
		}
		if strings.Contains(out, `" onmouseover="`) {
			t.Errorf("%s: the text broke out of an attribute", name)
		}
		if strings.Contains(out, "style=") {
			t.Errorf("%s: renders a style attribute (blocked by the CSP)", name)
		}
		if !strings.Contains(out, "&lt;") { // the avatar shows only the initials, still escaped
			t.Errorf("%s: the hostile text is not shown as text: %.200s", name, out)
		}
	}
}

// Why (#1199): the card says when a transaction started. With an instant the time is a <time> element that
// time.js repaints in the browser's zone ("started" stays outside it, so a bare day stamp remains);
// without one (the style guide sample) the text stands alone. Without any text the card draws no time.
func TestTxnCardStartedTime(t *testing.T) {
	base := TxnData{ID: "TX-0001", Title: "t", State: "Running", Summary: "s", Href: "/x", Total: 1}
	at := time.Date(2026, 11, 22, 17, 20, 0, 0, time.UTC)
	timed := base
	timed.Started, timed.StartedText = at, "Today 17:20"
	out := comp(t, TxnCard(timed))
	if want := `<span class="txn-when">started <time data-ts="day" datetime="2026-11-22T17:20:00Z">Today 17:20</time></span>`; !strings.Contains(out, want) {
		t.Errorf("want %s in %s", want, out)
	}
	plain := base
	plain.StartedText = "2 days ago"
	if out := comp(t, TxnCard(plain)); !strings.Contains(out, `<span class="txn-when">started 2 days ago</span>`) || strings.Contains(out, "<time") {
		t.Errorf("the fallback text alone: %s", out)
	}
	if out := comp(t, TxnCard(base)); strings.Contains(out, "txn-when") {
		t.Errorf("a card without a start time draws one: %s", out)
	}
}

// Why (#515): a sample of the page header, the crumbs or the error page on /styleguide sits inside
// a page that already has an h1, the shell's #crumbs and its Breadcrumb landmark. The samples must
// look the same (same classes) but carry none of that page-level identity, or the style guide
// fails the checks it documents. The real components keep it: the shell and CrumbsOOB need the id.
func TestSampleVariantsCarryNoPageIdentity(t *testing.T) {
	trail := []Crumb{{Label: "Devices", Path: "/x"}, {Label: "Lamp"}}
	real := map[string]string{
		"PageHeader": comp(t, PageHeader("T", "S")),
		"ErrorPage":  comp(t, ErrorPage("404", "T", "B", "/p", Btn{Label: "A"}, Btn{Label: "B"})),
		"Crumbs":     comp(t, Crumbs(trail, "")),
	}
	for name, want := range map[string]string{"PageHeader": "<h1>T</h1>", "ErrorPage": "<h1>T</h1>", "Crumbs": `id="crumbs"`} {
		if !strings.Contains(real[name], want) {
			t.Errorf("%s lost %q: the real component keeps its page-level identity", name, want)
		}
	}
	samples := map[string]string{
		"PageHeaderSample": comp(t, PageHeaderSample("T", "S")),
		"ErrorPageSample":  comp(t, ErrorPageSample("404", "T", "B", "/p", Btn{Label: "A"}, Btn{Label: "B"})),
		"CrumbsSample":     comp(t, CrumbsSample(trail, "")),
	}
	for name, out := range samples {
		for _, bad := range []string{"<h1", ` id="crumbs"`, "Breadcrumb"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
	}
	if !strings.Contains(samples["PageHeaderSample"], `class="pghead"`) || !strings.Contains(samples["ErrorPageSample"], `class="errpage"`) {
		t.Error("a sample lost the wrapper class that styles it")
	}
	if !strings.Contains(samples["CrumbsSample"], `class="crumbs"`) || !strings.Contains(samples["CrumbsSample"], ">Lamp<") {
		t.Errorf("the crumbs sample no longer draws the trail: %s", samples["CrumbsSample"])
	}
}

// Why: "Actions are buttons; links only navigate." A Btn with an address is a link (no form
// submission by accident), without one a button that never submits unless asked; a disabled link
// says so to assistive technology.
func TestButtonIsALinkOnlyWithAnAddress(t *testing.T) {
	if got := comp(t, Button(Btn{Label: "Go", Href: "/x", Kind: "primary"})); !strings.HasPrefix(got, `<a class="btn btn-primary" href="/x">`) {
		t.Errorf("link button: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "Do"})); !strings.HasPrefix(got, `<button type="button" class="btn"`) {
		t.Errorf("plain button must not submit a form: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "Send", Type: "submit", Name: "cmd", Value: "Status 5"})); !strings.Contains(got, `type="submit"`) || !strings.Contains(got, `name="cmd" value="Status 5"`) {
		t.Errorf("submit button with a value: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "No", Href: "/x", Disabled: true})); !strings.Contains(got, `aria-disabled="true"`) {
		t.Errorf("a disabled link must say so: %s", got)
	}
}

// Why (#1069): the one a row of buttons is on (the pager's page size) is told to assistive
// technology (aria-current="page") and styled as the selected one (btn-current), on a link and on a
// button alike; a button that is not current carries neither, so the mark means something.
func TestButtonCurrentIsMarkedForAssistiveTechnology(t *testing.T) {
	got := comp(t, Button(Btn{Label: "50", Href: "/x?per=50", Small: true, Current: true, Rel: "next"}))
	if want := `<a class="btn btn-sm btn-current" href="/x?per=50" rel="next" aria-current="page">50</a>`; got != want {
		t.Errorf("current link: %s, want %s", got, want)
	}
	if got := comp(t, Button(Btn{Label: "50", Current: true})); !strings.Contains(got, `aria-current="page"`) || !strings.Contains(got, "btn-current") {
		t.Errorf("current button: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "25", Href: "/x", Small: true})); strings.Contains(got, "aria-current") || strings.Contains(got, "btn-current") || strings.Contains(got, "rel=") {
		t.Errorf("a button that is not current must carry no mark: %s", got)
	}
}

// Why: a destructive confirmation repeats the verb in the danger tone; anything else is primary.
// A dialog posts with the CSRF token and its Cancel never submits.
func TestDialogVerbToneAndForm(t *testing.T) {
	del := comp(t, Dialog(DialogData{ID: "d", Title: "Delete?", Action: "/devices/1/delete", CSRF: "tok", Verb: "Delete device", Danger: true}))
	for _, want := range []string{`method="post" action="/devices/1/delete"`, `name="csrf" value="tok"`, `<button type="button" class="btn" data-close>Cancel</button>`, `class="btn btn-danger-solid">Delete device</button>`} {
		if !strings.Contains(del, want) {
			t.Errorf("delete dialog lacks %q", want)
		}
	}
	if ok := comp(t, Dialog(DialogData{ID: "r", Verb: "Restart"})); !strings.Contains(ok, `class="btn btn-primary">Restart</button>`) {
		t.Errorf("a non-destructive verb is primary: %s", ok)
	}
}

// Why: a field the server rejected is marked for assistive technology and points at its message,
// and the message replaces the help line instead of stacking under it.
func TestFieldErrorIsAnnounced(t *testing.T) {
	got := comp(t, Field(FieldData{Label: "Address", Name: "addr", Help: "IP or host", Error: "Enter an IP address."}))
	for _, want := range []string{`aria-invalid="true"`, `aria-describedby="err-addr"`, `<span class="ufield-err" id="err-addr">Enter an IP address.</span>`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "IP or host") {
		t.Error("the help line is shown next to the error")
	}
	if ok := comp(t, Field(FieldData{Label: "Address", Name: "addr", Help: "IP or host"})); strings.Contains(ok, "aria-invalid") {
		t.Error("a valid field is marked invalid")
	}
}

// Why: a typo in a tone must not drop the colour silently: an unknown tone renders neutral.
func TestUnknownToneIsNeutral(t *testing.T) {
	if got := comp(t, Badge("succes", "Done")); !strings.Contains(got, `class="bdg t-neutral"`) {
		t.Errorf("unknown tone: %s", got)
	}
	if got := comp(t, Badge(ToneOK, "Done")); !strings.Contains(got, `class="bdg t-ok"`) {
		t.Errorf("ok tone: %s", got)
	}
}

// Why: the bars' widths come from the server as SVG attributes (no inline style under the CSP).
// The parts must keep their proportions, leave the gaps between them, never overlap and never
// overflow the bar; an empty part takes no room (no stray gap).
func TestBarSegmentsKeepProportionsAndGaps(t *testing.T) {
	segs := healthSegments(54, 4, 2)
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(segs))
	}
	for i := 1; i < len(segs); i++ {
		if gap := segs[i].X - (segs[i-1].X + segs[i-1].W); gap != 2 {
			t.Errorf("gap before segment %d is %d units, want 2", i, gap)
		}
	}
	if end := segs[2].X + segs[2].W; end > 1000 {
		t.Errorf("the bar overflows: ends at %d of 1000", end)
	}
	if segs[0].W < 850 || segs[0].W > 900 {
		t.Errorf("54 of 60 online is %d units, want about 900", segs[0].W)
	}
	if one := healthSegments(10, 0, 0); len(one) != 1 || one[0].X != 0 || one[0].W != 1000 {
		t.Errorf("an all-online fleet is one full segment: %+v", one)
	}
	if none := healthSegments(0, 0, 0); len(none) != 0 {
		t.Errorf("an empty fleet draws no segment: %+v", none)
	}
	if tx := txnSegments(TxnData{OK: 1, Failed: 0, Active: 1, Total: 4}); len(tx) != 2 || tx[1].X-(tx[0].X+tx[0].W) != 1 {
		t.Errorf("transaction bar: %+v", tx)
	}
}

// Why: a user keeps one avatar colour everywhere (the same username, the same tone).
func TestAvatarToneIsStable(t *testing.T) {
	if first, again := AvatarTone("maren"), AvatarTone("maren"); first != again {
		t.Error("the avatar tone changes between calls")
	}
	seen := map[Tone]bool{}
	for _, u := range []string{"maren", "jonas", "ops", "ada", "lin", "kai", "olivia", "sam"} {
		seen[AvatarTone(u)] = true
	}
	if len(seen) < 2 {
		t.Error("every user gets the same avatar colour")
	}
}

// Why (D12): the skeleton is a lazily loaded fragment's state and tells assistive technology the
// region is busy; a page never renders it as its whole content.
func TestSkeletonIsBusy(t *testing.T) {
	got := comp(t, Skeleton("Loading devices…", 3))
	if !strings.Contains(got, `aria-busy="true"`) || strings.Count(got, `class="skel-row`) != 3 {
		t.Errorf("skeleton: %s", got)
	}
}

// Why: the devices still waiting are the empty part of a transaction's bar. Spreading the done,
// failed and running parts over the full width made a transaction with 9 of 12 devices queued
// look three quarters finished.
func TestTxnBarLeavesTheWaitingDevicesEmpty(t *testing.T) {
	segs := txnSegments(TxnData{OK: 5, Failed: 1, Active: 3, Total: 12})
	if len(segs) != 3 {
		t.Fatalf("want the three drawn parts, got %+v", segs)
	}
	end := segs[2].X + segs[2].W
	if end < 740 || end > 760 { // 9 of 12 = 750 units of 1000
		t.Errorf("the drawn parts end at %d, want about 750: %+v", end, segs)
	}
}

// Why (#1073): a badge that says "Reject" in a tone is a word plus colour; the mark is a second cue that colour-blind
// users and screen-magnifier users still see, while a screen reader reads the word only (the glyph is decoration).
func TestBadgeMarkIsAGlyphBesideTheWord(t *testing.T) {
	got := comp(t, BadgeMark(ToneWarn, "–", "Reject"))
	if !strings.Contains(got, `class="bdg t-warn"`) || !strings.Contains(got, `<span class="bdg-mark" aria-hidden="true">–</span>`) || !strings.HasSuffix(got, "Reject</span>") {
		t.Errorf("badge with mark: %s", got)
	}
	if got := comp(t, BadgeMark(ToneOK, "", "Create")); strings.Contains(got, "bdg-mark") {
		t.Errorf("an empty mark draws no glyph: %s", got)
	}
}

// Why (#1073): the count of a segment is meant to be read; "off policy" is the visible text the device list says, short enough for one line beside "Online" in the Status column.
func TestOffPolicyIsTheVisibleDeviationText(t *testing.T) {
	for n, want := range map[int]string{1: "1 off policy", 2: "2 off policy", 12: "12 off policy"} {
		if got := OffPolicy(n); got != want {
			t.Errorf("OffPolicy(%d) = %q, want %q", n, got, want)
		}
	}
}

// Why (#1073): a transaction bar told done, running and failed apart by hue only (about 1.1:1 between done and
// failed in dark), so a user who cannot tell green, blue and red apart could not read the progress. Each state
// has its own shape as well: done is one solid bar, running is stripes with gaps, failed is a two-row checker.
// Today every state is one rect, so this fails on a change back to colour alone.
func TestSegBarDoneRunningFailedDifferByShape(t *testing.T) {
	got := comp(t, SegBar(TxnData{OK: 5, Failed: 3, Active: 3, Total: 12}))
	rectRe := regexp.MustCompile(`<rect class="bar-seg (t-[a-z]+)" x="(\d+)" y="(\d+)" width="(\d+)" height="(\d+)"`)
	type rect struct{ x, y, w, h int }
	by := map[string][]rect{}
	for _, m := range rectRe.FindAllStringSubmatch(got, -1) {
		n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
		by[m[1]] = append(by[m[1]], rect{n(2), n(3), n(4), n(5)})
	}
	if len(by["t-ok"]) != 1 {
		t.Errorf("done is one solid rect, got %+v", by["t-ok"])
	}
	run := by["t-accent"]
	if len(run) < 3 {
		t.Fatalf("running is stripes, got %+v", run)
	}
	for i := 1; i < len(run); i++ {
		if run[i].x <= run[i-1].x+run[i-1].w {
			t.Errorf("running stripes %d and %d touch: no gap, so no stripe: %+v", i-1, i, run)
		}
	}
	rows := map[int]bool{}
	for _, r := range by["t-danger"] {
		rows[r.y] = true
	}
	if len(by["t-danger"]) < 3 || len(rows) != 2 {
		t.Errorf("failed is a checker over two rows, got %+v", by["t-danger"])
	}
	// the stripes of a part stay inside the part: the bar keeps its proportions and never overflows
	segs := txnSegments(TxnData{OK: 5, Failed: 3, Active: 3, Total: 12})
	for _, s := range segs {
		for _, r := range by[s.Tone.class()] {
			if r.x < s.X || r.x+r.w > s.X+s.W {
				t.Errorf("%s rect %+v leaves its part %+v", s.Tone.class(), r, s)
			}
		}
	}
}

// Why: a narrow column used to cut "tasmota-minimal.bin.gz" inside "bin" and a device's JSON reply
// inside "Boot". The parts are where a reader would cut; joined they are the original text, so
// nothing a user copies or an e2e test searches for changes.
func TestSoftPartsBreakAtNaturalPoints(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"tasmota-minimal.bin.gz", []string{"tasmota-", "minimal.", "bin.", "gz"}},
		{"mqtt.example.internal:1883", []string{"mqtt.", "example.", "internal:", "1883"}},
		{"127.0.0.1:20032", []string{"127.0.0.1:", "20032"}}, // the dots of an address stay together
		{"16.0.0", []string{"16.0.0"}},
		{"http://h/fw/a.bin", []string{"http://", "h/", "fw/", "a.", "bin"}},
		{"?chip=down|outdated|noncompliant", []string{"?chip=", "down|", "outdated|", "noncompliant"}}, // #510: a query in the changelog
		// a token's first character stays with it: a line never ends in a bare "/", "." or ":" (#510)
		{"/settings/system/snapshots", []string{"/settings/", "system/", "snapshots"}},
		{".dmp", []string{".dmp"}},
		{":8443", []string{":8443"}},
		{"--ctl-sm", []string{"--ctl-", "sm"}},
		{"Sign in at /login", []string{"Sign in at /login"}},
		{"", []string{""}},
	} {
		got := SoftParts(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("SoftParts(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Join(got, "") != tc.in {
			t.Errorf("SoftParts(%q) does not join back to the input: %q", tc.in, got)
		}
	}
	if got := SoftJSONParts(`{"A":1,"B":2}`); strings.Join(got, "|") != `{"A":1,|"B":2}` {
		t.Errorf("SoftJSONParts = %q", got)
	}
}

// Why: a KV row's time is shown by time.js in the browser's zone. The default is the short date
// and time; a row that names another format (a relative day for "Last sign-in") must get exactly
// that format, or Today/Yesterday would again be decided by the server's zone (#137).
func TestKVRowTimeFormatDefaultsToShortAndHonoursStamp(t *testing.T) {
	at := time.Date(2026, 10, 29, 9, 41, 0, 0, time.UTC)
	got := renderC(t, KV([]KVRow{{Key: "Seen", Value: "x", At: at}, {Key: "Last", Value: "y", At: at, Stamp: StampDay}}), context.Background())
	if !strings.Contains(got, `data-ts="short"`) || !strings.Contains(got, `data-ts="day"`) {
		t.Fatalf("formats:\n%s", got)
	}
}

// Why: the rule table's bar is the health bar with other parts. A rule that 17 of 18 devices meet
// must draw 17 green units and one warn segment, and say the same in words, because the colours
// alone tell a screen reader nothing.
func TestComplianceBarSegmentsAndText(t *testing.T) {
	segs := complianceSegments(17, 1, 0)
	if len(segs) != 2 || segs[0].Tone != ToneOK || segs[1].Tone != ToneWarn {
		t.Fatalf("segments: %+v", segs)
	}
	out := comp(t, ComplianceBar(17, 1, 2))
	if !strings.Contains(out, `aria-label="17 match, 1 deviate, 2 not judged"`) {
		t.Errorf("aria text: %s", out)
	}
	if strings.Contains(comp(t, ComplianceBar(0, 0, 0)), "<rect") {
		t.Error("a rule that applies to no device draws no segment")
	}
}

// Why (#1078, #840): a banner whose action leads off TasmoFleet (the broker ACL guide) opens it in a new tab, so the
// person keeps the page they were on (a confirmation, a form), without handing the guide the opener; a screen reader
// hears that a tab opens. The banner passes the action's Target and Rel through like ui.Button does, for every banner:
// dropping them would send the person away from an unsaved confirmation.
func TestBannerActionLinkKeepsTargetAndRel(t *testing.T) {
	got := comp(t, Banner(BannerData{Tone: ToneWarn, Title: "Lead.", Text: "Text.", Action: &Btn{Label: "Guide", Href: "https://example.invalid/guide", Target: "_blank", Rel: "noopener noreferrer"}}))
	want := `<a class="banner-btn" href="https://example.invalid/guide" target="_blank" rel="noopener noreferrer">Guide <span class="sr-only">(opens in a new tab)</span></a>`
	if !strings.Contains(got, want) {
		t.Errorf("the external action lost its target, rel or new-tab hint:\n got %s\nwant %s", got, want)
	}
	same := comp(t, Banner(BannerData{Tone: ToneWarn, Text: "Text.", Action: &Btn{Label: "Open", Href: "/x"}}))
	if strings.Contains(same, "target=") || strings.Contains(same, "rel=") || strings.Contains(same, "new tab") {
		t.Errorf("an action inside TasmoFleet opens in the same tab and says nothing of a new one: %s", same)
	}
}

// Why (#1072): a blocked button whose reason is a banner the page shows already names the banner with
// aria-describedby; the banner needs an id for that, and a banner without one must not render an empty id.
func TestBannerIDIsTheTargetOfADescribedBy(t *testing.T) {
	if got := comp(t, Banner(BannerData{Tone: ToneAccent, Text: "Managed elsewhere.", ID: "ud-info"})); !strings.Contains(got, `id="ud-info"`) {
		t.Errorf("the banner lost its id: %s", got)
	}
	if got := comp(t, Banner(BannerData{Tone: ToneAccent, Text: "Text."})); strings.Contains(got, "id=") {
		t.Errorf("a banner without an id renders one: %s", got)
	}
}

// Why (#1082): the offline banner's lead holds a time ("Offline since 2:17 PM."), and a time shown in the server's zone
// is wrong for a viewer in another. The lead takes the instant as a <time> stamp the browser rewrites, with the
// fallback text for a page without JavaScript, inside the bold lead.
func TestBannerLeadCanCarryATimeStamp(t *testing.T) {
	at := time.Date(2026, 11, 21, 14, 17, 0, 0, time.UTC)
	got := comp(t, Banner(BannerData{Tone: ToneWarn, Title: "Offline since ", TitleAt: at, TitleAtText: "Today 14:17", TitleAfter: ".", Text: "Controls are off."}))
	if !strings.Contains(got, `<b>Offline since <time data-visual-mask`) || !strings.Contains(got, `>Today 14:17</time>.</b>`) || !strings.Contains(got, "Controls are off.") {
		t.Errorf("the lead lost its time: %s", got)
	}
	if got := comp(t, Banner(BannerData{Tone: ToneWarn, Title: "Lead.", Text: "Text."})); strings.Contains(got, "<time") {
		t.Errorf("a lead without an instant renders a time: %s", got)
	}
}

// Why (#1072): a button that cannot act now must still be reachable by Tab and say why as text, so it is
// aria-disabled (never the disabled attribute, which no key reaches and no touch explains) and names the line that
// holds the reason; and it must not be able to do anything with JavaScript off, so it carries no address, dialog,
// name, value or submit type even when the caller set them.
func TestBtnDisabledReasonIsABlockedButtonThatCarriesNoAction(t *testing.T) {
	got := comp(t, Button(Btn{Label: "Restart", Href: "/devices/1/restart", Dialog: "dlg-restart", Type: "submit", Name: "n", Value: "v",
		DisabledReason: "The device is offline.", ReasonID: "why-1"}))
	for _, want := range []string{`<button type="button"`, `aria-disabled="true"`, `aria-describedby="why-1"`, `<span class="btn-why" id="why-1">The device is offline.</span>`} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q: %s", want, got)
		}
	}
	for _, bad := range []string{" disabled", "title=", "href=", "data-dialog", "name=", "value=", `type="submit"`} {
		if strings.Contains(got, bad) {
			t.Errorf("a blocked button must not carry %q: %s", bad, got)
		}
	}
}

// Why (#1072): several buttons may name one line the page shows already (a banner, a line for the whole row); the
// button then draws no second copy of the reason.
func TestBtnSharedReasonDrawsNoLineOfItsOwn(t *testing.T) {
	got := comp(t, Button(Btn{Label: "Back up", DisabledReason: "The device is offline.", ReasonID: "devhd-off", ReasonShared: true}))
	if !strings.Contains(got, `aria-describedby="devhd-off"`) || strings.Contains(got, "btn-why") || strings.Contains(got, "The device is offline.") {
		t.Errorf("shared reason: %s", got)
	}
}

// Why (#1072): a button without a reason is what it was: the native disabled attribute stays for a button that is
// off for a moment and says nothing.
func TestBtnDisabledWithoutReasonStaysNative(t *testing.T) {
	got := comp(t, Button(Btn{Label: "Save", Disabled: true}))
	if !strings.Contains(got, "disabled") || strings.Contains(got, "aria-disabled") {
		t.Errorf("plain disabled: %s", got)
	}
}
