package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// The audit table shows an action as a readable label, the target as a name,
// the result as a status badge, and one short "Summary" that says what the
// row is about without repeating the label or the target. The raw action,
// target and JSON stay available (tooltip, Details).

// auditSummaryMax is the longest summary in characters; longer ones are cut
// with an ellipsis and the full text is the tooltip.
const auditSummaryMax = 40

// auditLabels turn the dotted action keys into names a person reads.
var auditLabels = map[string]string{
	"auth.login.local": "Sign-in (local)", "auth.login.oidc": "Sign-in (SSO)", "auth.logout": "Sign-out",
	"auth.session.recheck": "Session re-check",
	"account.stepup":       "Confirm identity", "sysconfig.stepup": "Confirm identity", "restore.stepup": "Confirm identity", "import.stepup": "Confirm identity",
	"account.password.change": "Password change", "account.manage": "Manage user",
	"account.create": "Create user", "account.role": "Change user role", "account.disable": "Disable user",
	"account.enable": "Enable user", "account.password": "Reset password", "account.delete": "Delete user",
	"backup.create": "Backup", "backup.download": "Backup download",
	"command.send": "Console command", "command.rejected": "Console command refused",
	"device.power": "Power", "device.create": "Add device", "device.update": "Edit device",
	"device.delete": "Delete device", "device.upsert": "Device seen", "settings.set": "Device setting",
	"policy.live": "Policy check", "policy.live.pause": "Pause policy check", "policy.live.resume": "Resume policy check", "policy.fix.start": "Make compliant", "policy.fix": "Make compliant (device)", "policy.rules.save": "Save policy rules", "policy.rules.revert": "Revert policy rules",
	"sysconfig.update": "System settings", "sysconfig.reset": "Settings reset", "sysconfig.mqtt_test": "MQTT test", "sysconfig.sso_test": "SSO check", "account.sessions.end": "End sessions", "sysconfig.env_import": "Settings from environment",
	"restore.start": "Restore", "restore.finish": "Restore finished", "restore.download": "Restore download",
	"discovery.scan_start": "Network scan", "discovery.scan_end": "Network scan finished", "discovery.scan_cancel": "Network scan cancelled",
	"discovery.scan_refused": "Network scan refused", "discovery.accept": "Accept discovered device",
	"discovery.dismiss": "Ignore discovered device", "discovery.undismiss": "Un-ignore discovered device",
	"firmware.delete": "Delete firmware file", "firmware.prefetch": "Download firmware",
	"firmware.custom.upload": "Upload firmware", "firmware.custom.use": "Use uploaded firmware",
	"backup.bulk.start": "Start backups", "command.bulk.start": "Start command run",
	"import.tasmoadmin": "TasmoAdmin import", "transaction.retry": "Retry transaction", "transaction.start": "Start update",
}

// auditLabel is the readable name of an action; an unknown key becomes
// "Some thing" from "some.thing".
func auditLabel(action string) string {
	if l, ok := auditLabels[action]; ok {
		return l
	}
	return humanLabel(strings.NewReplacer(".", " ").Replace(action))
}

// Status values of an audit row: the Result column shows only this.
const (
	auditOK      = "ok"
	auditFailed  = "failed"
	auditRefused = "refused"
)

// auditStatus classifies the free-text result of an event.
func auditStatus(e model.AuditEvent) string {
	l := strings.ToLower(e.Result)
	switch {
	case l == "ok" || l == "" || l == "role changed":
		return auditOK
	case strings.Contains(l, "refus") || strings.Contains(l, "denied") || strings.Contains(l, "forbidden") || detailOf(e.Detail)["refused"] != nil:
		return auditRefused
	}
	return auditFailed
}

