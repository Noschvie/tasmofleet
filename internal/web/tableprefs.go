package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hellwand/tasmofleet/internal/auth"
)

// Table column preferences.
//
// Every data table in the UI is declared in tableDefs and marked in its
// template with data-table="<key>". The server rewrites the rendered HTML
// (applyTablePrefs): it writes the user's saved widths into <col width="..%">,
// the hidden columns into hcN classes (CSS hides the Nth cell) and adds the
// "Columns" menu, so the page is right on first paint (no layout jump) and the
// strict CSP (no inline style, no inline script) is respected. static/tables.js
// only adds the resize handles, the menu behaviour and saving.
//
// The two tables that are streamed over SSE (prefetch files, scan results) use
// the same path: the stream handler runs its fragment through applyTablePrefs
// with the preferences read when the stream opened, so every pushed fragment
// carries the saved layout, and tables.js sets the table up again after each
// swap (htmx:afterSettle), as it does for the transactions table.
//
// Widths are relative weights (percent of the data columns), so a saved layout
// fits any screen width and never makes the page scroll sideways. The device
// list is the exception (pxTables): its widths are pixels.

type colDef struct {
	ID    string
	Label string
	// Weight is the default relative width.
	Weight float64
	// Lock: the column cannot be hidden (the identifying column, actions).
	Lock bool
	// Optional: the template renders the column only for some users (admins).
	Optional bool
	// Sort: the header is a sort link (the page sorts on the server by this column's id).
	Sort bool
	// Off: hidden until the user shows it in the Columns menu.
	Off bool
	// Side: the edge the column is pinned to by default ("left", "right" or "" for none). A
	// pinned column stays visible at that edge when the table scrolls sideways (D19).
	Side string
	// Fixed: the column is pinned to Side for good: it cannot be moved, unpinned or pinned to
	// the other side (row actions, the identifying name of the device list).
	Fixed bool
}

type tableDef struct {
	Key  string
	Cols []colDef
}

// pxTables have pixel default widths (the weights are tenths of them) and scroll sideways when
// they need more room than the page has (the device list, D19). A column of such a table is as wide
// as the person set it, in pixels of content (tablePref.C, #828), and nothing else: the table is as
// wide as its columns (data-basew), widening one column takes nothing from the others, and a frame
// wider than the table keeps the room it has spare in an empty cell before the columns pinned right
// (spareID). The row menu column (fixed right) is as wide as its button (menuPx) and has no saved width.
var pxTables = map[string]bool{"devices": true}

// spareID names the empty column of a pixel table (in data-cols, for tables.js): the room a frame has
// beyond the table's own width. It is no column of the registry.
const spareID = "~"

// fixedWidth says whether a column of table key has a width of its own that no layout sets: the row
// menu of a pixel table, as wide as its button.
func fixedWidth(key string, cd colDef) bool {
	return pxTables[key] && cd.Fixed && cd.Side == sideRight
}

func c(id, label string, w float64) colDef { return colDef{ID: id, Label: label, Weight: w} }
func lk(id, label string, w float64) colDef {
	return colDef{ID: id, Label: label, Weight: w, Lock: true}
}

// srt marks a column as sortable, off as hidden by default.
func srt(cd colDef) colDef { cd.Sort = true; return cd }
func off(cd colDef) colDef { cd.Off = true; return cd }

// Pin sides: the left and right edge of a table, and "none" in a saved layout for a column the
// user unpinned although its default side is set.
const (
	sideLeft  = "left"
	sideRight = "right"
	sideNone  = "none"
)

// edge pins a column to a side for good (Fixed); pinned gives it a default side the user can change.
func edge(side string, cd colDef) colDef   { cd.Side, cd.Fixed = side, true; return cd }
func pinned(side string, cd colDef) colDef { cd.Side = side; return cd }

// act is a row-actions column: always pinned right.
func act(cd colDef) colDef { return edge(sideRight, cd) }

var tableDefs = func() map[string]tableDef {
	list := []tableDef{
		{"devices", []colDef{edge(sideLeft, srt(lk("name", "Name", 28))), srt(c("status", "Status", 17)), c("reach", "MQTT · HTTP", 15), srt(c("firmware", "Firmware", 14)), srt(c("family", "Chip", 10)), srt(c("address", "Address", 13)), srt(c("uptime", "Uptime", 11)), srt(c("wifi", "Wi-Fi", 12)), srt(c("restart", "Last restart", 18)), off(srt(c("otaurl", "OTA URL", 24))), off(srt(c("grouptopic", "Group topic", 14))), pinned(sideRight, srt(c("power", "Power", 7))), act(lk("actions", "Actions", 5))}},
		{"audit", []colDef{srt(lk("when", "When", 10)), srt(c("actor", "User", 12)), srt(c("action", "Action", 14)), srt(c("target", "Target", 10)), c("summary", "Summary", 22), srt(c("result", "Result", 12)), act(lk("details", "Details", 8))}},
		{"transaction_retry", []colDef{lk("device", "Device", 34), c("target", "Target", 33), c("last", "Last result", 33)}},
		{"device_settings", []colDef{lk("setting", "Setting", 22), c("command", "Command sent", 34), c("result", "Result", 16), c("readback", "Read back", 28)}},
	}
	m := make(map[string]tableDef, len(list))
	for _, d := range list {
		m[d.Key] = d
	}
	markSortable(m)
	return m
}()

