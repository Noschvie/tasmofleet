package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/netaddr"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Discovery is what the discovery pages need (*discovery.Manager).
type Discovery interface {
	discovery.Service
	// AcceptVersion applies the proposal only if it still has the shown version.
	AcceptVersion(ctx context.Context, actor string, id int64, version string) (model.DeviceID, error)
	// Ignored lists the dismissed proposals; Undismiss makes one visible again.
	Ignored(ctx context.Context) ([]discovery.Proposal, error)
	Undismiss(ctx context.Context, actor string, id int64, version string) error
	Scan(id discovery.ScanID) (discovery.ScanStatus, bool)
	Scans() []discovery.ScanStatus
	// Unidentified lists the topics seen on MQTT that are not a proposal (spec 11.3a).
	Unidentified() []discovery.Unidentified
	// Explain says why the device behind a MAC, IP address or topic is not offered as new (#713).
	// It reads only; it sends nothing to a device or the broker.
	Explain(ctx context.Context, query string) (discovery.Explanation, error)
}

func (h *handler) registerDiscovery(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op, adm func(http.Handler) http.Handler) {
	_ = op // discovery is admin only: proposals expose addresses and accept changes the inventory
	if h.d.Discovery == nil {
		return
	}
	route("GET /discovery/scan", adm, h.discoveryScanLatest)
	route("POST /discovery/scan", adm, h.discoveryScanStart)
	route("GET /discovery/scan/{id}", adm, h.discoveryScanPage)
	route("GET /discovery/scan/{id}/stream", adm, h.discoveryScanStream)
	route("POST /discovery/scan/{id}/cancel", adm, h.discoveryScanCancel)
	route("POST /discovery/proposals/{id}/accept", adm, h.discoveryAccept)
	route("POST /discovery/proposals/{id}/dismiss", adm, h.discoveryDismiss)
	route("POST /discovery/proposals/{id}/undismiss", adm, h.discoveryUndismiss)
	route("POST /discovery/proposals/accept", adm, h.discoveryAcceptSelected)
	route("POST /discovery/proposals/dismiss", adm, h.discoveryDismissSelected)
}

// OpenCount is what the table's heading counts: the new devices and the known devices at a new address or in a
// conflict that wait for a decision (the ignored rows listed after them are counted by Show ignored).
func (v discView) OpenCount() int {
	return v.NewCount + openMoves(v.Moves) + openMoves(v.Conflicts)
}

// discView is the Discovered box: one table of the address changes and conflicts a known device reported
// and the new devices (and, with ?ignored, the ignored ones), with selection, Ignore and Add.
type discView struct {
	Base, CSRF string
	// Off: MQTT discovery is switched off (or there is no broker), only scans find devices.
	Off       bool
	Settings  string // the settings page of discovery ("" when there is none)
	Moves     []moveRow
	Conflicts []moveRow
	Rows      []propRow
	NewCount  int
	Ignored   int
	ShowIgn   bool
	// State is the shown filter (all, online, offline), Sort and Dir the table's order (#130).
	State, Sort, Dir string
	Chips            []addChip
	// Empty: there are devices, but the filter hides all of them.
	Filtered bool
	// HasDefault: accepting uses the fleet default device password (N20, MAC proof required).
	HasDefault bool
	// LastAnnounce is the newest MQTT announcement (the empty state's "Last announcement").
	LastAnnounce     time.Time
	LastAnnounceText string // the moment as a day stamp: an announcement can be days old
	Error            string
	// Unverified is the stronger confirmation for "Apply" when the new address could not
	// prove the device's MAC.
	Unverified *unverifiedMove
	// Unident are the topics MQTT saw that are no proposal yet, with the reason.
	Unident []moveRow
	// Why is the answer of the lookup (#713), nil without ?why=; WhyQuery is the text in its field.
	Why      *whyView
	WhyQuery string
	// ProposalDays is the number of days after which an ignored device that was not seen is forgotten.
	ProposalDays int
	// Added is how many devices "Add N selected" added (its banner, with a link to the list); NeedPassword are the
	// rows it left because they need their web password, each with the link to its filled-in form (#1110).
	Added        int
	NeedPassword []needPasswordRow
}

// needPasswordRow is a device "Add N selected" did not add because it asks for its web password.
type needPasswordRow struct {
	ID         int64
	Name, Href string
}

// needPasswordPath is the address form filled from proposal id with the sentence that it needs its web password:
// where Add goes for such a device (#1110), and the link of its row in the "Add N selected" banner. It has no
// #card-manual: the password field takes the focus (autofocus), which a browser skips on a page opened at a fragment.
func needPasswordPath(id int64) string {
	return "/devices/new?proposal=" + strconv.FormatInt(id, 10) + "&need=password"
}

