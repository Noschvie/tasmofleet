package web

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/store"
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

// aclBrokerService is the settings service with a broker configured (tcp://broker.lan:1883, user "tf") and nothing
// hidden: the state in which the notice shows wherever a path goes over MQTT (#880).
func aclBrokerService(t *testing.T) *sysconfig.Service {
	t.Helper()
	mem := &sysMem{rows: map[string]store.SystemSetting{
		"MQTT_URL":  {Key: "MQTT_URL", Value: "tcp://broker.lan:1883"},
		"MQTT_USER": {Key: "MQTT_USER", Value: "tf"},
	}}
	cfg, err := config.Load([]string{"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: mem, Audit: &fakeAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// brokerWithOwnDevice is an env with a configured broker and the device that has its own MQTT password.
func brokerWithOwnDevice(t *testing.T) (*env, *sysconfig.Service) {
	t.Helper()
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) {
		inv := d.Inventory.(*fakeInv)
		inv.devs[7] = ownDevice()
		d.MQTTPasswords = &fakeMQTTPW{inv: inv}
	})
	return e, svc
}

func TestDeviceMQTTPasswordFieldCarriesTheBrokerACLWarning(t *testing.T) {
	e, _ := brokerWithOwnDevice(t)
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
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd, d.CommandBatch, d.TypedRoutes, d.SysConfig = &fakeCmd{}, &fakeBatch{}, rs.routes, aclBrokerService(t)
	})
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