func (d tableDef) col(id string) (colDef, bool) {
	for _, cd := range d.Cols {
		if cd.ID == id {
			return cd, true
		}
	}
	return colDef{}, false
}

// Limits of a stored table layout. A column narrower than minWeight percent is
// unreadable, one wider than maxWeight leaves the others no room.
const (
	minWeight    = 3.0
	maxWeight    = 90.0
	maxPrefBytes = 4096
	selectShare  = 3.5 // percent of the table for a leading checkbox column
	// selectPx is that column of a table with pixel widths: the width of its checkbox (#805).
	selectPx = 43
	// menuPx is the row menu column of a table with pixel widths: its 36 px button and 6 px of
	// padding on either side (#828). tables.js and app.css have the same number.
	menuPx = 48
)

// pxWidth formats a stored width (tenths of pixels) as the pixels of a <col width>.
func pxWidth(w float64) string {
	return strconv.FormatFloat(math.Round(w*10), 'f', -1, 64)
}

// tablePref is the stored layout of one table.
type tablePref struct {
	W map[string]float64 `json:"w,omitempty"` // column id -> relative width
	H []string           `json:"h,omitempty"` // hidden column ids
	V []string           `json:"v,omitempty"` // shown columns that are hidden by default
	O []string           `json:"o,omitempty"` // column order (movable columns only)
	// P: the pin side the user chose where it differs from the column's default: "left",
	// "right" or "none" (D19). A layout saved before pinning existed has no P and reads as
	// the defaults (devices: Name left, Power and Actions right).
	P map[string]string `json:"p,omitempty"`
	// B: W holds the widths of a pxTable as 0.36.1 saved them (#805): the drawn widths scaled to the
	// table's scroll floor, often narrower than their content (Address 86 px for 156 px, #828).
	// Before 0.36.1 a save stored the widths the browser drew, stretched to the frame, so a layout
	// without B or C has widths nobody chose. Only the browser knows the content widths: such a
	// layout is marked for tables.js (data-legacy), which lays it out with no column narrower than
	// its content, and its next save stores the widths with C. No migration: the stored rows stay as
	// they are until then.
	B bool `json:"b,omitempty"`
	// C: W holds the content widths of a pxTable (#828), drawn as they are: never stretched to a
	// wider frame, never scaled. The row menu has no width in it.
	C bool `json:"c,omitempty"`
}

func (p tablePref) empty() bool {
	return len(p.W) == 0 && len(p.H) == 0 && len(p.V) == 0 && len(p.O) == 0 && len(p.P) == 0
}

// side is the edge the column is pinned to for this user ("" when it is not pinned).
func (p tablePref) side(cd colDef) string {
	if cd.Fixed {
		return cd.Side
	}
	switch p.P[cd.ID] {
	case sideLeft, sideRight:
		return p.P[cd.ID]
	case sideNone:
		return ""
	}
	return cd.Side
}

// hidden says whether the column is hidden for this user: a default-off column
// until shown, any other once hidden. A locked column is never hidden.
func (p tablePref) hidden(cd colDef) bool {
	switch {
	case cd.Lock:
		return false
	case cd.Off:
		return !slices.Contains(p.V, cd.ID)
	}
	return slices.Contains(p.H, cd.ID)
}

// sortPref is the stored sort of one table (key "sort.<table>").
type sortPref struct {
	K string `json:"k,omitempty"`
	D string `json:"d,omitempty"`
}

// pagerPref is the stored page size of paged tables.
type pagerPref struct {
	Size int `json:"size,omitempty"`
}

// Preference keys: "pager" and "table.<key>".
const prefPager = "pager"

func prefKeyOf(table string) string { return "table." + table }

var errPref = fmt.Errorf("invalid preference")

// decodeStrict decodes exactly one JSON object without unknown fields.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", errPref, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing data", errPref)
	}
	return nil
}