// fromAddPage says whether an accept was posted by the Add page (its form carries from=add) and not by the
// dashboard's Needs attention, which posts only the token and the version: an added device opens from the Add page,
// the dashboard stays where it is.
func fromAddPage(r *http.Request) bool { return r.PostFormValue("from") == "add" }

// propRow is one discovered device of the New devices table.
type propRow struct {
	ID             int64
	Version        string
	Name, Hostname string // the device name and the hostname it reported (the phone card's line)
	Topic          string // its MQTT topic, the name cell's tooltip
	Address        string
	Source         string
	MQTT           bool
	Last           time.Time
	LastText       string
	Ignored        bool
	Offline        bool // its MQTT LWT says Offline: it is added as seen, without a probe
	OfflineSince   time.Time
	OfflineText    string // "Offline since 2 h ago" style words, "" when not offline or the moment is unknown
}

// moveRow is a known device that announced itself at a new address (or a conflict): a row of the
// Discovered table. The same type carries the topics MQTT saw but could not identify (Unident).
type moveRow struct {
	ID       int64
	Version  string
	Name     string
	Href     string // the stored device
	Seen     time.Time
	SeenText string // "4 min ago · MQTT discovery" (the unidentified topics)
	Source   string // "MQTT discovery", "Network scan": the Source column of the table
	LastText string // "4 min ago": the Last seen column
	Old, New string
	// Why is the row's own exception (a stored hostname or password, a refusal). What holds for every
	// row of its kind is said once above the table (discView.Help).
	Why      string
	CanApply bool
	Ignored  bool // ignored (shown with "Show ignored"): the only action is Show again
	Conflict bool // a conflict, not an address change
}

// The sentences above the Discovered table, one per kind of row that is listed (not ignored). A row
// repeats none of them.
const (
	moveHelp     = "Known devices marked New address announced themselves at a new IP and can’t be moved automatically. The new address must answer with the device’s MAC before TasmoFleet uses it."
	conflictHelp = "A Conflict means another device claims the address or MQTT topic of a known device. Nothing is changed until you resolve it in the inventory (edit or delete the stored device), then ignore it."
)

// Help says once what the rows of the table need explained: that Add uses the default password (when one is
// set), what a new address is, what a conflict is. Only kinds that have an open row are explained. A device that
// needs its own web password needs no sentence here: its Add opens the form for it (#1110).
func (v discView) Help() []string {
	var out []string
	if v.HasDefault && slices.ContainsFunc(v.Rows, func(r propRow) bool { return !r.Ignored }) {
		out = append(out, "Add uses the default device password from Settings.")
	}
	if openMoves(v.Moves) > 0 {
		out = append(out, moveHelp)
	}
	if openMoves(v.Conflicts) > 0 {
		out = append(out, conflictHelp)
	}
	return out
}

// HasTable says whether the table has a row: a new device, or a known device that reported something.
func (v discView) HasTable() bool { return len(v.Rows) > 0 || len(v.Moves) > 0 || len(v.Conflicts) > 0 }

// ignoredShown reads the "Show ignored" switch: any value turns it on. The checkbox of the GET form
// sends "on", the links and the hidden fields of the POST forms send "1".
func ignoredShown(q url.Values) bool { return q.Get("ignored") != "" }

// discoveryOff reports whether MQTT discovery cannot find devices: switched off in the
// settings, or no broker to listen on.
func (h *handler) discoveryOff() bool {
	if h.d.MQTT() == "disabled" {
		return true
	}
	if h.d.SysConfig != nil {
		for _, f := range h.d.SysConfig.State() {
			if f.Key == "DISCOVERY_MQTT" {
				return f.Value == "false"
			}
		}
	}
	return false
}