// Why (#853, follow-up of #840): a backup over MQTT carries every secret of the device through the broker (#607), so
// the places that start one say so, not only the restore: the device's Back up button, "Back up all now" and the
// schedule that backs up by itself. The one notice (data-acl-warning) is the one component of #840 with its one text;
// per-user dismissal was decided against, so there is nothing to close it.
//
// devHeadOf is the header block of a device page: the Back up button and what stands by it.
func devHeadOf(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="devhd"`)
	if i < 0 {
		t.Fatal("no device header in the page")
	}
	rest := page[i:]
	if j := strings.Index(rest, `role="tablist"`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// httpOnlyBackupFacts is a device a backup reaches over HTTP only: it is not on the broker.
func httpOnlyBackupFacts() transport.Facts {
	return transport.Facts{Now: time.Now(), HTTP: transport.HTTPState{State: transport.HTTPOK}, FirmwareBaseURL: true,
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FreeKB: 1700}}
}

func TestDeviceBackUpButtonCarriesTheBrokerACLWarningOnlyOverMQTT(t *testing.T) {
	now := time.Now()
	facts := withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{
		1: mqttFacts(now, model.MQTTPasswordOwn), 2: httpOnlyBackupFacts()}})
	e := setupWith(t, func(d *Deps) { facts(d); d.SysConfig = aclBrokerService(t) })
	if p := transport.Choose(mqttFacts(now, model.MQTTPasswordOwn)).Backup; p != transport.MQTT {
		t.Fatalf("the fixture's backup path is %v, not MQTT", p)
	}
	if p := transport.Choose(httpOnlyBackupFacts()).Backup; p != transport.HTTP {
		t.Fatalf("the HTTP fixture's backup path is %v, not HTTP", p)
	}
	for _, tab := range []string{"/devices/1", "/devices/1/backups"} {
		_, page := e.do(t, "GET", tab, "admin", nil)
		if n := countACL(t, page); n != 1 {
			t.Fatalf("%s has %d ACL notices, want 1", tab, n)
		}
		head := devHeadOf(t, page)
		if !strings.Contains(head, "data-acl-warning") {
			t.Errorf("%s: the notice is not in the header, by the Back up button", tab)
		}
		if strings.Index(head, "data-acl-warning") < strings.Index(head, ">Back up</button>") {
			t.Errorf("%s: the notice comes before the Back up button", tab)
		}
		if !strings.Contains(page, "every password in it") {
			t.Errorf("%s: the notice does not say that the backup carries the passwords", tab)
		}
	}
	_, page := e.do(t, "GET", "/devices/2", "admin", nil)
	if n := strings.Count(page, "data-acl-warning"); n != 0 {
		t.Errorf("a device that is backed up over HTTP has %d ACL notices", n)
	}
}

// Why (#853): the notice says "the warning stays visible until the broker has an ACL". It belongs to the device's
// backup path, not to whether Back up can be pressed right now: while a job holds the device the button is disabled,
// but the running backup is the very one that sends every password through the broker, and the live overview swaps
// the header out of band, so a notice tied to the button would vanish and come back and shift the page.
func TestDeviceBackUpNoticeStaysWhileAJobHoldsTheDevice(t *testing.T) {
	now := time.Now()
	jobs := &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindBackup, Commands: model.BackupMarker, State: model.JobApplying, CreatedAt: now}}}
	e := setupWith(t, func(d *Deps) {
		withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}})(d)
		d.Jobs = jobs
		d.SysConfig = aclBrokerService(t)
	})
	for _, tab := range []string{"/devices/1", "/devices/1/backups"} {
		_, page := e.do(t, "GET", tab, "admin", nil)
		if !strings.Contains(page, `title="Unavailable while a backup of the device is running."`) {
			t.Fatalf("%s: the fixture's Back up button is not held by the running backup", tab)
		}
		if n := strings.Count(devHeadOf(t, page), "data-acl-warning"); n != 1 {
			t.Errorf("%s: %d ACL notices in the header while a backup runs, want 1", tab, n)
		}
	}
}

// Why (#684, #853): the notice says that a backup travels over the broker. It belongs where a backup is started
// and stands once: Back up all now moved from the Devices card to the page header (the table is last on a
// phone), and the notice moved with it, right below the header, above the cards. It is not repeated in a card.
func TestFleetBackupsPageCarriesTheBrokerACLWarningOnceByBackUpAll(t *testing.T) {
	for _, role := range []string{"admin", "operator"} {
		t.Run(role, func(t *testing.T) {
			e := fleetEnv(t, fleetFixture(time.Now()))
			_, body := e.do(t, "GET", "/devices/backups", role, nil)
			if n := countACL(t, body); n != 1 {
				t.Fatalf("the Backups page has %d ACL notices, want 1 (by Back up all now; the same words twice on one page read as noise)", n)
			}
			for _, card := range []string{"schedule", "devices", "lastrun"} {
				if n := strings.Count(cardOf(t, body, card), "data-acl-warning"); n != 0 {
					t.Errorf("the %s card holds %d ACL notices, the notice stands once below the page header", card, n)
				}
			}
			head, notice, wrap := strings.Index(body, `class="pghead"`), strings.Index(body, "data-acl-warning"), strings.Index(body, `class="syswrap"`)
			if head < 0 || notice < head || notice > wrap {
				t.Fatalf("the notice (%d) is not between the page header (%d) and the card stacks (%d)", notice, head, wrap)
			}
			if role == "admin" {
				btn := strings.Index(body, "Back up all now")
				if btn < head || btn > notice || !strings.Contains(body[btn:notice], "</div>") {
					t.Errorf("the notice (%d) does not follow the page header that holds Back up all now (%d), outside its action row", notice, btn)
				}
			}
		})
	}
	// without a broker nothing is sent through one: no notice
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if n := strings.Count(body, "data-acl-warning"); n != 0 {
		t.Errorf("without a broker the Backups page has %d ACL notices", n)
	}
}

// Why (#853): the notice by the device's MQTT password field says that a backup or restore sends the password and the
// dump through the broker. Without a configured broker neither goes anywhere, so the notice would only scare.
func TestDevicePasswordFieldNoticeNeedsAConfiguredBroker(t *testing.T) {
	e, svc := brokerWithOwnDevice(t)
	_, page := e.do(t, "GET", "/devices/7/edit", "admin", nil)
	if n := countACL(t, page); n != 1 {
		t.Fatalf("with a broker the edit form has %d ACL notices, want 1", n)
	}
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"MQTT_URL", "MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	_, page = e.do(t, "GET", "/devices/7/edit", "admin", nil)
	if !strings.Contains(page, `name="mqtt_password_mode"`) {
		t.Fatal("the MQTT password section vanished with the broker: it must stay (the password can be stored first)")
	}
	if n := strings.Count(page, "data-acl-warning"); n != 0 {
		t.Errorf("without a broker the edit form has %d ACL notices", n)
	}
}

// aclPages are the pages whose notice hiding hides, each with how to ask for it: the Backups page (Back up
// all now), the bulk backup page and the System MQTT tab. The console, restore and bulk command confirmations and the
// device's Back up button read the same condition (brokerACLOpen); their tests above and below run with it open.
func aclShown(t *testing.T, e *env) (backups, mqttTab int) {
	t.Helper()
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	backups = countACL(t, body)
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	mqttTab = countACL(t, body)
	return backups, mqttTab
}

// aclSave posts the MQTT tab as the browser does: the broker as host, port and TLS, the user, and the hide box.
func aclSave(t *testing.T, e *env, host, port, user string, confirm bool) {
	t.Helper()
	form := url.Values{"mqtt.host": {host}, "mqtt.port": {port}, "v.MQTT_USER": {user}, "reuse.MQTT_PASSWORD": {"1"}, "mqtt.aclhide.seen": {"1"}}
	if confirm {
		form.Set("mqtt.aclhide", "1")
	}
	if r, body := e.do(t, "POST", "/settings/system?tab=mqtt", "admin", form); r.StatusCode >= 400 {
		t.Fatalf("saving the MQTT tab: %d %s", r.StatusCode, body)
	}
}

// aclApply changes system settings directly (not through the form), keeping the stored broker password.
func aclApply(t *testing.T, svc *sysconfig.Service, values map[string]string) {
	t.Helper()
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "admin", Values: values, Reuse: []string{"MQTT_PASSWORD"},
		StepUp: sysconfig.StepUp{Done: true, Possible: true}}); err != nil {
		t.Fatal(err)
	}
}

// aclHideNow is the admin's confirmation for the broker the service holds now.
func aclHideNow(t *testing.T, svc *sysconfig.Service) {
	t.Helper()
	var c config.Config
	for _, f := range svc.State() {
		switch f.Key {
		case "MQTT_URL":
			c.MQTTURL = f.Value
		case "MQTT_USER":
			c.MQTTUser = f.Value
		}
	}
	aclApply(t, svc, map[string]string{"MQTT_ACL_WARNING_HIDDEN": aclBinding(c)})
}

// aclCycle is what every place of the notice must do (#880): shown with a broker and nothing hidden (want notices),
// gone after hiding, and back when the broker is changed behind the hiding's back (by the settings
// service, not by the form that would drop it).
func aclCycle(t *testing.T, svc *sysconfig.Service, want int, count func() int) {
	t.Helper()
	if n := count(); n != want {
		t.Fatalf("before hiding: %d notices, want %d", n, want)
	}
	aclHideNow(t, svc)
	if n := count(); n != 0 {
		t.Fatalf("after hiding: %d notices, want none", n)
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://elsewhere.lan:1883"})
	if n := count(); n != want {
		t.Fatalf("after the broker changed: %d notices, want %d (the hidden warning is for the old broker)", n, want)
	}
}

// Why (#880): TasmoFleet cannot see an ACL, so a user whose broker has one would see a wrong red warning on every MQTT
// path for ever. The admin hides it, but only for the broker it was given for: another host, port or
// user is another broker, and the notice must be back without anyone remembering to remove the hiding.
func TestBrokerACLHiddenWarningHidesTheNoticeOnlyForTheBrokerItWasGivenFor(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("before hiding the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("hiding given in the save that also changed the user hid the notice (%d, %d): it is for the broker as saved before", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("after hiding the Backups page has %d and the MQTT tab %d notices, want 0 and 0", b, m)
	}
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.aclhide" value="1" checked`) {
		t.Error("the hide box is not ticked after hiding")
	}
	for _, tc := range []struct{ name, host, port, user string }{
		{"another host", "broker2.lan", "1883", "tf"},
		{"another port", "broker.lan", "1884", "tf"},
		{"another user", "broker.lan", "1883", "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
			aclSave(t, e, "broker.lan", "1883", "tf", false)
			aclSave(t, e, "broker.lan", "1883", "tf", true)
			if b, _ := aclShown(t, e); b != 0 {
				t.Fatalf("hiding did not hide the notice (%d)", b)
			}
			aclSave(t, e, tc.host, tc.port, tc.user, true)
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the Backups page has %d and the MQTT tab %d notices after the change, want 1 and 1", tc.name, b, m)
			}
			// the old broker entered again later (not through the form) must not bring the hiding back: it
			// was dropped with the change, not kept for the old broker
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883", "MQTT_USER": "tf"})
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the old hiding came back with the old broker (%d, %d notices)", tc.name, b, m)
			}
		})
	}
}

