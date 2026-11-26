package ui

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderC(t *testing.T, c templ.Component, ctx context.Context) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Why: the server finds a table by its data-table key and maps the header cells to the column
// registry (tableprefs.go), so the frame must carry the key, one header cell per column and a
// leading checkbox header only when the table selects rows; the row-actions header is for screen
// readers only. The select-all box needs select.js, so it starts hidden.
func TestTableFrame(t *testing.T) {
	rows := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<tr><td>r</td></tr>")
		return err
	})
	out := renderC(t, Table(TableData{Key: "devices", Class: "devtable dense", Select: "Select all", SortBase: "/d?q=a&b", Sort: "name", Dir: "asc",
		Cols: []TableCol{{Label: "Name", Col: "c-name"}, {Label: "Actions", SrOnly: true}}}), templ.WithChildren(context.Background(), rows))
	for _, want := range []string{
		`<table class="devtable dense" data-table="devices" data-sortbase="/d?q=a&amp;b" data-sort="name" data-dir="asc">`,
		`<col class="c-sel">`, `<col class="c-name"><col></colgroup>`,
		`<th scope="col" class="sel"><input type="checkbox" data-select-all hidden aria-label="Select all">`,
		`<th scope="col">Name</th>`, `<th scope="col"><span class="sr-only">Actions</span></th>`, `<tbody><tr><td>r</td></tr></tbody>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	plain := renderC(t, Table(TableData{Key: "audit", Cols: []TableCol{{Label: "When"}}}), context.Background())
	if strings.Contains(plain, "checkbox") || strings.Contains(plain, "data-sortbase") || strings.Contains(plain, "class=") {
		t.Errorf("a table without selection, sort or class renders none of them: %s", plain)
	}
}

// Why: device names come from users and devices; a card must print them as text. The card's
// selection box shares the device id with its table row (select.js), and each live part is
// swapped by the event the stream sends for that device.
func TestDeviceCardEscapesAndWiresLiveParts(t *testing.T) {
	out := renderC(t, DeviceCards(DeviceListData{Cards: []DeviceCardData{{
		ID: "7", Name: `<img src=x onerror=alert(1)>`, Href: "/tf/devices/7", Meta: []string{"10.0.0.7", "esp32", "14.3.0"}, Select: true,
	}}}), context.Background())
	if strings.Contains(out, "<img") || !strings.Contains(out, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("name not escaped: %s", out)
	}
	for _, want := range []string{`data-id="7"`, `name="id" value="7"`, `sse-swap="health-7"`, `sse-swap="power-7"`, `sse-swap="uptime-7"`, `sse-swap="wifi-7"`,
		`href="/tf/devices/7"`, `dcard-meta-part">10.0.0.7</span> · `, `dcard-meta-part">esp32</span> · `, `dcard-meta-part">14.3.0</span>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "style=") {
		t.Error("no inline style (CSP)")
	}
	// Why (#1133): the phone list is the cards and nothing above them; "Select all N" is a button of the bulk bar.
	if strings.Contains(out, "Select all") || strings.Contains(out, "data-select-all") {
		t.Errorf("no Select all row above the cards: %s", out)
	}
	ro := renderC(t, DeviceCard(DeviceCardData{ID: "1", Name: "a", Href: "/d/1"}), context.Background())
	if strings.Contains(ro, "checkbox") {
		t.Errorf("a card without selection has no box: %s", ro)
	}
	// the last line reads "Up 19d 1h · -60 dBm" (prototype): templ drops the leading space of
	// literal text, which glued the separator to the uptime ("1h· -60 dBm")
	if !strings.Contains(ro, `<span class="dl-sep"> · </span>`) {
		t.Errorf("the uptime and the signal are not separated by \" · \": %s", ro)
	}
}

// Why (#1094): a table without a Key is no registry table. Writing data-table="" would put it under
// the table[data-table] hooks of live.js and tables.js (resizing, pinned columns, the Columns chooser) that
// have no entry for it. The policy rule list is such a table.
func TestTableWithoutKeyIsNoRegistryTable(t *testing.T) {
	out := renderC(t, Table(TableData{Cols: []TableCol{{Label: "Rule"}}}), context.Background())
	if strings.Contains(out, "data-table") {
		t.Errorf("a keyless table carries data-table: %s", out)
	}
}

// Why (#1094): the caption names a table for a screen reader, and it must be the first child of the
// <table> (before the <colgroup>). It is read aloud, so by default it is not drawn (the page has a heading);
// a table without a heading draws it. The header cells say they head a column; the body attributes
// (the policy list's drag and order hooks) go on the <tbody>, escaped.
func TestTableCaptionScopeAndBodyAttrs(t *testing.T) {
	rows := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<tr><td>r</td></tr>")
		return err
	})
	out := renderC(t, Table(TableData{Caption: "Audit events", BodyAttrs: templ.Attributes{"data-rule-list": true, "data-order-form": "rule-order"},
		Cols: []TableCol{{Label: "When"}}}), templ.WithChildren(context.Background(), rows))
	for _, want := range []string{`<table><caption class="sr-only">Audit events</caption> <colgroup>`, `<th scope="col">When</th>`, `<tbody data-order-form="rule-order" data-rule-list>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	shown := renderC(t, Table(TableData{Caption: "Pins", CaptionVisible: true, Cols: []TableCol{{Label: "Pin"}}}), context.Background())
	if !strings.Contains(shown, `<caption>Pins</caption>`) {
		t.Errorf("a visible caption is not drawn: %s", shown)
	}
	none := renderC(t, Table(TableData{Cols: []TableCol{{Label: "Pin"}}}), context.Background())
	if strings.Contains(none, "<caption") {
		t.Errorf("a table with no caption text draws an empty caption: %s", none)
	}
}

// Why (#1094): the first cell of a row that names the row is a row header, so a screen reader says
// "Rule: Firmware" when it reads the other cells.
func TestRowHeadIsARowHeader(t *testing.T) {
	name := templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<a>x</a>")
		return err
	})
	out := renderC(t, RowHead(RowHeadData{Class: "p1", Label: "Rule"}), templ.WithChildren(context.Background(), name))
	if out != `<th scope="row" class="p1" data-label="Rule"><a>x</a></th>` {
		t.Errorf("row head: %s", out)
	}
}

// Why (#1094): the pager is the one paging control of every paged table; it names its position and
// the page sizes in text and keeps a page that cannot be reached a disabled button.
func TestPager(t *testing.T) {
	out := renderC(t, Pager(PagerData{Page: 2, Pages: 5, Total: 120, Noun: "events", Prev: "/a?page=1", Next: "/a?page=3",
		Sizes: []PagerSize{{N: 25, Href: "/a?page=2", Current: true}, {N: 50, Href: "/a?per=50"}}}), context.Background())
	for _, want := range []string{`<nav class="pager" aria-label="Pages">`, `rel="prev"`, `rel="next"`, "Page 2 of 5, 120 events", `aria-current="page"`, "Per page"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	empty := renderC(t, Pager(PagerData{Page: 1, Pages: 1, Noun: "events"}), context.Background())
	if !strings.Contains(empty, "No events") || strings.Count(empty, "disabled") != 2 {
		t.Errorf("an empty list says so and has both arrows disabled: %s", empty)
	}
}
