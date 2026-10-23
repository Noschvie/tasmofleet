package web

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the audit table is read by people. Each action type gets a short
// summary that adds information (a count, a version, a command) and never
// repeats what the Action and Target columns already say; failures show their
// reason there, because the Result column is only a status badge.
func TestAuditSummaryPerActionType(t *testing.T) {
	ev := func(action, target, result, detail string) model.AuditEvent {
		return model.AuditEvent{Action: action, Target: target, Result: result, Detail: detail}
	}
	cases := []struct {
		name   string
		e      model.AuditEvent
		label  string
		status string
		want   string
	}{
		{"login local", ev("auth.login.local", "", "ok", ""), "Sign-in (local)", "ok", ""},
		{"login sso", ev("auth.login.oidc", "", "ok", `{"user":"alice","role":"admin"}`), "Sign-in (SSO)", "ok", "role admin"},
		{"login sso denied", ev("auth.login.oidc", "", "denied: no matching group", `{"user":"x"}`), "Sign-in (SSO)", "refused", "No matching group"},
		{"logout", ev("auth.logout", "", "ok", ""), "Sign-out", "ok", ""},
		{"recheck role", ev("auth.session.recheck", "", "role changed", `{"role":"operator"}`), "Session re-check", "ok", "role operator"},
		{"recheck ended", ev("auth.session.recheck", "", "denied: refresh failed", ""), "Session re-check", "refused", "Refresh failed"},
		{"stepup password", ev("account.stepup", "", "ok", ""), "Confirm identity", "ok", "via password"},
		{"stepup sso", ev("account.stepup", "", "ok", `{"method":"oidc"}`), "Confirm identity", "ok", "via SSO"},
		{"stepup wrong", ev("account.stepup", "", "denied: wrong password", ""), "Confirm identity", "refused", "Wrong password"},
		{"password change", ev("account.password.change", "", "ok", ""), "Password change", "ok", ""},
		{"user create", ev("account.create", "", "ok", `{"user":"alice"}`), "Create user", "ok", "alice"},
		{"user delete", ev("account.delete", "", "ok", `{"user":"bob"}`), "Delete user", "ok", "bob"},
		{"backup", ev("backup.create", "device:1", "ok", `{"files":3,"firmware":"16.0.0"}`), "Backup", "ok", "3 files · 16.0.0"},
		{"bulk backup start", ev("backup.bulk.start", "transaction:5", "ok", `{"devices":[1,2],"skipped":1,"keep":false,"label":""}`), "Start backups", "ok", "2 devices · 1 left out"},
		{"bulk backup kept", ev("backup.bulk.start", "transaction:5", "ok", `{"devices":[1],"skipped":0,"keep":true,"label":"before 16"}`), "Start backups", "ok", "1 device · kept · label before 16"},
		{"bulk command start", ev("command.bulk.start", "transaction:6", "ok", `{"devices":[1,2,3],"skipped":0}`), "Start command run", "ok", "3 devices"},
		{"backup one file", ev("backup.create", "device:1", "ok", `{"files":1,"firmware":"15.1.0"}`), "Backup", "ok", "1 file · 15.1.0"},
		{"backup failed", ev("backup.create", "device:1", "tasmota: timeout", `{"files":0}`), "Backup", "failed", "The device did not answer in time"},
		{"backup download", ev("backup.download", "backup:12", "ok", `{"device":1,"kind":"snapshot"}`), "Backup download", "ok", "Snapshot"},
		{"command", ev("command.send", "device:1", "ok", `{"command":"Status 2","class":"read"}`), "Console command", "ok", "Status 2"},
		{"command refused", ev("command.rejected", "device:1", "refused: not allowed for operators", `{"command":"Reset 1"}`), "Console command refused", "refused", "Not allowed for operators"},
		{"power", ev("device.power", "device:1", "ok", `{"relay":2,"action":"ON","via":"mqtt","state":"ON"}`), "Power", "ok", "relay 2 on · mqtt"},
		{"device create", ev("device.create", "device:1", "ok", `{"name":"x","address":"172.31.13.5"}`), "Add device", "ok", "172.31.13.5"},
		{"device create upsert", ev("device.create", "device:1", "ok", `{"address":"172.31.13.5","via":"upsert"}`), "Add device", "ok", "172.31.13.5 · upsert"},
		{"device update names", ev("device.update", "device:1", "ok", `{"changed":["notes","ota_excluded"]}`), "Edit device", "ok", "notes, ota excluded"},
		{"device update address", ev("device.update", "device:1", "ok", `{"changed":["address"],"address_from":"172.31.13.5","address_to":"172.31.13.6"}`), "Edit device", "ok", "172.31.13.5 → 172.31.13.6"},
		{"device delete", ev("device.delete", "device:1", "ok", `{"name":"x","address":"172.31.13.5"}`), "Delete device", "ok", "172.31.13.5"},
		{"upsert refused", ev("device.upsert", "device:1", "inventory: matching device has a different MAC", `{"refused":"identity"}`), "Device seen", "refused", "address change – review"},
		{"upsert gone", ev("device.upsert", "device:1", "gone", `{"refused":"gone"}`), "Device seen", "refused", "device was removed meanwhile"},
		{"device setting", ev("settings.set", "device:1", "ok", `{"command":"SetOption4 1"}`), "Device setting", "ok", "SetOption4 1"},
		{"policy check", ev("policy.live", "fleet", "", `{"devices_read":12,"throttled":false}`), "Policy check", "ok", "12 devices read"},
		{"fix start", ev("policy.fix.start", "transaction:5", "ok", `{"devices":[1,2,3],"skipped":1}`), "Make compliant", "ok", "3 devices · 1 left out"},
		{"fix device", ev("policy.fix", "device:1", "ok", `{"transaction":5,"job":9,"commands":["PowerOnState 0","SetOption0 0"]}`), "Make compliant (device)", "ok", "PowerOnState 0; SetOption0 0 · #5"},
		{"fix device failed", ev("policy.fix", "device:1", "the device is unreachable", `{"transaction":5,"commands":["SaveData 0"]}`), "Make compliant (device)", "failed", "The device is unreachable"},
		{"rules save", ev("policy.rules.save", "policy:rules", "ok", `{"version":7,"revert_of":0,"rules":12,"changes":[{"kind":"added"},{"kind":"removed"}]}`), "Save policy rules", "ok", "v7 · 12 rules · 2 changes"},
		{"rules revert", ev("policy.rules.revert", "policy:rules", "ok", `{"version":8,"revert_of":3,"rules":10,"changes":[]}`), "Revert policy rules", "ok", "v8 from v3 · 10 rules · 0 changes"},
		{"sysconfig", ev("sysconfig.update", "settings:system", "ok", `{"changes":[{"key":"BACKUP_KEEP","old":"5","new":"7"},{"key":"POLL_INTERVAL"}]}`), "System settings", "ok", "BACKUP_KEEP, POLL_INTERVAL"},
		{"sysconfig reset", ev("sysconfig.reset", "settings:system", "ok", `{"changes":[{"key":"BACKUP_KEEP","old":"4","new":"10"}]}`), "Settings reset", "ok", "BACKUP_KEEP"},
		{"sysconfig denied", ev("sysconfig.update", "settings:system", "denied: step-up required", `{"changes":[]}`), "System settings", "refused", "Step-up required"},
		{"env import", ev("sysconfig.env_import", "settings:system", "ok", `{"keys":["MQTT_URL"]}`), "Settings from environment", "ok", "MQTT_URL"},
		{"restore start", ev("restore.start", "device:1", "ok", `{"restore":4,"info":"backup 9 (16.0.0)"}`), "Restore", "ok", "backup 9 (16.0.0)"},
		{"restore finish", ev("restore.finish", "device:1", "ok", `{"restore":4,"info":"settings restored"}`), "Restore finished", "ok", "settings restored"},
		{"restore download", ev("restore.download", "device:1", "ok", `{"restore":4}`), "Restore download", "ok", "served once"},
		{"scan start", ev("discovery.scan_start", "scan:s1", "ok", `{"cidrs":["172.31.13.0/24"],"probes":254}`), "Network scan", "ok", "172.31.13.0/24 · 254 probes"},
		{"scan end", ev("discovery.scan_end", "scan:s1", "ok", `{"state":"done","hits":3}`), "Network scan finished", "ok", "Done · 3 found"},
		{"scan refused", ev("discovery.scan_refused", "", "refused: outside device networks", `{"cidrs":["10.0.0.0/8"]}`), "Network scan refused", "refused", "Outside device networks"},
		{"accept", ev("discovery.accept", "proposal:5", "ok", `{"kind":"address-change","mac":"aa","address":"172.31.13.9"}`), "Accept discovered device", "ok", "Known device – address changed · 172.31.13.9"},
		{"dismiss", ev("discovery.dismiss", "proposal:5", "ok", `{}`), "Ignore discovered device", "ok", ""},
		{"firmware delete", ev("firmware.delete", "artifact:7", "ok", `{"release":"16.0.0","file":"tasmota.bin.gz","size":1}`), "Delete firmware file", "ok", "16.0.0 · tasmota.bin.gz"},
		{"firmware delete refused", ev("firmware.delete", "artifact:7", "refused: needed by transaction #3", `{}`), "Delete firmware file", "refused", "Needed by transaction #3"},
		{"prefetch", ev("firmware.prefetch", "release:16.0.0", "ok", `{"files":5}`), "Download firmware", "ok", "5 files"},
		{"custom upload", ev("firmware.custom.upload", "firmware:1.2.3/tasmota.bin", "ok", `{"label":"my build","release":"1.2.3"}`), "Upload firmware", "ok", "my build · 1.2.3"},
		{"custom upload refused", ev("firmware.custom.upload", "", "refused", `{"reason":"not a Tasmota image"}`), "Upload firmware", "refused", "not a Tasmota image"},
		{"custom use", ev("firmware.custom.use", "transaction:3", "ok", `{"label":"my build","devices":2}`), "Use uploaded firmware", "ok", "my build · 2 devices"},
		{"import", ev("import.tasmoadmin", "inventory", "ok", `{"rows":30,"created":27,"updated":3,"failed":0}`), "TasmoAdmin import", "ok", "27 created · 3 updated"},
		{"retry", ev("transaction.retry", "transaction:3", "ok", `{"retry_of":3,"batch":4,"devices":[1,2]}`), "Retry transaction", "ok", "2 devices · as #4"},
		{"start", ev("transaction.start", "transaction:12", "ok", `{"devices":5,"release":"16.1.0"}`), "Start update", "ok", "5 devices · 16.1.0"},
		{"unknown type", ev("thing.happened", "x:1", "ok", `{"b":2,"a":"x","nested":{"k":1}}`), "Thing happened", "ok", "a=x b=2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := auditLabel(c.e.Action); got != c.label {
				t.Errorf("label = %q, want %q", got, c.label)
			}
			if got := auditStatus(c.e); got != c.status {
				t.Errorf("status = %q, want %q", got, c.status)
			}
			if got := auditSummary(c.e); got != c.want {
				t.Errorf("summary = %q, want %q", got, c.want)
			}
			short := auditShort(c.e)
			if utf8.RuneCountInString(short) > auditSummaryMax || strings.ContainsAny(short, "\n\r") {
				t.Errorf("short summary %q breaks the one-line limit", short)
			}
			if c.status == auditOK && short != "" && (strings.EqualFold(short, c.label) || strings.EqualFold(short, auditTargetName(c.e.Target))) {
				t.Errorf("summary %q repeats the action or target", short)
			}
		})
	}
}