// Why (#880): hiding the warning is a decision about security, so it is audited like every system setting, and only an
// admin may make it (the settings page is admin only).
func TestBrokerACLHiddenWarningIsAuditedAndAdminOnly(t *testing.T) {
	var aud *fakeAudit
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); aud = d.Recorder.(*fakeAudit) })
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.aclhide": {"1"}, "mqtt.aclhide.seen": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
	if r, _ := e.do(t, "POST", "/settings/system?tab=mqtt", "operator", form); r.StatusCode != 403 {
		t.Fatalf("an operator saved the hiding: %d", r.StatusCode)
	}
	if b, _ := aclShown(t, e); b != 1 {
		t.Fatal("an operator's refused request hid the notice")
	}
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("the notice is still shown after the admin hid it (%d)", b)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, ev := range aud.events {
		if ev.Action == "sysconfig.update" && strings.Contains(ev.Detail, "MQTT_ACL_WARNING_HIDDEN") && ev.Result == "ok" {
			return
		}
	}
	t.Errorf("no audit entry names MQTT_ACL_WARNING_HIDDEN: %+v", aud.events)
}

// aclPath is one place the notice stands on: how to open it (each with its own settings service, as the cycle changes
// the broker), what asking for it as a role returns, and how many notices the page carries while it is open.
type aclPath struct {
	name string
	want int
	open func(t *testing.T) (*sysconfig.Service, func(role string) string)
}