// validatePref checks a client-sent document for key and returns the canonical
// JSON to store; "" means "nothing to store" (the preference is removed).
// Client JSON is never trusted: only known columns, clamped widths, known sizes.
func validatePref(key string, raw []byte) (string, error) {
	if len(raw) > maxPrefBytes {
		return "", fmt.Errorf("%w: too large", errPref)
	}
	switch {
	case key == prefTheme:
		return validateTheme(raw)
	case key == prefNewsSeen:
		return validateNewsSeen(raw)
	case key == prefPager:
		var p pagerPref
		if err := decodeStrict(raw, &p); err != nil {
			return "", err
		}
		if p.Size == 0 {
			return "", nil
		}
		if !validPageSize(p.Size) {
			return "", fmt.Errorf("%w: page size %d", errPref, p.Size)
		}
		return marshalPref(p)
	case strings.HasPrefix(key, "sort."):
		def, ok := tableDefs[strings.TrimPrefix(key, "sort.")]
		if !ok {
			return "", fmt.Errorf("%w: unknown table", errPref)
		}
		var p sortPref
		if err := decodeStrict(raw, &p); err != nil {
			return "", err
		}
		if p.K == "" && p.D == "" {
			return "", nil
		}
		if cd, ok := def.col(p.K); !ok || !cd.Sort {
			return "", fmt.Errorf("%w: column %q is not sortable", errPref, p.K)
		}
		if p.D != "asc" && p.D != "desc" {
			return "", fmt.Errorf("%w: direction %q", errPref, p.D)
		}
		return marshalPref(p)
	case strings.HasPrefix(key, "table."):
		def, ok := tableDefs[strings.TrimPrefix(key, "table.")]
		if !ok {
			return "", fmt.Errorf("%w: unknown table", errPref)
		}
		var p tablePref
		if err := decodeStrict(raw, &p); err != nil {
			return "", err
		}
		clean := tablePref{}
		for id, w := range p.W {
			if _, ok := def.col(id); !ok {
				return "", fmt.Errorf("%w: unknown column %q", errPref, id)
			}
			if math.IsNaN(w) || math.IsInf(w, 0) {
				return "", fmt.Errorf("%w: width of %q", errPref, id)
			}
			if cd, _ := def.col(id); fixedWidth(def.Key, cd) {
				continue // the row menu fits its button; an open page of an older version still sends it
			}
			if clean.W == nil {
				clean.W = map[string]float64{}
			}
			clean.W[id] = math.Round(math.Min(maxWeight, math.Max(minWeight, w))*10) / 10
		}
		seen := map[string]bool{}
		for _, id := range p.H {
			cd, ok := def.col(id)
			if !ok {
				return "", fmt.Errorf("%w: unknown column %q", errPref, id)
			}
			if cd.Lock {
				return "", fmt.Errorf("%w: column %q cannot be hidden", errPref, id)
			}
			if cd.Off { // hidden is its default: nothing to store
				continue
			}
			if !seen[id] {
				seen[id] = true
				clean.H = append(clean.H, id)
			}
		}
		sort.Strings(clean.H)
		for _, id := range p.V {
			cd, ok := def.col(id)
			if !ok || !cd.Off {
				return "", fmt.Errorf("%w: column %q is not hidden by default", errPref, id)
			}
			if !slices.Contains(clean.V, id) {
				clean.V = append(clean.V, id)
			}
		}
		sort.Strings(clean.V)
		for id, side := range p.P {
			cd, ok := def.col(id)
			if !ok || cd.Fixed {
				return "", fmt.Errorf("%w: column %q cannot be pinned", errPref, id)
			}
			if side != sideLeft && side != sideRight && side != sideNone {
				return "", fmt.Errorf("%w: pin side %q", errPref, side)
			}
			if side == cd.Side || (side == sideNone && cd.Side == "") {
				continue // the default needs no storage
			}
			if clean.P == nil {
				clean.P = map[string]string{}
			}
			clean.P[id] = side
		}
		for _, id := range p.O {
			cd, ok := def.col(id)
			if !ok || cd.Fixed {
				return "", fmt.Errorf("%w: column %q cannot be moved", errPref, id)
			}
			if slices.Contains(clean.O, id) {
				return "", fmt.Errorf("%w: column %q twice in the order", errPref, id)
			}
			clean.O = append(clean.O, id)
		}
		if slices.Equal(clean.O, defaultOrder(def)) {
			clean.O = nil // the default order needs no storage
		}
		// only widths of a pixel table have a size; content widths replace the ones 0.36.1 saved
		clean.C = p.C && pxTables[def.Key] && len(clean.W) > 0
		clean.B = !clean.C && p.B && pxTables[def.Key] && len(clean.W) > 0
		if clean.empty() {
			return "", nil
		}
		return marshalPref(clean)
	}
	return "", fmt.Errorf("%w: unknown key", errPref)
}

func marshalPref(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func validPageSize(n int) bool {
	for _, s := range pagerSizes {
		if s == n {
			return true
		}
	}
	return false
}

// ---- storage access ----

// userPrefs reads the signed-in user's preferences; an unreadable store only
// loses the personalisation, never the page.
func (h *handler) userPrefs(r *http.Request) map[string]string {
	if h.d.Prefs == nil {
		return nil
	}
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" {
		return nil
	}
	m, err := h.d.Prefs.UserPrefs(r.Context(), p.Subject)
	if err != nil {
		h.log.WarnContext(r.Context(), "read user preferences", "err", err)
		return nil
	}
	return m
}