func (h *handler) discoveryData(r *http.Request) discView {
	q := listParams(r)
	v := discView{ShowIgn: ignoredShown(q), Off: h.discoveryOff(), ProposalDays: proposalDays()}
	if h.d.SysConfig != nil {
		v.Settings = httpBase(r) + sysPath + "?section=discovery"
	}
	v.HasDefault = h.d.DefaultDevicePassword != nil && h.d.DefaultDevicePassword() != ""
	now := h.d.Clock.Now()
	props, err := h.d.Discovery.Proposals(r.Context())
	if err != nil {
		v.Error = "The discovered devices could not be loaded."
		h.log.ErrorContext(r.Context(), "discovery proposals", "err", err)
	}
	base := httpBase(r)
	for _, p := range props {
		if p.Via == discovery.ViaMQTT && p.SeenAt.After(v.LastAnnounce) {
			v.LastAnnounce = p.SeenAt
		}
		switch p.Kind {
		case discovery.KindNew:
			v.NewCount++
			v.Rows = append(v.Rows, h.propRow(now, p, false))
		default:
			v.addMove(h.moveRowOf(r.Context(), base, now, p, false))
		}
	}
	ignored, err := h.d.Discovery.Ignored(r.Context())
	if err != nil {
		v.Error = "The ignored devices could not be loaded."
		h.log.ErrorContext(r.Context(), "discovery ignored", "err", err)
	}
	for _, p := range ignored {
		if p.Via == discovery.ViaMQTT && p.SeenAt.After(v.LastAnnounce) {
			v.LastAnnounce = p.SeenAt // an ignored device still announced itself
		}
		v.Ignored++ // every kind: a dismissed address change or conflict is restored here too (#714)
		switch {
		case !v.ShowIgn:
		case p.Kind == discovery.KindNew:
			v.Rows = append(v.Rows, h.propRow(now, p, true))
		default:
			v.addMove(h.moveRowOf(r.Context(), base, now, p, true))
		}
	}
	if !v.LastAnnounce.IsZero() {
		v.LastAnnounceText = ui.StampRelativeText(now, v.LastAnnounce)
	}
	for _, u := range h.d.Discovery.Unidentified() {
		state := "online"
		if !u.Online {
			state = "offline"
		}
		addr := u.IP
		if u.Hostname != "" && addr != "" {
			addr = u.Hostname + " · " + addr
		} else if addr == "" {
			addr = u.Hostname
		}
		v.Unident = append(v.Unident, moveRow{Name: u.Topic, Seen: u.SeenAt, SeenText: agoWords(now, u.SeenAt) + " · " + state, New: addr, Why: capitalize(u.Reason) + "."})
	}
	if text := strings.TrimSpace(r.URL.Query().Get("why")); text != "" && r.Method == http.MethodGet {
		v.WhyQuery, v.Why = text, h.whyOf(r, base, text, v.Off)
	}
	v.State = discState(q)
	v.Sort, v.Dir = discSort(q)
	// the chips count what the ignored switch shows; the filter then narrows the rows
	counts := map[string]int{stateAll: len(v.Rows)}
	for _, row := range v.Rows {
		if row.Offline {
			counts[stateOffline]++
		} else {
			counts[stateOnline]++
		}
	}
	// The filter is worth a row of chips only while the list holds both states (or one is chosen,
	// so it can be left again): four online devices need none (#410).
	if v.State != stateAll || (counts[stateOnline] > 0 && counts[stateOffline] > 0) {
		for _, c := range []struct{ key, label string }{{stateAll, "All"}, {stateOnline, "Online"}, {stateOffline, "Offline"}} {
			v.Chips = append(v.Chips, addChip{Label: c.label, N: counts[c.key], Current: v.State == c.key,
				Href: discHref(base, c.key, v.Sort, v.Dir, v.ShowIgn)})
		}
	}
	if v.State != stateAll {
		kept := v.Rows[:0:0]
		for _, row := range v.Rows {
			if row.Offline == (v.State == stateOffline) {
				kept = append(kept, row)
			}
		}
		v.Filtered = len(v.Rows) > 0 && len(kept) == 0
		v.Rows = kept
	}
	sortRows(v.Rows, v.Sort, v.Dir)
	return v
}

// addMove files a row under Address changed or Conflicts.
func (v *discView) addMove(m moveRow) {
	if m.Conflict {
		v.Conflicts = append(v.Conflicts, m)
	} else {
		v.Moves = append(v.Moves, m)
	}
}