// aclPaths are all places of the notice except the System MQTT tab and the setup, which are the settings themselves.
func aclPaths(now time.Time) []aclPath {
	routed := func(extra func(d *Deps)) func(t *testing.T) (*sysconfig.Service, *env) {
		return func(t *testing.T) (*sysconfig.Service, *env) {
			svc := aclBrokerService(t)
			rs := &routeStub{}
			rs.set(false, 1, 2)
			e := setupWith(t, func(d *Deps) {
				withFacts(fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}})(d)
				d.DeviceCmd, d.TypedRoutes, d.SysConfig = &fakeCmd{reply: `{"Done":"yes"}`}, rs.routes, svc
				extra(d)
			})
			e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, LastSeen: now}
			return svc, e
		}
	}
	page := func(mk func(t *testing.T) (*sysconfig.Service, *env), method, path string, form func() url.Values) func(t *testing.T) (*sysconfig.Service, func(string) string) {
		return func(t *testing.T) (*sysconfig.Service, func(string) string) {
			svc, e := mk(t)
			return svc, func(role string) string {
				var f url.Values
				if form != nil {
					f = form()
				}
				_, body := e.do(t, method, path, role, f)
				return body
			}
		}
	}
	return []aclPath{
		{"device Back up", 1, page(routed(func(*Deps) {}), "GET", "/devices/1", nil)},
		{"bulk command", 1, page(routed(func(d *Deps) { d.CommandBatch = &fakeBatch{} }), "POST", "/devices/bulk/command",
			func() url.Values { return bulkForm("id", "1", "id", "2", "command", "PowerDelta 10") })},
		// the console page of device 1 carries its own notice and the header's one by Back up (its backup path is MQTT too)
		{"console", 2, page(routed(func(*Deps) {}), "POST", "/devices/1/console",
			func() url.Values { return consoleForm("WebPassword hunter2-880") })},
		{"restore confirmation", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			svc := aclBrokerService(t)
			e, _, rs, _ := newDiffEnvWith(t, svc)
			rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT first (FileUpload)"}
			return svc, func(role string) string { _, body := e.do(t, "GET", "/backups/1/restore", role, nil); return body }
		}},
		{"device MQTT password field", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			e, svc := brokerWithOwnDevice(t)
			return svc, func(role string) string { _, body := e.do(t, "GET", "/devices/7/edit", role, nil); return body }
		}},
		{"bulk backup page", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) {
				d.BackupBatch = &fakeBackupBatch{}
				d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}}
			})
			return svc, func(role string) string {
				_, body := e.do(t, "POST", "/devices/bulk/backup", role, url.Values{"csrf": {"tok"}, "id": {"1"}})
				return body
			}
		}},
		{"fleet backups", 1, func(t *testing.T) (*sysconfig.Service, func(string) string) {
			e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(now) })
			return svc, func(role string) string { _, body := e.do(t, "GET", "/devices/backups", role, nil); return body }
		}},
	}
}

