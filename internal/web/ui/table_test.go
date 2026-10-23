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
		`<th class="sel"><input type="checkbox" data-select-all hidden aria-label="Select all">`,
		`<th>Name</th>`, `<th><span class="sr-only">Actions</span></th>`, `<tbody><tr><td>r</td></tr></tbody>`,
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
	out := renderC(t, DeviceCards(DeviceListData{Select: true, Cards: []DeviceCardData{{
		ID: "7", Name: `<img src=x onerror=alert(1)>`, Href: "/tf/devices/7", Meta: []string{"10.0.0.7", "esp32", "14.3.0"}, Select: true,
	}}}), context.Background())
	if strings.Contains(out, "<img") || !strings.Contains(out, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("name not escaped: %s", out)
	}
	for _, want := range []string{`data-id="7"`, `name="id" value="7"`, `sse-swap="health-7"`, `sse-swap="power-7"`, `sse-swap="uptime-7"`, `sse-swap="wifi-7"`,
		`href="/tf/devices/7"`, `dcard-meta-part">10.0.0.7</span> · `, `dcard-meta-part">esp32</span> · `, `dcard-meta-part">14.3.0</span>`, "Select all 1", "data-select-all-row hidden"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "style=") {
		t.Error("no inline style (CSP)")
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
