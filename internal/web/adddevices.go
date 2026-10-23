package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The "Add devices" screen of design v2 (package P6): one page header and four tabs, Add
// manually (/devices/new), Discovered (/discovery), Network scan (/discovery/scan) and Import
// from TasmoAdmin (/import/tasmoadmin). Each tab is its own route; a tab whose service is not
// configured is left out.

// addTab keys: which tab a page is.
const (
	addTabManual = "manual"
	addTabDisc   = "disc"
	addTabScan   = "scan"
	addTabImport = "import"
)

// addHead is what every tab shows above its body: the tab row.
type addHead struct {
	Tabs []ui.Tab
}

// addView is the page view of one tab: the title of the browser tab and the breadcrumb trail
// are the same on all four ("Devices / Add devices"), the sub-navigation lights "Add devices".
func (h *handler) addView(r *http.Request, data any) view {
	v := h.view(r, "Add devices", "discovery", data)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Add devices"}}
	return v
}

// addTabs builds the tab row. Discovered carries the number of devices waiting for an admin
// (new devices and address changes, the badge of the sub-navigation).
func (h *handler) addTabs(r *http.Request, active string) addHead {
	base := httpBase(r)
	tabs := []ui.Tab{{Label: "Add manually", Href: base + "/devices/new", Current: active == addTabManual}}
	if h.d.Discovery != nil {
		disc := ui.Tab{Label: "Discovered", Href: base + "/discovery", Current: active == addTabDisc}
		if p, _ := auth.FromContext(r.Context()); p.Role >= model.RoleAdmin {
			disc.Badge = h.newDeviceCount(r)
		}
		tabs = append(tabs, disc, ui.Tab{Label: "Network scan", Href: base + "/discovery/scan", Current: active == addTabScan})
	}
	if h.d.Import != nil {
		tabs = append(tabs, ui.Tab{Label: "Import from TasmoAdmin", Href: base + "/import/tasmoadmin", Current: active == addTabImport})
	}
	return addHead{Tabs: tabs}
}

// addManualView is the Add manually tab: the prototype's address and web password, the fixed
// web user, and the remaining device fields folded under "More options".
type addManualView struct {
	addHead
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

func (h *handler) renderAddManual(w http.ResponseWriter, r *http.Request, status int, f addManualView) {
	f.addHead = h.addTabs(r, addTabManual)
	v := h.addView(r, f)
	f.Base, f.CSRF = v.Base, v.CSRF
	f.Device.HTTPPassword = "" // never rendered
	f.TagsText = strings.Join(f.Device.Tags, ", ")
	f.HasDefault = h.d.DefaultDevicePassword != nil && h.d.DefaultDevicePassword() != ""
	f.NoNetworks = v.Banner != ""
	h.show(w, r, status, "add_manual", v, addManualPage(f))
}

// deviceNewForm is the Add manually tab. ?proposal=<id> fills it from a discovered device (the
// "Add…" of the Discovered tab); ?address= and ?port= from a scan result.
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
		}
	} else if a := strings.TrimSpace(q.Get("address")); a != "" && h.d.Discovery != nil {
		port, _ := strconv.Atoi(q.Get("port")) // a bad port is just no port
		if port == 80 || port < 0 || port > 65535 {
			port = 0
		}
		f.Device = model.Device{Address: a, Port: port}
		f.Prefill = &addPrefill{Source: "network scan", Address: netaddr.Display(a, port, "http")}
	}
	h.renderAddManual(w, r, http.StatusOK, f)
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

// dayStamp is the "First seen" of a proposal: "Today 08:12", "Yesterday 18:30" or "12 Sep 2026".
func dayStamp(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	t = t.In(now.Location())
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case !t.Before(today):
		return "Today " + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday " + t.Format("15:04")
	}
	return t.Format("2 Jan 2006")
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

// metaTail is " · model · firmware" without the parts the device did not report: a phone card
// reads "127.0.0.1" rather than "127.0.0.1 · — · —" (the table keeps its dash per column).
func metaTail(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p != "" {
			b.WriteString(" · " + p)
		}
	}
	return b.String()
}

// showIgnoredHref is the "Show ignored" switch: a link to the other state.
func showIgnoredHref(p discView) string {
	if p.ShowIgn {
		return p.Base + "/discovery"
	}
	return p.Base + "/discovery?ignored=1"
}

// listSep is the separator before item i of n in running text: ", " or " and ".
func listSep(i, n int) string {
	if i == n-1 {
		return " and "
	}
	return ", "
}
