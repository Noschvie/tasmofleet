package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// Discovery is what the discovery pages need (*discovery.Manager).
type Discovery interface {
	discovery.Service
	// AcceptVersion applies the proposal only if it still has the shown version.
	AcceptVersion(ctx context.Context, actor string, id int64, version string) (model.DeviceID, error)
	// Ignored lists the dismissed proposals; Undismiss makes one visible again.
	Ignored(ctx context.Context) ([]discovery.Proposal, error)
	Undismiss(ctx context.Context, actor string, id int64) error
	Scan(id discovery.ScanID) (discovery.ScanStatus, bool)
	Scans() []discovery.ScanStatus
	// Unidentified lists the topics seen on MQTT that are not a proposal (spec 11.3a).
	Unidentified() []discovery.Unidentified
}

func (h *handler) registerDiscovery(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op, adm func(http.Handler) http.Handler) {
	_ = op // discovery is admin only: proposals expose addresses and accept changes the inventory
	if h.d.Discovery == nil {
		return
	}
	route("GET /discovery", adm, h.discoveryPage)
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

// discView is the Discovered tab: the address changes and conflicts a known device reported,
// then the new devices (and, with ?ignored=1, the ignored ones) with selection, Ignore and Add….
type discView struct {
	addHead
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
	// HasDefault: accepting uses the fleet default device password (N20, MAC proof required).
	HasDefault bool
	// LastAnnounce is the newest MQTT announcement (the empty state's "Last announcement").
	LastAnnounce time.Time
	Error        string
	// Unverified is the stronger confirmation for "Apply" when the new address could not
	// prove the device's MAC.
	Unverified *unverifiedMove
	// Unident are the topics MQTT saw that are no proposal yet, with the reason.
	Unident []moveRow
}

// propRow is one discovered device of the New devices table.
type propRow struct {
	ID              int64
	Version         string
	Label, Sub      string // the friendly name (or the host) and the host line
	Address, MAC    string
	Model, Firmware string // not stored with a proposal yet: "" shows a dash
	Source          string
	MQTT            bool
	First, Last     time.Time
	FirstText       string
	LastText        string
	Ignored         bool
	AddHref         string
	Offline         bool // its MQTT LWT says Offline: it is added as seen, without a probe
}

// firstSeenText is the fallback of a proposal's "First seen": a relative day, or "unknown".
func firstSeenText(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return ui.StampDayText(now, t)
}

// moveRow is a known device that announced itself at a new address (or a conflict).
type moveRow struct {
	ID       int64
	Version  string
	Name     string
	Href     string // the stored device
	Seen     time.Time
	SeenText string // "4 min ago · MQTT discovery"
	Old, New string
	Why      string
	CanApply bool
}

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
	v := discView{ShowIgn: r.URL.Query().Get("ignored") == "1", Off: h.discoveryOff()}
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
			v.Rows = append(v.Rows, h.propRow(base, now, p, false))
		default:
			m := moveRow{ID: p.ID, Version: p.Version, Name: p.Device.Name, Seen: p.SeenAt,
				SeenText: agoWords(now, p.SeenAt) + " · " + viaText(p.Via), New: netaddr.Display(p.Device.Address, p.Device.Port, "http"), CanApply: p.Kind == discovery.KindAddressChange}
			if p.ExistingID != 0 {
				if d, err := h.d.Inventory.Get(r.Context(), p.ExistingID); err == nil {
					m.Name, m.Href, m.Old = d.Name, base+"/devices/"+strconv.FormatInt(int64(d.ID), 10), netaddr.Display(d.Address, d.Port, "http")
					m.Why = moveWhy(p, d)
					if p.Kind == discovery.KindAddressChange {
						if merr := discovery.MoveRefusal(d); merr != nil {
							m.CanApply = false
							m.Why = capitalize(strings.TrimPrefix(merr.Error(), discovery.ErrNotAcceptable.Error()+": ")) + ". Change it in the device edit form, then dismiss this."
						}
					}
				}
			}
			if p.Kind == discovery.KindConflict {
				m.Why = "Another device claims its address or MQTT topic. Resolve this in the inventory (edit or delete the stored device), then dismiss it."
				v.Conflicts = append(v.Conflicts, m)
			} else {
				v.Moves = append(v.Moves, m)
			}
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
		if p.Kind != discovery.KindNew {
			continue
		}
		v.Ignored++
		if v.ShowIgn {
			v.Rows = append(v.Rows, h.propRow(base, now, p, true))
		}
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
	// the order they were discovered in, ignored ones after the new ones
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].Ignored != v.Rows[j].Ignored {
			return !v.Rows[i].Ignored
		}
		return v.Rows[i].ID < v.Rows[j].ID
	})
	return v
}

func (h *handler) propRow(base string, now time.Time, p discovery.Proposal, ignored bool) propRow {
	row := propRow{ID: p.ID, Version: p.Version, Label: p.Device.Name, Sub: p.Device.Topic,
		Address: netaddr.Display(p.Device.Address, p.Device.Port, "http"), MAC: strings.ToUpper(p.Device.MAC),
		Source: viaText(p.Via), MQTT: p.Via == discovery.ViaMQTT, First: p.FirstSeen, Last: p.SeenAt,
		FirstText: firstSeenText(now, p.FirstSeen), LastText: agoWords(now, p.SeenAt), Ignored: ignored,
		AddHref: base + "/devices/new?proposal=" + strconv.FormatInt(p.ID, 10), Offline: p.LWT == "Offline"}
	row.Source = strings.ToUpper(row.Source[:1]) + row.Source[1:]
	if row.Label == "" {
		row.Label, row.Sub = p.Device.Topic, "No friendly name reported"
	}
	return row
}