// moveRowOf is the row of an address change or a conflict; an ignored one can only be restored.
func (h *handler) moveRowOf(ctx context.Context, base string, now time.Time, p discovery.Proposal, ignored bool) moveRow {
	src := viaText(p.Via)
	m := moveRow{ID: p.ID, Version: p.Version, Name: p.Device.Name, Seen: p.SeenAt, Ignored: ignored, Conflict: p.Kind == discovery.KindConflict,
		SeenText: agoWords(now, p.SeenAt) + " · " + src, Source: strings.ToUpper(src[:1]) + src[1:], LastText: agoWords(now, p.SeenAt), New: netaddr.Display(p.Device.Address, p.Device.Port, "http"),
		CanApply: p.Kind == discovery.KindAddressChange && !ignored}
	if p.ExistingID != 0 {
		if d, err := h.d.Inventory.Get(ctx, p.ExistingID); err == nil {
			m.Name, m.Href, m.Old = d.Name, base+"/devices/"+strconv.FormatInt(int64(d.ID), 10), netaddr.Display(d.Address, d.Port, "http")
			m.Why = moveWhy(p, d)
			if p.Kind == discovery.KindAddressChange {
				if merr := discovery.MoveRefusal(d); merr != nil {
					m.CanApply = false
					m.Why = capitalize(strings.TrimPrefix(merr.Error(), discovery.ErrNotAcceptable.Error()+": ")) + ". Change it in the device edit form, then ignore this."
				}
			}
		}
	}
	if p.Kind == discovery.KindConflict {
		m.Why = "" // conflictHelp says it once above the table
	}
	return m
}

func (h *handler) propRow(now time.Time, p discovery.Proposal, ignored bool) propRow {
	row := propRow{ID: p.ID, Version: p.Version, Name: p.Device.Name, Hostname: p.Device.Net.Hostname, Topic: p.Device.Topic,
		Address: netaddr.Display(p.Device.Address, p.Device.Port, "http"),
		Source:  viaText(p.Via), MQTT: p.Via == discovery.ViaMQTT, Last: p.SeenAt,
		LastText: agoWords(now, p.SeenAt), Ignored: ignored, Offline: p.LWT == "Offline"}
	if row.Offline && !p.OfflineSince.IsZero() {
		row.OfflineSince, row.OfflineText = p.OfflineSince, agoWords(now, p.OfflineSince)
	}
	row.Source = strings.ToUpper(row.Source[:1]) + row.Source[1:]
	return row
}

// Label names the row where a single name is needed (a checkbox's label): the device name, else
// the hostname, else the topic.
func (r propRow) Label() string {
	return firstNonEmptyStr(r.Name, r.Hostname, r.Topic)
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// addChip is one state filter of the New devices list.
type addChip struct {
	Label   string
	N       int
	Href    string
	Current bool
}

// The states of the filter and the sort keys of the New devices table (#130).
const (
	stateAll     = "all"
	stateOnline  = "online"
	stateOffline = "offline"
)

var discSortKeys = map[string]bool{"name": true, "ip": true, "source": true, "last": true}

// discSort reads ?sort= and ?dir=: an unknown key falls back to the default, the newest
// announcement first.
func discSort(q url.Values) (key, dir string) {
	key, dir = q.Get("sort"), q.Get("dir")
	if !discSortKeys[key] {
		return "last", "desc"
	}
	if dir != "asc" && dir != "desc" {
		dir = "asc"
	}
	return key, dir
}

func discState(q url.Values) string {
	switch s := q.Get("state"); s {
	case stateOnline, stateOffline:
		return s
	}
	return stateAll
}

// listParams are the list's own parameters (state, sort, dir, ignored): from the URL, and on a
// POST from the form's hidden fields, so Add, Ignore and Show again leave the list as it was.
func listParams(r *http.Request) url.Values {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm() // a bad form just means the defaults
		for _, k := range []string{"state", "sort", "dir", "ignored"} {
			if v := r.PostForm.Get(k); v != "" {
				q.Set(k, v)
			}
		}
	}
	return q
}

// discBack is where an action returns to: the list with its filter and sort, and the result.
func discBack(r *http.Request, ok string) string {
	q := listParams(r)
	key, dir := discSort(q)
	href := discHref("", discState(q), key, dir, ignoredShown(q))
	if strings.Contains(href, "?") {
		return href + "&ok=" + ok
	}
	return href + "?ok=" + ok
}

// discQuery is the list's own parameters as they go into a URL or a form: the state filter and the sort
// when they differ from the default, and the ignored switch. Links (discHref) and the hidden fields of the
// Show ignored form (discFields) both come from it, so the two cannot drift.
func discQuery(state, sortKey, dir string, ignored bool) url.Values {
	q := url.Values{}
	if state != stateAll {
		q.Set("state", state)
	}
	if sortKey != "last" || dir != "desc" {
		q.Set("sort", sortKey)
		q.Set("dir", dir)
	}
	if ignored {
		q.Set("ignored", "1")
	}
	return q
}

// discHref is the Add page's URL with the given filter, sort and ignored switch.
func discHref(base, state, sortKey, dir string, ignored bool) string {
	q := discQuery(state, sortKey, dir, ignored)
	if len(q) == 0 {
		return base + "/devices/new"
	}
	return base + "/devices/new?" + q.Encode()
}

