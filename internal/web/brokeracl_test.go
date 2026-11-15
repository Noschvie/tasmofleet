package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
	"github.com/hellwand/tasmofleet/internal/transport"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// Why (#840, the product owner: "big, red and bold"): since 0.37.0 MQTT is the first transport for everything, so the
// broker hands every command, password and configuration dump to every client allowed to subscribe to the device's
// topics. An access control list on the broker is the only thing that keeps those clients out, and TasmoFleet cannot
// see whether there is one. The warning therefore stands where MQTT settings and MQTT-carried secrets appear, with the
// same lead and a link to the guide, and nowhere else (a notice everywhere is a notice nobody reads).
//
// countACL is how many ACL notices a page has; it also checks the lead and the link of each.
func countACL(t *testing.T, page string) int {
	t.Helper()
	n := strings.Count(page, "data-acl-warning")
	if n == 0 {
		return 0
	}
	if strings.Count(page, ui.BrokerACLLead) != n || strings.Count(page, `href="`+ui.BrokerACLDocURL+`"`) != n {
		t.Errorf("%d notices, but the lead appears %d times and the guide link %d times", n,
			strings.Count(page, ui.BrokerACLLead), strings.Count(page, `href="`+ui.BrokerACLDocURL+`"`))
	}
	if !strings.Contains(page, `class="banner tone-danger acl-warning"`) {
		t.Error("the notice is not the danger banner")
	}
	return n
}

func TestSystemMQTTTabCarriesTheBrokerACLWarning(t *testing.T) {
	e, _, _, _ := sysEnv(t, true)
	_, page := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if n := countACL(t, page); n != 1 {
		t.Fatalf("the MQTT tab has %d ACL notices, want 1", n)
	}
	// it belongs to the MQTT tab: after its opening tag and before the broker fields
	start := strings.Index(page, `data-sys-tab="mqtt"`)
	notice := strings.Index(page, "data-acl-warning")
	fields := strings.Index(page, `name="mqtt.host"`)
	if start < 0 || notice < start || notice > fields {
		t.Errorf("the notice is not at the top of the MQTT tab: tab %d, notice %d, broker field %d", start, notice, fields)
	}
}

func TestSetupMQTTSectionCarriesTheBrokerACLWarning(t *testing.T) {
	e, _, _ := setupHandlerEnv(t)
	_, page := e.do(t, "GET", "/setup", "admin", nil)
	if n := countACL(t, page); n != 1 {
		t.Fatalf("the setup page has %d ACL notices, want 1", n)
	}
	if strings.Index(page, "data-acl-warning") > strings.Index(page, `name="mqtt.host"`) {
		t.Error("the notice comes after the broker field")
	}
}

func TestDeviceMQTTPasswordFieldCarriesTheBrokerACLWarning(t *testing.T) {
	e, _ := mqttEnv(t, ownDevice())
	_, page := e.do(t, "GET", "/devices/7/edit", "admin", nil)
	if n := countACL(t, page); n != 1 {
		t.Fatalf("the edit form has %d ACL notices, want 1", n)
	}
	if strings.Index(page, "data-acl-warning") > strings.Index(page, `name="mqtt_password_mode"`) {
		t.Error("the notice comes after the MQTT password fields, not before them")
	}
	if !strings.Contains(page, "backup or restore") {
		t.Error("the notice does not say that a backup or restore carries the password and the dump through the broker")
	}
}

// Why (#826, #840): the sentence the console puts before a hidden command that goes over MQTT says plainly that,
// without an ACL, that is every client of the broker, and the confirmation shows the danger notice; over HTTP there
// is nothing to warn about.
func TestConsoleConfirmationOverMQTTWarnsAboutTheBrokerACL(t *testing.T) {
	for _, tc := range []struct {
		name string
		mqtt bool
		want int
	}{{"over MQTT", true, 1}, {"over HTTP", false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &routeStub{}
			if tc.mqtt {
				rs.set(false, 1)
			} else {
				rs.set(false)
			}
			e, _ := noticeEnv(t, rs)
			_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("WebPassword hunter2-840"))
			if !strings.Contains(body, "Confirm sending") {
				t.Fatalf("no confirmation: %s", body)
			}
			if n := countACL(t, body); n != tc.want {
				t.Fatalf("%d ACL notices on the confirmation, want %d", n, tc.want)
			}
			if tc.mqtt && !strings.Contains(body, "without a broker ACL that is every client of the broker") {
				t.Errorf("the route sentence does not say who can read it: %s", body)
			}
		})
	}
}

func TestBulkCommandPreviewOverMQTTWarnsAboutTheBrokerACL(t *testing.T) {
	rs := &routeStub{}
	rs.set(false, 1, 2)
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = &fakeBatch{}; d.TypedRoutes = rs.routes })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: time.Now()}
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
	if n := countACL(t, body); n != 1 {
		t.Fatalf("the preview has %d ACL notices, want 1: %s", n, body)
	}
	// the same preview with every device over HTTP has none
	rs.set(false)
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
	if n := strings.Count(body, "data-acl-warning"); n != 0 {
		t.Errorf("a preview over HTTP only has %d ACL notices", n)
	}
}

// Why (#607, #840): a restore carries the whole configuration, with every password in it, through the broker. Its
// confirmation is the last page before that happens, so it shows the danger notice, and only when the restore goes
// over MQTT.
func TestRestoreConfirmationOverMQTTWarnsAboutTheBrokerACL(t *testing.T) {
	e, _, rs := newDiffEnv(t)
	rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT first (FileUpload)"}
	_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if n := countACL(t, body); n != 1 {
		t.Fatalf("the restore page over MQTT has %d ACL notices, want 1", n)
	}
	for _, want := range []string{"whole configuration", "every password"} {
		if !strings.Contains(body, want) {
			t.Errorf("the restore notice lacks %q", want)
		}
	}
	rs.assess = restore.Assessment{PathNote: "over HTTP"}
	_, body = e.do(t, "GET", "/backups/1/restore", "admin", nil)
	if n := strings.Count(body, "data-acl-warning"); n != 0 {
		t.Errorf("the restore page over HTTP has %d ACL notices", n)
	}
}

// Why (#607, #840): a backup over MQTT puts the device's whole settings dump, with every secret, on the broker. The
// bulk backup page is where a person starts several at once, so it warns when a broker is configured; without a
// broker there is nothing to warn about.
func TestBulkBackupScopePageWarnsAboutTheBrokerACLWhenABrokerIsConfigured(t *testing.T) {
	withFacts := func(d *Deps) {
		d.BackupBatch = &fakeBackupBatch{}
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(time.Now(), model.MQTTPasswordOwn)}}
	}
	e, _, _, _ := sysEnvFull(t, true, withFacts)
	_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", url.Values{"csrf": {"tok"}, "id": {"1"}})
	if n := countACL(t, body); n != 1 {
		t.Fatalf("the bulk backup page has %d ACL notices, want 1: %s", n, body)
	}
	e, svc, _, _ := sysEnvFull(t, true, withFacts)
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, body = e.do(t, "POST", "/devices/bulk/backup", "admin", url.Values{"csrf": {"tok"}, "id": {"1"}})
	if n := strings.Count(body, "data-acl-warning"); n != 0 {
		t.Errorf("without a broker the bulk backup page has %d ACL notices", n)
	}
}
