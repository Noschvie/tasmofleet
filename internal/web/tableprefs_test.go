package web

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

type fakePrefs struct {
	mu   sync.Mutex
	rows map[string]map[string]string
}

func newFakePrefs() *fakePrefs { return &fakePrefs{rows: map[string]map[string]string{}} }

func (f *fakePrefs) UserPrefs(_ context.Context, u string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.rows[u] {
		out[k] = v
	}
	return out, nil
}
func (f *fakePrefs) SetUserPref(_ context.Context, u, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[u] == nil {
		f.rows[u] = map[string]string{}
	}
	f.rows[u][k] = v
	return nil
}
func (f *fakePrefs) DeleteUserPref(_ context.Context, u, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows[u], k)
	return nil
}

// Why: the browser is not trusted. Only known columns, clamped widths and
// offered page sizes may reach the database, or a crafted request could
// bloat it or break every page of the user.
func TestValidatePref(t *testing.T) {
	withOffColumnTable(t)
	cases := []struct {
		name, key, body string
		want            string // "" with wantErr false: removed
		wantErr         bool
	}{
		{"widths and hidden", "table.devices", `{"w":{"name":25,"status":15},"h":["family","uptime","family"]}`, `{"w":{"name":25,"status":15},"h":["family","uptime"]}`, false},
		{"width is clamped low", "table.devices", `{"w":{"name":-5}}`, `{"w":{"name":3}}`, false},
		{"width is clamped high", "table.devices", `{"w":{"name":5000}}`, `{"w":{"name":90}}`, false},
		{"empty resets", "table.devices", `{}`, "", false},
		{"unknown column in widths", "table.devices", `{"w":{"nope":10}}`, "", true},
		{"unknown column in hidden", "table.devices", `{"h":["nope"]}`, "", true},
		{"locked column cannot be hidden", "table.devices", `{"h":["name"]}`, "", true},
		{"unknown table", "table.nope", `{}`, "", true},
		{"unknown key", "other", `{}`, "", true},
		{"unknown field", "table.devices", `{"w":{},"x":1}`, "", true},
		// #805: "b" marks widths saved without the frame's stretch; it means something only for widths of a pixel table
		{"own widths", "table.devices", `{"w":{"name":25},"b":true}`, `{"w":{"name":25},"b":true}`, false},
		{"own widths without widths", "table.devices", `{"h":["family"],"b":true}`, `{"h":["family"]}`, false},
		{"own widths of a table of shares", "table.audit", `{"w":{"actor":20},"b":true}`, `{"w":{"actor":20}}`, false},
		// #828: "c" marks the content widths of a pixel table, drawn as they are; the row menu of the device
		// table fits its button and has no width to store, a stored one is dropped (an open page of an older
		// version may still send it)
		{"content widths", "table.devices", `{"w":{"name":25,"status":16},"c":true}`, `{"w":{"name":25,"status":16},"c":true}`, false},
		{"content widths replace own widths", "table.devices", `{"w":{"name":25},"b":true,"c":true}`, `{"w":{"name":25},"c":true}`, false},
		{"content widths without widths", "table.devices", `{"h":["family"],"c":true}`, `{"h":["family"]}`, false},
		{"content widths of a table of shares", "table.audit", `{"w":{"actor":20},"c":true}`, `{"w":{"actor":20}}`, false},
		{"the row menu has no width", "table.devices", `{"w":{"name":25,"actions":15.9},"c":true}`, `{"w":{"name":25},"c":true}`, false},
		{"the row menu's width alone stores nothing", "table.devices", `{"w":{"actions":5}}`, "", false},
		{"a table of shares keeps its actions share", "table.audit", `{"w":{"details":8}}`, `{"w":{"details":8}}`, false},
		{"not json", "table.devices", `[`, "", true},
		{"trailing data", "table.devices", `{} {}`, "", true},
		{"oversize", "table.devices", `{"h":["` + strings.Repeat("a", maxPrefBytes) + `"]}`, "", true},
		{"shown default-off column", "table.test_off", `{"v":["extra","extra"]}`, `{"v":["extra"]}`, false},
		{"hiding a default-off column stores nothing", "table.test_off", `{"h":["extra"]}`, "", false},
		{"only default-off columns can be shown", "table.devices", `{"v":["family"]}`, "", true},
		{"order", "table.devices", `{"o":["firmware","status"]}`, `{"o":["firmware","status"]}`, false},
		{"default order needs no storage", "table.devices", `{"o":["status","reach","firmware","family","address","uptime","wifi","restart","otaurl","grouptopic","power"]}`, "", false},
		{"a column fixed to its edge cannot move (Name)", "table.devices", `{"o":["status","name"]}`, "", true},
		{"a column fixed to its edge cannot move (Actions)", "table.devices", `{"o":["actions","status"]}`, "", true},
		{"Power is pinned but movable", "table.devices", `{"o":["power","status"]}`, `{"o":["power","status"]}`, false},
		{"pin left", "table.devices", `{"p":{"status":"left"}}`, `{"p":{"status":"left"}}`, false},
		{"pin right", "table.audit", `{"p":{"actor":"right"}}`, `{"p":{"actor":"right"}}`, false},
		{"unpin a column pinned by default", "table.devices", `{"p":{"power":"none"}}`, `{"p":{"power":"none"}}`, false},
		{"the default side needs no storage", "table.devices", `{"p":{"power":"right","status":"none"}}`, "", false},
		{"Actions cannot be unpinned", "table.devices", `{"p":{"actions":"none"}}`, "", true},
		{"Actions cannot change sides", "table.devices", `{"p":{"actions":"left"}}`, "", true},
		{"Name cannot be unpinned", "table.devices", `{"p":{"name":"none"}}`, "", true},
		{"unknown pin side", "table.devices", `{"p":{"status":"top"}}`, "", true},
		{"pin of an unknown column", "table.devices", `{"p":{"nope":"left"}}`, "", true},
		{"order names an unknown column", "table.devices", `{"o":["nope"]}`, "", true},
		{"order with a duplicate", "table.devices", `{"o":["name","name"]}`, "", true},
		{"sort", "sort.devices", `{"k":"uptime","d":"desc"}`, `{"k":"uptime","d":"desc"}`, false},
		{"sort by an unsortable column", "sort.audit", `{"k":"details","d":"asc"}`, "", true},
		{"sort direction", "sort.devices", `{"k":"name","d":"up"}`, "", true},
		{"sort reset", "sort.devices", `{}`, "", false},
		{"page size", "pager", `{"size":50}`, `{"size":50}`, false},
		{"page size reset", "pager", `{}`, "", false},
		{"page size not offered", "pager", `{"size":7}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := validatePref(c.key, []byte(c.body))
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("validatePref = %q, %v; want %q (err %v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

// Why: a registry that drifts from the templates would put widths on the
// wrong columns. Every data-table in a template must have as many header cells
// (a leading checkbox header excluded) as the registry has columns, optional
// admin-only columns excepted; every registry table must be used.
func TestTemplatesMatchColumnRegistry(t *testing.T) {
	tableRe := regexp.MustCompile(`(?s)<table\b[^>]*data-table="([a-z_]+)"[^>]*>(.*?)</table>`)
	used := map[string]bool{}
	files, _ := fs.Glob(Assets, "templates/*.html")
	ported, _ := filepath.Glob("*.templ") // pages already in templ (read from the source tree)
	sources := map[string]string{}
	for _, f := range append(files, ported...) {
		b, err := fs.ReadFile(Assets, f)
		if strings.HasSuffix(f, ".templ") {
			b, err = os.ReadFile(f)
		}
		if err != nil {
			t.Fatal(err)
		}
		sources[f] = string(b)
	}
	// tables whose frame is built in Go with ui.Table
	var dev strings.Builder
	if err := ui.Table(deviceTableData(devicesPage{RowsData: rowsData{Select: true}})).Render(context.Background(), &dev); err != nil {
		t.Fatal(err)
	}
	sources["deviceTableData"] = dev.String()
	var aud strings.Builder
	if err := auditView("", auditPage{Events: []auditRow{{}}}).Render(context.Background(), &aud); err != nil {
		t.Fatal(err)
	}
	sources["auditView"] = aud.String()
	for f, src := range sources {
		for _, m := range tableRe.FindAllStringSubmatch(src, -1) {
			def, ok := tableDefs[m[1]]
			if !ok {
				t.Errorf("%s: table %q is not in the registry", f, m[1])
				continue
			}
			used[m[1]] = true
			n := 0
			for _, th := range thRe.FindAllStringSubmatch(theadRe.FindString(m[2]), -1) {
				if !strings.Contains(th[0], `type="checkbox"`) {
					n++
				}
			}
			req := 0
			for _, cd := range def.Cols {
				if !cd.Optional {
					req++
				}
			}
			if n < req || n > len(def.Cols) {
				t.Errorf("%s: table %q has %d header cells, registry %d (%d required)", f, m[1], n, len(def.Cols), req)
			}
		}
	}
	for k := range tableDefs {
		if !used[k] {
			t.Errorf("registry table %q is used by no template", k)
		}
	}
}

const sampleTable = `<p>x</p><table class="devtable dense" data-table="devices"><colgroup><col class="c-sel"></colgroup>` +
	`<thead><tr><th><input type="checkbox" data-select-all></th><th>Name</th><th>Status</th><th>MQTT · HTTP</th><th>Firmware</th><th>Chip</th><th>Address</th><th>Uptime</th><th>Wi-Fi</th><th>Last restart</th><th>OTA URL</th><th>Group topic</th><th>Power</th><th>Actions</th></tr></thead>` +
	`<tbody><tr><td colspan="13">empty</td></tr><tr><td colspan="13">more</td></tr></tbody></table><p>y</p>`

// Why: without saved preferences the page must stay exactly as designed (own
// column widths, no forced layout) but still carry the menu.
func TestApplyTablePrefsDefaults(t *testing.T) {
	out := string(applyTablePrefs([]byte(sampleTable), nil))
	for _, want := range []string{`class="devtable dense pl1 pl2 hc11 hc12 pr14 pr15"`, `data-cols=",name:28:l:fix,status:17,reach:15,firmware:14,family:10,address:13,uptime:11,wifi:12,restart:18,otaurl:24:off,grouptopic:14:off,~,power:7:r,actions:5:r:fix"`,
		`data-order="status,reach,firmware,family,address,uptime,wifi,restart,otaurl,grouptopic,power"`, `<div class="tablewrap" data-key="devices">`, `<div class="tablescroll"><table`, `data-colid="family" checked`, `data-cols-reset`, `<col class="c-sel">`, "</table></div></div><p>y</p>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "tfix") || strings.Contains(out, `width="`) {
		t.Errorf("defaults must not force a layout: %s", out)
	}
	if strings.Contains(out, `data-colid="name"`) {
		t.Error("the identifying column must not be offered for hiding")
	}
}

// Why: the saved layout is written into the HTML on the server so the first
// paint is right; hidden columns get no <col> (a hidden cell takes no column)
// and the visible widths of a table of shares always add up to 100 percent
// (the device table has pixels, TestPixelTableDrawsItsColumnsInPixels).
func TestApplyTablePrefsAppliesWidthsAndHidden(t *testing.T) {
	prefs := map[string]string{"table.devices": `{"w":{"name":40,"status":10},"h":["family","uptime"]}`}
	out := string(applyTablePrefs([]byte(sampleTable), prefs))
	for _, want := range []string{`tfix`, `hc6`, `hc8`, `<col data-id="name" width="400">`, `data-colid="family">`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, `c-sel`) || strings.Contains(out, `data-id="family"`) || strings.Contains(out, `data-id="uptime"`) {
		t.Errorf("old colgroup kept or hidden column still has a col: %s", out)
	}
	audit := `<table data-table="audit"><thead><tr><th>When</th><th>User</th><th>Action</th><th>Target</th><th>Summary</th><th>Result</th><th>Details</th></tr></thead><tbody></tbody></table>`
	out = string(applyTablePrefs([]byte(audit), map[string]string{"table.audit": `{"w":{"actor":40,"action":10},"h":["target"]}`}))
	if strings.Contains(out, `data-id="target"`) || !strings.Contains(out, `<col data-id="actor"`) {
		t.Errorf("audit: hidden column still has a col, or a width is missing: %s", out)
	}
	sum := 0.0
	for _, m := range regexp.MustCompile(`<col data-id="[a-z]+" width="([0-9.]+)%">`).FindAllStringSubmatch(out, -1) {
		var v float64
		_, _ = fmt.Sscanf(m[1], "%f", &v)
		sum += v
	}
	if sum < 99.9 || sum > 100.1 {
		t.Errorf("widths add up to %.2f, want 100", sum)
	}
}

// Why: a corrupt stored row or a table the registry does not know must never
// break a page.
func TestApplyTablePrefsToleratesBadInput(t *testing.T) {
	out := string(applyTablePrefs([]byte(sampleTable), map[string]string{"table.devices": `{{{`}))
	if strings.Contains(out, "tfix") || !strings.Contains(out, "tablewrap") {
		t.Errorf("corrupt prefs should fall back to defaults: %s", out)
	}
	unknown := `<table data-table="nope"><thead><tr><th>A</th></tr></thead></table>`
	if got := string(applyTablePrefs([]byte(unknown), nil)); got != unknown {
		t.Errorf("unknown table changed: %s", got)
	}
}

func prefsEnv(t *testing.T) (*env, *fakePrefs) {
	t.Helper()
	fp := newFakePrefs()
	return setupWith(t, func(d *Deps) { d.Prefs = fp }), fp
}

// Why: preferences belong to the signed-in user; anonymous requests must not
// reach the store, and a viewer of another account must not see them.
func TestSavePrefAuthAndPerUser(t *testing.T) {
	e, fp := prefsEnv(t)
	resp, _ := e.do(t, "POST", "/prefs/pager", "", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous = %d, want 401", resp.StatusCode)
	}
	resp, _ = e.doJSON(t, "/prefs/table.devices", "operator", `{"w":{"name":30},"h":["family"]}`)
	if resp.StatusCode != 204 {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	if got := fp.rows["test:operator"]["table.devices"]; got != `{"w":{"name":30},"h":["family"]}` {
		t.Fatalf("stored %q", got)
	}
	if len(fp.rows["test:admin"]) != 0 {
		t.Fatal("another user got the preference")
	}
	// the saved layout is applied for that user only
	_, page := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(page, "tfix") || !strings.Contains(page, "hc5") {
		t.Error("saved layout not applied to the operator's page")
	}
	_, other := e.do(t, "GET", "/devices", "admin", nil)
	if strings.Contains(other, "tfix") {
		t.Error("layout leaked to another user")
	}
	// an empty document resets
	resp, _ = e.doJSON(t, "/prefs/table.devices", "operator", `{}`)
	if resp.StatusCode != 204 || len(fp.rows["test:operator"]) != 0 {
		t.Fatalf("reset = %d, rows %v", resp.StatusCode, fp.rows)
	}
}

// Why: unknown columns, oversize bodies and unknown keys are refused before
// anything is stored.
func TestSavePrefRejectsBadInput(t *testing.T) {
	e, fp := prefsEnv(t)
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"unknown column": {"/prefs/table.devices", `{"h":["bogus"]}`, 400},
		"locked column":  {"/prefs/table.devices", `{"h":["name"]}`, 400},
		"unknown key":    {"/prefs/whatever", `{}`, 400},
		"bad page size":  {"/prefs/pager", `{"size":13}`, 400},
		"too large":      {"/prefs/table.devices", `{"h":["` + strings.Repeat("x", 3*maxPrefBytes) + `"]}`, 413},
	} {
		resp, _ := e.doJSON(t, c.path, "operator", c.body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, c.want)
		}
	}
	if len(fp.rows) != 0 {
		t.Fatalf("rejected input was stored: %v", fp.rows)
	}
}