// discFields are the hidden fields of the Show ignored form: the filter and the sort the list has now,
// so ticking the box changes nothing else. The box itself is the form's own field.
func discFields(p discView) [][2]string {
	q := discQuery(p.State, p.Sort, p.Dir, false)
	var out [][2]string
	for _, k := range []string{"state", "sort", "dir"} {
		if v := q.Get(k); v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

// sortRows orders the rows by key: the ignored ones stay after the new ones, equal values keep
// the order they were discovered in (the ID), whatever the direction.
func sortRows(rows []propRow, key, dir string) {
	less := func(a, b propRow) int {
		switch key {
		case "name":
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case "ip":
			return compareAddr(a.Address, b.Address)
		case "source":
			return strings.Compare(a.Source, b.Source)
		}
		return a.Last.Compare(b.Last)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Ignored != b.Ignored {
			return !a.Ignored
		}
		c := less(a, b)
		if dir == "desc" {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
		return a.ID < b.ID
	})
}

// compareAddr orders addresses numerically (10.0.0.9 before 10.0.0.10); what is no IP sorts by text.
func compareAddr(a, b string) int {
	pa, ea := netip.ParseAddrPort(a)
	pb, eb := netip.ParseAddrPort(b)
	if ea == nil && eb == nil {
		if c := pa.Addr().Compare(pb.Addr()); c != 0 {
			return c
		}
		return int(pa.Port()) - int(pb.Port())
	}
	if aa, err := netip.ParseAddr(a); err == nil {
		if bb, err := netip.ParseAddr(b); err == nil {
			return aa.Compare(bb)
		}
	}
	return strings.Compare(a, b)
}

// moveWhy says why discovery left this address change to an admin when it is not the usual reason
// (the new address must prove the MAC, which moveHelp says once for all rows); "" for the usual one.
func moveWhy(p discovery.Proposal, d model.Device) string {
	switch {
	case !isIPAddress(d.Address):
		return "It has a stored hostname, so TasmoFleet won’t switch the address on its own."
	case d.HasCredentials():
		return "It has a stored web password, so TasmoFleet won’t move it on its own. Apply sends the password to the new address."
	}
	return ""
}

func isIPAddress(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

func (h *handler) discoveryRender(w http.ResponseWriter, r *http.Request, status int, p discView) {
	h.renderAdd(w, r, status, addManualView{}, &p)
}

// parseCIDRs reads ranges separated by whitespace or commas. A bare address
// is a /32 (or /128).
func parseCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p)
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not a range like 192.168.1.0/24 or an IP address", f)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	if len(out) > 32 {
		return nil, errors.New("too many ranges (at most 32)")
	}
	return out, nil
}

func (h *handler) discoveryScanStart(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	raw := r.PostFormValue("cidrs")
	fail := func(status int, msg string) {
		t := h.scanTabData(r, nil)
		t.Body.CIDRs, t.Body.Error = raw, msg
		h.scanRender(w, r, status, t)
	}
	cidrs, err := parseCIDRs(raw)
	if err == nil && len(cidrs) == 0 {
		err = errors.New("enter a range like 192.168.1.0/24")
	}
	if err != nil {
		fail(http.StatusUnprocessableEntity, capitalize(err.Error())+".")
		return
	}
	id, err := h.d.Discovery.StartScan(r.Context(), actor(r), cidrs)
	switch {
	case err == nil:
		redirect(w, r, "/discovery/scan/"+string(id))
	case errors.Is(err, discovery.ErrScanRange):
		fail(http.StatusUnprocessableEntity, scanRangeProblem(err))
	case errors.Is(err, discovery.ErrScanRunning):
		fail(http.StatusConflict, "A scan is already running. Stop it first.")
	default:
		h.fail(w, r, err)
	}
}

// scanRangeProblem is the sentence under the Ranges field for a refused scan:
// the service's detail ("10.99.0.0/24 is outside the device networks") without
// its package prefix, as a sentence.
func scanRangeProblem(err error) string {
	detail := strings.TrimPrefix(err.Error(), discovery.ErrScanRange.Error()+": ")
	return capitalize(detail) + "."
}

