package web

import (
	"slices"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The Settings page is eight tabs; each tab is a grid of cards, each card names the settings it
// holds, in reading order. A setting that no card names lands in Advanced, "Other settings", so it
// can never vanish from the UI. Adding a setting is: its sysconfig field, its key in a card here,
// and its sysFieldCopy entry.

// sysCardDef is a card of a tab. Class is "", "span2", "spanall" or "rows2". Depends is the
// data-depends rule that dims the card (sysform.js). StepUp marks cards whose settings ask the
// admin to confirm. Special cards (Backups storage, the deleted devices' backups) have no keys.
type sysCardDef struct {
	ID, Title, Note, Class, Depends string
	StepUp                          bool
	Keys                            []string
}

// sysTabDef is a tab with its cards.
type sysTabDef struct {
	Slug, Label string
	Cards       []sysCardDef
}

// sysCard is a card with the rows it shows.
type sysCard struct {
	sysCardDef
	Rows       []sysRow
	RestartAll bool // every setting of the card applies after a restart: one badge in the header
}

// sysTab is one tab of the page.
type sysTab struct {
	Label, Slug string
	Cards       []sysCard
}

// sysLayout is the approved tab order (maintainer, issue #275) with the cards of each tab.
var sysLayout = []sysTabDef{
	{"general", "General", []sysCardDef{
		{ID: "logging", Title: "Logging & audit", Keys: []string{"LOG_LEVEL", "AUDIT_RETENTION"}},
		{ID: "metrics", Title: "Prometheus metrics", Keys: []string{"METRICS_ENABLED", "METRICS_TOKEN", "METRICS_DEVICE_LABELS"}},
	}},
	{"network", "Network & devices", []sysCardDef{
		{ID: "networks", Title: "Device networks", Note: "Where your devices live.", Keys: []string{"DEVICE_NETWORKS"}},
		{ID: "health", Title: "Health checks", Note: "How often devices are checked.", Keys: []string{"POLL_INTERVAL", "POLICY_LIVE_INTERVAL", "TELEPERIOD", "CR_WARN_PERCENT"}},
		{ID: "credentials", Title: "Default device credentials", Note: "Prefilled for new devices.", Keys: []string{"DEVICE_DEFAULT_PASSWORD"}},
	}},
	{"mqtt", "MQTT", []sysCardDef{
		{ID: "broker", Title: "Broker", Note: "Changes apply at once. An empty host turns MQTT off.", Class: "span2", Keys: []string{"MQTT_URL", "MQTT_USER", "MQTT_PASSWORD"}},
		{ID: "topics", Title: "Topics", Keys: []string{"MQTT_FULLTOPIC"}},
		{ID: "mqttdiscovery", Title: "MQTT discovery", Keys: []string{"DISCOVERY_MQTT"}},
	}},
	{"scan", "Discovery scan", []sysCardDef{
		{ID: "scan", Title: "Network scan", Keys: []string{"DISCOVERY_SCAN", "DISCOVERY_SCAN_INTERVAL", "DISCOVERY_SCAN_CIDRS"}},
	}},
	{"backups", "Backups", []sysCardDef{
		{ID: "storage", Title: "Storage", Note: "Read-only.", Class: "span2"},
		{ID: "schedule", Title: "Schedule & retention", Class: "rows2", Keys: []string{"BACKUP_INTERVAL", "BACKUP_KEEP", "BACKUP_MAX_AGE"}},
		{ID: "orphans", Title: "Backups of deleted devices", Class: "span2"},
	}},
	{"firmware", "Firmware & updates", []sysCardDef{
		{ID: "source", Title: "Firmware source", Class: "span2", Keys: []string{"FIRMWARE_BASE_URL", "FIRMWARE_SOURCES", "FIRMWARE_REFRESH_INTERVAL"}},
		{ID: "crosscheck", Title: "Cross-check", Keys: []string{"FIRMWARE_ALLOW_SINGLE_SOURCE"}},
		{ID: "ota", Title: "Updates (OTA)", Keys: []string{"OTA_PARALLEL", "OTA_POWER_MAX_AGE", "OTA_RESTORE_OTAURL"}},
	}},
	{"signin", "Sign-in", []sysCardDef{
		{ID: "mode", Title: "Sign-in mode", Note: "Needs PUBLIC_URL for single sign-on. The local admin always works as a fallback.", Class: "spanall", StepUp: true, Keys: []string{"AUTH_MODE"}},
		{ID: "idp", Title: "Identity provider", Note: "Used while single sign-on is selected.", Class: "span2", Depends: "v.AUTH_MODE=oidc", StepUp: true,
			Keys: []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_SCOPES", "OIDC_DISPLAY_NAME"}},
		{ID: "roles", Title: "Roles and claims", Depends: "v.AUTH_MODE=oidc", StepUp: true,
			Keys: []string{"ADMIN_GROUPS", "OPERATOR_GROUPS", "OIDC_GROUPS_CLAIM", "OIDC_USERNAME_CLAIM"}},
	}},
	{"advanced", "Advanced", []sysCardDef{
		{ID: "other", Title: "Other settings"},
	}},
}

// sysAdvanced is the index of the Advanced tab and its catch-all card.
const sysAdvanced, sysOtherCard = 7, 0

// sysGroupCard is where a new setting that no card names yet lands, by its sysconfig group, so it
// shows next to its relatives; a group without an entry goes to Advanced, "Other settings".
var sysGroupCard = map[sysconfig.Group]string{
	sysconfig.GroupLogging: "logging", sysconfig.GroupAudit: "logging", sysconfig.GroupMetrics: "metrics",
	sysconfig.GroupNetwork: "networks", sysconfig.GroupHealth: "health", sysconfig.GroupMQTT: "broker",
	sysconfig.GroupDiscovery: "scan", sysconfig.GroupBackup: "schedule", sysconfig.GroupFirmware: "source",
	sysconfig.GroupOTA: "ota", sysconfig.GroupSignIn: "idp",
}

// sysPlace is the tab and card of a setting: the card that names it, else the card of its group,
// else Advanced.
func sysPlace(key string) (tab, card int) {
	for ti, t := range sysLayout {
		for ci, c := range t.Cards {
			if slices.Contains(c.Keys, key) {
				return ti, ci
			}
		}
	}
	if f, ok := sysconfig.Lookup(key); ok {
		for ti, t := range sysLayout {
			for ci, c := range t.Cards {
				if c.ID == sysGroupCard[f.Group] {
					return ti, ci
				}
			}
		}
	}
	return sysAdvanced, sysOtherCard
}

// sysTabsOf fills the layout with the rows. A setting that depends on a switch (sysFieldCopy.Dep)
// hangs under that switch when both sit in the same card.
func sysTabsOf(rows map[string]sysRow) []sysTab {
	tabs := make([]sysTab, len(sysLayout))
	for ti, def := range sysLayout {
		tabs[ti] = sysTab{Label: def.Label, Slug: def.Slug, Cards: make([]sysCard, len(def.Cards))}
		for ci, cd := range def.Cards {
			tabs[ti].Cards[ci] = sysCard{sysCardDef: cd}
		}
	}
	keys := make([]string, 0, len(rows))
	for _, f := range sysconfig.Fields() {
		if _, ok := rows[f.Key]; ok {
			keys = append(keys, f.Key)
		}
	}
	placed := map[string]bool{}
	for ti, def := range sysLayout {
		for ci, cd := range def.Cards {
			c := &tabs[ti].Cards[ci]
			for _, k := range cd.Keys {
				if row, ok := rows[k]; ok {
					c.Rows = append(c.Rows, row)
					placed[k] = true
				}
			}
		}
	}
	for _, k := range keys {
		if !placed[k] {
			ti, ci := sysPlace(k)
			tabs[ti].Cards[ci].Rows = append(tabs[ti].Cards[ci].Rows, rows[k])
		}
	}
	for ti := range tabs {
		for ci := range tabs[ti].Cards {
			c := &tabs[ti].Cards[ci]
			c.Rows = sysNest(c.Rows)
			c.RestartAll = sysRestartAll(c.Rows)
		}
	}
	return tabs
}

// sysNest hangs each dependent row under the switch it names, when that switch is in the card.
func sysNest(rows []sysRow) []sysRow {
	var out []sysRow
	for _, r := range rows {
		dep := sysFieldCopy(r.Key).Dep
		i := slices.IndexFunc(out, func(o sysRow) bool { return o.Key == dep })
		if dep != "" && i >= 0 {
			out[i].Deps = append(out[i].Deps, r)
			continue
		}
		out = append(out, r)
	}
	return out
}

// sysRestartAll says that every editable setting of a card applies after a restart, so the card
// carries one badge instead of each field.
func sysRestartAll(rows []sysRow) bool {
	n := 0
	for _, r := range rows {
		all := append([]sysRow{r}, r.Deps...)
		for _, x := range all {
			if x.Locked {
				continue
			}
			if x.Apply == sysconfig.Live {
				return false
			}
			n++
		}
	}
	return n > 0
}

// hasProblem is a row with an error or a stored secret that must be entered again: the page opens
// the tab it sits on.
func (r sysRow) hasProblem() bool {
	if r.Err != "" || r.NeedsReentry {
		return true
	}
	return slices.ContainsFunc(r.Deps, sysRow.hasProblem)
}

// sysCopy is what the page says about a setting: a short label and help (one line, at most 72
// characters), the longer text behind the info button, the width class and the switch it depends on.
type sysCopy struct {
	Label string // replaces the sysconfig label where the card already says what it is about
	Help  string
	Info  string
	Width string // full, half or xs
	Dep   string // key of the switch this setting only matters for
}

// sysCopies is the copy of every setting. Help is the one line under the field; Info is the rest.
var sysCopies = map[string]sysCopy{
	"LOG_LEVEL":       {Help: "Applies at once. Debug is verbose.", Width: "full"},
	"AUDIT_RETENTION": {Help: "At least 24h. 720h = 30 days.", Width: "xs", Info: "How long audit events and finished update transactions (with their logs) are kept. A running transaction is never removed. 8760h is 365 days."},

	"METRICS_ENABLED":       {Label: "Serve /metrics", Help: "Off: /metrics answers 404.", Width: "full"},
	"METRICS_TOKEN":         {Help: "Write-only. Empty: local scraper only.", Width: "half", Dep: "METRICS_ENABLED", Info: "Bearer token a remote Prometheus sends (Authorization: Bearer ...). Empty: only a scraper on this host, not behind a proxy, is served. Applies at once."},
	"METRICS_DEVICE_LABELS": {Help: "Off: metrics carry counts only.", Width: "full"},

	"DEVICE_NETWORKS":         {Help: "CIDRs, one per line or comma separated.", Width: "full", Info: "TasmoFleet never contacts an address outside these networks: no polling, no scan, no update."},
	"POLL_INTERVAL":           {Help: "Without MQTT. At least 1s.", Width: "xs"},
	"POLICY_LIVE_INTERVAL":    {Label: "Policy check read", Help: "0 = off.", Width: "xs", Info: "How often each device is read for the policy view (jittered, side-effect-free queries). 0 = no background reads; MQTT updates and the on-demand check stay."},
	"CR_WARN_PERCENT":         {Help: "Percent, 50 to 100.", Width: "xs", Info: "Warn on the device page when a device's settings text area (Status 2 CR) is fuller than this."},
	"TELEPERIOD":              {Label: "Telemetry period", Help: "Stale after 2× + 1 min.", Width: "xs", Info: "Expected telemetry period. A device counts as stale after twice this period plus a minute."},
	"DEVICE_DEFAULT_PASSWORD": {Help: "Write-only. Prefills new devices.", Width: "half", Info: "Prefilled as the web password when you add a device or accept a discovered one (user admin). Each device keeps its own copy; you can still change it per device."},

	"MQTT_URL":       {Label: "Host", Width: "full"},
	"MQTT_USER":      {Help: "Empty if the broker needs no login.", Width: "half"},
	"MQTT_PASSWORD":  {Help: "Write-only. Empty keeps it.", Width: "half"},
	"MQTT_FULLTOPIC": {Help: "Must contain %prefix% and %topic% once each.", Width: "full", Info: "Discovery listens on tasmota/discovery/+/config."},

	"DISCOVERY_MQTT":          {Help: "Needs MQTT. Finds are proposals.", Width: "full", Info: "A discovered device is never added on its own: an admin accepts it on the Discovered page."},
	"DISCOVERY_SCAN":          {Label: "Scheduled network scan", Help: "Probes the networks below on a schedule.", Width: "full"},
	"DISCOVERY_SCAN_INTERVAL": {Help: "At least 1h.", Width: "xs", Dep: "DISCOVERY_SCAN"},
	"DISCOVERY_SCAN_CIDRS":    {Help: "Inside the device networks, max /22 each.", Width: "full", Dep: "DISCOVERY_SCAN", Info: "Each CIDR must lie inside the device networks and hold at most 1024 addresses (/22)."},

	"BACKUP_INTERVAL": {Label: "Interval", Help: "e.g. 24h.", Width: "xs"},
	"BACKUP_KEEP":     {Label: "Kept per device", Help: "Lowering it can delete restore points.", Width: "xs"},
	"BACKUP_MAX_AGE":  {Label: "Never prune younger than", Help: "e.g. 720h.", Width: "xs", Info: "Protects restore points from repeated manual runs: a backup younger than this is never pruned, even above the per-device count."},

	"FIRMWARE_BASE_URL":            {Help: "Plain http:// address devices fetch firmware from.", Width: "full", Info: "Devices download firmware from this address, e.g. the container address. Required for updates."},
	"FIRMWARE_SOURCES":             {Help: "Two sources = cross-check.", Width: "full"},
	"FIRMWARE_REFRESH_INTERVAL":    {Help: "0 = only at start.", Width: "xs", Info: "How often the upstream index is refreshed in the background, e.g. 6h."},
	"FIRMWARE_ALLOW_SINGLE_SOURCE": {Help: "Off is recommended.", Width: "full", Info: "Off: a firmware file must be offered by two sources with the same checksum. On: one source is enough. Shown in the warning colour while on."},

	"OTA_PARALLEL":       {Help: "Fleet-wide.", Width: "xs"},
	"OTA_POWER_MAX_AGE":  {Label: "Relay state max age", Help: "Older states stay off.", Width: "xs", Info: "A relay state read before an update may switch relays back on only while it is younger than this."},
	"OTA_RESTORE_OTAURL": {Help: "Device gets its previous update address back.", Width: "full"},

	"AUTH_MODE":           {Width: "full"},
	"OIDC_ISSUER":         {Help: "With the trailing slash.", Width: "full"},
	"OIDC_CLIENT_ID":      {Help: "Registered for TasmoFleet at your IdP.", Width: "full"},
	"OIDC_CLIENT_SECRET":  {Help: "Write-only.", Width: "half", Info: "Sent only to the issuer it was entered for. Change the issuer and the secret must be entered again."},
	"OIDC_SCOPES":         {Help: "Comma separated.", Width: "half"},
	"OIDC_DISPLAY_NAME":   {Help: "“Sign in with <name>”.", Width: "half", Info: "Name of your identity provider on the login page. Default: Single sign-on."},
	"ADMIN_GROUPS":        {Help: "IdP groups, comma separated.", Width: "full"},
	"OPERATOR_GROUPS":     {Help: "IdP groups, comma separated.", Width: "full"},
	"OIDC_GROUPS_CLAIM":   {Help: "Default: groups.", Width: "full", Info: "Claim in the ID token that lists the user's groups."},
	"OIDC_USERNAME_CLAIM": {Help: "Default: preferred_username.", Width: "full", Info: "Claim shown as the user name."},
}

// sysFieldCopy is the copy of a setting; one without an entry (a new field) falls back to the
// sysconfig help, cut to its first sentence, and the half width.
func sysFieldCopy(key string) sysCopy {
	if c, ok := sysCopies[key]; ok {
		return c
	}
	f, _ := sysconfig.Lookup(key)
	help, _, _ := strings.Cut(f.Help, ". ")
	if len(help) > 72 {
		help = strings.TrimSpace(help[:72])
	}
	return sysCopy{Help: help, Width: "half", Info: f.Help}
}

// sysLabel is the label of a row: the card's own wording, else the sysconfig label.
func sysLabel(r sysRow) string {
	if l := sysFieldCopy(r.Key).Label; l != "" {
		return l
	}
	return r.Label
}

// sysCardBadges are the badges in a card's header: one Restart badge when every setting needs a
// restart, and the note that saving asks to confirm.
func sysCardBadges(c sysCard) []ui.SettingsBadge {
	var out []ui.SettingsBadge
	if c.RestartAll {
		out = append(out, ui.SettingsBadge{Tone: ui.ToneWarn, Label: "Restart"})
	}
	if c.StepUp {
		out = append(out, ui.SettingsBadge{Tone: ui.ToneAccent, Label: "Asks you to confirm"})
	}
	return out
}

// sysScheduleOn says whether the backup schedule switch is on: the interval is not zero. An
// interval the server rejected stays on, so the error is shown next to the switch that caused it.
func sysScheduleOn(r sysRow) bool {
	if r.Err != "" {
		return true
	}
	if r.Posted == "0" || r.Posted == "" {
		return false
	}
	d, err := time.ParseDuration(r.Posted)
	return err != nil || d != 0
}

// sysChecked lists the choices of a list setting that are on.
func sysChecked(r sysRow) []string {
	return strings.Split(r.Posted, ",")
}
