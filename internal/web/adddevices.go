package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/hellwand/tasmofleet/internal/discovery"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/netaddr"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The Add page (#410): one page with four boxes, one per way to add devices: Discovered and
// Network scan in the wide column, Add by address and Import from TasmoAdmin in the narrow one. The
// page is /devices/new; the boxes post to the routes of their own service (/discovery/..., the
// import fragments). A box whose service is not configured is left out. The network scan's full
// result lives on its own page (/discovery/scan/{id}).

// addPage is the view of the Add page.
type addPage struct {
	Base, CSRF string
	Manual     addManualView
	Disc       *discView     // nil without discovery
	Scan       *scanBodyView // nil without discovery
	Import     *importBody   // nil without the web import
	// ImportFolded: the fleet has devices, so the one-time TasmoAdmin import is a closed fold (#1110).
	ImportFolded bool
}

// addTitle names the page in the window title and the phone header.
const addTitle = "Add devices"

// renderAdd draws the Add page. The manual form comes from the caller (an empty one, a prefilled
// one, or the one a refused add came back with), and so does the discovery list when an action on
// it was refused; what is nil is read afresh.
func (h *handler) renderAdd(w http.ResponseWriter, r *http.Request, status int, f addManualView, d *discView) {
	v := h.view(r, addTitle, "discovery", nil)
	f.Base, f.CSRF = v.Base, v.CSRF
	f.Device.HTTPPassword, f.Device.MQTTPassword = "", "" // never rendered
	f.TagsText = strings.Join(f.Device.Tags, ", ")
	f.HasDefault = h.d.DefaultDevicePassword != nil && h.d.DefaultDevicePassword() != ""
	f.NoNetworks = v.Banner != ""
	page := addPage{Base: v.Base, CSRF: v.CSRF, Manual: f}
	if h.d.Discovery != nil {
		if d == nil {
			x := h.discoveryData(r)
			d = &x
			if r.URL.Query().Get("ok") == "accepted" {
				// "Add N selected" carries how many it added: a banner with the count and the way to the list
				// stays on the page (a toast would fade with its link), so more can be added from here.
				d.Added = 1
				if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n > 1 {
					d.Added = n
				}
			}
		}
		d.Base, d.CSRF = v.Base, v.CSRF
		page.Disc = d
		b := h.scanBoxData(r)
		page.Scan = &b
	}
	if h.d.Import != nil {
		b := h.importBodyOf(r)
		page.Import = &b
		devs, err := h.d.Inventory.List(r.Context())
		page.ImportFolded = err == nil && len(devs) > 0 // a failed read keeps the box open: nothing is hidden
	}
	switch r.URL.Query().Get("ok") {
	case "moved":
		v.Flash = "Address updated."
	case "dismissed":
		v.Flash = "Ignored."
	case "undismissed":
		v.Flash = "Shown again."
	}
	h.show(w, r, status, "add", v, addPageView(page))
}

// addManualView is the Add by address box: the prototype's address and web password, the fixed
// web user, and the remaining device fields folded under "More options".
type addManualView struct {
	Base, CSRF string
	Device     model.Device
	TagsText   string
	Error      string // a problem that belongs to no single field (banner)
	ErrField   string // the field Error belongs to
	Fallback   bool   // offer "Add without probing" after a failed probe
	// Tested is the device a "Test connection" found (nothing stored).
	Tested *model.Device
	// Prefill names the discovery proposal the form was filled from (Clear empties it).
	Prefill *addPrefill
	// HasDefault: a fleet default device password is set (N20). The value never reaches the page.
	HasDefault bool
	// NoNetworks: no device network is configured, every address would be refused.
	NoNetworks bool
	// NeedPassword: Add of the discovered device found it behind a web password (#1110); the form says so at the
	// password and puts the focus there.
	NeedPassword bool
}

// addPrefill is the banner of a form opened from a discovered device.
type addPrefill struct {
	Source, Name, Host, Address string
}

// MoreOpen keeps the "More options" fold open when one of its fields has a value or an error.
func (v addManualView) MoreOpen() bool {
	switch v.ErrField {
	case "name", "port", "tags", "notes":
		return true
	}
	return v.Device.Name != "" || v.Device.Port != 0 || v.TagsText != "" || v.Device.Notes != "" || v.Device.OTAExcluded
}

// FieldError is the sentence under the named input, or "".
func (v addManualView) FieldError(name string) string {
	if v.ErrField == name {
		return v.Error
	}
	return ""
}

// PasswordHelp is the line under the web password: where a prefilled password comes from, or
// what the field is for.
func (v addManualView) PasswordHelp() string {
	switch {
	case v.HasDefault && v.Prefill == nil:
		return "Prefilled from Settings → Devices."
	case v.HasDefault:
		return "The default password is not sent to a discovered address; type it if the device has one."
	}
	return "Only if the device has one. Stored encrypted."
}

// PasswordPlaceholder shows that the default password fills the empty field (the dots stand
// for it; the value itself never reaches the page).
func (v addManualView) PasswordPlaceholder() string {
	if v.HasDefault && v.Prefill == nil {
		return strings.Repeat("•", 16)
	}
	return ""
}

// needPasswordText is the sentence at the web password of the form that Add of a discovered device opens when the
// device asks for its password (#1110).
const needPasswordText = "This device needs its web password."

// deviceNewForm is the Add page. ?proposal=<id> fills the address form from a discovered device
// (a scan result's Add; with &need=password the Add of a device that asks for its web password,
// which then says so at the password); ?address= and ?port= from a scan result.
func (h *handler) deviceNewForm(w http.ResponseWriter, r *http.Request) {
	var f addManualView
	q := r.URL.Query()
	if id, err := strconv.ParseInt(q.Get("proposal"), 10, 64); err == nil && h.d.Discovery != nil {
		if p, ok := h.proposalByID(r, id); ok && p.Kind == discovery.KindNew {
			f.Device = model.Device{Name: p.Device.Name, Address: p.Device.Address, Port: p.Device.Port}
			if f.Device.Port == 80 {
				f.Device.Port = 0
			}
			f.Prefill = &addPrefill{Source: viaText(p.Via), Name: p.Device.Name, Host: p.Device.Topic, Address: netaddr.Display(p.Device.Address, p.Device.Port, "http")}
			if q.Get("need") == "password" { // only beside the device it is about: a gone proposal opens the plain form
				f.NeedPassword, f.ErrField, f.Error = true, "http_password", needPasswordText
			}
		}
	} else if a := strings.TrimSpace(q.Get("address")); a != "" && h.d.Discovery != nil {
		port, _ := strconv.Atoi(q.Get("port")) // a bad port is just no port
		if port == 80 || port < 0 || port > 65535 {
			port = 0
		}
		f.Device = model.Device{Address: a, Port: port}
		f.Prefill = &addPrefill{Source: "network scan", Address: netaddr.Display(a, port, "http")}
	}
	h.renderAdd(w, r, http.StatusOK, f, nil)
}

// proposalByID finds a listed (not dismissed) proposal.
func (h *handler) proposalByID(r *http.Request, id int64) (discovery.Proposal, bool) {
	props, err := h.d.Discovery.Proposals(r.Context())
	if err != nil {
		h.log.WarnContext(r.Context(), "discovery proposals", "err", err)
		return discovery.Proposal{}, false
	}
	for _, p := range props {
		if p.ID == id {
			return p, true
		}
	}
	return discovery.Proposal{}, false
}

// testedText is what "Test connection" found: the name and what it runs.
func testedText(d model.Device) string {
	s := d.Name
	if s == "" {
		s = "The device"
	}
	var parts []string
	if d.Identity.Hardware != "" {
		parts = append(parts, d.Identity.Hardware)
	}
	if d.Identity.Version != "" {
		parts = append(parts, "Tasmota "+d.Identity.Version)
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s + " answered."
}

// portText is the Port field: empty for the default.
func portText(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

// dash shows a value that is not known as a dash.
func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// metaTail is " · a · b" without the parts that are empty: a phone card reads "127.0.0.1"
// rather than "127.0.0.1 · —" (the table keeps its dash per column).
func metaTail(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p != "" {
			b.WriteString(" · " + p)
		}
	}
	return b.String()
}

// sortHeader is the link of a sortable header of the New devices table: the current column
// flips its direction, any other starts ascending; filter and ignored switch are kept.
func sortHeader(p discView, key string) (href, class, aria string) {
	dir, class, aria := "asc", "sortlink", "none"
	if key == p.Sort {
		class += " is-" + p.Dir
		aria = map[string]string{"asc": "ascending", "desc": "descending"}[p.Dir]
		if p.Dir == "asc" {
			dir = "desc"
		}
	}
	return discHref(p.Base, p.State, key, dir, p.ShowIgn), class, aria
}

// openMoves counts the rows that are not ignored: what waits for a decision, not the ignored rows
// listed after them.
func openMoves(rows []moveRow) int {
	n := 0
	for _, m := range rows {
		if !m.Ignored {
			n++
		}
	}
	return n
}

// listSep is the separator before item i of n in running text: ", " or " and ".
func listSep(i, n int) string {
	if i == n-1 {
		return " and "
	}
	return ", "
}

// nameTip is the tooltip of the name cell: the name with the MQTT topic, which has no column.
func nameTip(r propRow) string {
	if r.Topic == "" {
		return dash(r.Name)
	}
	return dash(r.Name) + " · topic " + r.Topic
}

// rowBtn is a button of a Discovered or scan row: it posts the shown version to the proposal's action (accept,
// dismiss, undismiss) through the row's form (#745). look gives the kind, size and the page's class.
func rowBtn(base string, id int64, version, label, verb string, look ui.Btn) ui.Btn {
	look.Label, look.Type, look.Name, look.Value = label, "submit", "version", version
	look.FormAction = base + "/discovery/proposals/" + strconv.FormatInt(id, 10) + "/" + verb
	return look
}

// propBtn is rowBtn for a new device of the Discovered table.
func propBtn(p discView, r propRow, label, verb string, look ui.Btn) ui.Btn {
	return rowBtn(p.Base, r.ID, r.Version, label, verb, look)
}

// offDescribed points a disabled button at the line that says why (aria-describedby), nil while it is enabled.
func offDescribed(off bool, id string) templ.Attributes {
	if !off {
		return nil
	}
	return templ.Attributes{"aria-describedby": id}
}

// needPasswordLead starts the banner of the devices "Add N selected" left because they need their web password:
// how many were added (unless the danger banner beside it says so already), then how many need it. With nothing
// added the devices are the subject: a count of nothing added would only stand in front of them.
func needPasswordLead(p discView) string {
	n := len(p.NeedPassword)
	if p.Added == 0 {
		if n == 1 {
			return "1 device needs its web password: "
		}
		return strconv.Itoa(n) + " devices need their web password: "
	}
	lead := ""
	if p.Error == "" {
		lead = plural(p.Added, "device") + " added. "
	}
	if n == 1 {
		return lead + "1 needs its web password: "
	}
	return lead + strconv.Itoa(n) + " need their web password: "
}

// addedListAction is the "Open the device list" of an "Add N selected" banner, nil when nothing was added: the list
// then holds nothing new.
func addedListAction(p discView) *ui.Btn {
	if p.Added == 0 {
		return nil
	}
	return &ui.Btn{Label: "Open the device list", Href: p.Base + "/devices"}
}

// listEnd is what follows item i of n in a list that ends a sentence: a comma and a space, after the last a full stop.
func listEnd(i, n int) string {
	if i == n-1 {
		return "."
	}
	return ", "
}

// cardHost is the hostname line of a phone card; it is left out when it only repeats the name.
func cardHost(r propRow) string {
	if r.Hostname == r.Name {
		return ""
	}
	return r.Hostname
}