// capitalize upper-cases the first letter of a message that starts lower case.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// discoveryScanStream pushes the scan status as an HTML fragment whenever it
// changes. The last fragment has no stream attributes, so the browser drops
// the connection instead of reconnecting.
func (h *handler) discoveryScanStream(w http.ResponseWriter, r *http.Request) {
	id := discovery.ScanID(r.PathValue("id"))
	if _, ok := h.d.Discovery.Scan(id); !ok {
		h.notFound(w, r)
		return
	}
	user := actor(r)
	if !h.acquireStream(user) {
		http.Error(w, "too many open event streams", http.StatusTooManyRequests)
		return
	}
	defer h.releaseStream(user)
	checker, _ := h.d.Guard.(sessionChecker)
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	poll := time.NewTicker(300 * time.Millisecond)
	defer poll.Stop()
	lastCheck := h.d.Clock.Now()
	lastKey := ""
	lastHits := -1
	for {
		s, ok := h.d.Discovery.Scan(id)
		if !ok {
			return
		}
		if key := fmt.Sprint(s.State, s.Done, len(s.Hits)); key != lastKey {
			lastKey = key
			// While it runs, the progress is replaced on every tick and the results (chips, bulk bar,
			// table) only when a device was found: the scan bar (Stop) is never replaced, and nothing
			// clickable is swapped away under the reader's mouse (#144). The end replaces the whole body.
			b := h.scanBodyData(r, s)
			var buf bytes.Buffer
			send := func(event string, comp templ.Component) error {
				var frag bytes.Buffer
				if err := comp.Render(r.Context(), &frag); err != nil {
					return err
				}
				writeSSE(&buf, event, frag.String())
				return nil
			}
			var err error
			if s.State != discovery.ScanRunning {
				err = send("scan-done", scanBody(b))
			} else {
				if len(s.Hits) != lastHits {
					lastHits = len(s.Hits)
					err = send("scan", scanLive(b))
				}
				if err == nil {
					err = send("scan-progress", scanProgress(b))
				}
			}
			if err != nil {
				return
			}
			if _, err := buf.WriteTo(w); err != nil || rc.Flush() != nil {
				return
			}
			if s.State != discovery.ScanRunning {
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
		}
		if now := h.d.Clock.Now(); now.Sub(lastCheck) >= h.d.SSERecheck {
			lastCheck = now
			if !adminStreamOpen(r, checker) {
				return
			}
		}
	}
}

func (h *handler) discoveryScanCancel(w http.ResponseWriter, r *http.Request) {
	id := discovery.ScanID(r.PathValue("id"))
	switch err := h.d.Discovery.CancelScan(r.Context(), actor(r), id); {
	case err == nil:
		redirect(w, r, "/discovery/scan/"+string(id))
	case errors.Is(err, discovery.ErrNotFound):
		h.notFound(w, r)
	default:
		h.fail(w, r, err)
	}
}

func proposalID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// shownProposal is a listed (not dismissed) proposal as the Discovered tab shows it: whether
// it can be accepted, and why not.
type shownProposal struct {
	P        discovery.Proposal
	CanApply bool
	Why      string
}

func (h *handler) shownProposal(r *http.Request, id int64) (shownProposal, bool) {
	p, ok := h.proposalByID(r, id)
	if !ok {
		return shownProposal{}, false
	}
	sp := shownProposal{P: p, CanApply: p.Kind != discovery.KindConflict}
	if p.Kind == discovery.KindConflict {
		sp.Why = "Resolve this in the inventory (edit or delete the stored device), then ignore it."
	}
	if p.Kind == discovery.KindAddressChange && p.ExistingID != 0 {
		if d, err := h.d.Inventory.Get(r.Context(), p.ExistingID); err == nil {
			if merr := discovery.MoveRefusal(d); merr != nil {
				sp.CanApply = false
				sp.Why = capitalize(strings.TrimPrefix(merr.Error(), discovery.ErrNotAcceptable.Error()+": ")) + ". Change it in the device edit form, then ignore this."
			}
		}
	}
	return sp, true
}

// discoveryAccept adds a new device or applies an address change ("Apply").
func (h *handler) discoveryAccept(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	id, ok := proposalID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	shown, ok := h.shownProposal(r, id)
	if !ok { // unknown, dismissed or overtaken: nothing the admin can have looked at
		h.notFound(w, r)
		return
	}
	fail := func(status int, msg string) {
		p := h.discoveryData(r)
		p.Error = msg
		h.discoveryRender(w, r, status, p)
	}
	if !shown.CanApply {
		fail(http.StatusConflict, shown.Why)
		return
	}
	version := r.PostFormValue("version")
	if version == "" {
		fail(http.StatusUnprocessableEntity, "The form is out of date. Reload the page and try again.")
		return
	}
	ctx := r.Context()
	prefix := fmt.Sprintf("accept:%d:%s:", id, version)
	if token := r.PostFormValue("token"); token != "" {
		pc, ok := h.cmds.take(h.d.Clock.Now(), token, h.bindingOf(r))
		if !ok || !strings.HasPrefix(pc.cmd, prefix) {
			fail(http.StatusConflict, "The confirmation expired. Press \"Apply\" again.")
			return
		}
		ctx = inventory.WithUnverifiedMoveConfirmed(ctx, strings.TrimPrefix(pc.cmd, prefix))
	}
	if pw := h.defaultPasswordFor(r); pw != "" {
		ctx = discovery.WithDevicePassword(ctx, pw)
	}
	devID, err := h.d.Discovery.AcceptVersion(ctx, actor(r), id, version)
	var um *inventory.UnverifiedMoveError
	switch {
	case err == nil && shown.P.Kind == discovery.KindAddressChange:
		redirect(w, r, discBack(r, "moved"))
	case err == nil && fromAddPage(r):
		redirect(w, r, "/devices/"+strconv.FormatInt(int64(devID), 10)+"?ok=added") // where a manual add ends too
	case err == nil:
		redirect(w, r, "/") // the dashboard's Needs attention: the row leaves it, the device joins the fleet
	case errors.Is(err, discovery.ErrNeedsPassword):
		redirect(w, r, needPasswordPath(id))
	case errors.As(err, &um):
		token := h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), prefix+um.Target, nil)
		p := h.discoveryData(r)
		p.Unverified = &unverifiedMove{Target: um.Target, Token: token, ProposalID: id, Version: version, Name: shown.P.Device.Name}
		h.discoveryRender(w, r, http.StatusOK, p)
	case errors.Is(err, discovery.ErrNotFound):
		h.notFound(w, r)
	case errors.Is(err, discovery.ErrNotAcceptable), errors.Is(err, discovery.ErrStale):
		fail(http.StatusConflict, capitalize(strings.TrimPrefix(err.Error(), "discovery: "))+".")
	case errors.Is(err, inventory.ErrInvalid), errors.Is(err, store.ErrConflict):
		fail(http.StatusUnprocessableEntity, errText(err))
	default:
		h.fail(w, r, err)
	}
}