// Why: the endpoint writes data, so it sits behind the same CSRF check as every
// other POST: a request without the token never reaches the store.
func TestSavePrefNeedsCSRFToken(t *testing.T) {
	fp := newFakePrefs()
	h, err := Handler(Deps{Guard: fakeGuard{}, Inventory: newInv(), Health: fakeHealth{}, Audit: &fakeAudit{}, Recorder: &fakeAudit{}, Hub: nil, Prefs: fp})
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := httpx.NewCSRF(fakeGuard{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(csrf.Middleware(h))
	defer srv.Close()
	for token, want := range map[string]int{"": 403, "wrong": 403, "tok": 204} {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/prefs/pager", strings.NewReader(`{"size":50}`))
		req.Header.Set("X-Test-Role", "operator")
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set(httpx.CSRFHeader, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("token %q: status %d, want %d", token, resp.StatusCode, want)
		}
	}
	if len(fp.rows["test:operator"]) != 1 {
		t.Errorf("rows = %v", fp.rows)
	}
}

// Why: the page size is a user setting: a saved size applies without a URL
// parameter, an explicit one still wins, and links to the saved size stay short.
func TestAuditUsesSavedPageSize(t *testing.T) {
	// The events are placed relative to the clock the handler uses: the audit page only
	// lists the last 7 days, so a fixed calendar date would fall out of the window.
	now := time.Date(2030, 1, 15, 12, 0, 0, 0, time.UTC)
	fp := newFakePrefs()
	e := setupWith(t, func(d *Deps) { d.Prefs = fp; d.Clock = noonClock{now} })
	for i := 0; i < 60; i++ {
		e.aud.Record(t.Context(), model.AuditEvent{ID: int64(i), At: now.Add(-time.Hour).Add(time.Duration(i) * time.Second), Actor: "alice", Action: "device.update", Target: "device:1", Result: "ok"})
	}
	_, body := e.do(t, "GET", "/audit", "admin", nil)
	if n := strings.Count(body, "detail-row"); n < 25 || n > 26 { // 25 rows, plus the CSS-free class mentions
		t.Fatalf("default page has %d rows", n)
	}
	_ = fp.SetUserPref(t.Context(), "test:admin", "pager", `{"size":50}`)
	_, body = e.do(t, "GET", "/audit", "admin", nil)
	if !strings.Contains(body, "Page 1 of 2, 60 events") {
		t.Errorf("saved size 50 not applied: pages text missing")
	}
	_, body = e.do(t, "GET", "/audit?per=25", "admin", nil)
	if !strings.Contains(body, "Page 1 of 3, 60 events") {
		t.Errorf("explicit per must win over the saved size")
	}
}

// Why: a header tooltip is one short sentence saying what the column shows;
// the registry and the tip list must not drift apart (a new column without a
// tip would show nothing, a long tip becomes a wall of text).
func TestEveryColumnHasAShortTip(t *testing.T) {
	for key, def := range tableDefs {
		for _, cd := range def.Cols {
			tip, ok := colTips[key+"."+cd.ID]
			switch {
			case !ok:
				t.Errorf("%s.%s has no header tooltip", key, cd.ID)
			case len(tip) > 80:
				t.Errorf("%s.%s tooltip is %d characters, want at most 80", key, cd.ID, len(tip))
			case strings.Count(tip, ". ") > 0 || !strings.HasSuffix(tip, "."):
				t.Errorf("%s.%s tooltip %q must be exactly one sentence", key, cd.ID, tip)
			}
		}
	}
	for k := range colTips {
		key, id, _ := strings.Cut(k, ".")
		if _, ok := tableDefs[key].col(id); !ok {
			t.Errorf("tooltip for unknown column %q", k)
		}
	}
}

// Why: sorting must work without script (plain links), flip the direction of
// the current column, keep the filters, and mark the state for screen readers.
func TestSortableHeadersAreLinks(t *testing.T) {
	in := strings.Replace(sampleTable, `data-table="devices"`, `data-table="devices" data-sortbase="/tf/devices?q=a&amp;health=online" data-sort="uptime" data-dir="asc"`, 1)
	out := string(applyTablePrefs([]byte(in), nil))
	for _, want := range []string{
		`aria-sort="ascending"`, `href="/tf/devices?q=a&amp;health=online&amp;sort=uptime&amp;dir=desc"`,
		`href="/tf/devices?q=a&amp;health=online&amp;sort=name&amp;dir=asc"`, `class="sortlink is-asc"`,
		`title="Time since the last restart."`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, `type="checkbox" data-select-all></a>`) {
		t.Error("the selection header must not be a sort link")
	}
}

// withOffColumnTable registers a table with a column that is hidden by default for the test.
// No real table has one since the device list shows Last restart like the prototype (P2), but
// the registry keeps the option and its rules.
func withOffColumnTable(t *testing.T) {
	t.Helper()
	tableDefs["test_off"] = tableDef{Key: "test_off", Cols: []colDef{lk("name", "Name", 40), c("state", "State", 30), c("more", "More", 20), off(c("extra", "Extra", 10))}}
	colTips["test_off.name"], colTips["test_off.state"], colTips["test_off.more"], colTips["test_off.extra"] = "Name.", "State.", "More.", "Extra."
	t.Cleanup(func() {
		delete(tableDefs, "test_off")
		for _, id := range []string{"name", "state", "more", "extra"} {
			delete(colTips, "test_off."+id)
		}
	})
}

// Why: a column hidden by default stays hidden until the user shows it, with no layout forced
// on everyone else (its <col> goes away instead).
func TestDefaultOffColumn(t *testing.T) {
	withOffColumnTable(t)
	table := `<table data-table="test_off"><colgroup><col><col><col><col class="c-extra"></colgroup><thead><tr><th>Name</th><th>State</th><th>More</th><th>Extra</th></tr></thead>` +
		`<tbody><tr><td>a</td><td>b</td><td>c</td><td>d</td></tr><tr><td>e</td><td>f</td><td>g</td><td>h</td></tr></tbody></table>`
	out := string(applyTablePrefs([]byte(table), nil))
	if !strings.Contains(out, "hc4") || strings.Contains(out, "c-extra") || strings.Contains(out, "tfix") {
		t.Errorf("default-off column must be hidden without forcing a layout: %s", out)
	}
	if !strings.Contains(out, `data-colid="extra">`) {
		t.Errorf("the Columns menu must offer it unchecked: %s", out)
	}
	shown := string(applyTablePrefs([]byte(table), map[string]string{"table.test_off": `{"v":["extra"]}`}))
	if strings.Contains(shown, "hc4") || !strings.Contains(shown, `data-colid="extra" checked`) || !strings.Contains(shown, `data-id="extra"`) {
		t.Errorf("a shown default-off column must render: %s", shown)
	}
}

// Why: the Columns button belongs in the page's own toolbar when the template
// marks the place, so both share one row; otherwise it gets a bar above the table.
func TestColumnsMenuGoesIntoTheMarkedToolbar(t *testing.T) {
	in := strings.Replace(sampleTable, `<p>x</p>`, `<p class="toolbar">x`+colMenuMarker+`</p>`, 1)
	out := string(applyTablePrefs([]byte(in), nil))
	i, j := strings.Index(out, "colmenu"), strings.Index(out, "<table")
	if i < 0 || i > j || strings.Contains(out, "tabletools") || strings.Contains(out, colMenuMarker) || !strings.Contains(out, `data-key="devices"><summary`) {
		t.Errorf("menu not in the toolbar: %s", out)
	}
	if !strings.Contains(string(applyTablePrefs([]byte(sampleTable), nil)), `<div class="tabletools"><details class="colmenu"`) {
		t.Error("without a marker the menu needs its own bar")
	}
}

// Why: the last sort of a table is remembered per user, so the list opens the
// way the user left it; the saved sort is also what a table refresh and a
// plain visit use, and a crafted column name is ignored, never stored.
func TestDevicesSortIsRememberedPerUser(t *testing.T) {
	e, fp := prefsEnv(t)
	_, body := e.do(t, "GET", "/devices?sort=firmware&dir=desc", "admin", nil)
	if !strings.Contains(body, `aria-sort="descending"`) || !strings.Contains(body, "sort=firmware&amp;dir=asc") {
		t.Fatalf("explicit sort not applied:\n%s", body[:min(len(body), 400)])
	}
	saved := ""
	for _, rows := range fp.rows {
		saved += rows["sort.devices"]
	}
	if saved != `{"k":"firmware","d":"desc"}` {
		t.Fatalf("saved sort %q", saved)
	}
	_, body = e.do(t, "GET", "/devices", "admin", nil) // a plain visit
	if !strings.Contains(body, `data-sort="firmware" data-dir="desc"`) {
		t.Fatal("the saved sort was not used")
	}
	_, frag := e.do(t, "GET", "/devices/table", "admin", nil)
	if !strings.Contains(frag, `data-sort="firmware"`) {
		t.Fatal("the table refresh lost the saved sort")
	}
	_, body = e.do(t, "GET", "/devices?sort=password&dir=desc", "admin", nil)
	if strings.Contains(body, `data-sort="password"`) || !strings.Contains(body, `data-sort="firmware"`) {
		t.Fatal("an unknown column must be ignored")
	}
	_, other := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(other, `data-sort="name"`) {
		t.Fatal("another user keeps the default sort")
	}
}

// Why: a reordered column moves header and body cells together, keeps the
// selection checkbox and the Name first and Power and Actions last (pinned right),
// and the saved order comes with the first paint (the page must not jump).
func TestColumnOrderIsRenderedOnTheServer(t *testing.T) {
	in := sampleWithRow()
	out := string(applyTablePrefs([]byte(in), map[string]string{"table.devices": `{"o":["address","status"]}`}))
	if !strings.Contains(out, `<tr><td>S</td><td>n</td><td>ad</td><td>st</td><td>mh</td><td>fw</td><td>fa</td><td>up</td><td>wi</td><td>re</td><td>ot</td><td>gt</td>`+spareTD+`<td>po</td><td>ac</td></tr>`) {
		t.Errorf("body cells not moved:\n%s", out)
	}
	if got := heads(out); got != ",Name,Address,Status,MQTT · HTTP,Firmware,Chip,Uptime,Wi-Fi,Last restart,OTA URL,Group topic,,Power,Actions" {
		t.Errorf("header order %q", got)
	}
	if !strings.Contains(out, `data-cols=",name:28:l:fix,address:13,status:17,`) || !strings.Contains(out, "tfix") || !strings.Contains(out, `<td colspan="13">empty</td>`) {
		t.Errorf("script map, forced layout or the empty row wrong:\n%s", out)
	}
	if !strings.Contains(out, `power:7:r,actions:5:r:fix"`) || strings.Contains(out, `data-moveid="actions"`) || strings.Contains(out, `data-moveid="name"`) {
		t.Error("Power must stay pinned right; Name and Actions are not offered for moving")
	}
	// the default order is left alone
	if def := string(applyTablePrefs([]byte(in), nil)); !strings.Contains(def, `<td>S</td><td>n</td><td>st</td>`) {
		t.Error("default order changed")
	}
}

// spareTD is the empty cell a pixel table has before the columns pinned right (#828).
const spareTD = `<td class="spare" aria-hidden="true"></td>`

// sampleWithRow is sampleTable with one data row whose cells name their column.
func sampleWithRow() string {
	row := `<tr><td>S</td><td>n</td><td>st</td><td>mh</td><td>fw</td><td>fa</td><td>ad</td><td>up</td><td>wi</td><td>re</td><td>ot</td><td>gt</td><td>po</td><td>ac</td></tr>`
	return strings.Replace(sampleTable, `<tbody><tr><td colspan="13">empty</td></tr>`, `<tbody>`+row+`<tr><td colspan="13">empty</td></tr>`, 1)
}

// heads lists the header texts of a rewritten table.
func heads(out string) string {
	var hs []string
	for _, m := range regexp.MustCompile(`<th\b[^>]*>(?:<a[^>]*>)?([^<]*)`).FindAllStringSubmatch(out, -1) {
		hs = append(hs, m[1])
	}
	return strings.Join(hs, ",")
}

// Why (D19): Power and Actions are the controls you want next to the row wherever the table
// scrolls, Name says which row it is. Pinned columns are grouped at their edge in the user's
// order (a column pinned left comes right after Name, one pinned right right before Power and
// Actions), carry the class that makes them stick, and an unpinned Power joins the scrolling
// columns. The Columns menu shows the choice as pressed buttons.
func TestPinnedColumnsAreGroupedAtTheirEdge(t *testing.T) {
	in := sampleWithRow()
	out := string(applyTablePrefs([]byte(in), map[string]string{"table.devices": `{"p":{"wifi":"left","status":"right"}}`}))
	if got := heads(out); got != ",Name,Wi-Fi,MQTT · HTTP,Firmware,Chip,Address,Uptime,Last restart,OTA URL,Group topic,,Status,Power,Actions" {
		t.Errorf("header order %q", got)
	}
	if !strings.Contains(out, `<tr><td>S</td><td>n</td><td>wi</td><td>mh</td><td>fw</td><td>fa</td><td>ad</td><td>up</td><td>re</td><td>ot</td><td>gt</td>`+spareTD+`<td>st</td><td>po</td><td>ac</td></tr>`) {
		t.Errorf("body cells not grouped:\n%s", out)
	}
	if !strings.Contains(out, `class="devtable dense pl1 pl2 pl3 hc10 hc11 pr13 pr14 pr15 tfix"`) {
		t.Errorf("pin classes wrong:\n%s", out)
	}
	for _, want := range []string{
		`data-pin="left" data-pinid="wifi" aria-pressed="true"`, `data-pin="right" data-pinid="status" aria-pressed="true"`,
		`data-pin="right" data-pinid="power" aria-pressed="true"`, `data-pin="left" data-pinid="firmware" aria-pressed="false"`,
		`Name</label><span class="colfix">Pinned left · always</span>`, `Actions</label><span class="colfix">Pinned right · always</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu: missing %q", want)
		}
	}
	unpinned := string(applyTablePrefs([]byte(in), map[string]string{"table.devices": `{"p":{"power":"none"},"o":["power","status"]}`}))
	if got := heads(unpinned); got != ",Name,Power,Status,MQTT · HTTP,Firmware,Chip,Address,Uptime,Wi-Fi,Last restart,OTA URL,Group topic,,Actions" {
		t.Errorf("unpinned Power: header order %q", got)
	}
	if strings.Contains(unpinned, " pr3") || !strings.Contains(unpinned, " pr15") {
		t.Errorf("unpinned Power must not stick, Actions must:\n%s", unpinned)
	}
	// a layout saved before pinning existed reads as the defaults
	old := string(applyTablePrefs([]byte(in), map[string]string{"table.devices": `{"w":{"name":30}}`}))
	if !strings.Contains(old, " pr14 pr15") || !strings.Contains(old, "pl1 pl2") {
		t.Errorf("an old layout lost the default pins:\n%s", old)
	}
}

// Why (QA #51): a Columns button above a table with fewer than two rows offers
// a choice about nothing; the table itself keeps its saved layout.
func TestNoColumnsMenuAboveATableWithFewerThanTwoRows(t *testing.T) {
	one := strings.Replace(sampleTable, `<tr><td colspan="13">more</td></tr>`, "", 1)
	if out := string(applyTablePrefs([]byte(one), nil)); strings.Contains(out, "colmenu") || !strings.Contains(out, `data-cols=`) {
		t.Errorf("one row: want no Columns menu but a rewritten table:\n%s", out)
	}
	if out := string(applyTablePrefs([]byte(sampleTable), nil)); !strings.Contains(out, "colmenu") {
		t.Errorf("two rows: the Columns menu is missing:\n%s", out)
	}
}

// Why (design v2, P2; #828): the device table has pixel widths and scrolls sideways. A column is as
// wide as the person set it, in pixels, and nothing else: no frame stretches it, and widening one
// column takes nothing from the others (the table grows and scrolls). The server writes the widths
// in pixels (tenths in the saved layout, "c") and names the table's width (data-basew): the selection
// column at its checkbox (43 px), the data columns, and the row menu at its button (48 px), which is
// never a saved width. tables.js computes the same number, so a page and its reload agree.
func TestPixelTableDrawsItsColumnsInPixels(t *testing.T) {
	def := string(applyTablePrefs([]byte(sampleTable), nil))
	if !strings.Contains(def, " data-px") || strings.Contains(def, "data-basew") || strings.Contains(def, "data-legacy") {
		t.Errorf("default layout: want data-px and no saved size:\n%s", def)
	}
	saved := string(applyTablePrefs([]byte(sampleTable), map[string]string{"table.devices": `{"w":{"firmware":18,"family":6,"actions":15.9},"c":true}`}))
	for _, want := range []string{
		`<colgroup><col width="43"><col data-id="name" width="280"><col data-id="status" width="170">`,
		`<col data-id="firmware" width="180"><col data-id="family" width="60">`,
		// the room a wider frame has spare goes to the empty column before the columns pinned right
		`<col class="spare"><col data-id="power" width="70"><col data-id="actions" width="48"></colgroup>`,
		// 43 + (28+17+15+18+6+13+11+12+18+7) tenths + 48 = 43 + 1450 + 48
		`data-basew="1541"`,
	} {
		if !strings.Contains(saved, want) {
			t.Errorf("saved layout: missing %q in\n%s", want, saved)
		}
	}
	if strings.Contains(saved, "data-legacy") || strings.Contains(saved, "%") {
		t.Errorf("a layout of content widths is drawn as it is, in pixels:\n%s", saved)
	}
	// a layout without widths (only hidden columns) has the default widths, which are no stretch either
	hid := string(applyTablePrefs([]byte(sampleTable), map[string]string{"table.devices": `{"h":["family"]}`}))
	if !strings.Contains(hid, `data-basew="1441"`) || strings.Contains(hid, "data-legacy") {
		t.Errorf("hidden columns only: want data-basew 1541 - 100:\n%s", hid)
	}
}

// Why (#828): a layout saved before this version holds widths that are no content widths. Before 0.36.1 a save
// stored the widths the browser drew, stretched to the frame (no "b"); 0.36.1 stored them scaled to the table's
// scroll floor ("b"), so Address came back 86 px wide for 156 px of content. Only the browser knows the content
// widths, so the server draws the stored pixels and says what they are (data-legacy), tables.js lays them out so
// that no column is narrower than its content, and the next save stores content widths ("c"). The row menu's
// stored width is ignored in both.
func TestPixelTableLayoutsSavedBefore828AreMarkedForTheBrowser(t *testing.T) {
	cases := []struct{ name, layout, legacy string }{
		{"before 0.36.1: stretched", `{"w":{"actions":15.9,"address":12.6,"name":33.3},"h":["power"]}`, `data-legacy="shares"`},
		{"0.36.1: scaled to the floor", `{"w":{"actions":8.2,"address":8.6,"name":17.5},"h":["power"],"b":true}`, `data-legacy="px"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := string(applyTablePrefs([]byte(sampleTable), map[string]string{"table.devices": c.layout}))
			if !strings.Contains(out, c.legacy) || strings.Contains(out, "data-basew") {
				t.Errorf("want %s and no size of its own in\n%s", c.legacy, out)
			}
			if !strings.Contains(out, `<col data-id="actions" width="48">`) {
				t.Errorf("the row menu fits its button, whatever was stored:\n%s", out)
			}
			if !regexp.MustCompile(`<col data-id="name" width="(333|175)">`).MatchString(out) {
				t.Errorf("the stored pixels are drawn until tables.js has the content widths:\n%s", out)
			}
		})
	}
}

