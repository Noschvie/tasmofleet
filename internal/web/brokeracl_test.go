package web

import (
	"context"
	"net/url"
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
// confirmed: the state in which the notice shows wherever a path goes over MQTT (#880).
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

func TestFleetBackupsPageCarriesTheBrokerACLWarningOnceByBackUpAll(t *testing.T) {
	for _, role := range []string{"admin", "operator"} {
		t.Run(role, func(t *testing.T) {
			e := fleetEnv(t, fleetFixture(time.Now()))
			_, body := e.do(t, "GET", "/devices/backups", role, nil)
			if n := countACL(t, body); n != 1 {
				t.Fatalf("the Backups page has %d ACL notices, want 1 (by Back up all now; the same words twice on one page read as noise)", n)
			}
			if n := strings.Count(cardOf(t, body, "schedule"), "data-acl-warning"); n != 0 {
				t.Errorf("the Schedule card repeats the notice %d times, it stands once by Back up all now", n)
			}
			if n := strings.Count(cardOf(t, body, "devices"), "data-acl-warning"); n != 1 {
				t.Errorf("the Devices card has %d ACL notices, want 1", n)
			}
			if role == "admin" {
				devs := cardOf(t, body, "devices")
				if strings.Index(devs, "data-acl-warning") > strings.Index(devs, "<table") {
					t.Error("the notice comes after the device table, not by the Back up all now button above it")
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

// aclPages are the pages whose notice the confirmation hides, each with how to ask for it: the Backups page (Back up
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

// aclSave posts the MQTT tab as the browser does: the broker as host, port and TLS, the user, and the confirmation box.
func aclSave(t *testing.T, e *env, host, port, user string, confirm bool) {
	t.Helper()
	form := url.Values{"mqtt.host": {host}, "mqtt.port": {port}, "v.MQTT_USER": {user}, "reuse.MQTT_PASSWORD": {"1"}, "mqtt.acl.seen": {"1"}}
	if confirm {
		form.Set("mqtt.acl", "1")
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

// aclConfirmNow is the admin's confirmation for the broker the service holds now.
func aclConfirmNow(t *testing.T, svc *sysconfig.Service) {
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
	aclApply(t, svc, map[string]string{"MQTT_ACL_CONFIRMED": aclBinding(c)})
}

// aclCycle is what every place of the notice must do (#880): shown with a broker and no confirmation (want notices),
// gone after the confirmation, and back when the broker is changed behind the confirmation's back (by the settings
// service, not by the form that would drop it).
func aclCycle(t *testing.T, svc *sysconfig.Service, want int, count func() int) {
	t.Helper()
	if n := count(); n != want {
		t.Fatalf("before the confirmation: %d notices, want %d", n, want)
	}
	aclConfirmNow(t, svc)
	if n := count(); n != 0 {
		t.Fatalf("after the confirmation: %d notices, want none", n)
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://elsewhere.lan:1883"})
	if n := count(); n != want {
		t.Fatalf("after the broker changed: %d notices, want %d (the confirmation is for the old broker)", n, want)
	}
}

// Why (#880): TasmoFleet cannot see an ACL, so a user whose broker has one would see a wrong red warning on every MQTT
// path for ever. The admin's confirmation hides it, but only for the broker it was given for: another host, port or
// user is another broker, and the notice must be back without anyone remembering to remove the confirmation.
func TestBrokerACLConfirmationHidesTheNoticeOnlyForTheBrokerItWasGivenFor(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("before the confirmation the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Fatalf("a confirmation given in the save that also changed the user hid the notice (%d, %d): it is for the broker as saved before", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "tf", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("after the confirmation the Backups page has %d and the MQTT tab %d notices, want 0 and 0", b, m)
	}
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.acl" value="1" checked`) {
		t.Error("the confirmation box is not ticked after the confirmation")
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
				t.Fatalf("the confirmation did not hide the notice (%d)", b)
			}
			aclSave(t, e, tc.host, tc.port, tc.user, true)
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the Backups page has %d and the MQTT tab %d notices after the change, want 1 and 1", tc.name, b, m)
			}
			// the old broker entered again later (not through the form) must not bring the confirmation back: it
			// was dropped with the change, not kept for the old broker
			aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883", "MQTT_USER": "tf"})
			if b, m := aclShown(t, e); b != 1 || m != 1 {
				t.Errorf("%s: the old confirmation came back with the old broker (%d, %d notices)", tc.name, b, m)
			}
		})
	}
}

// Why (#880): the confirmation is a decision about security, so it is audited like every system setting, and only an
// admin may make it (the settings page is admin only).
func TestBrokerACLConfirmationIsAuditedAndAdminOnly(t *testing.T) {
	var aud *fakeAudit
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()); aud = d.Recorder.(*fakeAudit) })
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.acl": {"1"}, "mqtt.acl.seen": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
	if r, _ := e.do(t, "POST", "/settings/system?tab=mqtt", "operator", form); r.StatusCode != 403 {
		t.Fatalf("an operator saved the confirmation: %d", r.StatusCode)
	}
	if b, _ := aclShown(t, e); b != 1 {
		t.Fatal("an operator's refused request hid the notice")
	}
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("the notice is still shown after the admin confirmed (%d)", b)
	}
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, ev := range aud.events {
		if ev.Action == "sysconfig.update" && strings.Contains(ev.Detail, "MQTT_ACL_CONFIRMED") && ev.Result == "ok" {
			return
		}
	}
	t.Errorf("no audit entry names MQTT_ACL_CONFIRMED: %+v", aud.events)
}

// Why (#880): the notice is one condition everywhere, so a confirmation hides it on the confirmations of console,
// bulk command and restore, by the device's Back up button, on the device's MQTT password field and on the bulk backup
// page too, not only on the page it was made on, and a changed broker brings it back on each of them.
func TestBrokerACLConfirmationHidesTheNoticeOnEveryMQTTPath(t *testing.T) {
	now := time.Now()
	// each case has its own settings service: the cycle changes the broker
	routed := func(extra func(d *Deps)) (*sysconfig.Service, *env) {
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
	t.Run("device Back up", func(t *testing.T) {
		svc, e := routed(func(*Deps) {})
		aclCycle(t, svc, 1, func() int {
			_, body := e.do(t, "GET", "/devices/1", "admin", nil)
			return strings.Count(body, "data-acl-warning")
		})
	})
	t.Run("bulk command", func(t *testing.T) {
		svc, e := routed(func(d *Deps) { d.CommandBatch = &fakeBatch{} })
		aclCycle(t, svc, 1, func() int {
			_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "PowerDelta 10"))
			return strings.Count(body, "data-acl-warning")
		})
	})
	t.Run("console", func(t *testing.T) {
		svc, e := routed(func(*Deps) {})
		// the console page of device 1 carries its own notice and the header's one by Back up (its backup path is MQTT too)
		aclCycle(t, svc, 2, func() int {
			_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("WebPassword hunter2-880"))
			return strings.Count(body, "data-acl-warning")
		})
	})
	t.Run("restore confirmation", func(t *testing.T) {
		svc := aclBrokerService(t)
		e, _, rs, _ := newDiffEnvWith(t, svc)
		rs.assess = restore.Assessment{ViaMQTT: true, PathNote: "over MQTT first (FileUpload)"}
		aclCycle(t, svc, 1, func() int {
			_, body := e.do(t, "GET", "/backups/1/restore", "admin", nil)
			return strings.Count(body, "data-acl-warning")
		})
	})
	t.Run("device MQTT password field", func(t *testing.T) {
		e, svc := brokerWithOwnDevice(t)
		aclCycle(t, svc, 1, func() int {
			_, body := e.do(t, "GET", "/devices/7/edit", "admin", nil)
			return strings.Count(body, "data-acl-warning")
		})
	})
	t.Run("bulk backup page", func(t *testing.T) {
		e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) {
			d.BackupBatch = &fakeBackupBatch{}
			d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: mqttFacts(now, model.MQTTPasswordOwn)}}
		})
		aclCycle(t, svc, 1, func() int {
			_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", url.Values{"csrf": {"tok"}, "id": {"1"}})
			return strings.Count(body, "data-acl-warning")
		})
	})
}