// selectedProposals reads the ticked rows of the New devices table: "id" values of the form
// "<id>:<version>", so an accept still applies only what the admin saw. The table and the phone
// cards both carry a box for every row, so the browser posts each ticked id twice (#836): a
// repeated id counts once, the first copy wins.
func selectedProposals(r *http.Request) (ids []int64, versions []string) {
	_ = r.ParseForm() // a broken body just means nothing is selected
	seen := map[int64]bool{}
	for _, raw := range r.PostForm["id"] {
		idText, version, _ := strings.Cut(raw, ":")
		if id, err := strconv.ParseInt(idText, 10, 64); err == nil && id > 0 && !seen[id] {
			seen[id] = true
			ids, versions = append(ids, id), append(versions, version)
		}
	}
	return ids, versions
}

// discoveryAcceptSelected adds every ticked new device ("Add N selected"); each one goes
// through the same checks as a single accept.
func (h *handler) discoveryAcceptSelected(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	ids, versions := selectedProposals(r)
	if len(ids) == 0 {
		p := h.discoveryData(r)
		p.Error = "Select the devices to add first."
		h.discoveryRender(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	// An empty version would make AcceptVersion skip the stale check and add whatever the
	// proposal holds now, not what the admin saw; refuse it like a single accept does.
	if slices.Contains(versions, "") {
		p := h.discoveryData(r)
		p.Error = "The form is out of date. Reload the page and try again."
		h.discoveryRender(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	ctx := r.Context()
	if pw := h.defaultPasswordFor(r); pw != "" {
		ctx = discovery.WithDevicePassword(ctx, pw)
	}
	var added []model.DeviceID
	var failed []string
	var needPW []needPasswordRow
	base := httpBase(r)
	for i, id := range ids {
		shown, ok := h.shownProposal(r, id)
		if !ok || shown.P.Kind != discovery.KindNew {
			failed = append(failed, fmt.Sprintf("#%d is no longer listed", id))
			continue
		}
		devID, err := h.d.Discovery.AcceptVersion(ctx, actor(r), id, versions[i])
		switch {
		case errors.Is(err, discovery.ErrNeedsPassword):
			// not a failure: it is added through its filled-in form, which the banner links
			needPW = append(needPW, needPasswordRow{ID: id, Name: nameOr(shown.P), Href: base + needPasswordPath(id)})
		case err != nil:
			h.log.InfoContext(ctx, "discovery: add selected", "id", id, "err", err)
			failed = append(failed, nameOr(shown.P)+": "+strings.TrimPrefix(errText(err), "discovery: "))
		default:
			added = append(added, devID)
		}
	}
	switch {
	case len(failed) == 0 && len(needPW) == 0 && len(added) == 1:
		redirect(w, r, "/devices/"+strconv.FormatInt(int64(added[0]), 10)+"?ok=added") // one device: as a single Add
		return
	case len(failed) == 0 && len(needPW) == 0:
		// discBack always ends in "ok=accepted", so "&n=" starts a new parameter; the count lets
		// the banner say how many devices were added.
		redirect(w, r, discBack(r, "accepted")+"&n="+strconv.Itoa(len(added)))
		return
	case len(failed) == 0 && len(added) == 0 && len(needPW) == 1:
		redirect(w, r, needPasswordPath(needPW[0].ID)) // one device that needs its password: as a single Add
		return
	}
	p := h.discoveryData(r)
	p.Added, p.NeedPassword = len(added), needPW
	status := http.StatusOK
	if len(failed) > 0 {
		status = http.StatusConflict
		p.Error = fmt.Sprintf("%s added, %d not: %s.", plural(len(added), "device"), len(failed), strings.Join(failed, "; "))
	}
	h.discoveryRender(w, r, status, p)
}

// formOutOfDate is what a page that no longer matches the proposals behind its numbers is told.
const formOutOfDate = "The form is out of date. Reload the page and try again."

// discoveryError shows the Add page with msg, for a request that changed nothing.
func (h *handler) discoveryError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	p := h.discoveryData(r)
	p.Error = msg
	h.discoveryRender(w, r, status, p)
}

// discoveryDismissSelected ignores every ticked new device ("Ignore selected"). Each tick
// carries the version that was shown; one whose proposal changed since (its number can be
// reused by another device) is left alone and reported, the others are ignored.
func (h *handler) discoveryDismissSelected(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	ids, versions := selectedProposals(r)
	if slices.Contains(versions, "") { // like Add: nothing the admin saw can be named without it
		h.discoveryError(w, r, http.StatusUnprocessableEntity, formOutOfDate)
		return
	}
	ignored, stale := 0, 0
	for i, id := range ids {
		switch err := h.d.Discovery.Dismiss(r.Context(), actor(r), id, versions[i]); {
		case err == nil:
			ignored++
		case errors.Is(err, discovery.ErrNotFound): // already gone: nothing left to hide
		case errors.Is(err, discovery.ErrStale):
			stale++
		default:
			h.fail(w, r, err)
			return
		}
	}
	switch {
	case stale == 0:
		redirect(w, r, discBack(r, "dismissed"))
	case ignored == 0:
		h.discoveryError(w, r, http.StatusConflict, formOutOfDate)
	default:
		h.discoveryError(w, r, http.StatusConflict, fmt.Sprintf("%s ignored, %d not: %s", plural(ignored, "device"), stale, formOutOfDate))
	}
}

func nameOr(p discovery.Proposal) string {
	if p.Device.Name != "" {
		return p.Device.Name
	}
	return p.Device.Address
}

func (h *handler) discoveryDismiss(w http.ResponseWriter, r *http.Request) {
	h.discoveryToggle(w, r, h.d.Discovery.Dismiss, "dismissed")
}

func (h *handler) discoveryUndismiss(w http.ResponseWriter, r *http.Request) {
	h.discoveryToggle(w, r, h.d.Discovery.Undismiss, "undismissed")
}

// discoveryToggle is Ignore or Show again of the proposal numbered in the path. The form carries
// the version the page showed; the number alone is not enough because it is reused after
// the row behind it is deleted, so a stale page would act on another device (#745).
func (h *handler) discoveryToggle(w http.ResponseWriter, r *http.Request, act func(ctx context.Context, actor string, id int64, version string) error, done string) {
	limitBody(w, r)
	id, ok := proposalID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	version := r.PostFormValue("version")
	if version == "" {
		h.discoveryError(w, r, http.StatusUnprocessableEntity, formOutOfDate)
		return
	}
	switch err := act(r.Context(), actor(r), id, version); {
	case err == nil:
		redirect(w, r, discBack(r, done))
	case errors.Is(err, discovery.ErrNotFound):
		h.notFound(w, r)
	case errors.Is(err, discovery.ErrStale):
		h.discoveryError(w, r, http.StatusConflict, formOutOfDate)
	default:
		h.fail(w, r, err)
	}
}