// Why (#828): when the frame is wider than the table, the room left over is shown as empty room after the last
// data column, before the columns pinned right: it goes to no column (the row menu was 159 px wide for a 48 px
// button, and the empty part of it read as part of Uptime), and the row menu stays at the frame's right edge. The
// server gives every row an empty cell there; it is no column of the registry, so the Columns menu has no row
// for it and it moves with the pins.
func TestPixelTableHasASpareCellBeforeTheColumnsPinnedRight(t *testing.T) {
	in := sampleWithRow()
	spare := spareTD
	for _, c := range []struct{ name, layout, row, classes string }{
		{"default layout", "", `<td>gt</td>` + spare + `<td>po</td><td>ac</td></tr>`, `pl1 pl2 hc11 hc12 pr14 pr15`},
		{"saved layout", `{"w":{"name":30},"c":true}`, `<td>gt</td>` + spare + `<td>po</td><td>ac</td></tr>`, `pl1 pl2 hc11 hc12 pr14 pr15 tfix`},
		{"Power unpinned", `{"p":{"power":"none"}}`, `<td>po</td>` + spare + `<td>ac</td></tr>`, `pl1 pl2 hc11 hc12 pr15`},
		{"Status pinned right", `{"p":{"status":"right"}}`, `<td>gt</td>` + spare + `<td>st</td><td>po</td><td>ac</td></tr>`, `pr14 pr15`},
	} {
		t.Run(c.name, func(t *testing.T) {
			prefs := map[string]string{}
			if c.layout != "" {
				prefs["table.devices"] = c.layout
			}
			out := string(applyTablePrefs([]byte(in), prefs))
			if !strings.Contains(out, c.row) || !strings.Contains(out, `<th class="spare" aria-hidden="true"></th>`) {
				t.Errorf("want the spare cell before the columns pinned right (%s) in\n%s", c.row, out)
			}
			if !strings.Contains(out, c.classes) || !strings.Contains(out, ",~,") {
				t.Errorf("want classes %q and the spare column in data-cols:\n%s", c.classes, out)
			}
			if strings.Count(out, `class="spare"`) != 3 { // header, row, <col>; the rows of one spanning cell keep their cell
				t.Errorf("want one spare cell per row and one <col>:\n%s", out)
			}
			if strings.Contains(out, `data-colrow="~"`) || strings.Contains(out, `data-colid="~"`) {
				t.Errorf("the spare room is no column of the Columns menu:\n%s", out)
			}
		})
	}
	// a table of shares fills its frame: no spare room
	audit := `<table data-table="audit"><thead><tr><th>When</th><th>User</th><th>Action</th><th>Target</th><th>Summary</th><th>Result</th><th>Details</th></tr></thead><tbody><tr><td>a</td><td>b</td><td>c</td><td>d</td><td>e</td><td>f</td><td>g</td></tr></tbody></table>`
	if out := string(applyTablePrefs([]byte(audit), nil)); strings.Contains(out, "spare") {
		t.Errorf("a table of shares has no spare cell:\n%s", out)
	}
}

