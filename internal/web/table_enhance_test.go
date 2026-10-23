package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: a table with no row has nothing to sort, select or hide; offering a select-all box,
// sort links and a Columns chooser over "No saved versions yet" read as a broken page.
func TestEmptyTableOffersNoControls(t *testing.T) {
	sortFixture(t)
	in := `<table class="dense" data-table="t_versions"><thead><tr><th><input type="checkbox" data-select-all aria-label="all"></th><th>Version</th><th>When</th><th>Who</th><th>Note</th><th>Actions</th></tr></thead><tbody><tr><td colspan="6" class="empty">No saved versions yet.</td></tr></tbody></table>`
	sorts := map[string]sortLink{"t_versions": {Base: "/tf/x", Key: "version", Dir: "desc"}}
	out := string(applyTablePrefsSorted([]byte(in), nil, sorts))
	for _, bad := range []string{"data-select-all", "sortlink", "colmenu"} {
		if strings.Contains(out, bad) {
			t.Errorf("empty table still has %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "is-empty") {
		t.Errorf("empty table is not marked for CSS:\n%s", out)
	}
}

// Why: a confirmation table is read, not rearranged. It has more than two columns, so the column-count
// rule (fewer than three: no chooser) does not cover it and it needs the explicit data-nomenu.
func TestNoMenuAttributeSuppressesTheColumnsChooser(t *testing.T) {
	sortFixture(t)
	tbl := func(attr string) string {
		return `<table data-table="t_versions"` + attr + `><thead><tr><th>Version</th><th>When</th><th>Who</th><th>Note</th><th>Actions</th></tr></thead><tbody><tr><td>1</td><td>a</td><td>u</td><td>n</td><td></td></tr><tr><td>2</td><td>b</td><td>v</td><td>m</td><td></td></tr></tbody></table>`
	}
	if out := string(applyTablePrefs([]byte(tbl("")), nil)); !strings.Contains(out, "colmenu") {
		t.Fatalf("control: the chooser is missing without the attribute:\n%s", out)
	}
	if out := string(applyTablePrefs([]byte(tbl(" data-nomenu")), nil)); strings.Contains(out, "colmenu") {
		t.Errorf("data-nomenu still gets a chooser:\n%s", out)
	}
}

// Why: a "delete these files" confirmation is read, not rearranged: no Columns chooser, and
// every line names the file that goes.
func TestFirmwareDeleteSelectedConfirmIsNoList(t *testing.T) {
	fw := &fakeMgrFW{releases: baseRelease(), arts: []model.Artifact{
		{ID: 1, Release: "16.1.0", Filename: "tasmota.bin.gz", Family: model.ChipESP8266, Variant: "tasmota", Size: 10, Source: "github"},
		{ID: 2, Release: "16.1.0", Filename: "tasmota32.bin", Family: model.ChipESP32, Variant: "tasmota", Size: 10, Source: "github"},
	}}
	e := fwEnv(t, fw)
	_, body := e.do(t, "POST", "/firmware/delete-selected", "admin", url.Values{"csrf": {"tok"}, "id": {"1", "2"}})
	if strings.Contains(body, "colmenu") {
		t.Errorf("confirmation table has a Columns chooser:\n%s", body)
	}
	for _, want := range []string{"16.1.0 · tasmota.bin.gz", "16.1.0 · tasmota32.bin", "Delete 2 files"} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, body)
		}
	}
}