// Why: a long error or command must never widen the row; it is cut at 40
// characters with an ellipsis and the full text stays in the tooltip.
func TestAuditSummaryIsClippedToOneLine(t *testing.T) {
	long := model.AuditEvent{Action: "command.send", Result: "ok", Detail: `{"command":"Backlog Power1 1; Delay 10; Power1 0; Delay 10; Power2 1"}`}
	short := auditShort(long)
	if utf8.RuneCountInString(short) != auditSummaryMax || !strings.HasSuffix(short, "…") {
		t.Fatalf("short = %q", short)
	}
	if !strings.Contains(auditSummary(long), "Power2 1") {
		t.Fatal("the full text must stay available")
	}
}

// Why: the Result column carries one of three statuses and nothing else, so a
// failure is never mistaken for a refusal: a refused attempt is the system
// working as designed, a failure needs a look.
func TestAuditStatusSeparatesRefusedFromFailed(t *testing.T) {
	for result, want := range map[string]string{
		"ok": auditOK, "role changed": auditOK, "refused: no": auditRefused, "denied: rate limited": auditRefused,
		"forbidden": auditRefused, "tasmota: timeout": auditFailed, "error: boom": auditFailed, "interrupted: x": auditFailed,
	} {
		if got := auditStatus(model.AuditEvent{Result: result}); got != want {
			t.Errorf("status(%q) = %q, want %q", result, got, want)
		}
	}
}

// Why: the target column shows names, not keys, for every kind the audit log writes.
func TestAuditTargetNames(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "fleet": "All devices", "settings:system": "System settings", "backup:12": "Backup #12",
		"scan:abc": "Network scan", "proposal:5": "Discovered device #5", "artifact:7": "Firmware file #7",
		"firmware:16.0.0/tasmota.bin.gz": "tasmota.bin.gz (16.0.0)", "release:16.0.0": "Release 16.0.0", "weird": "weird",
	} {
		if got := auditTargetName(in); got != want {
			t.Errorf("auditTargetName(%q) = %q, want %q", in, got, want)
		}
	}
}