// moveWhy says in a sentence why discovery left an address change to an admin.
func moveWhy(p discovery.Proposal, d model.Device) string {
	switch {
	case !isIPAddress(d.Address):
		return "It has a stored hostname, so TasmoFleet won’t switch the address on its own."
	case d.HasCredentials():
		return "It has a stored web password, so TasmoFleet won’t move it on its own. Apply sends the password to the new address."
	}
	return "The new address must answer with the device’s MAC before TasmoFleet uses it."
}

func isIPAddress(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

func (h *handler) discoveryRender(w http.ResponseWriter, r *http.Request, status int, p discView) {
	p.addHead = h.addTabs(r, addTabDisc)
	v := h.addView(r, p)
	p.Base, p.CSRF = v.Base, v.CSRF
	switch r.URL.Query().Get("ok") {
	case "accepted":
		v.Flash = "Device added."
	case "moved":
		v.Flash = "Address updated."
	case "dismissed":
		v.Flash = "Dismissed."
	case "undismissed":
		v.Flash = "Restored."
	}
	h.show(w, r, status, "discovery", v, discoveredPage(p))
}

func (h *handler) discoveryPage(w http.ResponseWriter, r *http.Request) {
	h.discoveryRender(w, r, http.StatusOK, h.discoveryData(r))
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
			return nil, fmt.Errorf("%q is not a range like 172.31.13.0/24 or an IP address", f)
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
		err = errors.New("enter a range like 192.168.20.0/24")
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
			if checker != nil {
				if _, err := checker.Principal(r.Clone(httpx.Detach(r.Context()))); err != nil {
					return
				}
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
		sp.Why = "Resolve this in the inventory (edit or delete the stored device), then dismiss it."
	}
	if p.Kind == discovery.KindAddressChange && p.ExistingID != 0 {
		if d, err := h.d.Inventory.Get(r.Context(), p.ExistingID); err == nil {
			if merr := discovery.MoveRefusal(d); merr != nil {
				sp.CanApply = false
				sp.Why = capitalize(strings.TrimPrefix(merr.Error(), discovery.ErrNotAcceptable.Error()+": ")) + ". Change it in the device edit form, then dismiss this."
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
	_, err := h.d.Discovery.AcceptVersion(ctx, actor(r), id, version)
	var um *inventory.UnverifiedMoveError
	switch {
	case err == nil:
		if shown.P.Kind == discovery.KindAddressChange {
			redirect(w, r, "/discovery?ok=moved")
		} else {
			redirect(w, r, "/discovery?ok=accepted")
		}
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
// "<id>:<version>", so an accept still applies only what the admin saw.
func selectedProposals(r *http.Request) (ids []int64, versions []string) {
	_ = r.ParseForm() // a broken body just means nothing is selected
	for _, raw := range r.PostForm["id"] {
		idText, version, _ := strings.Cut(raw, ":")
		if id, err := strconv.ParseInt(idText, 10, 64); err == nil && id > 0 {
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
	added, failed := 0, []string{}
	for i, id := range ids {
		shown, ok := h.shownProposal(r, id)
		if !ok || shown.P.Kind != discovery.KindNew {
			failed = append(failed, fmt.Sprintf("#%d is no longer listed", id))
			continue
		}
		if _, err := h.d.Discovery.AcceptVersion(ctx, actor(r), id, versions[i]); err != nil {
			h.log.InfoContext(ctx, "discovery: add selected", "id", id, "err", err)
			failed = append(failed, nameOr(shown.P)+": "+strings.TrimPrefix(errText(err), "discovery: "))
			continue
		}
		added++
	}
	if len(failed) == 0 {
		redirect(w, r, "/discovery?ok=accepted")
		return
	}
	p := h.discoveryData(r)
	p.Error = fmt.Sprintf("%s added, %d not: %s.", plural(added, "device"), len(failed), strings.Join(failed, "; "))
	h.discoveryRender(w, r, http.StatusConflict, p)
}

// discoveryDismissSelected ignores every ticked new device ("Ignore selected").
func (h *handler) discoveryDismissSelected(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	ids, _ := selectedProposals(r)
	for _, id := range ids {
		if err := h.d.Discovery.Dismiss(r.Context(), actor(r), id); err != nil && !errors.Is(err, discovery.ErrNotFound) {
			h.fail(w, r, err)
			return
		}
	}
	redirect(w, r, "/discovery?ok=dismissed")
}

func nameOr(p discovery.Proposal) string {
	if p.Device.Name != "" {
		return p.Device.Name
	}
	return p.Device.Address
}

func (h *handler) discoveryDismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := proposalID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	switch err := h.d.Discovery.Dismiss(r.Context(), actor(r), id); {
	case err == nil:
		redirect(w, r, "/discovery?ok=dismissed")
	case errors.Is(err, discovery.ErrNotFound):
		h.notFound(w, r)
	default:
		h.fail(w, r, err)
	}
}

func (h *handler) discoveryUndismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := proposalID(r)
	if !ok {
		h.notFound(w, r)
		return
	}
	switch err := h.d.Discovery.Undismiss(r.Context(), actor(r), id); {
	case err == nil:
		redirect(w, r, "/discovery?ok=undismissed")
	case errors.Is(err, discovery.ErrNotFound):
		h.notFound(w, r)
	default:
		h.fail(w, r, err)
	}
}
