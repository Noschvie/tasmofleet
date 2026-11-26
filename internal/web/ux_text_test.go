package web

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: "1 deviation(s)" read like unfinished text; the UI says "1 deviation", "2 deviations".
func TestPluralAgreesWithTheCount(t *testing.T) {
	for _, c := range []struct {
		n    any
		want string
	}{{1, "1 device"}, {0, "0 devices"}, {2, "2 devices"}, {int64(1), "1 device"}} {
		if got := plural(c.n, "device"); got != c.want {
			t.Errorf("plural(%v) = %q, want %q", c.n, got, c.want)
		}
	}
}

// Why: the audit log shows what was acted on (a name, a transaction) and links to it;
// older rows say "batch:3" and must read like the new "transaction:3".
func TestAuditTargetReadsAsAName(t *testing.T) {
	names := map[model.DeviceID]string{12: "kitchen lamp"}
	for _, c := range []struct{ in, text, href string }{
		{"device:12", "kitchen lamp", "/tf/devices/12"},
		{"device:99", "removed device 99", ""},
		{"batch:3", "job TX-0003", "/tf/transactions/3"},
		{"transaction:3", "job TX-0003", "/tf/transactions/3"},
		{"policy:rules", "Policy rules", ""},
		{"backup:7", "Backup #7", ""},
		{"fleet", "All devices", ""},
		{"device:x", "device:x", ""},
	} {
		text, href := auditTarget(c.in, names, "/tf")
		if text != c.text || href != c.href {
			t.Errorf("auditTarget(%q) = %q, %q; want %q, %q", c.in, text, href, c.text, c.href)
		}
	}
}

// Why: an incomplete snapshot is a warning, not a note; only the exact backup note counts.
func TestSnapshotGaps(t *testing.T) {
	if got := snapshotGaps("snapshot incomplete: Status 2, Status 5"); got != "Status 2, Status 5" {
		t.Errorf("got %q", got)
	}
	for _, n := range []string{"", "before update", "snapshot incomplete"} {
		if got := snapshotGaps(n); got != "" {
			t.Errorf("snapshotGaps(%q) = %q, want empty", n, got)
		}
	}
}

// A health detail that only repeats the Source column adds noise; one that
// names a cause (rejected credentials, an error kind) must stay.
func TestHealthDetailOnlyWhenItAddsInformation(t *testing.T) {
	for _, c := range []struct {
		src    model.HealthSource
		detail string
		want   string
	}{
		{model.SourceHTTP, "http poll ok", ""},
		{model.SourceHTTP, "", ""},
		{model.SourceMQTT, "MQTT", ""},
		{model.SourceHTTP, "http poll: credentials rejected", "http poll: credentials rejected"},
		// #1094: the stored token of a state change that the LWT topic caused ("mqtt lwt", also in rows written
		// before this version) reads as words; any other detail is shown as it is
		{model.SourceMQTT, "mqtt lwt", "MQTT LWT topic"},
		{model.SourceHTTP, "change 3", "change 3"},
	} {
		if got := healthDetail(c.src, c.detail); got != c.want {
			t.Errorf("healthDetail(%q,%q) = %q, want %q", c.src, c.detail, got, c.want)
		}
	}
}