func detailOf(raw string) map[string]any {
	var m map[string]any
	if raw == "" || json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

func dStr(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func dInt(m map[string]any, k string) (int, bool) {
	v, ok := m[k].(float64)
	return int(v), ok
}

// dList is the length of a JSON array.
func dList(m map[string]any, k string) (int, bool) {
	v, ok := m[k].([]any)
	return len(v), ok
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// clip cuts s to max characters, ending in an ellipsis.
func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// failureText is the reason of a failed or refused row without the status
// word in front of it.
func failureText(e model.AuditEvent) string {
	r := e.Result
	for _, p := range []string{"refused: ", "denied: ", "error: ", "interrupted: "} {
		r = strings.TrimPrefix(r, p)
	}
	if t, ok := knownReason(r); ok {
		return t
	}
	for _, p := range []string{"no build can be chosen: ", "select: ", "fwselect: ", "tasmota: ", "probe failed: "} {
		r = strings.TrimPrefix(r, p)
	}
	if r == "" {
		return ""
	}
	return strings.ToUpper(r[:1]) + r[1:] // keep hyphens: "step-up" stays "step-up"
}

// auditSummary is the full summary of an event; auditShort clips it. It never
// repeats the action label or the target name. A failed or refused row shows
// the reason instead, a row of an unknown type its scalar details.
func auditSummary(e model.AuditEvent) string {
	d := detailOf(e.Detail)
	if st := auditStatus(e); st != auditOK {
		if e.Action == "device.upsert" {
			switch dStr(d, "refused") {
			case "identity":
				return "address change – review"
			case "gone":
				return "device was removed meanwhile"
			}
		}
		if e.Action == "firmware.custom.upload" && dStr(d, "reason") != "" {
			return dStr(d, "reason")
		}
		return failureText(e)
	}
	if s, ok := summaryOK(e, d); ok {
		return s
	}
	return fallbackSummary(d)
}

func auditShort(e model.AuditEvent) string { return clip(auditSummary(e), auditSummaryMax) }

func summaryOK(e model.AuditEvent, d map[string]any) (string, bool) {
	switch e.Action {
	case "auth.login.local", "auth.logout", "account.password.change", "discovery.scan_cancel",
		"discovery.dismiss", "discovery.undismiss", "restore.stepup", "import.stepup", "sysconfig.stepup":
		return "", true
	case "auth.login.oidc":
		return join(roleText(dStr(d, "role"))), true
	case "auth.session.recheck":
		return roleText(dStr(d, "role")), true
	case "account.stepup":
		if dStr(d, "method") == "oidc" {
			return "via SSO", true
		}
		return "via password", true
	case "account.manage", "account.create", "account.role", "account.disable", "account.enable", "account.password", "account.delete":
		return dStr(d, "user"), true
	case "backup.create":
		n, _ := dInt(d, "files")
		return join(count(n, "file", "files"), dStr(d, "firmware")), true
	case "backup.download":
		return label(dStr(d, "kind")), true
	case "command.send", "command.rejected":
		return dStr(d, "command"), true
	case "device.power":
		n, _ := dInt(d, "relay")
		return join(fmt.Sprintf("relay %d %s", n, strings.ToLower(dStr(d, "action"))), dStr(d, "via")), true
	case "device.create", "device.upsert":
		return join(dStr(d, "address"), dStr(d, "via")), true
	case "device.update":
		if from, to := dStr(d, "address_from"), dStr(d, "address_to"); from != "" {
			return from + " → " + to, true
		}
		ch, _ := d["changed"].([]any)
		var names []string
		for _, c := range ch {
			if s, ok := c.(string); ok {
				names = append(names, strings.ReplaceAll(s, "_", " "))
			}
		}
		if len(names) == 0 {
			return "no change", true
		}
		return strings.Join(names, ", "), true
	case "device.delete":
		return join(dStr(d, "address")), true
	case "settings.set":
		return dStr(d, "command"), true
	case "policy.live":
		if dStr(d, "throttled") == "true" {
			return "skipped (recent read)", true
		}
		n, _ := dInt(d, "devices_read")
		return count(n, "device read", "devices read"), true
	case "policy.rules.save", "policy.rules.revert":
		n, _ := dInt(d, "rules")
		c, _ := dList(d, "changes")
		v := "v" + dStr(d, "version")
		if rv := dStr(d, "revert_of"); rv != "" && rv != "0" {
			v += " from v" + rv
		}
		return join(v, count(n, "rule", "rules"), count(c, "change", "changes")), true
	case "sysconfig.update", "sysconfig.reset":
		return settingKeys(d, "changes"), true
	case "sysconfig.env_import":
		return settingKeys(d, "keys"), true
	case "restore.start", "restore.finish":
		return dStr(d, "info"), true
	case "restore.download":
		return "served once", true
	case "discovery.scan_start", "discovery.scan_refused":
		cidrs, _ := d["cidrs"].([]any)
		var cs []string
		for _, c := range cidrs {
			if s, ok := c.(string); ok {
				cs = append(cs, s)
			}
		}
		n, _ := dInt(d, "probes")
		if n > 0 {
			return join(strings.Join(cs, ", "), count(n, "probe", "probes")), true
		}
		return strings.Join(cs, ", "), true
	case "discovery.scan_end":
		n, _ := dInt(d, "hits")
		return join(label(dStr(d, "state")), fmt.Sprintf("%d found", n)), true
	case "discovery.accept":
		return join(label(dStr(d, "kind")), dStr(d, "address")), true
	case "firmware.delete":
		return join(dStr(d, "release"), dStr(d, "file")), true
	case "firmware.prefetch":
		n, _ := dInt(d, "files")
		return count(n, "file", "files"), true
	case "firmware.custom.upload":
		return join(dStr(d, "label"), dStr(d, "release")), true
	case "firmware.custom.use":
		n, _ := dInt(d, "devices")
		return join(dStr(d, "label"), count(n, "device", "devices")), true
	case "import.tasmoadmin":
		c, _ := dInt(d, "created")
		u, _ := dInt(d, "updated")
		f, _ := dInt(d, "failed")
		out := join(fmt.Sprintf("%d created", c), fmt.Sprintf("%d updated", u))
		if f > 0 {
			out = join(out, fmt.Sprintf("%d failed", f))
		}
		return out, true
	case "transaction.retry":
		n, _ := dList(d, "devices")
		return join(count(n, "device", "devices"), "as #"+dStr(d, "batch")), true
	case "policy.fix.start":
		n, _ := dList(d, "devices")
		s, _ := dInt(d, "skipped")
		out := count(n, "device", "devices")
		if s > 0 {
			out = join(out, fmt.Sprintf("%d left out", s))
		}
		return out, true
	case "backup.bulk.start":
		n, _ := dList(d, "devices")
		s, _ := dInt(d, "skipped")
		out := count(n, "device", "devices")
		if dStr(d, "keep") == "true" {
			out = join(out, "kept")
		}
		if l := dStr(d, "label"); l != "" {
			out = join(out, "label "+l)
		}
		if s > 0 {
			out = join(out, fmt.Sprintf("%d left out", s))
		}
		return out, true
	case "command.bulk.start":
		n, _ := dList(d, "devices")
		s, _ := dInt(d, "skipped")
		out := count(n, "device", "devices")
		if s > 0 {
			out = join(out, fmt.Sprintf("%d left out", s))
		}
		return out, true
	case "policy.fix":
		var cmds []string
		list, _ := d["commands"].([]any)
		for _, c := range list {
			if s, ok := c.(string); ok {
				cmds = append(cmds, s)
			}
		}
		return join(strings.Join(cmds, "; "), "#"+dStr(d, "transaction")), true
	case "transaction.start":
		n, _ := dInt(d, "devices")
		return join(count(n, "device", "devices"), dStr(d, "release")), true
	}
	return "", false
}

func roleText(role string) string {
	if role == "" {
		return ""
	}
	return "role " + role
}

// settingKeys names the changed settings of a sysconfig audit record
// ({"changes":[{"key":..}]} or {"keys":[..]}).
func settingKeys(d map[string]any, field string) string {
	list, _ := d[field].([]any)
	var keys []string
	for _, x := range list {
		switch v := x.(type) {
		case string:
			keys = append(keys, v)
		case map[string]any:
			if k := dStr(v, "key"); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return strings.Join(keys, ", ")
}

// fallbackSummary is key=value for the scalar details of an action this page
// does not know yet, so a new audit record is never blank.
func fallbackSummary(d map[string]any) string {
	var keys []string
	for k, v := range d {
		switch v.(type) {
		case string, float64, bool:
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+dStr(d, k))
	}
	return strings.Join(parts, " ")
}

// auditTargetText is the target as a person reads it. Devices and
// transactions are resolved by auditTarget; the rest of the known kinds are
// named here, anything else is shown as it is.
func auditTargetName(target string) string {
	kind, id, _ := strings.Cut(target, ":")
	switch target {
	case "":
		return ""
	case "fleet":
		return "All devices"
	case "inventory":
		return "Inventory"
	case "settings:system":
		return "System settings"
	case "policy:rules":
		return "Policy rules"
	}
	switch kind {
	case "backup":
		return "Backup #" + id
	case "scan":
		return "Network scan"
	case "proposal":
		return "Discovered device #" + id
	case "artifact":
		return "Firmware file #" + id
	case "release":
		return "Release " + id
	case "firmware":
		if rel, file, ok := strings.Cut(id, "/"); ok {
			return file + " (" + rel + ")"
		}
	}
	return target
}
