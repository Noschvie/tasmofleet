package ui

import (
	"context"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

// Why (#137): a transaction row's "when" is Today/Yesterday in the browser's zone, so with an
// instant it is a <time data-ts="relative">; without one (the styleguide's sample words) plain text.
func TestTxnRowWhenIsADayStampWhenItHasAnInstant(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 41, 0, 0, time.UTC)
	got := comp(t, TxnRow(TxnRowData{ID: "TX-1", When: "Today 09:41", WhenAt: at, Href: "/x"}))
	if !strings.Contains(got, `<time data-ts="relative" datetime="2026-10-22T09:41:00Z">Today 09:41</time>`) {
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

// compKids renders c with kid as its children.
func compKids(t *testing.T, c, kid templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(templ.WithChildren(context.Background(), kid), &b); err != nil {
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
		"BadgeOf":          BadgeOf(BadgeData{Tone: Tone(h), Label: h, Class: h, Title: h, ID: h, Large: true}),
		"BadgeOfLink":      BadgeOf(BadgeData{Tone: ToneAccent, Label: h, Class: h, Title: h, ID: h, Href: h}),
		"CopyText":         CopyButton(CopyData{Label: h, Text: h, Aria: h}),
		"CopyFrom":         CopyButton(CopyData{Label: h, From: h, Aria: h}),
		"ButtonAttrs":      Button(Btn{Label: h, Type: "submit", ID: h, Form: h, FormAction: h, NoValidate: true, Tall: true}),
		"ButtonPage":       Button(Btn{Label: h, Type: "submit", Class: h, Attrs: templ.Attributes{"data-x": h, "title": h, "hx-post": h}}),
		"ButtonLinkPage":   Button(Btn{Label: h, Href: "/x", Class: h, Attrs: templ.Attributes{"data-x": h, "download": h}}),
		"BadgeOfAttrs":     BadgeOf(BadgeData{Tone: ToneOK, Label: h, Attrs: templ.Attributes{"data-x": h}}),
		"MenuButton":       MenuButton(h, h),
		"CardOf":           CardOf(CardData{Title: h, Sub: h, ID: h, Class: h, Rank: 2, Tag: h, LabelledBy: h, Aria: h, Attrs: templ.Attributes{"data-x": h}}),
		"CardOfForm":       CardOf(CardData{Title: h, Tag: "form", Aria: h, Attrs: templ.Attributes{"action": templ.SafeURL(h), "method": h}}),
		"ButtonLinkID":     Button(Btn{Label: h, Href: "/x", ID: h, Tall: true}),
		"StatusDot":        StatusDot(ToneOK, h),
		"Avatar":           Avatar(h, h),
		"Card":             Card(h, h),
		"ListCard":         ListCard(h, h, "/x"),
		"KV":               KV([]KVRow{{Key: h, Value: h, Mono: true}}),
		"KVAction":         KV([]KVRow{{Key: h, Value: h, Action: &Btn{Label: h, Kind: "link", Attrs: templ.Attributes{"hx-post": h}}}}),
		"Tabs":             Tabs(h, []Tab{{Label: h, Href: "/x"}, {Label: h, Reason: h}}),
		"RowMenu":          RowMenu(h, []MenuItem{{Label: h, Href: "/x"}, {Label: h, Disabled: true, Why: h}}, false),
		"Banner":           Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Action: &Btn{Label: h}}),
		"BannerProps":      Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Class: h, ID: h, Pulse: true, Flash: true, Attrs: templ.Attributes{"data-x": h}, Action: &Btn{Label: h, Dialog: h, Attrs: templ.Attributes{"hx-post": h, "data-y": h}}}),
		"BannerLink":       Banner(BannerData{Tone: ToneWarn, Title: h, Text: h, Action: &Btn{Label: h, Href: "/x", Target: "_blank", Rel: "noopener"}}),
		"Toast":            ToastStatic(ToastData{Tone: ToneDanger, Text: h, Action: h}),
		"Field":            Field(FieldData{Label: h, Name: "n", Value: h, Placeholder: h, Error: h}),
		"Select":           SelectField(h, "n", []Option{{Value: h, Label: h}}),
		"Check":            Check("n", h, h, false, false),
		"RadioCards":       RadioCards(h, "n", []Option{{Value: h, Label: h, Text: h}}),
		"Segmented":        Segmented(h, "n", []Option{{Value: h, Label: h}}),
		"Toggle":           Toggle(ToggleData{Name: "n", Label: h, Text: h, On: true, Attrs: templ.Attributes{"data-x": h}}),
		"ToggleBare":       Toggle(ToggleData{Name: "n", Value: h, ID: h, Attrs: templ.Attributes{"aria-describedby": h}}),
		"Power":            PowerSwitch(PowerSwitchData{Label: h, On: true}),
		"PowerLabelledBy":  PowerSwitch(PowerSwitchData{LabelledBy: h, Class: h, Submit: true, Attrs: templ.Attributes{"title": h, "hx-post": h}}),
		"StatTile":         StatTile(StatTileData{Label: h, Value: h, Hint: h, Href: "/x"}),
		"Attention": AttentionList(AttentionData{Note: h, CSRF: h, PropsTitle: h, Props: []AttProposal{{Name: h, Text: h, Primary: h, Secondary: h, PrimaryAction: "/a", SecondaryAction: "/b"}}, PropsMore: AttMore{Text: h, Href: "/m", Label: h},
			Groups: []AttGroup{{Title: h, Items: []AttItem{{Name: h, Text: h, Href: "/x"}}, More: AttMore{Text: h, Href: "/y", Label: h}}, {Title: h, More: AttMore{Text: h}}}}),
		"AttentionStatus": AttentionList(AttentionData{Groups: []AttGroup{{Title: h, Items: []AttItem{{Name: h, Text: h, Href: "/x", Status: &StatusBadgeData{State: h, Tone: Tone(h), Label: h, Title: h}}}}}}),
		"StatusBadge":     StatusBadge(StatusBadgeData{State: h, Tone: Tone(h), Label: h, Title: h}),
		"InfoTile":        InfoTile(InfoTileData{Key: h, Label: h, Value: h, Tone: Tone(h), Word: true, Mask: true, Bar: 40, BarTone: Tone(h), Hint: h}),
		"InfoTileAt":      InfoTile(InfoTileData{Key: h, Label: h, Value: h, HintPrefix: h, HintAt: time.Unix(1700000000, 0), HintAtText: h}),
		"TxnCard":         TxnCard(TxnData{ID: h, Title: h, State: h, Summary: h, StartedText: h, Href: "/x", OK: 1, Total: 1}),
		"TxnRow":          TxnRow(TxnRowData{ID: h, Title: h, Result: h, When: h, Href: "/x"}),
		"Dialog":          Dialog(DialogData{ID: "d", Title: h, Body: h, Command: h, Changes: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Verb: h}),
		"StepUp":          StepUp(StepUpData{ID: "s", Name: h, Sub: h, What: h, IdP: h, IdPHref: "/x", Local: true, Action: "/x", CSRF: h, Error: h}),
		"ConfirmPage":     ConfirmPage(ConfirmData{Title: h, Heading: h + "x", Text: h, Warning: h, Facts: []KVRow{{Key: h, Value: h}}, Action: "/x", CSRF: h, Fields: []KVRow{{Key: h, Value: h}}, Verb: h, CancelHref: "/x", CancelLabel: h}),
		"Empty":           Empty(h, h),
		"EmptySteps":      EmptySteps(h, []string{h, h}),
		"NoMatch":         NoMatch(h, "/x"),
		"Skeleton":        Skeleton(h, 2),
		"ErrorPage":       ErrorPage(h, h, h, h, Btn{Label: h, Href: "/x"}, Btn{Label: h}),
		"AuthCard":        AuthCard("", h, h),
		"Console":         Console(ConsoleData{Title: h, Action: "/x", CSRF: h, Lines: []ConsoleLine{{Time: h, Text: h, Kind: "err"}}, Quick: []QuickCmd{{Command: h}, {Command: h, Href: h, Dialog: h}, {Command: h, Why: h, WhyID: h}}}),
		"Diff":            Diff(DiffData{OldLabel: h, NewLabel: h, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"DiffUnified":     Diff(DiffData{OldLabel: h, NewLabel: h, Unified: true, Rows: []DiffRow{{Key: h, Old: h, New: h, Changed: true}}}),
		"Notes":           Notes([]NoteBlock{{Kind: "h", Text: h}, {Kind: "p", Text: h}, {Kind: "ul", Items: []string{h}}, {Kind: "code", Text: h}, {Kind: "breaking", Text: h, Items: []string{h}}}),
		"LivePill":        LivePill("ok", h),
		"Table":           Table(TableData{Caption: h, CaptionVisible: true, Class: h, Cols: []TableCol{{Label: h, Class: h}}, BodyAttrs: templ.Attributes{"data-x": h, "style": h, "onclick": h}}),
		"TableSrCaption":  Table(TableData{Caption: h, Key: "audit", Select: h, SortBase: h, Cols: []TableCol{{Label: h, SrOnly: true}}}),
		"RowHead":         RowHead(RowHeadData{Class: h, Label: h}),
		"Pager":           Pager(PagerData{Page: 1, Pages: 2, Total: 30, Noun: h, Prev: "/p", Next: "/n", Sizes: []PagerSize{{N: 25, Href: h, Current: true}}}),
		"ChipInput":       Chip(ChipData{Label: h, Count: h, Sub: h, Long: h, Title: h, Name: h, Value: h, Form: h, Remove: true, Attrs: templ.Attributes{"data-x": h}}),
		"ChipRadio":       Chip(ChipData{Label: h, Name: h, Value: h, Radio: true, Checked: true}),
		"ChipLink":        Chip(ChipData{Label: h, Count: h, Title: h, Href: h, Current: true, Attrs: templ.Attributes{"data-x": h}}),
		"ChipLinkPath":    Chip(ChipData{Label: h, Href: "/x?q=" + h}),
		"ChipButton":      Chip(ChipData{Label: h, Count: h, Title: h, Current: true, Attrs: templ.Attributes{"hx-post": h}}),
		"Tag":             Tag(h),
		"TagOf":           TagOf(TagData{Label: h, Title: h, Attrs: templ.Attributes{"data-x": h}}),
		"TagLink":         TagOf(TagData{Label: h, Title: h, Href: h, Mono: true}),
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
	if want := `<span class="txn-when">started <time data-ts="relative" datetime="2026-11-22T17:20:00Z">Today 17:20</time></span>`; !strings.Contains(out, want) {
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

// Why: a KV row's time is shown by time.js in the browser's zone. The default is the date and time
// (date-time); a row that names another format (a relative day for "Last sign-in") must get exactly
// that format, or Today/Yesterday would again be decided by the server's zone (#137).
func TestKVRowTimeFormatDefaultsToDateTimeAndHonoursStamp(t *testing.T) {
	at := time.Date(2026, 10, 29, 9, 41, 0, 0, time.UTC)
	got := renderC(t, KV([]KVRow{{Key: "Seen", Value: "x", At: at}, {Key: "Last", Value: "y", At: at, Stamp: StampRelative}}), context.Background())
	if !strings.Contains(got, `data-ts="date-time"`) || !strings.Contains(got, `data-ts="relative"`) {
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

// Why (#1268): the health and compliance bars fill their segments with the tone's text colour, which meets the
// track with 3:1 or more, and a divider in the track's colour sits on every boundary between two drawn segments, so
// online next to offline is told apart by that divider and not by hue alone. The 2-unit gap alone is under a pixel
// on a narrow bar. One line per boundary, centred in the gap, none for one segment and none for an empty part; the
// count comes from segments(), never from a fixed number.
func TestBarsDrawADividerOnEveryBoundary(t *testing.T) {
	for _, c := range []struct {
		name string
		html string
		segs []barSeg
	}{
		{"health", comp(t, HealthBar(54, 4, 2)), healthSegments(54, 4, 2)},
		{"health, two parts", comp(t, HealthBar(54, 0, 6)), healthSegments(54, 0, 6)},
		{"health, one part", comp(t, HealthBar(10, 0, 0)), healthSegments(10, 0, 0)},
		{"health, empty", comp(t, HealthBar(0, 0, 0)), healthSegments(0, 0, 0)},
		{"compliance", comp(t, ComplianceBar(17, 1, 2)), complianceSegments(17, 1, 2)},
		{"compliance, one part", comp(t, ComplianceBar(18, 0, 0)), complianceSegments(18, 0, 0)},
	} {
		lines := regexp.MustCompile(`<line class="bar-gap" x1="(\d+)" x2="(\d+)" y1="0" y2="8"></line>`).FindAllStringSubmatch(c.html, -1)
		want := max(len(c.segs)-1, 0)
		if len(lines) != want {
			t.Errorf("%s: %d dividers, want %d (one per boundary between drawn segments): %s", c.name, len(lines), want, c.html)
			continue
		}
		for i, m := range lines {
			x, _ := strconv.Atoi(m[1])
			end, next := c.segs[i].X+c.segs[i].W, c.segs[i+1].X
			if m[1] != m[2] || x != (end+next)/2 {
				t.Errorf("%s: divider %d at x1=%s x2=%s, want the centre %d of the gap between %d and %d", c.name, i, m[1], m[2], (end+next)/2, end, next)
			}
		}
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

// Why (#1091, DS-007, DS-009): pages wrote their banners by hand because ui.Banner could not hold what they needed (a
// list, a link or a time in the text, a hook class, a data attribute, a pulsing dot, an action that opens a dialog or
// posts with htmx), and a hand-written banner is where a second action and a missing aria-hidden crept in. The banner
// takes children after its text, a class, attributes, the pulse and the flash mark; its one action is a full ui.Button
// (the dialog and the attributes arrive), and there is no way to give it a second one. The decorative dot is hidden
// from a screen reader.
func TestBannerProps(t *testing.T) {
	withKids := comp(t, templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		kids := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
			_, err := io.WriteString(w, `<ul class="banner-list"><li>one</li></ul>`)
			return err
		})
		return Banner(BannerData{
			Tone: ToneAccent, Title: "Lead.", Text: "Text ", Class: "cmd-check x", ID: "b1", Pulse: true, Flash: true,
			Attrs:  templ.Attributes{"data-cooldown": true, "class": "evil", "style": "x", "onclick": "x"},
			Action: &Btn{Label: "Open", Href: "/x", Dialog: "dlg", Attrs: templ.Attributes{"hx-post": "/p", "data-live-retry": true}},
		}).Render(templ.WithChildren(ctx, kids), w)
	}))
	for _, want := range []string{
		`<div class="banner t-accent cmd-check x" id="b1" role="status" data-flash data-cooldown>`,
		`<span class="banner-dot pulse" aria-hidden="true"></span>`,
		`<div class="banner-text"><b>Lead.</b> Text <ul class="banner-list"><li>one</li></ul></div>`,
		`<a class="banner-btn" href="/x" data-dialog="dlg" data-live-retry hx-post="/p">Open</a>`,
	} {
		if !strings.Contains(withKids, want) {
			t.Errorf("banner lacks %q:\n%s", want, withKids)
		}
	}
	for _, bad := range []string{"evil", "style=", "onclick"} {
		if strings.Contains(withKids, bad) {
			t.Errorf("the banner renders what pageAttrs refuses (%q): %s", bad, withKids)
		}
	}
	if plain := comp(t, Banner(BannerData{Tone: ToneWarn, Text: "T."})); !strings.Contains(plain, `<span class="banner-dot" aria-hidden="true"></span>`) || strings.Contains(plain, "pulse") || strings.Contains(plain, "data-flash") {
		t.Errorf("a plain banner has a still dot and no flash mark: %s", plain)
	}
	// One action: the struct has exactly one button field. A second one would bring back the two-button banners that
	// the design system forbids; the other answer of such a banner is a link in its text or a button on the page.
	n := 0
	rt := reflect.TypeOf(BannerData{})
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Type == reflect.TypeOf((*Btn)(nil)) || rt.Field(i).Type == reflect.TypeOf([]Btn(nil)) || rt.Field(i).Type == reflect.TypeOf([]*Btn(nil)) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("BannerData has %d action fields, the design system allows exactly one", n)
	}
}

// Why (#1091): the one action of the name offer ("Use device name") needs the answer's other half, Keep, somewhere
// else on the page: a row of a card may carry one button beside its value, so Keep sits next to the stored name it
// keeps. A row without an action renders as before, byte for byte (cards are compared by live.js as strings).
func TestKVRowActionFollowsTheValue(t *testing.T) {
	got := comp(t, KV([]KVRow{{Key: "Device name", Value: "lamp", Mono: true, Action: &Btn{Label: "Keep", Kind: "link", Attrs: templ.Attributes{"hx-post": "/k"}}}, {Key: "Host", Value: "h"}}))
	if want := `<dd class="mono">lamp <button type="button" class="btn btn-link" hx-post="/k">Keep</button></dd>`; !strings.Contains(got, want) {
		t.Errorf("the row lacks %q:\n%s", want, got)
	}
	if want := `<dd class="">h</dd>`; !strings.Contains(got, want) {
		t.Errorf("a row without an action changed: %s", got)
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

// Why (#1092): a page that wants a badge with its own class, a hint, a link or the large size used to write the
// markup by hand, because Badge(tone, label) takes neither. BadgeOf is the one place that markup lives, so it must
// keep what the pages' selectors and the live swaps rely on: bdg and t-<tone> first, the page's class after, a fixed
// order (live.js memoises a swapped fragment as a string), the id before the class.
func TestBadgeOfKeepsTheClassesAPageSelectsOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    BadgeData
		want string
	}{
		{"tone and label", BadgeData{Tone: ToneOK, Label: "Cached"}, `<span class="bdg t-ok">Cached</span>`},
		{"class and title", BadgeData{Tone: ToneWarn, Label: "3 off policy", Class: "dv-devs", Title: "Differs from the policy"}, `<span class="bdg t-warn dv-devs" title="Differs from the policy">3 off policy</span>`},
		{"large with an id", BadgeData{Tone: ToneNeutral, Label: "Succeeded", Large: true, ID: "txn-state"}, `<span id="txn-state" class="bdg t-neutral bdg-lg">Succeeded</span>`},
		{"large and class", BadgeData{Tone: ToneOK, Label: "Done", Large: true, Class: "bk-state"}, `<span class="bdg t-ok bdg-lg bk-state">Done</span>`},
		{"link", BadgeData{Tone: ToneAccent, Label: "Fetching…", Class: "fw-fetching", Href: "/firmware/prefetch/7"}, `<a class="bdg t-accent fw-fetching" href="/firmware/prefetch/7">Fetching…</a>`},
		{"unknown tone", BadgeData{Tone: "t-bogus", Label: "x"}, `<span class="bdg t-neutral">x</span>`},
	} {
		if got := comp(t, BadgeOf(tc.b)); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := comp(t, BadgeOf(BadgeData{Tone: ToneOK, Label: "x", Href: "javascript:alert(1)"})); strings.Contains(got, "javascript:") {
		t.Errorf("a badge link must not carry a script URL: %s", got)
	}
}

// Why (#1092): the Copy button was written by hand five times, once as a primary. One component keeps it small, hidden
// until copy.js shows it (so a page without JavaScript has no dead button), and with exactly one source: the value, or
// the element whose text is copied (a one-time password must not be in the markup twice).
func TestCopyButtonIsHiddenAndHasOneSource(t *testing.T) {
	if got, want := comp(t, CopyButton(CopyData{Text: "45"})), `<button type="button" class="btn btn-sm" data-copy-text="45" hidden>Copy</button>`; got != want {
		t.Errorf("text: got %s, want %s", got, want)
	}
	if got, want := comp(t, CopyButton(CopyData{Text: "abc", Aria: "Copy the SHA-256 of x.bin"})), `<button type="button" class="btn btn-sm" data-copy-text="abc" aria-label="Copy the SHA-256 of x.bin" hidden>Copy</button>`; got != want {
		t.Errorf("aria: got %s, want %s", got, want)
	}
	got := comp(t, CopyButton(CopyData{From: "#otp-value", Text: "secret"}))
	if want := `<button type="button" class="btn btn-sm" data-copy-from="#otp-value" hidden>Copy</button>`; got != want {
		t.Errorf("from: got %s, want %s", got, want)
	}
	if strings.Contains(got, "secret") {
		t.Errorf("a value copied from an element must not be in the markup: %s", got)
	}
}

// Why (#1092): the tall size and the danger link are Btn options now, and the typed attributes the tall buttons need
// (the sign-in link's id, a start button outside its form, the single sign-on post that skips validation) are fields,
// so those buttons stop being hand-written.
func TestBtnTallDangerLinkAndFormAttributes(t *testing.T) {
	if got := comp(t, Button(Btn{Label: "Sign in", Kind: "primary", Tall: true, Type: "submit"})); !strings.HasPrefix(got, `<button type="submit" class="btn btn-primary btn-tall">`) {
		t.Errorf("tall: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "Delete", Kind: "danger-link", Href: "/x"})); !strings.HasPrefix(got, `<a class="btn btn-danger-link" href="/x">`) {
		t.Errorf("danger link: %s", got)
	}
	got := comp(t, Button(Btn{Label: "Start", Kind: "primary", Tall: true, Type: "submit", ID: "start-go", Form: "start"}))
	if want := `<button type="submit" class="btn btn-primary btn-tall" id="start-go" form="start">Start</button>`; got != want {
		t.Errorf("form button: got %s, want %s", got, want)
	}
	got = comp(t, Button(Btn{Label: "SSO", Type: "submit", FormAction: "/auth/oidc", NoValidate: true}))
	if want := `<button type="submit" class="btn" formaction="/auth/oidc" formnovalidate>SSO</button>`; got != want {
		t.Errorf("formaction: got %s, want %s", got, want)
	}
	if got := comp(t, Button(Btn{Label: "SSO", Href: "/sso", ID: "sso", Tall: true})); got != `<a class="btn btn-tall" href="/sso" id="sso">SSO</a>` {
		t.Errorf("link with id: %s", got)
	}
	if got := comp(t, Button(Btn{Label: "Plain"})); strings.Contains(got, "form") || strings.Contains(got, "id=") || strings.Contains(got, "tall") {
		t.Errorf("a plain button carries none of the new attributes: %s", got)
	}
}

// withChildren renders c with one child component that writes html as it is (test markup, trusted).
func withChildren(t *testing.T, c templ.Component, html string) string {
	t.Helper()
	child := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, html)
		return err
	})
	var b strings.Builder
	if err := c.Render(templ.WithChildren(context.Background(), child), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Why (#1092): the device, backups, policy and shell pages wrote about a hundred buttons by hand because Button had no
// place for what they need: a class of the page's own (the selector or test that finds the button), the script and
// htmx attributes (data-close, hx-post, aria-pressed, download, title), and content after the label (a count, an icon).
// Without them a page copies the markup again, and the copy drifts from the component. The keys of Attrs come out
// sorted and after the typed attributes, so the string is the same every time (live.js memoises swapped fragments).
func TestButtonKeepsPageHooks(t *testing.T) {
	link := Button(Btn{Label: "Delete", Kind: "danger", Href: "/x", Dialog: "dlg", Class: "bk-del", Attrs: templ.Attributes{"title": "Hint", "data-dlg-verb": "Delete backup"}})
	if got, want := comp(t, link), `<a class="btn btn-danger bk-del" href="/x" data-dialog="dlg" data-dlg-verb="Delete backup" title="Hint">Delete</a>`; got != want {
		t.Errorf("link: got %s, want %s", got, want)
	}
	btn := Button(Btn{Label: "Back up selected", Type: "submit", Form: "picker", Disabled: true, Class: "x", Attrs: templ.Attributes{"data-needs-selection": true, "aria-describedby": "why", "hx-disable": false}})
	if got, want := withChildren(t, btn, "<span data-select-suffix></span>"), `<button type="submit" class="btn x" form="picker" disabled aria-describedby="why" data-needs-selection>Back up selected<span data-select-suffix></span></button>`; got != want {
		t.Errorf("button: got %s, want %s", got, want)
	}
	if got, want := withChildren(t, Button(Btn{Type: "submit", Name: "kept", Value: "1"}), `<span class="bk-pin"></span>Keep`), `<button type="submit" class="btn" name="kept" value="1"><span class="bk-pin"></span>Keep</button>`; got != want {
		t.Errorf("children without a label: got %s, want %s", got, want)
	}
	// The blocked variant carries no action (#1072): the class stays (the look), the page's attributes do not, or a
	// blocked button would act again through hx-post or data-dialog.
	got := comp(t, Button(Btn{Label: "Restore", DisabledReason: "No backup yet", ReasonID: "why", Class: "bk-restore", Href: "/r", Attrs: templ.Attributes{"hx-post": "/r", "data-close": true, "data-fill": "verb"}}))
	if !strings.Contains(got, `class="btn bk-restore"`) || strings.Contains(got, "hx-post") || strings.Contains(got, "data-close") || strings.Contains(got, "data-fill") || strings.Contains(got, "/r") {
		t.Errorf("blocked: keeps the class, drops the page's attributes and the address: %s", got)
	}
}

// Why (#1092): Attrs is the door for what no typed field covers, so it must not become a second way to write what the
// fields write (a duplicate class, id or href would be two values in one tag, and the browser takes the first), nor a
// way around the CSP (style, on* are inline code). Those keys are dropped, whatever their case, and the rest stays.
func TestPageAttrsRefuseWhatTheComponentOwns(t *testing.T) {
	bad := templ.Attributes{"class": "evil", "style": "color:red", "onclick": "x()", "ONMOUSEOVER": "x()", "id": "evil", "href": "/evil", "type": "reset", "data-dialog": "evil", "formaction": "/evil", "aria-label": "evil", "hx-get": "/ok"}
	for name, c := range map[string]templ.Component{
		"link":   Button(Btn{Label: "x", Href: "/x", Attrs: bad}),
		"button": Button(Btn{Label: "x", Type: "submit", Attrs: bad}),
		"badge":  BadgeOf(BadgeData{Tone: ToneOK, Label: "x", Attrs: bad}),
		"card":   CardOf(CardData{Title: "x", Attrs: bad}),
	} {
		got := comp(t, c)
		for _, banned := range []string{"evil", "style=", "onclick", "onmouseover", "ONMOUSEOVER"} {
			if strings.Contains(got, banned) {
				t.Errorf("%s: Attrs wrote %q: %s", name, banned, got)
			}
		}
		if !strings.Contains(got, `hx-get="/ok"`) {
			t.Errorf("%s: an allowed key is dropped too: %s", name, got)
		}
	}
}

// Why (#1092): the system settings badge that the push script finds (data-push-this) and hides until it has an answer
// was written by hand because BadgeOf had no way to carry those attributes.
func TestBadgeOfAttrs(t *testing.T) {
	got := comp(t, BadgeOf(BadgeData{Tone: ToneAccent, Label: "This browser", Attrs: templ.Attributes{"hidden": true, "data-push-this": true}}))
	if want := `<span class="bdg t-accent" data-push-this hidden>This browser</span>`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Why (#1092): the "⋯" button of a row menu was written by hand in three places because only RowMenu could draw it,
// and RowMenu draws a list of links. names_test pins the attribute order (class, aria-label, title); the title only
// appears when it is set, so RowMenu's own output does not change.
func TestMenuButton(t *testing.T) {
	if got, want := comp(t, MenuButton("Actions for Lamp", "")), `<summary class="btn btn-ghost btn-icon icon" aria-label="Actions for Lamp">⋯</summary>`; got != want {
		t.Errorf("no title: got %s, want %s", got, want)
	}
	if got, want := comp(t, MenuButton("Actions for Lamp", "Actions for Lamp")), `<summary class="btn btn-ghost btn-icon icon" aria-label="Actions for Lamp" title="Actions for Lamp">⋯</summary>`; got != want {
		t.Errorf("title: got %s, want %s", got, want)
	}
	if got := comp(t, RowMenu("Actions", []MenuItem{{Label: "a", Href: "/x"}}, false)); !strings.Contains(got, `<summary class="btn btn-ghost btn-icon icon" aria-label="Actions">⋯</summary>`) {
		t.Errorf("RowMenu draws the same button: %s", got)
	}
}

// Why (#1092): the 37 hand-written cards needed what Card lacked (a phone stack place, a class, an id, another
// element, an h2 that a label names, attributes of a live swap). CardOf is the one place that writes the card, so it
// must keep the order the page CSS and the class-string tests rely on (rank, ucard, the page's class), a fixed set of
// elements (a tag from data would be markup), and the h2 id that aria-labelledby points at. Card(title, sub) is the
// short form and renders what it always did: the golden string is the part a change of CardOf can break; the
// cases with new fields are new API and only fail on a revert when they compare a zero value.
func TestCardOf(t *testing.T) {
	if got, want := comp(t, Card("T", "S")), `<section class="ucard"><h2>T</h2><p class="ucard-sub">S</p></section>`; got != want {
		t.Errorf("Card: got %s, want %s", got, want)
	}
	if got, want := comp(t, Card("", "")), `<section class="ucard"></section>`; got != want {
		t.Errorf("Card without text: got %s, want %s", got, want)
	}
	if got, want := comp(t, CardOf(CardData{Title: "Tasmota 15", Rank: 2, Class: "fw-sel", ID: "release", LabelledBy: "fw-sel-h"})), `<section class="r2 ucard fw-sel" id="release" aria-labelledby="fw-sel-h"><h2 id="fw-sel-h">Tasmota 15</h2></section>`; got != want {
		t.Errorf("rank, class, id and h2 id: got %s, want %s", got, want)
	}
	if got, want := withChildren(t, CardOf(CardData{Class: "confirm-card", LabelledBy: "confirm-h"}), `<h2 id="confirm-h">Delete</h2>`), `<section class="ucard confirm-card" aria-labelledby="confirm-h"><h2 id="confirm-h">Delete</h2></section>`; got != want {
		t.Errorf("heading in the children: got %s, want %s", got, want)
	}
	for tag, want := range map[string]string{"": "section", "section": "section", "article": "article", "aside": "aside", "form": "form", "div": "div", "script": "section", "SECTION": "section", `x" onload="y`: "section"} {
		if got := comp(t, CardOf(CardData{Tag: tag, Aria: "A"})); !strings.HasPrefix(got, "<"+want+" ") || !strings.HasSuffix(got, "</"+want+">") {
			t.Errorf("Tag %q: got %s, want a %s", tag, got, want)
		}
	}
	form := comp(t, CardOf(CardData{Tag: "form", Class: "useradd", Attrs: templ.Attributes{"method": "post", "action": templ.SafeURL("/tf/users")}}))
	if want := `<form class="ucard useradd" action="/tf/users" method="post"></form>`; form != want {
		t.Errorf("form card: got %s, want %s", form, want)
	}
	if got, want := comp(t, CardOf(CardData{Tag: "div", ID: "prefetch", Class: "fw-prefetch", Attrs: templ.Attributes{"hx-swap": "outerHTML", "sse-swap": "prefetch"}})), `<div class="ucard fw-prefetch" id="prefetch" hx-swap="outerHTML" sse-swap="prefetch"></div>`; got != want {
		t.Errorf("live card: got %s, want %s", got, want)
	}
	if got, want := comp(t, CardOf(CardData{Tag: "article", Rank: 1, Class: "rel-notes", Aria: "Release notes"})), `<article class="r1 ucard rel-notes" aria-label="Release notes"></article>`; got != want {
		t.Errorf("article: got %s, want %s", got, want)
	}
}

// Why (#1093): a confirmation has one title. The page header is it; a card heading that only says it again made every
// confirm page read twice (A6-017). A heading that adds something stays, and the card is named either way.
func TestConfirmPageDrawsNoHeadingThatRepeatsItsTitle(t *testing.T) {
	base := ConfirmData{Title: "Delete this rule?", Text: "t", Action: "/x", CSRF: "c", Verb: "Delete rule", Danger: true, CancelHref: "/y", CancelLabel: "Cancel"}
	for name, heading := range map[string]string{"none": "", "same": "Delete this rule?"} {
		c := base
		c.Heading = heading
		out := comp(t, ConfirmPage(c))
		if strings.Contains(out, "<h2") || strings.Contains(out, "aria-labelledby=\"confirm-h\"") {
			t.Errorf("%s: the card repeats the title: %s", name, out)
		}
		if !strings.Contains(out, `aria-label="Delete this rule?"`) || strings.Count(out, "Delete this rule?") != 2 {
			t.Errorf("%s: the page header and the card's name are the title, once each: %s", name, out)
		}
	}
	c := base
	c.Heading = "Send 3 devices?"
	if out := comp(t, ConfirmPage(c)); !strings.Contains(out, `<h2 id="confirm-h">Send 3 devices?</h2>`) || !strings.Contains(out, `aria-labelledby="confirm-h"`) {
		t.Errorf("a heading of its own is kept and names the card: %s", out)
	}
}

// Why (#1093): a quick command that interrupts the device is never one click. With Href it is a link to the
// confirmation (the dialog when JavaScript is on), not a submit button of the quick form; blocked, it is the
// blocked button and carries no action.
func TestConsoleQuickCommandThatNeedsAConfirmationIsALink(t *testing.T) {
	out := comp(t, Console(ConsoleData{Title: "c", Action: "/x", CSRF: "c", Quick: []QuickCmd{
		{Command: "Status 5"},
		{Command: "Restart 1", Href: "/devices/1/restart", Dialog: "dlg-restart"},
	}}))
	if !strings.Contains(out, `name="command" value="Status 5">Status 5</button>`) {
		t.Errorf("a plain quick command still sends: %s", out)
	}
	if strings.Contains(out, `value="Restart 1"`) || !strings.Contains(out, `href="/devices/1/restart"`) || !strings.Contains(out, `data-dialog="dlg-restart"`) {
		t.Errorf("Restart 1 must lead to the confirmation, not send: %s", out)
	}
	out = comp(t, Console(ConsoleData{Title: "c", Action: "/x", CSRF: "c", Quick: []QuickCmd{{Command: "Restart 1", Why: "It is offline.", WhyID: "devhd-why"}}}))
	if strings.Contains(out, `value="Restart 1"`) || !strings.Contains(out, `aria-disabled="true"`) || !strings.Contains(out, `aria-describedby="devhd-why"`) {
		t.Errorf("blocked Restart 1 sends nothing and says why: %s", out)
	}
}

// tight removes the whitespace templ puts between elements on separate lines (it draws nothing in a flex box).
func tight(s string) string {
	for _, p := range [][2]string{{"> <", "><"}, {"> </", "></"}} {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	return s
}

// Why (#1095): eight CSS families drew the same two things (a toggleable filter and a read-only label), each a
// little different (28 or 34 px, pill or 7 px, inverted or tinted), and a page had to pick the right copy. One
// component draws each, in the three shapes a filter has: a label around an input (works without JavaScript), a
// link (a filter that changes the address) and a button (a filter of a draft that has no page to link to).
func TestChipForms(t *testing.T) {
	box := tight(comp(t, Chip(ChipData{Label: "Mon", Long: "Monday", Name: "day", Value: "mon", Checked: true, Form: "f"})))
	for _, want := range []string{
		`<label class="chip">`, `<input type="checkbox" name="day" value="mon" form="f" checked>`,
		`<span class="chip-l">Mon<span class="sr-only"> Monday</span></span>`,
	} {
		if !strings.Contains(box, want) {
			t.Errorf("checkbox chip: want %s in %s", want, box)
		}
	}
	if got := comp(t, Chip(ChipData{Label: "Now", Name: "at", Value: "now", Radio: true})); !strings.Contains(got, `type="radio"`) || strings.Contains(got, "checked") {
		t.Errorf("radio chip: %s", got)
	}
	link := tight(comp(t, Chip(ChipData{Label: "All", Count: "12", Href: "/devices?chip=", Current: true, Title: "everything"})))
	for _, want := range []string{`<a class="chip" href="/devices?chip="`, `aria-current="page"`, `title="everything"`, `<span class="chip-n">12</span>`} {
		if !strings.Contains(link, want) {
			t.Errorf("link chip: want %s in %s", want, link)
		}
	}
	if got := comp(t, Chip(ChipData{Label: "Offline", Href: "/x"})); strings.Contains(got, "aria-current") || strings.Contains(got, "chip-n") {
		t.Errorf("a chip that is not current, and has no count, says neither: %s", got)
	}
	btn := comp(t, Chip(ChipData{Label: "Deviate", Count: "3", Current: true, Attrs: templ.Attributes{"hx-post": "/p"}}))
	for _, want := range []string{`<button type="button" class="chip"`, `aria-current="true"`, `hx-post="/p"`} {
		if !strings.Contains(btn, want) {
			t.Errorf("button chip: want %s in %s", want, btn)
		}
	}
}

// Why (#1095): a tag with a count drops it while it is ticked and shows a cross instead (policy tags and
// exceptions), the quick picks of the update start have a second line, and a script (policy.js, updatestart.js)
// finds the input by its attributes: the attributes of a label chip belong on its input, where the control is.
func TestChipPartsAndInputAttrs(t *testing.T) {
	got := tight(comp(t, Chip(ChipData{Label: "kitchen", Count: "4", Remove: true, Name: "tag", Value: "kitchen", Attrs: templ.Attributes{"data-n": "4"}})))
	for _, want := range []string{`<input type="checkbox" name="tag" value="kitchen" data-n="4">`, `<span class="chip-n">4</span>`, `<span class="chip-x" aria-hidden="true">×</span>`} {
		if !strings.Contains(got, want) {
			t.Errorf("tag chip: want %s in %s", want, got)
		}
	}
	if two := tight(comp(t, Chip(ChipData{Label: "Tonight", Sub: "Wed 25 Nov", Name: "at", Value: "x", Radio: true}))); !strings.Contains(two, `<span class="chip-l">Tonight</span><span class="chip-sub">Wed 25 Nov</span>`) {
		t.Errorf("two-line chip: %s", two)
	}
}

// Why (#1095): the chip is one class set. The old inverted look and the old family names must not come back
// through a field (a page's Attrs must not carry a class, id, style or script into the chip).
func TestChipAttrsRefuseWhatTheComponentOwns(t *testing.T) {
	bad := templ.Attributes{"class": "evil", "style": "color:red", "onclick": "x()", "id": "evil", "title": "evil", "aria-current": "evil", "hx-get": "/ok"}
	for name, c := range map[string]templ.Component{
		"input":  Chip(ChipData{Label: "x", Name: "n", Value: "v", Attrs: bad}),
		"link":   Chip(ChipData{Label: "x", Href: "/x", Attrs: bad}),
		"button": Chip(ChipData{Label: "x", Attrs: bad}),
		"tag":    TagOf(TagData{Label: "x", Href: "/x", Attrs: bad}),
	} {
		got := comp(t, c)
		for _, banned := range []string{"evil", "style=", "onclick"} {
			if strings.Contains(got, banned) {
				t.Errorf("%s: Attrs wrote %q: %s", name, banned, got)
			}
		}
		if !strings.Contains(got, `hx-get="/ok"`) {
			t.Errorf("%s: an allowed key is dropped too: %s", name, got)
		}
	}
}

// Why (#1095): a tag is a read-only outlined label for a kind or a source; it is never a state (that is a badge),
// so it has no tone and no state class. A link tag (a device in a list) and a mono tag (a device name) are the
// same tag with a link and the mono font.
func TestTag(t *testing.T) {
	if got, want := comp(t, Tag("Proxy")), `<span class="tag">Proxy</span>`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got, want := comp(t, TagOf(TagData{Label: "no OTA", Title: "why"})), `<span class="tag" title="why">no OTA</span>`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	got := comp(t, TagOf(TagData{Label: "kitchen", Href: "/devices/1", Mono: true, Title: "t"}))
	if want := `<a class="tag mono" href="/devices/1" title="t">kitchen</a>`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := comp(t, TagOf(TagData{Label: "MQTT", Attrs: templ.Attributes{"data-via-mqtt": true}})); got != `<span class="tag" data-via-mqtt>MQTT</span>` {
		t.Errorf("attrs: %s", got)
	}
}

// Why (#1096): there are two switches and each page used to draw its own, with its own sizes and off state. The
// setting switch is a checkbox that posts "true" (the settings form's contract) unless a page names another value,
// and the bare form (no Label) draws only the switch, which a <label for> names. A row's power switch is named by
// the ids of its row (aria-labelledby, so the fragment holds no name and no id), or by a label of its own.
func TestSwitchForms(t *testing.T) {
	got := comp(t, Toggle(ToggleData{Name: "v.X", ID: "f-X", On: true, Attrs: templ.Attributes{"aria-describedby": "help-X"}}))
	if want := `<label class="uswitch"><input type="checkbox" role="switch" name="v.X" value="true" id="f-X" checked aria-describedby="help-X"><span class="sw" aria-hidden="true"></span></label>`; got != want {
		t.Errorf("bare toggle:\n got %s\nwant %s", got, want)
	}
	got = comp(t, Toggle(ToggleData{Name: "state", Value: "enabled", Label: "Enabled", Text: "why"}))
	for _, want := range []string{`<label class="utoggle">`, `name="state" value="enabled"`, `<b>Enabled</b>`, `<span>why</span>`} {
		if !strings.Contains(got, want) {
			t.Errorf("toggle card lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "checked") || strings.Contains(got, " id=") {
		t.Errorf("an off toggle without an id: %s", got)
	}

	got = comp(t, PowerSwitch(PowerSwitchData{LabelledBy: "dvn-1 rly-1", On: true, Attrs: templ.Attributes{"hx-post": "/p", "title": "t"}}))
	if want := `<button type="button" class="pwr" role="switch" aria-checked="true" aria-labelledby="dvn-1 rly-1" hx-post="/p" title="t"><span class="sw" aria-hidden="true"></span></button>`; got != want {
		t.Errorf("row switch:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "aria-label=") || strings.Contains(got, " id=") {
		t.Errorf("a switch named by its row has no label and no id of its own: %s", got)
	}
	got = comp(t, PowerSwitch(PowerSwitchData{Label: "Relay 1 is Off", Submit: true, Class: "devrelay-sw", Disabled: true}))
	if want := `<button type="submit" class="pwr devrelay-sw" role="switch" aria-checked="false" aria-label="Relay 1 is Off" disabled><span class="sw" aria-hidden="true"></span></button>`; got != want {
		t.Errorf("relay switch:\n got %s\nwant %s", got, want)
	}
	a, b := comp(t, PowerSwitch(PowerSwitchData{LabelledBy: "x", Attrs: templ.Attributes{"hx-vals": "1", "hx-post": "/p", "title": "t", "hx-swap": "s"}})), comp(t, PowerSwitch(PowerSwitchData{LabelledBy: "x", Attrs: templ.Attributes{"hx-swap": "s", "title": "t", "hx-post": "/p", "hx-vals": "1"}}))
	if a != b {
		t.Errorf("the markup depends on the order of Attrs (live.js memoises the swapped fragment as a string):\n%s\n%s", a, b)
	}
}

// Why (#1096): a page may not make a switch say two things (a second role or aria-checked), run script (on*) or
// draw outside the CSP (style); the keys the component writes itself are its own. A button keeps its title (the
// reason a switch is off), an input has no use for one.
func TestSwitchAttrsRefuseWhatTheComponentOwns(t *testing.T) {
	bad := templ.Attributes{"class": "evil", "style": "color:red", "onclick": "x()", "id": "evil", "role": "evil", "aria-checked": "evil", "checked": "evil", "aria-label": "evil", "aria-labelledby": "evil", "type": "evil", "hx-get": "/ok"}
	for name, c := range map[string]templ.Component{
		"toggle": Toggle(ToggleData{Name: "n", Attrs: bad}),
		"card":   Toggle(ToggleData{Name: "n", Label: "x", Attrs: bad}),
		"power":  PowerSwitch(PowerSwitchData{Label: "x", Attrs: bad}),
	} {
		got := comp(t, c)
		for _, banned := range []string{"evil", "style=", "onclick"} {
			if strings.Contains(got, banned) {
				t.Errorf("%s: Attrs wrote %q: %s", name, banned, got)
			}
		}
		if !strings.Contains(got, `hx-get="/ok"`) {
			t.Errorf("%s: an allowed key is dropped too: %s", name, got)
		}
	}
	if got := comp(t, Toggle(ToggleData{Name: "n", Attrs: templ.Attributes{"title": "t"}})); strings.Contains(got, "title") {
		t.Errorf("a title on the input: %s", got)
	}
}

// Why (#1097): the device list's script reads data-state to switch a row's actions, and the phone card's CSS reads
// the t-<tone> class of .dv-st; the badge keeps both, in the class order the swapped fragments have always had
// (live.js memoises them as strings), and the dot is decoration for a screen reader.
func TestStatusBadgeKeepsTheHooksOfTheListCells(t *testing.T) {
	got := comp(t, StatusBadge(StatusBadgeData{State: "offline", Tone: ToneDanger, Label: "Offline", Title: "No answer"}))
	want := `<span class="bdg dv-st t-danger" data-state="offline" title="No answer"><span class="dv-dot" aria-hidden="true"></span>Offline</span>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if bare := comp(t, StatusBadge(StatusBadgeData{State: "online", Tone: ToneOK, Label: "Online"})); strings.Contains(bare, "title=") {
		t.Errorf("no hint, no title attribute: %s", bare)
	}
}

// Why (#1097): a word value is set smaller than a number, a tile without a tone keeps the text colour (a neutral
// tone would grey the Power reading), and the tile's children (Make compliant) and the note beside the label render.
func TestInfoTileSetsWordsAndNumbersApart(t *testing.T) {
	word := comp(t, InfoTile(InfoTileData{Key: "reach", Label: "Reachability", Value: "MQTT · HTTP", Word: true, Tone: ToneOK, Hint: "Connected"}))
	if !strings.Contains(word, `<div class="itile-v itile-word t-ok">MQTT · HTTP</div>`) || !strings.Contains(word, `data-tile="reach"`) {
		t.Errorf("word tile: %s", word)
	}
	num := comp(t, InfoTile(InfoTileData{Key: "power", Label: "Power", Value: "12 W", Hint: "now"}))
	if !strings.Contains(num, `<div class="itile-v">12 W</div>`) {
		t.Errorf("a number without a tone has the one class: %s", num)
	}
	note := templ.ComponentFunc(func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "<i>note</i>"); return err })
	kid := templ.ComponentFunc(func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "<b>kid</b>"); return err })
	full := compKids(t, InfoTile(InfoTileData{Key: "k", Label: "L", Value: "1", Note: note}), kid)
	if !strings.Contains(full, "L <i>note</i>") && !strings.Contains(full, "L<i>note</i>") || !strings.Contains(full, "<b>kid</b></div>") {
		t.Errorf("note beside the label, children after the hint: %s", full)
	}
}

// Why (#1097): the MQTT-down reason sits inside its badge, after the word, in both shapes of the badge.
func TestBadgeOfRendersChildrenAfterTheLabel(t *testing.T) {
	kid := templ.ComponentFunc(func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "<i>why</i>"); return err })
	for _, b := range []BadgeData{{Tone: ToneWarn, Label: "MQTT down", Class: "dv-mdown"}, {Tone: ToneWarn, Label: "MQTT down", Href: "/x"}} {
		if got := compKids(t, BadgeOf(b), kid); !strings.Contains(got, "MQTT down<i>why</i></") {
			t.Errorf("children do not follow the label: %s", got)
		}
	}
}

// Why (#1104): a sentence that holds an instant (a dialog body, a banner text, a row, a release card, a confirm page) must
// show it as one <time> element with its datetime and its format key, because a date the server writes into the text is
// in the server's zone and order, and time.js can only repaint an element. Every carrier renders the same
// StampedText; the hostile text around the instant stays escaped, and a sentence without an instant has no element.
func TestStampedSentencesRenderOneTimeElement(t *testing.T) {
	at := time.Date(2026, 11, 20, 14, 17, 0, 0, time.UTC)
	s := Stamped{Lead: "Lead " + hostile + " ", At: at, Stamp: StampDate, Tail: " tail " + hostile}
	for name, c := range map[string]templ.Component{
		"dialog":  Dialog(DialogData{ID: "d", Title: "T", BodyStamp: s, Verb: "Go"}),
		"banner":  Banner(BannerData{Tone: ToneAccent, TextStamp: s}),
		"confirm": ConfirmPage(ConfirmData{Title: "T", TextStamp: s, Verb: "Go"}),
		"row":     KV([]KVRow{{Key: "k", At: at, Stamp: StampDate, Lead: s.Lead, Tail: s.Tail}}),
		"option":  RadioCards("l", "n", []Option{{Value: "v", Label: "L", TextStamp: s}}),
		"direct":  StampedText(s),
	} {
		got := comp(t, c)
		if n := strings.Count(got, "<time"); n != 1 {
			t.Errorf("%s: %d time elements, want 1: %s", name, n, got)
		}
		if !strings.Contains(got, `<time data-ts="date" datetime="2026-11-20T14:17:00Z">`) || !strings.Contains(got, ">20 Nov 2026</time>") {
			t.Errorf("%s: the instant is not a date stamp: %s", name, got)
		}
		if strings.Contains(got, "<script>") || strings.Contains(got, `" onmouseover="x`) {
			t.Errorf("%s: the hostile text around the instant is not escaped: %s", name, got)
		}
	}
	if got := comp(t, Dialog(DialogData{ID: "d", Title: "T", Body: "Plain body.", Verb: "Go"})); strings.Contains(got, "<time") || !strings.Contains(got, "Plain body.") {
		t.Errorf("a dialog without an instant keeps its Body: %s", got)
	}
	if got := comp(t, StampedText(Stamped{Lead: "a", Tail: "b"})); got != "ab" {
		t.Errorf("a sentence without an instant is its lead and tail, got %q", got)
	}
}

// Why (#1104): Plain is the one way for Go code to put an instant into an attribute or a button label; it must be the
// text the element would carry, so a page without JavaScript and an aria-label say the same.
func TestStampedPlainIsTheElementsFallbackText(t *testing.T) {
	at := time.Date(2026, 11, 20, 14, 17, 0, 0, time.UTC)
	s := Stamped{Lead: "Select the backup of ", At: at}
	if got, want := s.Plain(), "Select the backup of "+StampText(at, StampDateTime); got != want {
		t.Errorf("Plain = %q, want %q", got, want)
	}
	if got := (Stamped{Lead: "a ", At: at, Text: "Today 14:17", Tail: " b"}).Plain(); got != "a Today 14:17 b" {
		t.Errorf("a relative fallback text is kept: %q", got)
	}
}

// Why: banners and toasts take their colour from the one tone class set (.t-ok ... .t-accent) that badges and dots
// use. A banner that wrote a class of its own from the tone name left a tone nobody styled with no colour at all, and a name
// outside the five (a typo, the old "bad" or "info") drew nothing and said nothing; Tone.class() draws it neutral.
func TestBannerAndToastTakeTheToneClassOfTheTone(t *testing.T) {
	for tone, class := range map[Tone]string{
		ToneOK: "t-ok", ToneWarn: "t-warn", ToneDanger: "t-danger", ToneNeutral: "t-neutral", ToneAccent: "t-accent",
		Tone("bad"): "t-neutral", Tone("info"): "t-neutral", Tone(""): "t-neutral",
	} {
		b := comp(t, Banner(BannerData{Tone: tone, Text: "x"}))
		if !strings.Contains(b, `<div class="banner `+class+`"`) {
			t.Errorf("banner of tone %q: want class %s in %s", tone, class, b)
		}
		ts := comp(t, ToastStatic(ToastData{Tone: tone, Text: "x"}))
		if !strings.Contains(ts, `<div class="toast `+class+` has-x"`) {
			t.Errorf("toast of tone %q: want class %s in %s", tone, class, ts)
		}
	}
}
