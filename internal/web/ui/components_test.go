package ui

import (
	"context"
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
		"PageHeader":  PageHeader(h, h),
		"Button":      Button(Btn{Label: h, Aria: h}),
		"ButtonLink":  Button(Btn{Label: h, Href: "/x", Dialog: h}),
		"Badge":       Badge(Tone(h), h),
		"StatusDot":   StatusDot(ToneOK, h),
		"Avatar":      Avatar(h, h),
		"Card":        Card(h, h),
		"ListCard":    ListCard(h, h, "/x"),
		"KV":          KV([]KVRow{{Key: h, Value: h, Mono: true}}),
		"Tabs":        Tabs(h, []Tab{{Label: h, Href: "/x"}, {Label: h, Reason: h}}),
		"RowMenu":     RowMenu(h, []MenuItem{{Label: h, Href: "/x"}, {Label: h, Disabled: true, Why: h}}, false),
		"Banner":      Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Action: &Btn{Label: h}}),
		"ErrorSum":    ErrorSummary(h),
		"Toast":       ToastStatic(ToastData{Tone: ToneDanger, Text: h, Action: h}),
		"Field":       Field(FieldData{Label: h, Name: "n", Value: h, Placeholder: h, Error: h}),
		"Select":      SelectField(h, "n", []Option{{Value: h, Label: h}}),
		"Check":       Check("n", h, h, false, false),
		"RadioCards":  RadioCards(h, "n", []Option{{Value: h, Label: h, Text: h}}),
		"Segmented":   Segmented(h, "n", []Option{{Value: h, Label: h}}),
		"Toggle":      Toggle("n", h, h, true, false),
		"Power":       PowerSwitch(h, true, false),
		"StatTile":    StatTile(StatTileData{Label: h, Value: h, Hint: h, Href: "/x"}),
		"Attention":   AttentionList([]AttProposal{{Name: h, Text: h, Primary: h, Secondary: h, PrimaryAction: "/a", SecondaryAction: "/b"}}, []AttItem{{Name: h, Text: h, Href: "/x"}}, h),
		"TxnCard":     TxnCard(TxnData{ID: h, Title: h, State: h, Summary: h, Href: "/x", OK: 1, Total: 1}),
		"TxnRow":      TxnRow(TxnRowData{ID: h, Title: h, Result: h, When: h, Href: "/x"}),
		"Dialog":      Dialog(DialogData{ID: "d", Title: h, Body: h, Command: h, Changes: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Verb: h}),
		"StepUp":      StepUp(StepUpData{ID: "s", Name: h, Sub: h, What: h, IdP: h, IdPHref: "/x", Local: true, Action: "/x", CSRF: h, Error: h}),
		"ConfirmPage": ConfirmPage(ConfirmData{Title: h, Heading: h, Text: h, Warning: h, Facts: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Fields: []KVRow{{Key: h, Value: h}}, Verb: h, CancelHref: "/x", CancelLabel: h}),
		"Empty":       Empty(h, h),
		"NoMatch":     NoMatch(h, "/x"),
		"Skeleton":    Skeleton(h, 2),
		"ErrorPage":   ErrorPage(h, h, h, h, Btn{Label: h, Href: "/x"}, Btn{Label: h}),
		"AuthCard":    AuthCard("", h, h),
		"Console":     Console(ConsoleData{Title: h, Action: "/x", CSRF: h, Lines: []ConsoleLine{{Time: h, Text: h, Kind: "err"}}, Quick: []string{h}}),
		"Diff":        Diff(DiffData{OldLabel: h, NewLabel: h, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"DiffUnified": Diff(DiffData{OldLabel: h, NewLabel: h, Unified: true, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"Notes":       Notes([]NoteBlock{{Kind: "h", Text: h}, {Kind: "p", Text: h}, {Kind: "ul", Items: []string{h}}, {Kind: "code", Text: h}, {Kind: "breaking", Text: h, Items: []string{h}}}),
		"LivePill":    LivePill("ok", h),
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