// Why (#880): the notice is one condition everywhere, so hiding it hides it on the confirmations of console,
// bulk command and restore, by the device's Back up button, on the device's MQTT password field, on the bulk backup
// page and on the Backups page too, not only on the page it was made on, and a changed broker brings it back on each of them.
func TestBrokerACLHiddenWarningHidesTheNoticeOnEveryMQTTPath(t *testing.T) {
	for _, p := range aclPaths(time.Now()) {
		t.Run(p.name, func(t *testing.T) {
			svc, get := p.open(t)
			aclCycle(t, svc, p.want, func() int { return strings.Count(get("admin"), "data-acl-warning") })
		})
	}
}

// Why (#962): "Hide it" leads an admin straight to the option, so nobody has to find System, MQTT and the box by hand.
// Only an admin may save that setting: for everyone else the sentence stays plain text, as a link would end on a page
// they are refused. Each page builds the link from its own request context, so every page that carries the notice is
// rendered here: one that lost the principal or the base path would drop the link without anyone noticing.
func TestBrokerACLNoticeLinksOnlyAdminsToTheHideOption(t *testing.T) {
	const link = `<a href="/tf/settings/system?tab=mqtt#mqtt-acl-hide">Hide it</a> under System → MQTT.`
	plain := 0 // the notices an operator saw: the plain text variant must really have been rendered
	for _, p := range aclPaths(time.Now()) {
		t.Run(p.name, func(t *testing.T) {
			_, get := p.open(t)
			if got := strings.Count(get("admin"), link); got != p.want {
				t.Errorf("an admin gets %d links to the option, want %d (one per notice, with the base path)", got, p.want)
			}
			for _, role := range []string{"operator", "viewer"} {
				body := get(role)
				notices := strings.Count(body, "data-acl-warning")
				if role == "operator" {
					plain += notices
				}
				if got := strings.Count(body, ui.BrokerACLHide); got != notices {
					t.Errorf("%s: %d notices, %d say where to hide it", role, notices, got)
				}
				if strings.Contains(body, "mqtt-acl-hide") || strings.Contains(body, ">"+ui.BrokerACLHideLink+"</a>") {
					t.Errorf("%s gets a link to a setting only an admin may save", role)
				}
			}
		})
	}
	if plain == 0 {
		t.Fatal("an operator saw no notice on any page: the plain text variant was not checked")
	}
	// the link has a target: the option exists on the MQTT tab, ticked or not, and also where the broker is not valid
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	for _, step := range []string{"unticked", "ticked", "no valid broker"} {
		switch step {
		case "ticked":
			aclSave(t, e, "broker.lan", "1883", "", true)
		case "no valid broker":
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
		}
		_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
		if strings.Count(body, `id="mqtt-acl-hide"`) != 1 {
			t.Errorf("%s: the MQTT tab has %d elements with the id the link leads to, want 1", step, strings.Count(body, `id="mqtt-acl-hide"`))
		}
	}
}