// savedPageSize is the user's page size, or the default.
func savedPageSize(prefs map[string]string) int {
	var p pagerPref
	if json.Unmarshal([]byte(prefs[prefPager]), &p) == nil && validPageSize(p.Size) {
		return p.Size
	}
	return pagerSizes[0]
}

// savePref is POST /prefs/{key}: a small JSON document, validated and stored
// for the signed-in user. CSRF is enforced by the global middleware (token
// header); the body is limited before it is read.
func (h *handler) savePref(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPrefBytes))
	if err != nil {
		http.Error(w, "preference too large", http.StatusRequestEntityTooLarge)
		return
	}
	key := r.PathValue("key")
	val, err := validatePref(key, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if val == "" {
		err = h.d.Prefs.DeleteUserPref(r.Context(), p.Subject, key)
	} else {
		err = h.d.Prefs.SetUserPref(r.Context(), p.Subject, key, val)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- rendering ----

var (
	tableTagRe  = regexp.MustCompile(`<table\b[^>]*\bdata-table="([a-z_]+)"[^>]*>`)
	theadRe     = regexp.MustCompile(`(?s)<thead\b.*?</thead>`)
	thRe        = regexp.MustCompile(`(?s)<th\b[^>]*>(.*?)</th>`)
	colgroupRe  = regexp.MustCompile(`(?s)<colgroup\b.*?</colgroup>`)
	captionRe   = regexp.MustCompile(`(?s)^\s*<caption\b.*?</caption>`)
	classAttrRe = regexp.MustCompile(`\bclass="([^"]*)"`)
)

const maxHideClasses = 16 // CSS has hc1..hc16

// applyTablePrefs rewrites every <table data-table="key"> of an HTML page or
// fragment: saved widths and hidden columns, the data-cols map for the script
// and the Columns menu. Tables it cannot map (template and registry out of
// step; a test guards this) are left as they are.
func applyTablePrefs(in []byte, prefs map[string]string) []byte {
	return applyTablePrefsSorted(in, prefs, nil)
}

// applyTablePrefsSorted is applyTablePrefs for a page that also gives its
// tables sortable headers (see pageSorts); a table whose template sets its own
// data-sortbase keeps it.
func applyTablePrefsSorted(in []byte, prefs map[string]string, sorts map[string]sortLink) []byte {
	if !bytes.Contains(in, []byte(`data-table="`)) {
		return in
	}
	var out bytes.Buffer
	rest := in
	for {
		loc := tableTagRe.FindSubmatchIndex(rest)
		if loc == nil {
			out.Write(rest)
			return out.Bytes()
		}
		end := bytes.Index(rest[loc[1]:], []byte("</table>"))
		if end < 0 {
			out.Write(rest)
			return out.Bytes()
		}
		end += loc[1]
		out.Write(rest[:loc[0]])
		tag, body := string(rest[loc[0]:loc[1]]), string(rest[loc[1]:end])
		if head, newBody, menu, ok := rewriteTable(string(rest[loc[2]:loc[3]]), tag, body, prefs, sorts); ok {
			// The Columns button goes where the template marked it (its own toolbar);
			// otherwise it gets a bar above the table.
			pre := out.Bytes()
			if bodyRows(newBody) < 2 {
				// #51: nothing to choose between on a table with fewer than two rows
				menu = ""
			}
			if i := bytes.LastIndex(pre, []byte(colMenuMarker)); i >= 0 && !bytes.Contains(pre[i:], []byte("<table")) {
				fixed := append(append(append([]byte{}, pre[:i]...), menu...), pre[i+len(colMenuMarker):]...)
				out.Reset()
				out.Write(fixed)
			} else if menu != "" {
				head = strings.Replace(head, scrollOpen, `<div class="tabletools">`+menu+`</div>`+scrollOpen, 1)
			}
			out.WriteString(head + newBody + "</table></div></div>")
		} else {
			out.WriteString(tag + body + "</table>")
		}
		rest = rest[end+len("</table>"):]
	}
}

var rowRe = regexp.MustCompile(`<tr[\s>]`)

// bodyRows counts the rows of a table body after its header.
func bodyRows(body string) int {
	if i := strings.Index(body, "</thead>"); i >= 0 {
		body = body[i:]
	}
	return len(rowRe.FindAllStringIndex(body, -1))
}

var selectAllRe = regexp.MustCompile(`<input\b[^>]*data-select-all[^>]*>`)

// emptyBody reports a table with no data row: none at all, or only the "Nothing here yet"
// placeholder row (td.empty).
func emptyBody(body string) bool {
	n := bodyRows(body)
	return n == 0 || (n == 1 && strings.Contains(body, `class="empty"`))
}

// headerColumns maps the header cells of a table body to registry columns. A
// leading checkbox header (row selection) is a fixed column without an id.
func headerColumns(def tableDef, body string) ([]colDef, bool) {
	var cols []colDef
	next := 0
	for _, m := range thRe.FindAllStringSubmatch(theadRe.FindString(body), -1) {
		if strings.Contains(m[0], `type="checkbox"`) {
			cols = append(cols, colDef{})
			continue
		}
		if next >= len(def.Cols) {
			return nil, false
		}
		cols = append(cols, def.Cols[next])
		next++
	}
	required := 0
	for _, cd := range def.Cols {
		if !cd.Optional {
			required++
		}
	}
	return cols, next >= required && next > 0
}

// defaultOrder lists the movable columns of def in their default order.
func defaultOrder(def tableDef) []string {
	var ids []string
	for _, cd := range def.Cols {
		if !cd.Fixed {
			ids = append(ids, cd.ID)
		}
	}
	return ids
}

// columnOrder returns perm with perm[newPosition] = old index, and the movable columns (old
// indexes) in the user's order, which is the order the Columns menu lists them in. The table
// shows the selection column first, then the column fixed left, the columns pinned left, the
// unpinned ones, the columns pinned right and the one fixed right (D19). Within each group the
// columns follow the saved order; columns the order does not name follow in their default order.
func columnOrder(cols []colDef, pref tablePref) (perm, order []int) {
	var sel, fixL, fixR, movable []int
	for i, cd := range cols {
		switch {
		case cd.ID == "":
			sel = append(sel, i)
		case cd.Fixed && cd.Side == sideLeft:
			fixL = append(fixL, i)
		case cd.Fixed:
			fixR = append(fixR, i)
		default:
			movable = append(movable, i)
		}
	}
	taken := map[int]bool{}
	for _, id := range pref.O {
		for _, i := range movable {
			if cols[i].ID == id && !taken[i] {
				taken[i] = true
				order = append(order, i)
			}
		}
	}
	for _, i := range movable {
		if !taken[i] {
			order = append(order, i)
		}
	}
	var left, mid, right []int
	for _, i := range order {
		switch pref.side(cols[i]) {
		case sideLeft:
			left = append(left, i)
		case sideRight:
			right = append(right, i)
		default:
			mid = append(mid, i)
		}
	}
	perm = slices.Concat(sel, fixL, left, mid, right, fixR)
	return perm, order
}

var (
	trRe   = regexp.MustCompile(`(?s)(<tr\b[^>]*>)(.*?)</tr>`)
	cellRe = regexp.MustCompile(`(?s)<t[dh]\b[^>]*>.*?</t[dh]>`)
)

// permuteCells moves the cells of every row (header and body) to the new
// column order. A row with another number of cells (an empty-state row, a
// detail row spanning columns) is left alone.
func permuteCells(body string, perm []int) string {
	return trRe.ReplaceAllStringFunc(body, func(row string) string {
		m := trRe.FindStringSubmatch(row)
		cells := cellRe.FindAllString(m[2], -1)
		if len(cells) != len(perm) || strings.Contains(m[2], "colspan=") {
			return row
		}
		var b strings.Builder
		b.WriteString(m[1])
		for _, old := range perm {
			b.WriteString(cells[old])
		}
		b.WriteString("</tr>")
		return b.String()
	})
}

func pct(v float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".") + "%"
}

// tagAttr reads an attribute value from an opening tag ("" when absent).
func tagAttr(tag, name string) string {
	m := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// sortHrefP is the link of a sortable header: the page's own URL (without the
// sort parameters, filters kept) plus this column; the current sort column
// flips its direction.
func sortHrefP(base, keyParam, dirParam, id, curKey, curDir string) string {
	dir := "asc"
	if id == curKey && curDir == "asc" {
		dir = "desc"
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
		if strings.HasSuffix(base, "?") || strings.HasSuffix(base, "&") {
			sep = ""
		}
	}
	return base + sep + keyParam + "=" + id + "&" + dirParam + "=" + dir
}

// decorateHeaders gives every header cell its one-sentence tooltip and turns
// the sortable ones into sort links (plain links: no script needed).
func decorateHeaders(key, lead string, cols []colDef, tag, body string, empty bool) string {
	base, curKey, curDir := tagAttr(tag, "data-sortbase"), tagAttr(tag, "data-sort"), tagAttr(tag, "data-dir")
	kp, dp := sortParams(lead)
	n := -1
	return theadRe.ReplaceAllStringFunc(body, func(head string) string {
		return thRe.ReplaceAllStringFunc(head, func(th string) string {
			n++
			if strings.Contains(th, `type="checkbox"`) {
				if empty { // select all of nothing
					return selectAllRe.ReplaceAllString(th, "")
				}
				return th
			}
			if n >= len(cols) || cols[n].ID == "" || cols[n].ID == spareID {
				return th
			}
			cd := cols[n]
			open := strings.Index(th, ">")
			opening, inner := th[:open], th[open+1:len(th)-len("</th>")]
			if tip := colTips[key+"."+cd.ID]; tip != "" && !strings.Contains(opening, " title=") {
				opening += ` title="` + html.EscapeString(tip) + `"`
			}
			if cd.Sort && base != "" && !empty {
				cls, aria := "sortlink", "none"
				if cd.ID == curKey {
					cls += " is-" + curDir
					aria = map[string]string{"asc": "ascending", "desc": "descending"}[curDir]
				}
				opening += ` aria-sort="` + aria + `"`
				inner = `<a class="` + cls + `" href="` + html.EscapeString(sortHrefP(base, kp, dp, cd.ID, curKey, curDir)) + `">` + inner + `<span class="sortmark" aria-hidden="true"></span></a>`
			}
			return opening + ">" + inner + "</th>"
		})
	})
}

var colRe = regexp.MustCompile(`<col\b[^>]*>`)

// insertCol puts tag before the n-th <col> (0-based) of the first colgroup, or at its end.
func insertCol(body string, n int, tag string) string {
	loc := colgroupRe.FindStringIndex(body)
	if loc == nil {
		return body
	}
	g := body[loc[0]:loc[1]]
	at := len(g) - len("</colgroup>")
	if cs := colRe.FindAllStringIndex(g, -1); n < len(cs) {
		at = cs[n][0]
	}
	return body[:loc[0]] + g[:at] + tag + g[at:] + body[loc[1]:]
}

// insertSpare gives every row of n cells (header and body) the empty cell of a pixel table at place at
// (#828): the room a frame has beyond the table's own width. A row with another number of cells (an
// empty-state row, a detail row spanning columns) is left alone, as permuteCells does.
func insertSpare(body string, at, n int) string {
	return trRe.ReplaceAllStringFunc(body, func(row string) string {
		m := trRe.FindStringSubmatch(row)
		cells := cellRe.FindAllString(m[2], -1)
		if len(cells) != n || strings.Contains(m[2], "colspan=") {
			return row
		}
		cell := `<td class="spare" aria-hidden="true"></td>`
		if strings.HasPrefix(cells[0], "<th") {
			cell = `<th class="spare" aria-hidden="true"></th>`
		}
		return m[1] + strings.Join(slices.Insert(cells, at, cell), "") + "</tr>"
	})
}

// dropCol removes the n-th <col> (0-based) of the first colgroup.
func dropCol(body string, n int) string {
	return colgroupRe.ReplaceAllStringFunc(body, func(g string) string {
		i := -1
		return colRe.ReplaceAllStringFunc(g, func(c string) string {
			if i++; i == n {
				return ""
			}
			return c
		})
	})
}

// colMenuMarker in a template marks where the Columns button goes: in the
// page's own toolbar, so both share one row. Without it the button gets a bar
// of its own above the table.
const colMenuMarker = `<i class="colslot"></i>` // not a comment: html/template drops those

// scrollOpen opens the box a table scrolls sideways in when it is wider than the page (its pinned
// columns stick to the edges, D19); the Columns bar sits above it, so the open chooser is never
// clipped by the scrolling box.
const scrollOpen = `<div class="tablescroll">`

func rewriteTable(key, tag, body string, prefs map[string]string, sorts map[string]sortLink) (head, newBody, menu string, ok bool) {
	def, found := tableDefs[key]
	if !found {
		return "", "", "", false
	}
	cols, mapped := headerColumns(def, body)
	if !mapped || len(cols) > maxHideClasses {
		return "", "", "", false
	}
	lead := key // the table whose layout and sort this one has
	if f := tableFollows[key]; f != "" {
		lead = f
	}
	var pref tablePref
	if raw := prefs[prefKeyOf(lead)]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &pref) // a corrupt row just means defaults
	}

	perm, order := columnOrder(cols, pref)
	reordered := false
	for i, old := range perm {
		reordered = reordered || i != old
	}
	orig := cols
	if reordered {
		moved := make([]colDef, len(cols))
		for i, old := range perm {
			moved[i] = cols[old]
		}
		cols = moved
		body = permuteCells(body, perm)
	}
	px := pxTables[key]
	spareAt := -1 // the empty cell of a pixel table: before the columns pinned right (#828)
	if px && len(cols) < maxHideClasses {
		spareAt = len(cols)
		for i, cd := range cols {
			if cd.ID != "" && pref.side(cd) == sideRight {
				spareAt = i
				break
			}
		}
		body = insertSpare(body, spareAt, len(cols))
		cols = slices.Insert(slices.Clone(cols), spareAt, colDef{ID: spareID})
	}

	classes := []string{}
	ids := make([]string, len(cols)) // "id:default weight[:off][:l|:r][:fix]" for the script; "" for a checkbox column, spareID for the spare one
	var colgroup strings.Builder
	fixed, total := 0.0, 0.0
	// a pixel table: the pixels of its data columns, and how many row menus it has (menuPx each)
	dataPx, menus, ownWidths := 0.0, 0, pref.C
	custom := reordered // the static <colgroup> follows the default order
	dropped := []int{}  // default-off columns that stay off: their <col> goes
	for i, cd := range cols {
		if cd.ID == spareID {
			ids[i] = spareID
			continue
		}
		if cd.ID == "" {
			// the selection column is always pinned left: its checkbox stays with the row
			classes = append(classes, "pl"+strconv.Itoa(i+1))
			fixed = selectShare
			continue
		}
		ids[i] = cd.ID + ":" + strconv.FormatFloat(cd.Weight, 'f', -1, 64)
		if cd.Off {
			ids[i] += ":off"
		}
		switch pref.side(cd) {
		case sideLeft:
			ids[i] += ":l"
		case sideRight:
			ids[i] += ":r"
		}
		if cd.Fixed {
			ids[i] += ":fix"
		}
		if pref.hidden(cd) {
			classes = append(classes, "hc"+strconv.Itoa(i+1))
			if cd.Off {
				dropped = append(dropped, i)
			} else {
				custom = true
			}
			continue
		}
		switch pref.side(cd) { // pinned columns stick to their edge when the table scrolls sideways (CSS plN, prN)
		case sideLeft:
			classes = append(classes, "pl"+strconv.Itoa(i+1))
		case sideRight:
			classes = append(classes, "pr"+strconv.Itoa(i+1))
		}
		total += colWeight(cd, pref)
		if fixedWidth(key, cd) {
			menus++
			continue // no layout sets its width
		}
		dataPx += math.Round(colWeight(cd, pref) * 10)
		if _, set := pref.W[cd.ID]; set || cd.Off {
			custom = true
		}
	}
	if !pref.C {
		// widths saved before #828 are no content widths; without any, the layout has the default widths
		ownWidths = !slices.ContainsFunc(cols, func(cd colDef) bool {
			_, set := pref.W[cd.ID]
			return set && !fixedWidth(key, cd)
		})
	}
	var orderIDs []string
	for _, i := range order {
		orderIDs = append(orderIDs, orig[i].ID)
	}
	menuB := columnsMenu(orig, order, pref)
	if custom {
		classes = append(classes, "tfix")
		colgroup.WriteString("<colgroup>")
		for _, cd := range cols {
			switch {
			case cd.ID == spareID:
				colgroup.WriteString(`<col class="spare">`) // no width: it takes what the frame has beyond the columns
			case cd.ID == "" && px:
				colgroup.WriteString(`<col width="` + strconv.Itoa(selectPx) + `">`)
			case cd.ID == "":
				colgroup.WriteString(`<col width="` + pct(selectShare) + `">`)
			case pref.hidden(cd):
				// no <col>: a hidden cell takes no column
			case fixedWidth(key, cd):
				colgroup.WriteString(`<col data-id="` + cd.ID + `" width="` + strconv.Itoa(menuPx) + `">`)
			case px:
				colgroup.WriteString(`<col data-id="` + cd.ID + `" width="` + pxWidth(colWeight(cd, pref)) + `">`)
			default:
				colgroup.WriteString(`<col data-id="` + cd.ID + `" width="` + pct(colWeight(cd, pref)/total*(100-fixed)) + `">`)
			}
		}
		colgroup.WriteString("</colgroup>")
		body = colgroupRe.ReplaceAllString(body, "")
	} else {
		// the static <colgroup> has no <col> for the spare cell: the dropped columns go first (their
		// places do not count it), then it gets one where its cells are
		before := 0
		for k := len(dropped) - 1; k >= 0; k-- {
			at := dropped[k]
			if spareAt >= 0 && at > spareAt {
				at--
			} else if spareAt >= 0 {
				before++
			}
			body = dropCol(body, at)
		}
		if spareAt >= 0 {
			body = insertCol(body, spareAt-before, `<col class="spare">`)
		}
	}
	// A table whose template names no sort gets the page's own (tablesort.go: pageSorts).
	if sl, has := sorts[lead]; has && !strings.Contains(tag, "data-sortbase=") {
		tag = strings.Replace(tag, "<table", `<table data-sortbase="`+html.EscapeString(sl.Base)+`" data-sort="`+sl.Key+`" data-dir="`+sl.Dir+`"`, 1)
	}
	// A table with nothing in it offers nothing to sort, select or hide: its header goes (CSS).
	empty := emptyBody(body)
	if empty {
		classes = append(classes, "is-empty")
	}
	body = decorateHeaders(key, lead, cols, tag, body, empty)

	attrs := ` data-cols="` + strings.Join(ids, ",") + `" data-order="` + strings.Join(orderIDs, ",") + `"`
	if px {
		attrs += ` data-px`
		switch {
		case custom && ownWidths:
			// the table's own width: the selection column at its checkbox, the data columns, the row menus
			w := dataPx + float64(menus*menuPx)
			if fixed > 0 {
				w += selectPx
			}
			attrs += ` data-basew="` + strconv.Itoa(int(w)) + `"`
		case custom && pref.B:
			attrs += ` data-legacy="px"` // 0.36.1: the layout's pixels, scaled below their content (#828)
		case custom:
			attrs += ` data-legacy="shares"` // before 0.36.1: widths stretched to a frame (#805)
		}
	}
	if lead != key {
		attrs = ` data-follows="` + lead + `"`
	}
	if len(classes) > 0 {
		add := strings.Join(classes, " ")
		if classAttrRe.MatchString(tag) {
			tag = classAttrRe.ReplaceAllString(tag, `class="$1 `+add+`"`)
		} else {
			attrs += ` class="` + add + `"`
		}
	}
	tag = strings.Replace(tag, "<table", "<table"+attrs, 1)

	menu = `<details class="colmenu" data-key="` + key + `"><summary class="colbtn" aria-label="Columns">Columns</summary>` +
		`<div class="colpanel" role="group" aria-label="Columns">` + menuB + `</div></details>`
	if lead != key || empty || len(def.Cols) < 3 || strings.Contains(tag, " data-nomenu") {
		// a mirror has its leader's menu, an empty table has nothing to choose, with fewer than
		// three columns there is nothing to choose or move (Device and Result of a bulk preview),
		// and a confirmation table is no list to rearrange (data-nomenu)
		menu = ""
	}
	head = `<div class="tablewrap" data-key="` + key + `">` + scrollOpen + tag
	// the caption names the table and is its first child: the colgroup this function wrote goes after it
	if caption := captionRe.FindString(body); caption != "" {
		return head, caption + colgroup.String() + body[len(caption):], menu, true
	}
	return head, colgroup.String() + body, menu, true
}