// Why (#1094): a table is named by its <caption>, which must be the first child of the <table>
// (HTML content model; a caption after the <colgroup> is dropped by browsers). The server writes the
// user's own <colgroup> into the table, so it must go after the caption on every path: defaults, a
// custom order, a hidden column and a pinned one.
func TestApplyTablePrefsKeepsTheCaptionFirst(t *testing.T) {
	table := func(inner string) string {
		return `<table data-table="audit"><caption class="sr-only">Audit events</caption>` + inner + `</table>`
	}
	head := `<thead><tr><th scope="col">When</th><th scope="col">User</th><th scope="col">Action</th><th scope="col">Target</th><th scope="col">Summary</th><th scope="col">Result</th><th scope="col">Details</th></tr></thead><tbody><tr><td>1</td><td>2</td><td>3</td><td>4</td><td>5</td><td>6</td><td>7</td></tr><tr><td>1</td><td>2</td><td>3</td><td>4</td><td>5</td><td>6</td><td>7</td></tr></tbody>`
	colgroup := `<colgroup><col class="c-time"><col class="c-actor"><col class="c-action"><col class="c-target"><col><col class="c-res"><col class="c-det"></colgroup>`
	for name, pref := range map[string]string{
		"defaults": "", "custom order": `{"o":["action","actor"]}`, "hidden column": `{"h":["target"]}`,
		"pinned column": `{"p":{"result":"r"}}`, "width": `{"w":{"actor":40}}`,
	} {
		prefs := map[string]string{}
		if pref != "" {
			prefs["table.audit"] = pref
		}
		for _, withGroup := range []bool{true, false} {
			in := table(head)
			if withGroup {
				in = table(colgroup + head)
			}
			out := string(applyTablePrefs([]byte(in), prefs))
			i := strings.Index(out, "<table")
			after := out[i+strings.Index(out[i:], ">")+1:]
			if !strings.HasPrefix(after, `<caption class="sr-only">Audit events</caption>`) {
				t.Errorf("%s (colgroup %v): the caption is not the first child: %.160s", name, withGroup, after)
			}
			if strings.Count(out, "<caption") != 1 {
				t.Errorf("%s (colgroup %v): the caption is not there exactly once", name, withGroup)
			}
		}
	}
}