// Why (#962, the maintainer: "No sentence that says or implies the broker has an ACL, anywhere"): TasmoFleet cannot see
// an ACL, and the switch exists for a broker that has none as much as for one that has. So neither the option, its
// hint, the notice's link nor the audit entry of the save may claim one, and the label and hint are the agreed words.
func TestBrokerACLHideOptionNeverClaimsAnACL(t *testing.T) {
	const label = "Hide the broker ACL warning"
	const hint = "Every client of this broker can read device backups, passwords included. The warning comes back when you change the broker."
	banned := []string{"has an ACL", "have an ACL", "Confirm", "confirm", "CONFIRMED", "Clients read only", "own topics"}
	check := func(what, text string) {
		t.Helper()
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s says %q", what, b)
			}
		}
	}
	f, ok := sysconfig.Lookup("MQTT_ACL_WARNING_HIDDEN")
	if !ok || f.Label != label || f.Help != hint {
		t.Errorf("the field is %q / %q, want the agreed label and hint", f.Label, f.Help)
	}
	check("the field", f.Label+" "+f.Help)
	var aud *fakeAudit
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); aud = d.Recorder.(*fakeAudit) })
	tab := func() string { _, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil); return body }
	for _, step := range []string{"unticked", "ticked", "no valid broker"} {
		switch step {
		case "ticked":
			aclSave(t, e, "broker.lan", "1883", "", true)
		case "no valid broker":
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
		}
		body := tab()
		at := strings.Index(body, `id="mqtt-acl-hide"`)
		end := strings.Index(body[max(at, 0):], "</small>")
		if at < 0 || end < 0 {
			t.Fatalf("the MQTT tab (%s) has no option to look at", step)
		}
		check("the option on the MQTT tab ("+step+")", body[at:at+end])
		if step != "no valid broker" && (!strings.Contains(body, ">"+label+"<") || !strings.Contains(body, hint)) {
			t.Errorf("the MQTT tab (%s) does not show the label and the hint of the option", step)
		}
	}
	// The notice's body says what it warns about, "unless the broker has an ACL": a condition, not a claim. What it
	// says about hiding it is the line that must not claim one.
	hideLine := regexp.MustCompile(`<span class="acl-hide">(.*?)</span>`)
	for _, p := range aclPaths(time.Now()) {
		_, get := p.open(t)
		m := hideLine.FindAllStringSubmatch(get("admin"), -1)
		if len(m) != p.want {
			t.Errorf("%s: %d hide lines, want %d", p.name, len(m), p.want)
		}
		for _, l := range m {
			check("the hide line on "+p.name, l[1])
		}
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	n := 0
	for _, ev := range aud.events {
		if ev.Action == "sysconfig.update" {
			check("the audit entry of a save", ev.Detail)
			n++
		}
	}
	if n == 0 {
		t.Error("no audit entry of a save to check")
	}
}

// Why (#880): unticking the box and saving is how an admin takes the hiding back, for instance after the ACL was
// removed from the broker. The notice must be back at once, on the page that shows it and on the MQTT tab.
func TestBrokerACLHiddenWarningIsTakenBackByUntickingTheBox(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("hiding did not hide the notice (%d, %d)", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "", false) // mqtt.aclhide.seen is posted, mqtt.aclhide is not: the box is unticked
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Errorf("after unticking the box the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
}

// Why (#880): removing the broker (a reset of MQTT_URL) ends the hiding with it. Entering the very same broker
// again later is a new decision: the notice shows until the admin hides it again.
func TestBrokerACLHiddenWarningDoesNotSurviveRemovingTheBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("hiding did not hide the notice (%d)", b)
	}
	// the browser posts the broker fields and the ticked box next to the reset box
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.aclhide": {"1"}, "mqtt.aclhide.seen": {"1"}, "clear.MQTT_URL": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
	if r, body := e.do(t, "POST", "/settings/system?tab=mqtt", "admin", form); r.StatusCode >= 400 {
		t.Fatalf("resetting the broker: %d %s", r.StatusCode, body)
	}
	for _, f := range svc.State() {
		if f.Key == "MQTT_URL" && f.Value != "" {
			t.Fatalf("the reset did not remove the broker: %q", f.Value)
		}
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883"}) // the same broker, entered again
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Errorf("the old hiding came back with the same broker (%d, %d notices)", b, m)
	}
}

// Why (#880): a stored broker URL that does not split into host and port (a path, user information) cannot carry the
// confirmation. A box that saves nothing would be a lie, so it is not offered, and the page says what to do.
func TestBrokerACLBoxIsNotOfferedWithoutAValidBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.aclhide"`) {
		t.Fatal("the box is not offered with a valid broker")
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if strings.Contains(body, `name="mqtt.aclhide"`) || !strings.Contains(body, "Save a valid broker first") {
		t.Error("with a broker URL that cannot be split the page still offers the box, or does not say to save a valid broker")
	}
}

// Why (#880, #962): the second line tells the person who still sees the warning where to hide it, in words that do not
// say the broker has an ACL (TasmoFleet cannot know; the maintainer's wording of #962).
func TestBrokerACLNoticeSaysWhereToHideIt(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	_, body := e.do(t, "GET", "/devices/backups", "operator", nil)
	if !strings.Contains(body, ui.BrokerACLHide) {
		t.Error("the notice by Back up all now does not say where to hide it")
	}
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if strings.Contains(body, ui.BrokerACLHide) {
		t.Error("the MQTT tab sends the person to itself")
	}
}