func colWeight(cd colDef, p tablePref) float64 {
	if w, ok := p.W[cd.ID]; ok {
		return w
	}
	return cd.Weight
}

// columnsMenu is the Columns chooser of the prototype (D19): the column fixed left on top, then
// every movable column in the user's order with its drag grip, show/hide box, Pin left, Pin right
// and the move buttons, the column fixed right at the bottom, and Reset to default. A column the
// user cannot hide (Lock) shows a ticked, disabled box.
func columnsMenu(cols []colDef, order []int, pref tablePref) string {
	var b strings.Builder
	b.WriteString(`<p class="colhint">Drag ⋮⋮ or use ↑ ↓ to reorder. Drag a header edge to resize; double-click it to fit. Saved for you, for this table.</p>`)
	fixedRow := func(cd colDef) {
		note := map[string]string{sideLeft: "Pinned left · always", sideRight: "Pinned right · always"}[cd.Side]
		b.WriteString(`<div class="colrow is-fixed"><span class="colgrip" aria-hidden="true"></span><label><input type="checkbox" checked disabled> ` +
			html.EscapeString(cd.Label) + `</label><span class="colfix">` + note + `</span></div>`)
	}
	for _, cd := range cols {
		if cd.Fixed && cd.Side == sideLeft {
			fixedRow(cd)
		}
	}
	for n, i := range order {
		cd := cols[i]
		label := html.EscapeString(cd.Label)
		b.WriteString(`<div class="colrow" draggable="true" data-colrow="` + cd.ID + `"><span class="colgrip" aria-hidden="true">⋮⋮</span>`)
		if cd.Lock {
			b.WriteString(`<label><input type="checkbox" checked disabled> ` + label + `</label>`)
		} else {
			b.WriteString(`<label><input type="checkbox" data-colid="` + cd.ID + `"` + map[bool]string{true: " checked", false: ""}[!pref.hidden(cd)] + `> ` + label + `</label>`)
		}
		side := pref.side(cd)
		for _, s := range []string{sideLeft, sideRight} {
			b.WriteString(`<button type="button" class="colpin" data-pin="` + s + `" data-pinid="` + cd.ID + `" aria-pressed="` +
				strconv.FormatBool(side == s) + `" title="Pin ` + label + ` to the ` + s + `">Pin ` + s + `</button>`)
		}
		move := func(dir, arrow string, disabled bool) string {
			return `<button type="button" class="colmove" data-move="` + dir + `" data-moveid="` + cd.ID + `" aria-label="Move ` + label + ` ` + dir + `"` +
				map[bool]string{true: " disabled", false: ""}[disabled] + `>` + arrow + `</button>`
		}
		b.WriteString(move("up", "↑", n == 0) + move("down", "↓", n == len(order)-1) + `</div>`)
	}
	for _, cd := range cols {
		if cd.Fixed && cd.Side == sideRight {
			fixedRow(cd)
		}
	}
	b.WriteString(`<div class="colfoot"><span>Pinned columns keep their side when you scroll sideways.</span>` +
		`<button type="button" class="colreset" data-cols-reset>Reset to default</button></div>`)
	return b.String()
}