// Why (#880): unticking the box and saving is how an admin takes the confirmation back, for instance after the ACL was
// removed from the broker. The notice must be back at once, on the page that shows it and on the MQTT tab.
func TestBrokerACLConfirmationIsTakenBackByUntickingTheBox(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, m := aclShown(t, e); b != 0 || m != 0 {
		t.Fatalf("the confirmation did not hide the notice (%d, %d)", b, m)
	}
	aclSave(t, e, "broker.lan", "1883", "", false) // mqtt.acl.seen is posted, mqtt.acl is not: the box is unticked
	if b, m := aclShown(t, e); b != 1 || m != 1 {
		t.Errorf("after unticking the box the Backups page has %d and the MQTT tab %d notices, want 1 and 1", b, m)
	}
}

// Why (#880): removing the broker (a reset of MQTT_URL) ends the confirmation with it. Entering the very same broker
// again later is a new decision: the notice shows until the admin confirms again.
func TestBrokerACLConfirmationDoesNotSurviveRemovingTheBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	aclSave(t, e, "broker.lan", "1883", "", true)
	if b, _ := aclShown(t, e); b != 0 {
		t.Fatalf("the confirmation did not hide the notice (%d)", b)
	}
	// the browser posts the broker fields and the ticked box next to the reset box
	form := url.Values{"mqtt.host": {"broker.lan"}, "mqtt.port": {"1883"}, "mqtt.acl": {"1"}, "mqtt.acl.seen": {"1"}, "clear.MQTT_URL": {"1"}, "reuse.MQTT_PASSWORD": {"1"}}
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
		t.Errorf("the old confirmation came back with the same broker (%d, %d notices)", b, m)
	}
}

// Why (#880): a stored broker URL that does not split into host and port (a path, user information) cannot carry the
// confirmation. A box that saves nothing would be a lie, so it is not offered, and the page says what to do.
func TestBrokerACLBoxIsNotOfferedWithoutAValidBroker(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	_, body := e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if !strings.Contains(body, `name="mqtt.acl"`) {
		t.Fatal("the box is not offered with a valid broker")
	}
	aclApply(t, svc, map[string]string{"MQTT_URL": "tcp://broker.lan:1883/path"})
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if strings.Contains(body, `name="mqtt.acl"`) || !strings.Contains(body, "Save a valid broker first") {
		t.Error("with a broker URL that cannot be split the page still offers the box, or does not say to save a valid broker")
	}
}

// Why (#880): the second line tells the person who still sees the warning where to say that the broker is safe.
func TestBrokerACLNoticeSaysWhereToConfirm(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = fleetFixture(time.Now()) })
	_, body := e.do(t, "GET", "/devices/backups", "admin", nil)
	if !strings.Contains(body, ui.BrokerACLConfirm) {
		t.Error("the notice by Back up all now does not say where to confirm the ACL")
	}
	_, body = e.do(t, "GET", "/settings/system?tab=mqtt", "admin", nil)
	if strings.Contains(body, ui.BrokerACLConfirm) {
		t.Error("the MQTT tab sends the person to itself")
	}
}