// Why (#1133, A1-026): hiding a column is the common job of the Columns menu, pinning and re-ordering the rare one,
// and the menu put 48 small buttons (two pins and two arrows per column) between a person and the box they came for.
// The boxes come first, as a plain list; the pins, the arrows and the draggable rows sit in a "Customize order" fold
// that starts closed, so they are neither on screen nor in the tab order until it is opened. The attributes the script
// reads (data-colid, data-colrow, data-pin, data-move) stay as they were, and Reset to default stays outside the fold.
func TestColumnsMenuKeepsPinningAndOrderInAClosedFold(t *testing.T) {
	out := string(applyTablePrefs([]byte(sampleWithRow()), map[string]string{"table.devices": `{"p":{"wifi":"left"}}`}))
	menu := regexp.MustCompile(`(?s)<div class="colpanel".*?</div></details>`).FindString(out)
	if menu == "" {
		t.Fatalf("no Columns menu: %s", out)
	}
	start := strings.Index(menu, `<details class="colorder">`)
	if start < 0 {
		t.Fatalf("no closed Customize order fold in the menu: %s", menu)
	}
	endRel := strings.Index(menu[start:], "</details>")
	if endRel < 0 {
		t.Fatalf("the fold never closes: %s", menu)
	}
	before, fold, after := menu[:start], menu[start:start+endRel], menu[start+endRel:]
	if !strings.Contains(fold, "<summary>Customize order</summary>") {
		t.Errorf("the fold is named Customize order: %s", fold)
	}
	// every box is before the fold: hiding a column never needs it
	if n := strings.Count(before, `type="checkbox"`); n != strings.Count(menu, `type="checkbox"`) || n < 13 {
		t.Errorf("%d of %d checkboxes before the fold, want all of the 13 columns", n, strings.Count(menu, `type="checkbox"`))
	}
	// every control that pins, moves or drags sits inside it, and nothing else of them outside
	for _, attr := range []string{`data-pin="`, `data-move="`, `draggable="true"`, `data-colrow="`} {
		if strings.Contains(before, attr) || strings.Contains(after, attr) || !strings.Contains(fold, attr) {
			t.Errorf("%s belongs in the fold only", attr)
		}
	}
	if got, want := strings.Count(fold, `data-colrow="`), strings.Count(before, `data-colid="`); got != want {
		t.Errorf("%d draggable rows in the fold for %d hideable columns", got, want)
	}
	if strings.Contains(fold, `type="checkbox"`) {
		t.Error("a column has one box, in the list before the fold (data-colid is looked up in the menu)")
	}
	for _, want := range []string{`data-pin="left" data-pinid="wifi" aria-pressed="true"`, `data-move="up" data-moveid="status"`} {
		if !strings.Contains(fold, want) {
			t.Errorf("the fold lost %q", want)
		}
	}
	if !strings.Contains(after, "data-cols-reset") || strings.Contains(fold, "data-cols-reset") {
		t.Error("Reset to default stays outside the fold, always in reach")
	}
	if !strings.Contains(fold, "Drag ⋮⋮ or use ↑ ↓ to reorder") || !strings.Contains(fold, "Pinned columns keep their side") {
		t.Errorf("the help for ordering and pinning is in the fold, not above the boxes: %s", fold)
	}
}
